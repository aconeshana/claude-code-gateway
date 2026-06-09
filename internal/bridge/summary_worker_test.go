package bridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	rfake "github.com/anthropics/claude-code-gateway/internal/runtime/fake"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// TestBuildSummaryPrompt_InjectsAdminMarker is the canary for our two-layer
// admin-detection scheme. The worker prompt MUST start (or near-start) with
// AdminSessionMarker — otherwise admin-spawned sessions are only detected
// by cwd-prefix, losing the fingerprint fallback. A future refactor that
// rewrites buildRecapPrompt and forgets the marker would silently
// regress; this test catches that.
//
// Also asserts the marker is identical to what runtime/claude/discoverer.go
// looks for (cross-package coupling, see adminPromptFingerprints).
func TestBuildSummaryPrompt_InjectsAdminMarker(t *testing.T) {
	// Write a minimal JSONL file with one user turn so buildRecapPrompt
	// doesn't early-return "" (it returns "" for empty/missing files).
	f, err := os.CreateTemp(t.TempDir(), "*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","isMeta":false,"isSidechain":false,"message":{"role":"user","content":"test prompt"}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	f.Close()

	prompt := buildRecapPrompt(f.Name())
	if !strings.Contains(prompt, AdminSessionMarker) {
		t.Fatalf("prompt missing AdminSessionMarker %q — admin-session fingerprint detection will silently break for sessions spawned outside /tmp", AdminSessionMarker)
	}
	if strings.Count(prompt, AdminSessionMarker) != 1 {
		t.Errorf("AdminSessionMarker should appear once, got %d", strings.Count(prompt, AdminSessionMarker))
	}
}

func TestCleanSummaryOutput_StripsPrefixesAndQuotes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// Plain text output — the new contract: admin writes directly.
		{"plain text", "You're fixing the CI pipeline in summary_worker.go.", "You're fixing the CI pipeline in summary_worker.go."},
		{"strips surrounding whitespace", "  修复 CI 流程  ", "修复 CI 流程"},
		{"strips backtick wrapping", "`调试 worker 限速`", "调试 worker 限速"},
		{"strips double-quote wrapping", `"重构 session manager"`, "重构 session manager"},
		{"strips single-quote wrapping", `'修复 bug'`, "修复 bug"},
		{"multiline kept", "你在重构 session manager。\n下一步是更新 persist 层。", "你在重构 session manager。\n下一步是更新 persist 层。"},
		{"empty", "", ""},
		{"only whitespace", "   \n\n  ", ""},
		{"skip meta sentinel", "_skip_meta_", "_skip_meta_"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cleanSummaryOutput(c.in)
			if got != c.want {
				t.Errorf("cleanSummaryOutput(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSummaryWorker_EnqueueAndStats(t *testing.T) {
	mgr := newWorkerTestManager(t)
	// Worker without admin: regenerate will return "admin not configured".
	w := newSummaryWorker(mgr, nil, nil)
	w.Enqueue(summaryJob{SessionID: "x", SourceRef: "/tmp/sess.jsonl"})

	stats := w.Stats()
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1", stats.Pending)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := w.Stats()
		if s.Failed == 1 && s.Pending == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("worker did not process job: stats = %+v", w.Stats())
}

func TestSummaryWorker_QueueFullCountsFailure(t *testing.T) {
	mgr := newWorkerTestManager(t)
	w := newSummaryWorker(mgr, nil, nil)
	for i := 0; i < cap(w.queue); i++ {
		w.queue <- summaryJob{SessionID: "x"}
	}
	w.Enqueue(summaryJob{SessionID: "overflow"})

	if w.Stats().Failed != 1 {
		t.Errorf("expected Failed=1 from queue overflow, got %d", w.Stats().Failed)
	}
}

func newWorkerTestManager(t *testing.T) *session.Manager {
	t.Helper()
	rt := rfake.NewRuntime(claude.Codec{})
	return session.NewManager(rt, t.TempDir(), "default", 8, 0, 0)
}
