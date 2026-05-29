// Package claude — discoverer.go scans ~/.claude/projects/*/*.jsonl to enumerate
// sessions that exist on disk. The result feeds bridge.runDiscovery, which
// imports them into session.Manager as Origin="external" sessions.
//
// Layout assumptions verified against claude-code 2.1.x:
//
//	~/.claude/projects/
//	  <escaped-cwd>/                          # e.g. -Users-xmly-weflow
//	    <session-uuid>.jsonl                  # top-level session transcript
//	    <session-uuid>/                       # fork/subagent data, ignored
//
// Each transcript is append-only JSONL with one record per line.
// Field extraction mirrors claude's own listSessionsImpl precedence:
//
//	customTitle (tail then head) > aiTitle (tail then head) > lastPrompt > firstPrompt
//
// See claude-code/src/utils/listSessionsImpl.ts:97-119.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

const (
	// projectsDirDefault is the default location relative to user home.
	projectsDirDefault = ".claude/projects"

	// headBytes is how much of the head we slurp to find cwd / firstPrompt.
	// Most files have cwd on line 3, well within 16KB.
	headBytes = 16 * 1024
	// tailBytes is how much of the tail we read for customTitle/aiTitle/lastPrompt.
	// Title lines are small JSON objects (<200B) appended at the end.
	tailBytes = 64 * 1024

	// summaryTruncate is the max rune length for InitialSummary.
	summaryTruncate = 80
)

// Discoverer scans claude-code session transcripts on disk.
type Discoverer struct {
	projectsDir string
	cache       *MTimeCache
}

// SessionJSONLPath returns the on-disk transcript path for a claude-code
// session given its working directory and runtime (CLI) session id. The
// claude CLI encodes the workdir by replacing "/" with "-" and writes
// transcripts under ~/.claude/projects/<encoded>/<id>.jsonl.
//
// This is the canonical implementation. persist.SessionJSONLPath delegates
// here to avoid duplication; bridge code that needs the path should import
// this package directly.
func SessionJSONLPath(workingDir, cliSessionID string) string {
	if cliSessionID == "" || workingDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	encoded := strings.ReplaceAll(workingDir, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded, cliSessionID+".jsonl")
}

// NewDiscoverer returns a Discoverer for the claude runtime.
// projectsDir, if empty, defaults to ~/.claude/projects.
// cachePath, if empty, defaults to ~/.ccg/.gateway-discovery-cache.json.
func NewDiscoverer(projectsDir, cachePath string) *Discoverer {
	if projectsDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			projectsDir = filepath.Join(home, projectsDirDefault)
		}
	}
	if cachePath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cachePath = filepath.Join(home, ".ccg", ".gateway-discovery-cache.json")
		}
	}
	return &Discoverer{
		projectsDir: projectsDir,
		cache:       NewMTimeCache(cachePath),
	}
}

// Name implements runtime.Discoverer.
func (d *Discoverer) Name() string { return "claude-code" }

// Scan walks the projects directory and returns one DiscoveredSession per
// top-level jsonl file (fork subdirectories are not descended into).
func (d *Discoverer) Scan(ctx context.Context, opts runtime.ScanOpts) ([]runtime.DiscoveredSession, error) {
	if d.projectsDir == "" {
		return nil, errors.New("projects dir is empty (no home directory)")
	}
	if _, err := os.Stat(d.projectsDir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	_ = d.cache.Load()

	var cutoff time.Time
	if opts.WindowDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -opts.WindowDays)
	}

	entries, err := os.ReadDir(d.projectsDir)
	if err != nil {
		return nil, fmt.Errorf("read projects dir: %w", err)
	}

	var results []runtime.DiscoveredSession
	for _, projEntry := range entries {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if !projEntry.IsDir() {
			continue
		}
		projDir := filepath.Join(d.projectsDir, projEntry.Name())
		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projDir, f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}
			mtime := info.ModTime()
			if !cutoff.IsZero() && mtime.Before(cutoff) {
				continue
			}

			// Cache lookup
			if cached, ok := d.cache.Get(path, mtime); ok {
				results = append(results, cached)
				continue
			}

			rec, err := parseSession(path, mtime)
			if err != nil || rec.RuntimeID == "" {
				continue
			}
			d.cache.Put(path, mtime, rec)
			results = append(results, rec)
		}
	}

	if err := d.cache.Save(); err != nil {
		// non-fatal: cache is a perf optimization
		_ = err
	}
	return results, nil
}

// parseSession reads head+tail of the jsonl file and extracts metadata
// without parsing every line.
func parseSession(path string, mtime time.Time) (runtime.DiscoveredSession, error) {
	rec := runtime.DiscoveredSession{
		RuntimeID:    strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		LastActivity: mtime,
		SourceRef:    path,
	}

	f, err := os.Open(path)
	if err != nil {
		return rec, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return rec, err
	}
	size := info.Size()
	if size == 0 {
		return rec, errors.New("empty file")
	}

	// Read head
	head := make([]byte, min64(headBytes, size))
	if _, err := io.ReadFull(f, head); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return rec, err
	}

	// Read tail (may overlap with head for small files; that's fine)
	tailOffset := int64(0)
	if size > tailBytes {
		tailOffset = size - tailBytes
	}
	tail := make([]byte, size-tailOffset)
	if _, err := f.ReadAt(tail, tailOffset); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return rec, err
	}

	rec.WorkingDir = extractFirstField(head, "cwd")
	rec.CustomTitle = lastNonEmpty(
		extractLastField(tail, "customTitle"),
		extractLastField(head, "customTitle"),
	)
	// InitialSummary: prefer the tail lastPrompt (most recent action); fall
	// back to first user message if no lastPrompt was ever written.
	summary := lastNonEmpty(
		extractLastField(tail, "lastPrompt"),
		extractFirstUserText(head),
	)
	rec.InitialSummary = truncateRunes(summary, summaryTruncate)

	// Detect admin-internal sessions: the gateway's own summary worker
	// drives an admin claude-code session, which itself shows up on disk.
	// Two signals:
	//   1. cwd matches AdminWorkdirPrefix (primary — survives prompt changes)
	//   2. transcript contains worker-prompt fingerprint (fallback for
	//      sessions started before the dedicated workdir was introduced)
	if rec.WorkingDir != "" && strings.HasPrefix(rec.WorkingDir, AdminWorkdirPrefix) {
		rec.IsAdminInternal = true
	} else if isAdminInternal(head, tail) {
		rec.IsAdminInternal = true
	}

	// Count user-authored turns by streaming the whole file once and looking
	// for `"type":"user"` lines. This is the cheapest accurate measure of
	// "how much real conversation happened" — admin-internal sessions are
	// already filtered above so this number reflects actual user turns.
	// Skip the count for admin-internal records to save the I/O.
	if !rec.IsAdminInternal {
		if n, err := countUserTurns(path); err == nil {
			rec.MessageCount = n
		}
	}

	return rec, nil
}

// countUserTurns streams the jsonl file and counts lines whose `type` field
// equals "user" — i.e. user-authored conversation turns. Cheap (~50ms for a
// 50MB file). Returns 0 + error when the file can't be opened.
func countUserTurns(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<22) // 4MB max line
	const needle = `"type":"user"`
	n := 0
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), []byte(needle)) {
			n++
		}
	}
	return n, scanner.Err()
}

// AdminWorkdirPrefix mirrors bridge.AdminWorkdirPrefix. Kept as a copy here
// to avoid an import cycle (bridge depends on runtime; runtime can't import
// bridge). If the bridge constant changes, update this in lockstep.
const AdminWorkdirPrefix = "/tmp/claude-code-gateway-admin"

// adminPromptFingerprints are stable markers used to identify admin-internal
// transcripts. Two strategies:
//
//  1. AdminSessionMarker (`[GATEWAY_ADMIN_SESSION_v1]`) — injected at the head
//     of every worker prompt. Doesn't change when prompt wording is tuned;
//     bump the suffix (v2, ...) only if you intentionally invalidate detection
//     for a fleet rotation.
//  2. Legacy fingerprints — substrings from pre-marker prompt versions, kept
//     so old transcripts on disk are still classified correctly without
//     needing a one-shot migration. Safe to drop once they're cleaned up.
//
// Both lists are checked; any hit flags the session as admin-internal.
var adminPromptFingerprints = []string{
	"[GATEWAY_ADMIN_SESSION_v1]", // stable marker — preferred
	// legacy (pre-marker) — remove after enough time has passed
	"总结一个 claude-code session",
	"总结这个 claude-code session", // 旧 admin worker 用 "这个"，跟 "一个" 差一字
	"30 字内中文总结",                 // admin worker summary task
	"运行 `tail -n",                 // admin worker scans other session jsonls
	`jq -r 'select((.type == "user"`,
	"_skip_meta_",
}

func isAdminInternal(head, tail []byte) bool {
	for _, fp := range adminPromptFingerprints {
		if bytes.Contains(head, []byte(fp)) || bytes.Contains(tail, []byte(fp)) {
			return true
		}
	}
	return false
}

// extractFirstField scans line-by-line and returns the value of the first
// JSON object that contains `field`.
func extractFirstField(buf []byte, field string) string {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			continue
		}
		if raw, ok := m[field]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// extractLastField scans line-by-line and returns the value of the last JSON
// object that contains `field`.
func extractLastField(buf []byte, field string) string {
	var found string
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			continue
		}
		if raw, ok := m[field]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				found = s
			}
		}
	}
	return found
}

// extractFirstUserText finds the first non-meta user message and returns its
// content as a string. Handles both string content and content blocks.
func extractFirstUserText(buf []byte) string {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var d struct {
			Type    string          `json:"type"`
			IsMeta  bool            `json:"isMeta"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			continue
		}
		if d.Type != "user" || d.IsMeta {
			continue
		}
		// Try string content first
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(d.Message, &msg); err != nil {
			continue
		}
		var asStr string
		if err := json.Unmarshal(msg.Content, &asStr); err == nil && asStr != "" {
			return asStr
		}
		// Try blocks
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					return b.Text
				}
			}
		}
	}
	return ""
}

func lastNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
