package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/session"
)

func TestBuildProcessingHUDNote_AllFields(t *testing.T) {
	turnStart := time.Now().Add(-17 * time.Minute)
	lastEvent := time.Now().Add(-30 * time.Second)
	got := buildProcessingHUDNote(
		"claude-sonnet-4-6",
		"main",
		64,
		"Bash · tcpdump…",
		turnStart,
		lastEvent,
	)
	checks := []string{
		"sonnet-4-6",   // model short form
		"git:(main)",   // branch
		"64%",          // context pct
		"🔧 Bash",      // current tool with prefix
		"tcpdump",      // tool arg preview
		"已运行 17m",    // elapsed
		"30s ago",      // since last event
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("HUD missing %q in: %q", want, got)
		}
	}
}

func TestBuildProcessingHUDNote_NoToolFallsBackToProcessing(t *testing.T) {
	turnStart := time.Now().Add(-3 * time.Minute)
	got := buildProcessingHUDNote("claude-sonnet-4-6", "", -1, "", turnStart, time.Time{})
	if !strings.Contains(got, "处理中...") {
		t.Errorf("no tool → want generic '处理中...' suffix, got %q", got)
	}
	if !strings.Contains(got, "已运行 3m") {
		t.Errorf("want 已运行 3m, got %q", got)
	}
}

func TestBuildProcessingHUDNote_ZeroTimeOmitsElapsed(t *testing.T) {
	got := buildProcessingHUDNote("claude-sonnet-4-6", "", -1, "", time.Time{}, time.Time{})
	if strings.Contains(got, "已运行") {
		t.Errorf("zero turnStart should omit elapsed cell, got %q", got)
	}
}

func TestBuildProcessingHUDNote_RecentEventHidesAgoCell(t *testing.T) {
	// Sub-5s "ago" is noise — should be hidden.
	turnStart := time.Now().Add(-10 * time.Second)
	lastEvent := time.Now().Add(-1 * time.Second)
	got := buildProcessingHUDNote("claude-sonnet-4-6", "", -1, "", turnStart, lastEvent)
	if strings.Contains(got, "ago") {
		t.Errorf("event within 5s should not show 'ago', got %q", got)
	}
}

func TestFormatShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{72*time.Minute + 15*time.Second, "1h12m"},
		{3 * time.Hour, "3h"},
	}
	for _, c := range cases {
		if got := formatShortDuration(c.d); got != c.want {
			t.Errorf("formatShortDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestExtractCurrentTool_Bash(t *testing.T) {
	raw := json.RawMessage(`{
		"message": {
			"content": [
				{"type": "text", "text": "I'll run that command."},
				{"type": "tool_use", "name": "Bash", "input": {"command": "tcpdump -i en0 udp port 9993"}}
			]
		}
	}`)
	got := extractCurrentTool(raw)
	if !strings.HasPrefix(got, "Bash · tcpdump") {
		t.Errorf("got %q, want Bash · tcpdump… prefix", got)
	}
}

func TestExtractCurrentTool_Edit_StripsPath(t *testing.T) {
	raw := json.RawMessage(`{
		"message": {
			"content": [{"type": "tool_use", "name": "Edit", "input": {"file_path": "/Users/xmly/weflow/claude-code-gateway/internal/bridge/renderer.go"}}]
		}
	}`)
	got := extractCurrentTool(raw)
	want := "Edit · renderer.go"
	if got != want {
		t.Errorf("got %q, want %q (should strip dir)", got, want)
	}
}

func TestExtractCurrentTool_TextOnlyReturnsEmpty(t *testing.T) {
	raw := json.RawMessage(`{"message": {"content": [{"type": "text", "text": "hello"}]}}`)
	if got := extractCurrentTool(raw); got != "" {
		t.Errorf("text-only message → want \"\", got %q", got)
	}
}

func TestExtractCurrentTool_LastWins(t *testing.T) {
	// Multi-tool turns: caller wants the most recent (latest in stream).
	raw := json.RawMessage(`{
		"message": {
			"content": [
				{"type": "tool_use", "name": "Grep", "input": {"pattern": "foo"}},
				{"type": "tool_use", "name": "Read", "input": {"file_path": "a.go"}}
			]
		}
	}`)
	got := extractCurrentTool(raw)
	if !strings.HasPrefix(got, "Read") {
		t.Errorf("got %q, want Read… (last tool_use wins)", got)
	}
}

func TestHasOnlyText(t *testing.T) {
	textOnly := json.RawMessage(`{"message": {"content": [{"type": "text", "text": "hi"}]}}`)
	if !hasOnlyText(textOnly) {
		t.Errorf("text-only message should return true")
	}
	mixed := json.RawMessage(`{"message": {"content": [{"type": "text", "text": "hi"}, {"type": "tool_use", "name": "Bash"}]}}`)
	if hasOnlyText(mixed) {
		t.Errorf("mixed message (text + tool_use) should return false")
	}
	empty := json.RawMessage(`{"message": {"content": []}}`)
	if hasOnlyText(empty) {
		t.Errorf("empty content should return false")
	}
}

// TestEnsureProgressCard_ToolOnlyAssistantTriggersCard is the regression
// test for the "6 minutes of silence" bug: when the model goes straight
// to a tool_use without producing any user-visible text first, we must
// still render an initial progress card so the user sees that the
// agent is working. Without this, a turn opening with a long Bash
// (e.g. a multi-minute tcpdump) leaves the chat dead until either the
// tool finishes or the agent finally speaks.
func TestEnsureProgressCard_ToolOnlyAssistantTriggersCard(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	state := &streamState{
		sess:         sess,
		project:      "proj",
		sessionShort: "abc12345",
		contextPct:   0,
	}

	// Simulate the CLI emitting an assistant message that is purely a
	// tool_use (the failure mode from the production log).
	raw := json.RawMessage(`{
		"type": "assistant",
		"message": {
			"content": [{"type": "tool_use", "name": "Bash", "input": {"command": "tcpdump -i en0"}}]
		}
	}`)
	b.handleSessionEvent(context.Background(), sess, "c1", state, raw)

	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("expected 1 outbound (initial progress card), got %d", len(out))
	}
	if out[0].Card == nil {
		t.Fatalf("outbound has no card: %+v", out[0])
	}
	if !strings.HasPrefix(out[0].Card.Title, "Processing") {
		t.Errorf("card title = %q, want 'Processing…' prefix", out[0].Card.Title)
	}
	// The HUD note should advertise the running tool so the user can
	// see what the agent is busy with.
	var hudNote string
	for _, sec := range out[0].Card.Sections {
		if sec.Note != "" {
			hudNote = sec.Note
		}
	}
	if !strings.Contains(hudNote, "Bash") {
		t.Errorf("HUD note should advertise current tool 'Bash', got %q", hudNote)
	}

	// The streamState must remember the messageID so subsequent appendText
	// reuses (rather than re-creating) the card.
	state.mu.Lock()
	gotMsgID := state.messageID
	state.mu.Unlock()
	if gotMsgID == "" {
		t.Errorf("expected streamState.messageID to be set after card send")
	}
	// Stop heartbeat to avoid background activity bleeding into next test.
	state.flush()
}

// TestEnsureProgressCard_NoToolNoCard guards against accidentally sending
// blank cards: assistant messages that are pure thinking (no tool_use,
// no text) must not trigger the empty card path.
func TestEnsureProgressCard_NoToolNoCard(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	state := &streamState{sess: sess, project: "proj", sessionShort: "abc"}

	// Empty content array — no text, no tool. Pre-fix code returned early;
	// post-fix should also return early because there's no tool to advertise.
	raw := json.RawMessage(`{"type": "assistant", "message": {"content": []}}`)
	b.handleSessionEvent(context.Background(), sess, "c1", state, raw)

	if got := len(ch.Outbound()); got != 0 {
		t.Errorf("empty assistant message should not produce a card, got %d", got)
	}
	state.flush()
}
