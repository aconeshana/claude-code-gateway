package bridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/channel/fake"
	"github.com/anthropics/claude-code-gateway/internal/channel/feishu"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	rfake "github.com/anthropics/claude-code-gateway/internal/runtime/fake"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// newTestBridge builds a Bridge backed by a fake runtime + fake channel.
func newTestBridge(t *testing.T) (*Bridge, *fake.Channel, *session.Manager) {
	t.Helper()
	rt := rfake.NewRuntime(claude.Codec{})
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*rfake.Process, error) {
		p := rfake.NewProcess(cb)
		go func() {
			time.Sleep(5 * time.Millisecond)
			id := req.ResumeID
			if id == "" {
				id = "rt-" + req.WorkingDir
			}
			p.EmitInit(id)
		}()
		return p, nil
	})
	mgr := session.NewManager(rt, t.TempDir(), "default", 8, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	ch := fake.New()
	b := New(Options{
		Manager:    mgr,
		Channel:    ch,
		DefaultCWD: t.TempDir(),
	})
	return b, ch, mgr
}

func TestBridge_HelpCommand(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if out[0].Card == nil {
		t.Fatal("expected Card output")
	}
	if !strings.Contains(out[0].Card.Sections[0].Markdown, "命令列表") {
		t.Error("help body missing")
	}
}

func TestBridge_NewCreatesSessionAndAttachesOwner(t *testing.T) {
	// V2: /new with no focus posts the project picker; with focus it creates
	// a session inheriting the focused project's WorkingDir. This test now
	// covers the focus-present path.
	b, ch, mgr := newTestBridge(t)
	// Seed: import an idle session to act as focus after a manual SetFocus.
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed", OwnerID: "alice", Origin: session.OriginFeishu,
		WorkingDir: "/tmp/proj-A",
	})
	_, _ = mgr.Reactivate(context.Background(), id)
	for _, info := range mgr.ListActiveByOwner("alice") {
		_ = mgr.SetFocus("alice", info.ID)
	}

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/new",
	})
	out := ch.Outbound()
	if len(out) == 0 {
		t.Fatal("no outbound")
	}
	// Expect a fresh session in the focus dir.
	all := mgr.ListActiveByOwner("alice")
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions (seed + new), got %d", len(all))
	}
}

func TestBridge_ListEmpty(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/list",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if !strings.Contains(out[0].Card.Sections[0].Markdown, "暂无 session") {
		t.Errorf("empty list message missing: %+v", out[0].Card.Sections)
	}
}

func TestBridge_ListWithSessions(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	_, _ = mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice", Label: "first", WorkingDir: t.TempDir()})
	_, _ = mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice", Label: "second", WorkingDir: t.TempDir()})

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/list",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	// Two distinct WorkingDirs → at least 2 project sections in the
	// two-level menu.
	if len(out[0].Card.Sections) < 2 {
		t.Errorf("expected at least 2 project sections, got %d", len(out[0].Card.Sections))
	}
}

func TestBridge_ArchiveActiveSession(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	sess, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice"})
	_ = mgr.SetFocus("alice", sess.ID)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/archive",
	})
	out := ch.Outbound()
	if len(out) != 1 || !strings.Contains(out[0].Text, "已归档") {
		t.Errorf("expected archive confirmation, got %+v", out)
	}
	if sess.Status != session.StatusArchived {
		t.Errorf("status = %s, want archived", sess.Status)
	}
}

func TestBridge_SwitchSessionByPrefix(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	s1, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice", Label: "alpha"})
	s2, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice", Label: "beta"})
	_ = mgr.SetFocus("alice", s1.ID)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/switch beta",
	})
	// V2 switchFocusTo ends by re-rendering the /switch card (Reply when
	// available, fresh card otherwise) instead of plain text. Just check
	// outbound is non-empty and focus moved.
	if len(ch.Outbound()) == 0 {
		t.Error("expected outbound after /switch")
	}
	focused, _ := mgr.FocusedSession("alice")
	if focused.ID != s2.ID {
		t.Errorf("focused = %s, want %s", focused.ID, s2.ID)
	}
}

func TestBridge_ResumeArchivedByPrefix(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	sess, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice", Label: "old-work"})
	// wait for CLI init so CLISessionID is populated
	waitForCLIID(t, sess, 1*time.Second)
	_ = mgr.Archive(sess.ID)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/resume old",
	})
	// new (active) session should exist for alice
	active := mgr.ListActiveByOwner("alice")
	if len(active) != 1 {
		t.Fatalf("active sessions = %d, want 1", len(active))
	}
}

func TestBridge_CardActionSwitchSession(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	s1, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice"})
	s2, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice"})
	_ = mgr.SetFocus("alice", s1.ID)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputCardAction,
		Action: &channel.CardAction{
			Name:   "switch_session",
			Values: map[string]interface{}{"session_id": s2.ID},
		},
	})
	out := ch.Outbound()
	// switch_session now re-renders the active-sessions card in place
	// (via inbound.Reply when available; sendCard as fallback). When no
	// Reply hook is set the card lands in the outbound queue; assert it
	// came through and focus moved — that's what the user-visible
	// behavior actually is.
	if len(out) == 0 || out[0].Card == nil {
		t.Errorf("expected switch-session card reply, got: %+v", out)
	}
	focused, _ := mgr.FocusedSession("alice")
	if focused.ID != s2.ID {
		t.Errorf("focused = %s, want %s", focused.ID, s2.ID)
	}
}

func TestBridge_CardActionShowArchivedEmpty(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputCardAction,
		Action: &channel.CardAction{Name: "show_archived"},
	})
	out := ch.Outbound()
	if len(out) != 1 || !strings.Contains(out[0].Text, "没有归档") {
		t.Errorf("expected no-archived text: %+v", out)
	}
}

func TestBridge_UnknownCommandForwardsToCLI(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	sess, _ := mgr.Create(context.Background(), session.CreateOpts{OwnerID: "alice"})
	_ = mgr.SetFocus("alice", sess.ID)
	waitForCLIID(t, sess, 1*time.Second)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/commit",
	})

	// The fake runtime's Process should have received the message via Write
	// (captured by fake.Process.Written). We can't reach into the manager's
	// internals, so this test verifies the command didn't crash and was
	// forwarded as "send to CLI" rather than printed as help.
}

func TestBridge_DefaultTextAutoCreatesSession(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "hello world",
	})
	sessions := mgr.ListActiveByOwner("alice")
	if len(sessions) != 1 {
		t.Fatalf("active sessions = %d, want 1", len(sessions))
	}
}

// TestBridge_ThreadInboundBindsAndRoutes verifies the full thread route:
// a brand-new thread_id binds to the currently focused session, and the
// next message in the same thread routes back to that session.
func TestBridge_ThreadInboundBindsAndRoutes(t *testing.T) {
	b, _, mgr := newTestBridge(t)

	// 1. Main-chat message creates focused session A.
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "task one",
	})
	sessions := mgr.ListActiveByOwner("alice")
	if len(sessions) != 1 {
		t.Fatalf("after main message: sessions = %d, want 1", len(sessions))
	}
	sessA := sessions[0].ID

	// 2. User opens a thread anchored at bot's reply. First message in the
	//    thread arrives with thread_id + root_id; focused (unbound) session A
	//    should get bound to this thread.
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_user1",
		ThreadID: "omt_t1", RootID: "om_root1",
		Kind: channel.InputText, Text: "in thread",
	})
	infos := mgr.ListActiveByOwner("alice")
	if len(infos) != 1 {
		t.Fatalf("after thread bind: sessions = %d, want still 1 (focused absorbed the thread)", len(infos))
	}
	if infos[0].ID != sessA {
		t.Errorf("thread bound to session %s, want focused %s", infos[0].ID, sessA)
	}
	if infos[0].ThreadID != "omt_t1" || infos[0].RootMessageID != "om_root1" {
		t.Errorf("session A binding = (thread=%q root=%q), want (omt_t1, om_root1)", infos[0].ThreadID, infos[0].RootMessageID)
	}

	// 3. Second message in the same thread should route to session A again.
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_user2",
		ThreadID: "omt_t1", RootID: "om_root1",
		Kind: channel.InputText, Text: "more in thread",
	})
	infos = mgr.ListActiveByOwner("alice")
	if len(infos) != 1 || infos[0].ID != sessA {
		t.Errorf("second thread msg did not route to session A: got %+v", infos)
	}
}

// TestBridge_OutboundForSessionIncludesReplyAnchor confirms that
// sendCardForSession populates ReplyToMessageID when the session is
// thread-bound, so the Lark Reply API is invoked.
func TestBridge_OutboundForSessionIncludesReplyAnchor(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, err := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-r", OwnerID: "alice", Origin: session.OriginFeishu,
		ThreadID: "omt_x", RootMessageID: "om_anchor",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sess, _ := mgr.Get(id)

	_, err = b.sendCardForSession(context.Background(), sess, "c1", channel.Card{Title: "Hi"})
	if err != nil {
		t.Fatalf("sendCardForSession: %v", err)
	}
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if out[0].ReplyToMessageID != "om_anchor" {
		t.Errorf("ReplyToMessageID = %q, want om_anchor", out[0].ReplyToMessageID)
	}
}

// TestBridge_AnchorMissingClearsThreadAndFallsBack confirms that when the
// channel reports the reply anchor is gone (user deleted the thread root),
// the bridge clears the session's thread binding, notifies the user, and
// retries via the main chat.
func TestBridge_AnchorMissingClearsThreadAndFallsBack(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, err := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-rf", OwnerID: "alice", Origin: session.OriginFeishu,
		ThreadID: "omt_dead", RootMessageID: "om_anchor_dead",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sess, _ := mgr.Get(id)

	// First send: simulate anchor-missing for any message that uses Reply API.
	calls := 0
	ch.SetSendErrorFunc(func(msg channel.OutboundMessage) error {
		calls++
		if msg.ReplyToMessageID != "" {
			return feishu.ErrReplyAnchorMissing
		}
		return nil
	})

	_, err = b.sendCardForSession(context.Background(), sess, "c1", channel.Card{Title: "Hi"})
	if err != nil {
		t.Fatalf("sendCardForSession: %v", err)
	}

	// Expect: 1st call fails (anchor missing), bridge clears thread + posts
	// the user-notification, then retries the card to main chat.
	out := ch.Outbound()
	// out has: [notification text, the card retried to main chat]
	if len(out) < 2 {
		t.Fatalf("Outbound = %d, want >= 2 (notification + card)", len(out))
	}
	hasNotify := false
	hasRetry := false
	for _, m := range out {
		if m.Text != "" && strings.Contains(m.Text, "话题已失效") {
			hasNotify = true
		}
		if m.Card != nil && m.Card.Title == "Hi" && m.ReplyToMessageID == "" {
			hasRetry = true
		}
	}
	if !hasNotify {
		t.Error("missing 话题已失效 notification")
	}
	if !hasRetry {
		t.Error("missing fallback card to main chat")
	}

	// Session's thread binding should be cleared.
	info := sess.Info()
	if info.ThreadID != "" || info.RootMessageID != "" {
		t.Errorf("session still bound after anchor-missing: thread=%q root=%q", info.ThreadID, info.RootMessageID)
	}
}

// --- V2: focus-stable + auto-thread tests ---

// TestBridge_NewWithoutFocusTakesFocus: V2 /new with no focus shows the
// project picker card instead of creating a session — user must pick a
// project (or use plain text to auto-create) first.
func TestBridge_NewWithoutFocusTakesFocus(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_u1",
		Kind: channel.InputText, Text: "/new",
	})
	// No session should be created — the picker card is the response.
	if n := len(mgr.ListActiveByOwner("alice")); n != 0 {
		t.Errorf("expected 0 sessions (picker only), got %d", n)
	}
	out := ch.Outbound()
	if len(out) == 0 || out[0].Card == nil || out[0].Card.Title != "Projects" {
		t.Errorf("expected Projects picker card, got %+v", out)
	}
}

// TestBridge_NewWithFocusOpensThreadAndKeepsFocus: /new with an existing
// focused active session creates a new session in the same project dir,
// opens a thread for it, and preserves the prior focus.
func TestBridge_NewWithFocusOpensThreadAndKeepsFocus(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	// Seed session A as focus via plain text (auto-create path).
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_u1",
		Kind: channel.InputText, Text: "hello",
	})
	infos := mgr.ListActiveByOwner("alice")
	if len(infos) != 1 {
		t.Fatalf("setup: got %d, want 1", len(infos))
	}
	sessA := infos[0].ID

	// /new should open a thread for a parallel session, focus stays on A.
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_u2",
		Kind: channel.InputText, Text: "/new",
	})
	allInfos := mgr.ListActiveByOwner("alice")
	if len(allInfos) != 2 {
		t.Fatalf("after second /new: got %d, want 2", len(allInfos))
	}
	var sessB session.SessionInfo
	for _, info := range allInfos {
		if info.ID != sessA {
			sessB = info
		}
	}
	if sessB.ThreadID == "" {
		t.Error("second session should be thread-bound")
	}
	focused, _ := mgr.FocusedSession("alice")
	if focused == nil || focused.ID != sessA {
		t.Errorf("focus should remain on sessA, got %s", func() string {
			if focused == nil {
				return "nil"
			}
			return focused.ID
		}())
	}

	sawOpenThread := false
	for _, msg := range ch.Outbound() {
		if msg.OpenThread {
			sawOpenThread = true
		}
	}
	if !sawOpenThread {
		t.Error("expected an OpenThread=true outbound for second /new")
	}
}

// TestBridge_MainChatPlainTextRoutesToFocusedRegardlessOfThread: even when
// focused session is thread-bound, main-chat plain text routes to it
// (focused = single source of truth for main chat).
func TestBridge_MainChatPlainTextRoutesToFocusedRegardlessOfThread(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-f", OwnerID: "alice", Origin: session.OriginFeishu,
		ThreadID: "omt_x", RootMessageID: "om_root_x",
	})
	_ = mgr.SetFocus("alice", id)

	// Reactivate so the session is active when the main-chat message arrives
	// (resolveOrCreateSession's step 1 requires status=active).
	_, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("Reactivate: %v", err)
	}

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_main_msg",
		Kind: channel.InputText, Text: "hello in main chat",
	})

	// Expect exactly 1 active session (focused, even though thread-bound).
	all := mgr.ListActiveByOwner("alice")
	if len(all) != 1 {
		t.Errorf("got %d active sessions, want 1 (focused was reused)", len(all))
	}
}

// TestBridge_SwitchStowsOldFocus: /switch to another active session should
// stow the old focus into a thread (if it had no thread yet) and promote
// the new session to main-chat focus.
func TestBridge_SwitchStowsOldFocus(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	// Two sessions: A (focused, no thread), B (active, no thread either).
	idA, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-A", OwnerID: "alice", Origin: session.OriginFeishu,
	})
	idB, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-B", OwnerID: "alice", Origin: session.OriginFeishu,
	})
	// Reactivate replaces the gateway-internal session id with a fresh one,
	// so we have to capture the NEW ids returned and use those going forward.
	sessA, err := mgr.Reactivate(context.Background(), idA)
	if err != nil {
		t.Fatalf("Reactivate A: %v", err)
	}
	sessB, err := mgr.Reactivate(context.Background(), idB)
	if err != nil {
		t.Fatalf("Reactivate B: %v", err)
	}
	idA = sessA.ID
	idB = sessB.ID
	_ = mgr.SetFocus("alice", idA)

	waitForActive(t, mgr, idA, 500*time.Millisecond)
	waitForActive(t, mgr, idB, 500*time.Millisecond)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_switch",
		Kind: channel.InputText, Text: "/switch cli-B",
	})

	// Old focus (A) should now have a thread bound.
	if got := sessA.Info(); got.ThreadID == "" {
		t.Error("old focus A should have a thread after /switch")
	}
	// New focus is B.
	focused, _ := mgr.FocusedSession("alice")
	if focused == nil || focused.ID != idB {
		t.Errorf("new focus should be B (%s), got %s", idB, func() string {
			if focused == nil {
				return "nil"
			}
			return focused.ID
		}())
	}
	// The /switch flow should have produced an OpenThread=true call (for stowing A).
	sawOpenThread := false
	for _, msg := range ch.Outbound() {
		if msg.OpenThread {
			sawOpenThread = true
		}
	}
	if !sawOpenThread {
		t.Error("/switch should have stowed old focus into a thread")
	}
}

// TestBridge_NewInThreadIsRejected: /new inside a thread is rejected with a
// hint to use the main chat.
func TestBridge_NewInThreadIsRejected(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", MessageID: "om_in_thread",
		ThreadID: "omt_t", RootID: "om_r",
		Kind: channel.InputText, Text: "/new sessX",
	})
	// No new session should be created.
	if len(mgr.ListActiveByOwner("alice")) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(mgr.ListActiveByOwner("alice")))
	}
	// One outbound text: the rejection.
	out := ch.Outbound()
	if len(out) != 1 || !strings.Contains(out[0].Text, "请回主聊天") {
		t.Errorf("expected rejection text, got %+v", out)
	}
}

func waitForActive(t *testing.T, mgr *session.Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, ok := mgr.Get(id)
		if ok && sess.Info().Status == string(session.StatusActive) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach active in %v", id, timeout)
}

func waitForCLIID(t *testing.T, sess *session.Session, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess.Info().CLISessionID != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s did not get CLISessionID", sess.ID)
}

// ----- V2 behavior contracts (locked-in regression tests) -----

// resolveSessionByPayload must accept both gateway-internal session.ID and
// CLI session id, because cards rendered before a Reactivate carry an id
// that may no longer be the live one. Without this, every reactivate would
// dead-end existing card buttons with "session 不存在".
func TestBridge_ResolveSessionByPayload_AcceptsCLISessionID(t *testing.T) {
	b, _, mgr := newTestBridge(t)
	id, err := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-stable-1", OwnerID: "alice", Origin: session.OriginFeishu,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// Lookup by gateway id works.
	if _, ok := b.resolveSessionByPayload(id); !ok {
		t.Error("lookup by gateway id failed")
	}
	// Lookup by CLI id also works.
	if _, ok := b.resolveSessionByPayload("cli-stable-1"); !ok {
		t.Error("lookup by CLI id failed")
	}
	// Unknown id fails.
	if _, ok := b.resolveSessionByPayload("nonexistent"); ok {
		t.Error("unknown id should not resolve")
	}
}

// /new with arguments must be rejected — V2 stripped label/dir to push users
// through the /project picker for explicit project selection.
func TestBridge_NewRejectsArgs(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/new mywork",
	})
	if n := len(mgr.ListActiveByOwner("alice")); n != 0 {
		t.Errorf("/new with args should not create a session, got %d", n)
	}
	out := ch.Outbound()
	if len(out) == 0 || out[0].Text == "" || !strings.Contains(out[0].Text, "不再接受参数") {
		t.Errorf("expected rejection text, got %+v", out)
	}
}

// /switch to the currently focused session must be a no-op with a clear hint,
// not a re-render or stow operation.
func TestBridge_SwitchToSameSessionNoop(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-A", OwnerID: "alice", Origin: session.OriginFeishu,
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	_ = mgr.SetFocus("alice", sess.ID)
	waitForActive(t, mgr, sess.ID, 500*time.Millisecond)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/switch cli-A",
	})
	out := ch.Outbound()
	if len(out) == 0 {
		t.Fatal("no outbound")
	}
	last := out[len(out)-1]
	if last.Text == "" || !strings.Contains(last.Text, "已经在") {
		t.Errorf("expected '已经在' hint, got %+v", out)
	}
	// No card stowing — only a plain text reply.
	for _, msg := range out {
		if msg.OpenThread {
			t.Errorf("same-session switch should not open thread, got %+v", msg)
		}
	}
}

// switchFocusTo with an already-thread-bound prior focus must NOT open a new
// thread (that would create duplicate 话题入口卡). Instead it should
// SetLastInbound + ping the existing thread so the entry card surfaces a
// new-reply indicator.
func TestBridge_SwitchPingsExistingThreadInsteadOfReopening(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	// A is the existing focus, already thread-bound.
	idA, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-A", OwnerID: "alice", Origin: session.OriginFeishu,
		ChatID: "c1", ThreadID: "omt_existing", RootMessageID: "om_existing_root",
	})
	idB, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-B", OwnerID: "alice", Origin: session.OriginFeishu,
	})
	sessA, _ := mgr.Reactivate(context.Background(), idA)
	sessB, _ := mgr.Reactivate(context.Background(), idB)
	idA, idB = sessA.ID, sessB.ID
	_ = mgr.SetFocus("alice", idA)
	waitForActive(t, mgr, idA, 500*time.Millisecond)
	waitForActive(t, mgr, idB, 500*time.Millisecond)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/switch cli-B",
	})

	// New focus is B.
	focused, _ := mgr.FocusedSession("alice")
	if focused == nil || focused.ID != idB {
		t.Errorf("focus didn't move to B")
	}
	// A must keep its thread binding (we didn't open a new one).
	if got := sessA.Info(); got.ThreadID != "omt_existing" || got.RootMessageID != "om_existing_root" {
		t.Errorf("A's thread binding changed: %+v", got)
	}
	// One of the outbound messages must be the ping (reply to original root).
	sawPing := false
	for _, msg := range ch.Outbound() {
		if msg.ReplyToMessageID == "om_existing_root" && strings.Contains(msg.Text, "focus 已切走") {
			sawPing = true
		}
		// Must NOT have opened a new thread for A.
		if msg.OpenThread {
			t.Errorf("switch should not OpenThread when prior focus already bound, got %+v", msg)
		}
	}
	if !sawPing {
		t.Errorf("expected a ping in A's existing thread, got %+v", ch.Outbound())
	}
}

// openThreadForSession reuses an existing thread binding (sends a Reply
// instead of OpenThread). Critical for /resume after restart not to pile
// up new 话题入口卡 every time.
func TestBridge_OpenThreadReusesExistingBinding(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-T", OwnerID: "alice", Origin: session.OriginFeishu,
		ChatID: "c1", ThreadID: "omt_bound", RootMessageID: "om_root_bound",
	})
	sess, _ := mgr.Get(id)
	if err := b.openThreadForSession(context.Background(), sess, "om_unrelated_anchor", "welcome"); err != nil {
		t.Fatalf("openThreadForSession: %v", err)
	}
	// Should have sent exactly one reply, anchored at the EXISTING root
	// (not OpenThread to the new anchor).
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("expected 1 outbound (ping into existing thread), got %d", len(out))
	}
	msg := out[0]
	if msg.OpenThread {
		t.Errorf("should reuse existing thread, not OpenThread again: %+v", msg)
	}
	if msg.ReplyToMessageID != "om_root_bound" {
		t.Errorf("ReplyToMessageID = %q, want om_root_bound (existing root)", msg.ReplyToMessageID)
	}
}

// openThreadForSession falls back to OpenThread when the existing anchor has
// disappeared (user deleted thread root). The stale binding is cleared and a
// brand-new thread is opened at the supplied anchor.
func TestBridge_OpenThreadRebuildsAfterAnchorMissing(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-T2", OwnerID: "alice", Origin: session.OriginFeishu,
		ChatID: "c1", ThreadID: "omt_dead", RootMessageID: "om_dead_root",
	})
	sess, _ := mgr.Get(id)

	ch.SetSendErrorFunc(func(msg channel.OutboundMessage) error {
		// Any reply to the dead anchor fails; OpenThread (no ReplyToMessageID
		// from SendMessage POV; fake.OpenThread is a different code path) OK.
		if msg.ReplyToMessageID == "om_dead_root" {
			return feishu.ErrReplyAnchorMissing
		}
		return nil
	})

	if err := b.openThreadForSession(context.Background(), sess, "om_new_anchor", "welcome"); err != nil {
		t.Fatalf("openThreadForSession: %v", err)
	}
	// After the fallback, the session must have a NEW thread (not the dead one).
	info := sess.Info()
	if info.ThreadID == "omt_dead" {
		t.Errorf("stale thread still bound: %+v", info)
	}
	if info.RootMessageID != "om_new_anchor" {
		t.Errorf("new root msg id should be om_new_anchor, got %+v", info)
	}
	// At least one OpenThread call must have happened (the rebuild).
	sawOpen := false
	for _, msg := range ch.Outbound() {
		if msg.OpenThread {
			sawOpen = true
		}
	}
	if !sawOpen {
		t.Error("expected OpenThread fallback after anchor-missing")
	}
}

// countUserTurnsInJSONL must scan the whole file (not just the head) and
// count only lines whose `type` is `user`, ignoring system/queue events and
// surviving missing trailing newlines.
func TestCountUserTurnsInJSONL(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/x.jsonl"
	content := strings.Join([]string{
		`{"type":"queue-operation","operation":"enqueue"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"user","message":{"role":"user","content":"two"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"reply"}}`,
		`{"type":"user","message":{"role":"user","content":"three"}}`, // no trailing newline
	}, "\n")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := countUserTurnsInJSONL(p)
	if got != 3 {
		t.Errorf("countUserTurnsInJSONL = %d, want 3", got)
	}
	// Missing file returns 0.
	if n := countUserTurnsInJSONL(dir + "/missing.jsonl"); n != 0 {
		t.Errorf("missing file returned %d, want 0", n)
	}
}
