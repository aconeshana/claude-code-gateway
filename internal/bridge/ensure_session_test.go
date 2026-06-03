package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// TestEnsureCurrentSession_AutoResumesIdle covers the core auto-resume
// promise: when a user runs a session-dependent command without a
// currently-focused session, the bridge falls back to ResolveResumable,
// sets focus, and (when mustBeActive=true) reactivates — so the command
// "just works" instead of nagging the user to /switch first.
func TestEnsureCurrentSession_AutoResumesIdle(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	// Seed: an idle session with no focus set.
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-idle", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})
	if _, ok := mgr.FocusedSession("alice"); ok {
		t.Fatal("precondition: expected no focus before the test")
	}

	got, err := b.ensureCurrentSession(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1",
	}, true)
	if err != nil {
		t.Fatalf("ensureCurrentSession failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected a session, got nil")
	}
	if got.Info().Status != string(session.StatusActive) {
		t.Errorf("expected session to be active after reactivate, got %s",
			got.Info().Status)
	}
	if focused, _ := mgr.FocusedSession("alice"); focused == nil {
		t.Error("expected focus to be set on the auto-resumed session")
	}
	// Sanity: the returned session's CLISessionID should match the idle
	// one we seeded — Reactivate preserves the CLI session identity.
	if got.Info().CLISessionID != "cli-idle" {
		t.Errorf("CLI session mismatch: got %s, want cli-idle", got.Info().CLISessionID)
	}
	_ = id // referenced for clarity
}

// TestEnsureCurrentSession_NoSessionAtAll covers the "nothing to resume"
// case: command handlers must surface a clear, actionable error instead
// of returning a nil session that callers may dereference.
func TestEnsureCurrentSession_NoSessionAtAll(t *testing.T) {
	b, _, _ := newTestBridge(t)
	_, err := b.ensureCurrentSession(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1",
	}, false)
	if err == nil {
		t.Fatal("expected error when no session exists, got nil")
	}
	if !strings.Contains(err.Error(), "/new") {
		t.Errorf("error should point user at /new, got: %v", err)
	}
}

// TestEnsureCurrentSession_NoReactivateWhenNotNeeded keeps the idle
// session as-is when mustBeActive=false. Metadata-only commands
// (/rename, /archive) rely on this to avoid paying a 10–30s reactivate
// cost just to flip a field.
func TestEnsureCurrentSession_NoReactivateWhenNotNeeded(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-idle", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})

	got, err := b.ensureCurrentSession(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1",
	}, false)
	if err != nil {
		t.Fatalf("ensureCurrentSession failed: %v", err)
	}
	if got.ID != id {
		t.Errorf("expected the original idle session id %s, got %s", id, got.ID)
	}
	if got.Info().Status == string(session.StatusActive) {
		t.Errorf("session should still be idle (mustBeActive=false), got active")
	}
}

// TestCmdRename_AutoResolvesIdle is the user-facing acceptance test
// for the audit: /rename should NOT fail with "no active session" when
// the user has an idle session sitting around — it should silently
// reuse it.
func TestCmdRename_AutoResolvesIdle(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-idle", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_rename",
		Kind: channel.InputText, Text: "/rename new-name",
	})

	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("expected 1 outbound, got %d", len(out))
	}
	body := out[0].Text
	if strings.Contains(body, "没有") || strings.Contains(body, "失败") {
		t.Errorf("expected silent success, got error-shaped reply: %q", body)
	}
	if !strings.Contains(body, "new-name") {
		t.Errorf("rename reply should confirm the new name, got: %q", body)
	}

	// Verify the title actually landed on the idle session.
	sess, _ := mgr.Get(id)
	if got := sess.Info().CustomTitle; got != "new-name" {
		t.Errorf("CustomTitle = %q, want new-name", got)
	}
}

// TestCmdArchive_AutoResolvesIdle: same acceptance for /archive — it
// should archive the idle session without asking for /switch first.
func TestCmdArchive_AutoResolvesIdle(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-idle", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_arch",
		Kind: channel.InputText, Text: "/archive",
	})

	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("expected 1 outbound, got %d", len(out))
	}
	if strings.Contains(out[0].Text, "没有") {
		t.Errorf("expected archive to auto-resolve idle session, got: %q", out[0].Text)
	}
	sess, _ := mgr.Get(id)
	if got := sess.Info().Status; got != string(session.StatusArchived) {
		t.Errorf("expected status=archived, got %s", got)
	}
}
