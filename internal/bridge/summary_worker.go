package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

// summaryJob is a single unit of work for the summary worker: regenerate a
// summary for sessionID by reading the last few exchanges from sourceRef.
type summaryJob struct {
	SessionID    string
	CLISessionID string // stable fallback: CLI session ID from the jsonl; used when gateway UUID is no longer in manager
	SourceRef    string // jsonl path (for external sessions)
}

// SummaryPromptVersion tracks the prompt-engineering iteration. Bump when
// the prompt changes meaningfully, the admin model changes, or admin.query
// result extraction changes — discovery will re-enqueue any session whose
// persisted PromptVersion is lower than the current one.
//
// v7: jq-based prompt (requires jq binary, 12-20 char output)
// v8: Pure-Go JSONL parsing, no jq dependency. awaySummary-style prompt:
//
//	2-3 sentences describing the task and next step (官方 recap 风格).
//	Removed <summary> tag requirement — admin outputs plain text directly.
const SummaryPromptVersion = 8

// summaryWorker generates session summaries asynchronously. A single
// goroutine consumes the queue serially to avoid hammering the admin LLM.
//
// Rate limiting: each successful query is followed by a Sleep(rate) tick.
// During the "incremental" mode (queue length < 10), rate drops to 0 to keep
// up with low-volume churn.
type summaryWorker struct {
	mgr     *session.Manager
	admin   *admin
	persist *persist.JSONStore

	queue chan summaryJob

	rateMu sync.Mutex
	rate   time.Duration // sleep between jobs; 0 = no sleep

	mu    sync.Mutex
	stats WorkerStats
}

// WorkerStats is exported so /status can read a snapshot.
type WorkerStats struct {
	Pending     int       `json:"pending"`
	Done        int       `json:"done"`
	Failed      int       `json:"failed"`
	Started     time.Time `json:"started"`
	LastUpdate  time.Time `json:"last_update"`
	LastError   string    `json:"last_error,omitempty"`
	CurrentRate string    `json:"current_rate,omitempty"`
}

func newSummaryWorker(mgr *session.Manager, ad *admin, p *persist.JSONStore) *summaryWorker {
	return &summaryWorker{
		mgr:     mgr,
		admin:   ad,
		persist: p,
		queue:   make(chan summaryJob, 500),
		// No rate limit by default: each admin.query takes 20-30s with sonnet
		// (the model itself paces us). Adding sleep on top just slows down
		// the catch-up after a fresh discovery scan without protecting any
		// real quota — admin runs one job at a time inside this single
		// goroutine.
		rate:  0,
		stats: WorkerStats{Started: time.Now()},
	}
}

// Enqueue submits a job. Non-blocking; drops the job (and counts a failure)
// when the buffer is full.
func (w *summaryWorker) Enqueue(j summaryJob) {
	if w == nil {
		return
	}
	select {
	case w.queue <- j:
		w.mu.Lock()
		w.stats.Pending++
		w.stats.LastUpdate = time.Now()
		w.mu.Unlock()
	default:
		w.mu.Lock()
		w.stats.Failed++
		w.stats.LastError = "queue full"
		w.stats.LastUpdate = time.Now()
		w.mu.Unlock()
	}
}

// Stats returns a snapshot for /status.
func (w *summaryWorker) Stats() WorkerStats {
	if w == nil {
		return WorkerStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.stats
	w.rateMu.Lock()
	if w.rate == 0 {
		s.CurrentRate = "no limit"
	} else {
		s.CurrentRate = fmt.Sprintf("%s/job", w.rate)
	}
	w.rateMu.Unlock()
	return s
}

// Run consumes the queue until ctx is canceled.
func (w *summaryWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.queue:
			w.process(ctx, job)
		}
	}
}

func (w *summaryWorker) process(ctx context.Context, job summaryJob) {
	w.mu.Lock()
	w.stats.Pending--
	w.stats.LastUpdate = time.Now()
	w.mu.Unlock()

	// Rate is set once at construction (default 0 = no sleep, model paces us).
	// Read under the lock so /config can mutate it without races.
	w.rateMu.Lock()
	rate := w.rate
	w.rateMu.Unlock()

	t0 := time.Now()
	log.Printf("[summary-worker] start job %s", job.SessionID)
	if err := w.regenerate(ctx, job); err != nil {
		w.mu.Lock()
		w.stats.Failed++
		w.stats.LastError = err.Error()
		w.stats.LastUpdate = time.Now()
		w.mu.Unlock()
		log.Printf("[summary-worker] job %s failed after %v: %v", job.SessionID, time.Since(t0), err)
	} else {
		w.mu.Lock()
		w.stats.Done++
		w.stats.LastUpdate = time.Now()
		w.mu.Unlock()
		log.Printf("[summary-worker] job %s done in %v", job.SessionID, time.Since(t0))
	}
	if rate > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(rate):
		}
	}
}

// RefreshNow synchronously regenerates the summary for sessionID using
// sourceRef as the transcript file. Bypasses the worker queue — used by the
// [刷新摘要] button so user clicks aren't blocked behind background tasks.
// admin.query itself is still serialized via admin.mu (one CLI process),
// so a refresh may briefly wait for an in-flight worker job to finish, but
// won't be queued behind pending ones.
func (w *summaryWorker) RefreshNow(ctx context.Context, sessionID, sourceRef string) error {
	return w.regenerate(ctx, summaryJob{SessionID: sessionID, SourceRef: sourceRef})
}

func (w *summaryWorker) regenerate(ctx context.Context, job summaryJob) error {
	if w.admin == nil {
		return fmt.Errorf("admin not configured")
	}
	if job.SourceRef == "" {
		return fmt.Errorf("missing source ref")
	}
	// Hard belt: refuse to summarize admin-internal sessions. Source of
	// truth is the session's Origin field — not a path string match.
	// Discovery should already filter these out before enqueueing, but if
	// one slips through we don't want to burn an admin round on it.
	if sess, ok := w.mgr.Get(job.SessionID); ok {
		if sess.Info().Origin == session.OriginAdmin {
			return fmt.Errorf("skipped admin session: %s", job.SessionID)
		}
	}
	// Delegate file reading to the admin session itself — it has Bash/Read
	// tools and understands jsonl natively. We give it a strict step-by-step
	// recipe AND require it to wrap the final answer in <summary>…</summary>
	// so we can extract it deterministically (the model is free to think out
	// loud beforehand; we only keep what's between the tags).
	prompt := buildRecapPrompt(job.SourceRef)
	if prompt == "" {
		return fmt.Errorf("transcript empty or unreadable: %s", job.SourceRef)
	}
	result, err := w.admin.query(ctx, prompt)
	if err != nil {
		return err
	}
	summary := cleanSummaryOutput(result)
	if summary == "" {
		return fmt.Errorf("admin returned empty summary (raw len=%d)", len([]rune(result)))
	}
	if summary == "_skip_meta_" {
		// Admin classified this as a meta session (worker / eval session
		// analyzing other sessions, OR a real user session too short/empty
		// to summarize). Don't burn future retries — record an empty
		// summary at the current PromptVersion so discovery treats this
		// as "done". The UI's renderSessionTitle uses fallback text when
		// summary is empty, so the user sees something sensible.
		if cliID := cliIDFromJob(w.mgr, job); cliID != "" {
			w.mgr.SetExternalSummary(cliID, session.ExternalAugmentation{
				Summary:       "",
				PromptVersion: SummaryPromptVersion,
			})
			if w.persist != nil {
				_ = w.persist.Save(w.mgr)
			}
		}
		return nil
	}
	if r := []rune(summary); len(r) > 150 {
		summary = string(r[:150]) + "…"
	}
	if err := w.mgr.SetSummary(job.SessionID, summary); err != nil {
		// Session was removed from manager after the job was enqueued (e.g.
		// destroyed or restarted). The summary was generated successfully —
		// still persist it via the stable CLISessionID so discovery won't
		// re-enqueue on the next scan.
		if cliID := cliIDFromJob(w.mgr, job); cliID != "" {
			log.Printf("[summary-worker] session %s gone from manager, persisting via CLISessionID %s", job.SessionID, cliID)
			w.mgr.SetExternalSummary(cliID, session.ExternalAugmentation{
				Summary:       summary,
				PromptVersion: SummaryPromptVersion,
			})
			if w.persist != nil {
				_ = w.persist.Save(w.mgr)
			}
			return nil
		}
		return err
	}
	if cliID := cliIDFromJob(w.mgr, job); cliID != "" {
		w.mgr.SetExternalSummary(cliID, session.ExternalAugmentation{
			Summary:       summary,
			PromptVersion: SummaryPromptVersion,
		})
	}
	if w.persist != nil {
		_ = w.persist.Save(w.mgr)
	}
	return nil
}

// claudeIDOf looks up the runtime (claude) session id for a gateway session.
// Returns empty string when the session is gone, in which case the caller
// should skip persistence.
func claudeIDOf(mgr *session.Manager, sessionID string) string {
	sess, ok := mgr.Get(sessionID)
	if !ok {
		return ""
	}
	return sess.Info().CLISessionID
}

// cliIDFromJob resolves the CLI session ID for a job. It first tries the
// in-memory manager (works for active sessions); falls back to the stable
// CLISessionID carried in the job itself (works after the gateway UUID is
// gone from the manager, e.g. session was destroyed between enqueue and run).
func cliIDFromJob(mgr *session.Manager, job summaryJob) string {
	if cliID := claudeIDOf(mgr, job.SessionID); cliID != "" {
		return cliID
	}
	return job.CLISessionID
}

// AdminSessionMarker is a stable substring we inject at the head of every
// worker prompt. It's the canonical signal for "this jsonl belongs to a
// gateway-internal admin session" — runtime/claude/discoverer.go grep for
// it as a fingerprint. Keep stable across prompt versions; if you need to
// change the marker, bump the suffix (v2, v3, ...) AND update the
// discoverer fingerprint list in the same commit.
const AdminSessionMarker = "[GATEWAY_ADMIN_SESSION_v1]"

// recapMessageWindow is the number of recent turns passed to the LLM.
// Matches the official CLI awaySummary constant (30 messages ≈ 15 exchanges).
const recapMessageWindow = 30

// buildRecapPrompt builds the summary/recap prompt used by both the summary
// worker (background, triggered by SUMMARY_INTERVAL) and the /recap command
// (on-demand). The conversation context is read from the JSONL file and
// embedded directly — no tools, no jq, admin acts as a pure text LLM.
//
// Prompt taken verbatim from the official Claude Code source:
//   File:    src/services/awaySummary.ts :: buildAwaySummaryPrompt()
//   Version: Claude Code v2.1.88
//
// The only difference from the official implementation: the official version
// optionally prepends a "Session memory" block read from CLAUDE.md/memory
// files. We omit that block for now.
//
// Returns "" when the transcript file is empty or unreadable (caller should
// treat as _skip_meta_).
func buildRecapPrompt(jsonlPath string) string {
	turns := readRecentTurns(jsonlPath, recapMessageWindow)
	if turns == "" {
		return ""
	}
	// The leading AdminSessionMarker is injected so discoverer.go can
	// identify this as a gateway-internal admin session and skip it from
	// /list. It is NOT part of the official prompt.
	return AdminSessionMarker + "\n" +
		"The following is the recent conversation from a claude-code session:\n\n" +
		turns + "\n\n" +
		"The user stepped away and is coming back. Write exactly 1-3 short sentences. " +
		"Start by stating the high-level task — what they are building or debugging, not implementation details. " +
		"Next: the concrete next step. " +
		"Skip status reports and commit recaps.\n" +
		"If the entire session is a meta task where AI is analyzing other sessions (e.g. running tail/jq on jsonl files), output _skip_meta_ instead.\n" +
		"Output the description directly, no explanation, no prefix."
}

// cleanSummaryOutput trims whitespace and quotes from the admin's plain-text
// output. Unlike the old cleanAdminSummary, there is no <summary> tag to
// extract — admin outputs the summary directly as its final response.
func cleanSummaryOutput(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	return strings.TrimSpace(s)
}

// buildSummaryPrompt is kept as a thin alias for buildRecapPrompt so that
// existing callers (eval_exports.go, admin.go updateSummary) compile without
// change during the migration. Will be removed once all callers are updated.
//
// Deprecated: call buildRecapPrompt directly.
func buildSummaryPrompt(jsonlPath string) string {
	return buildRecapPrompt(jsonlPath)
}

// cleanAdminSummary is kept for eval_exports.go compatibility.
//
// Deprecated: cleanSummaryOutput replaces this.
func cleanAdminSummary(s string) string {
	return cleanSummaryOutput(s)
}
