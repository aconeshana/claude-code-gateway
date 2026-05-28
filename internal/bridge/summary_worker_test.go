package bridge

import (
	"context"
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
// rewrites buildSummaryPrompt and forgets the marker would silently
// regress; this test catches that.
//
// Also asserts the marker is identical to what runtime/claude/discoverer.go
// looks for (cross-package coupling, see adminPromptFingerprints).
func TestBuildSummaryPrompt_InjectsAdminMarker(t *testing.T) {
	prompt := buildSummaryPrompt("/some/path.jsonl")
	if !strings.Contains(prompt, AdminSessionMarker) {
		t.Fatalf("prompt missing AdminSessionMarker %q — admin-session fingerprint detection will silently break for sessions spawned outside /tmp", AdminSessionMarker)
	}
	// Marker should appear exactly once — the discoverer counts it as a
	// boolean signal; duplicates won't hurt correctness but suggest the
	// prompt structure drifted.
	if strings.Count(prompt, AdminSessionMarker) != 1 {
		t.Errorf("AdminSessionMarker should appear once, got %d", strings.Count(prompt, AdminSessionMarker))
	}
}

func TestCleanAdminSummary_StripsPrefixesAndQuotes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// Primary path: tag extraction
		{"tag only", "<summary>修复 CI 流程</summary>", "修复 CI 流程"},
		{"tag after preamble", "让我分析一下...\n根据日志:\n<summary>调试 worker 限速</summary>", "调试 worker 限速"},
		{"tag with newlines inside", "<summary>修复\n飞书\n卡片</summary>", "修复\n飞书\n卡片"},
		{"tag with surrounding text", "好的,这是答案:\n\n<summary>重构 session manager</summary>\n\n说明: 略", "重构 session manager"},
		{"tag with wrapping quotes inside", `<summary>"修复 bug"</summary>`, "修复 bug"},

		// No tag → empty (strict, no fallback). Polluting summaries with
		// last-line heuristics caused the v6 disaster.
		{"no tag plain text", "已修复 bug", ""},
		{"no tag with preamble", "Summary: 添加摘要功能", ""},
		{"no tag truncated mid-sentence", "现在我明白了。这个 session 是在...", ""},
		{"empty", "", ""},
		{"only whitespace", "   \n\n  ", ""},
		// Truncated tag (the actual v6 failure mode — admin wrote thinking
		// >60 runes and our cap chopped the closing tag).
		{"truncated tag start", "<summary>真正摘要", ""},
		{"truncated tag end", "summary>实际摘要</summa", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cleanAdminSummary(c.in)
			if got != c.want {
				t.Errorf("cleanAdminSummary(%q) = %q, want %q", c.in, got, c.want)
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
