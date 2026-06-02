// Package plan reads Claude Code plan files from ~/.claude/plans/ and
// exposes them as Plan records keyed by filename.
//
// Plans are markdown documents Claude CLI generates in plan mode. The
// gateway treats them as read-only — disk is the source of truth, scanner
// only does extraction. Display-friendly title comes from the first
// markdown heading (or the first non-empty line if no heading).
package plan

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultPlansDir is where Claude Code writes plan files. Override only in
// tests.
const DefaultPlansDir = ".claude/plans"

// Plan is the metadata we extract from one plan file. Body is the full
// markdown contents (small files, typically 2-8 KB).
type Plan struct {
	Filename string    // e.g. "snug-yawning-newt.md" — stable id
	Title    string    // first heading or first non-empty line
	MTime    time.Time // file mtime
	Size     int64     // bytes
	Body     string    // full markdown
}

// Index scans a plans directory and returns Plan records. No caching — the
// directory is small (<100 files typically) and Read costs are negligible.
// Callers can cache on their side if needed.
type Index struct {
	dir string
}

// NewIndex constructs an index over dir. Empty dir defaults to
// ~/.claude/plans.
func NewIndex(dir string) *Index {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, DefaultPlansDir)
		}
	}
	return &Index{dir: dir}
}

// Dir returns the resolved directory the index reads from.
func (i *Index) Dir() string { return i.dir }

// List returns all plans sorted newest-first by mtime.
func (i *Index) List() ([]Plan, error) {
	if i.dir == "" {
		return nil, errors.New("plans dir is empty")
	}
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Plan, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p, err := i.read(e.Name())
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].MTime.After(out[b].MTime)
	})
	return out, nil
}

// Get returns one plan by filename (stable id). Filename must include the
// .md extension. Returns an error wrapping os.ErrNotExist if missing.
func (i *Index) Get(filename string) (Plan, error) {
	if i.dir == "" {
		return Plan{}, errors.New("plans dir is empty")
	}
	// Defense: reject path traversal — filename should be a bare basename.
	if strings.ContainsAny(filename, "/\\") || filename == "" {
		return Plan{}, errors.New("invalid filename")
	}
	return i.read(filename)
}

func (i *Index) read(name string) (Plan, error) {
	path := filepath.Join(i.dir, name)
	info, err := os.Stat(path)
	if err != nil {
		return Plan{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Filename: name,
		Title:    extractTitle(body),
		MTime:    info.ModTime(),
		Size:     info.Size(),
		Body:     string(body),
	}, nil
}

// extractTitle returns the first markdown heading (lines starting with #),
// stripped of leading #s and whitespace. Falls back to the first non-empty
// line, then to "(untitled)" if the file is essentially empty.
func extractTitle(body []byte) string {
	var firstNonEmpty string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "# "))
		}
		if firstNonEmpty == "" {
			firstNonEmpty = line
		}
	}
	if firstNonEmpty != "" {
		return firstNonEmpty
	}
	return "(untitled)"
}
