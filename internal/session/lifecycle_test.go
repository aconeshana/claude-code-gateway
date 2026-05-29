package session

import (
	"context"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/runtime/fake"
)

// TestManager_AdminOriginExcludedFromListAndCount asserts Origin=admin
// sessions are gateway plumbing — they live in the manager but are excluded
// from every user-facing view: ListDiscoverableByOwner, the active-count cap,
// and ResolveResumable.
func TestManager_AdminOriginExcludedFromListAndCount(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	// Two sessions: one user-owned external (visible with shareExternal),
	// one admin-internal (must never be visible).
	_, err := mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "ext-user", WorkingDir: "/tmp/u", Origin: OriginExternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "adm-1", WorkingDir: "/tmp/claude-code-gateway-admin", Origin: OriginAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ListDiscoverableByOwner with share=true should return ext-user, NOT adm-1.
	got := mgr.ListDiscoverableByOwner("alice", true)
	for _, info := range got {
		if info.CLISessionID == "adm-1" {
			t.Errorf("admin session leaked into ListDiscoverableByOwner: %+v", info)
		}
	}
	if len(got) != 1 {
		t.Errorf("expected exactly 1 visible (ext-user), got %d", len(got))
	}
}

func TestManager_AdminOriginNotCountedTowardMaxSessions(t *testing.T) {
	// maxSessions = 2 — would be hit by 1 user + 1 admin if admin counted.
	// We expect admin sessions to be excluded from the cap.
	rt := fake.NewRuntime(claude.Codec{})
	mgr := NewManager(rt, t.TempDir(), "default", 2, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	// Inject an admin-active session directly (bypassing Create which
	// would spawn a runtime — we just need a counted entry).
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		p := fake.NewProcess(cb)
		go func() {
			time.Sleep(5 * time.Millisecond)
			p.EmitInit("rt-1")
		}()
		return p, nil
	})

	// Two real Create calls (would hit max if admin counted)
	if _, err := mgr.Create(context.Background(), CreateOpts{OwnerID: "a", Origin: OriginAdmin}); err != nil {
		t.Fatalf("first admin create: %v", err)
	}
	if _, err := mgr.Create(context.Background(), CreateOpts{OwnerID: "a"}); err != nil {
		t.Fatalf("first user create: %v", err)
	}
	if _, err := mgr.Create(context.Background(), CreateOpts{OwnerID: "a"}); err != nil {
		t.Fatalf("second user create blocked by admin count: %v", err)
	}
	// Third user create should fail (we now have 2 user sessions)
	if _, err := mgr.Create(context.Background(), CreateOpts{OwnerID: "a"}); err == nil {
		t.Error("expected third user create to hit maxSessions=2")
	}
}

func TestManager_ResolveResumableSkipsAdmin(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	// Give alice an admin session (unusual but conceivable if someone set
	// OwnerID + Origin=admin) and a real external one. Resolve should
	// pick the external, not the admin.
	_, _ = mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "adm", OwnerID: "alice", Origin: OriginAdmin,
	})
	_, _ = mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "real", OwnerID: "alice", Origin: OriginFeishu,
	})
	sess := mgr.ResolveResumable("alice")
	if sess == nil || sess.CLISessionID != "real" {
		got := "nil"
		if sess != nil {
			got = sess.CLISessionID
		}
		t.Errorf("ResolveResumable returned %q, want real", got)
	}
}

func newOwnedManager(t *testing.T) (*Manager, *fake.Runtime) {
	t.Helper()
	rt := fake.NewRuntime(claude.Codec{})
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		p := fake.NewProcess(cb)
		go func() {
			time.Sleep(5 * time.Millisecond)
			id := req.ResumeID
			if id == "" {
				id = "rt-fresh-" + req.WorkingDir[len(req.WorkingDir)-3:]
			}
			p.EmitInit(id)
		}()
		return p, nil
	})
	mgr := NewManager(rt, t.TempDir(), "default", 8, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	return mgr, rt
}

func waitForCLISessionID(t *testing.T, sess *Session, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess.Info().CLISessionID != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s did not get CLISessionID in %v", sess.ID, timeout)
}

func TestManager_CreateAttachesOwnerMetadata(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, err := mgr.Create(context.Background(), CreateOpts{
		OwnerID:     "alice",
		Label:       "my-work",
		ChatID:      "chat-1",
		ChannelKind: "feishu",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.OwnerID != "alice" {
		t.Errorf("OwnerID = %q, want alice", sess.OwnerID)
	}
	if sess.Label != "my-work" {
		t.Errorf("Label = %q, want my-work", sess.Label)
	}
	if sess.Status != StatusActive {
		t.Errorf("Status = %s, want active", sess.Status)
	}

	focused, ok := mgr.FocusedSession("alice")
	if !ok || focused.ID != sess.ID {
		t.Error("FocusedSession should return the newly created session")
	}
}

func TestManager_ListByFilter(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	s1, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	s2, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	_, _ = mgr.Create(context.Background(), CreateOpts{OwnerID: "bob"})

	if got := mgr.ListBy(Filter{}); len(got) != 3 {
		t.Errorf("all sessions = %d, want 3", len(got))
	}
	if got := mgr.ListBy(Filter{OwnerID: "alice"}); len(got) != 2 {
		t.Errorf("alice = %d, want 2", len(got))
	}
	if got := mgr.ListBy(Filter{OwnerID: "bob"}); len(got) != 1 {
		t.Errorf("bob = %d, want 1", len(got))
	}

	_ = mgr.Archive(s1.ID)
	if got := mgr.ListActiveByOwner("alice"); len(got) != 1 {
		t.Errorf("alice active = %d, want 1", len(got))
	}
	if got := mgr.ListArchivedByOwner("alice"); len(got) != 1 {
		t.Errorf("alice archived = %d, want 1", len(got))
	}
	_ = s2 // keep referenced
}

func TestManager_ArchiveStopsRuntime(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	waitForCLISessionID(t, sess, 1*time.Second)

	if err := mgr.Archive(sess.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if sess.Status != StatusArchived {
		t.Errorf("Status = %s, want archived", sess.Status)
	}
	if _, ok := mgr.FocusedSession("alice"); ok {
		t.Error("FocusedSession should be empty after archiving the focused one")
	}
}

func TestManager_ReactivateArchivedCreatesNewSession(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, _ := mgr.Create(context.Background(), CreateOpts{
		OwnerID: "alice",
		Label:   "important",
	})
	waitForCLISessionID(t, sess, 1*time.Second)
	cliID := sess.Info().CLISessionID
	_ = mgr.Archive(sess.ID)

	newSess, err := mgr.Reactivate(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if newSess.ID == sess.ID {
		t.Error("Reactivate should produce a new session.ID")
	}
	if newSess.OwnerID != "alice" {
		t.Errorf("OwnerID lost: %q", newSess.OwnerID)
	}
	if newSess.Label != "important" {
		t.Errorf("Label lost: %q", newSess.Label)
	}
	if newSess.Status != StatusActive {
		t.Errorf("Status = %s, want active", newSess.Status)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Error("old session should be removed after Reactivate")
	}
	waitForCLISessionID(t, newSess, 1*time.Second)
	if got := newSess.Info().CLISessionID; got != cliID {
		t.Errorf("resumed CLISessionID = %q, want %q", got, cliID)
	}
}

func TestManager_TransitionToIdle(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	waitForCLISessionID(t, sess, 1*time.Second)

	mgr.TransitionToIdle(sess.ID)
	if sess.Status != StatusIdle {
		t.Errorf("Status = %s, want idle", sess.Status)
	}
	if _, ok := mgr.FocusedSession("alice"); ok {
		t.Error("focus should be cleared after TransitionToIdle on focused session")
	}
	cliID := sess.Info().CLISessionID
	if hint := mgr.ResumeHint("alice"); hint != cliID {
		t.Errorf("ResumeHint = %q, want %q", hint, cliID)
	}
}

func TestManager_ResolveResumablePrefersIdleOverArchived(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	s1, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice", Label: "archived-one"})
	waitForCLISessionID(t, s1, 1*time.Second)
	_ = mgr.Archive(s1.ID)

	s2, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice", Label: "idle-one"})
	waitForCLISessionID(t, s2, 1*time.Second)
	mgr.TransitionToIdle(s2.ID)

	res := mgr.ResolveResumable("alice")
	if res == nil {
		t.Fatal("ResolveResumable returned nil")
	}
	if res.ID != s2.ID {
		t.Errorf("preferred = %s (%s), want %s (idle-one)", res.ID, res.Label, s2.ID)
	}
}

func TestManager_FindByPrefixSkipsArchived(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	s1, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice", Label: "demo-feature"})
	s2, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice", Label: "other"})
	_ = mgr.Archive(s1.ID)

	got, err := mgr.FindByPrefix("alice", "demo")
	if err == nil {
		t.Errorf("FindByPrefix on archived-only match should error, got %v", got)
	}
	if g, err := mgr.FindByPrefix("alice", "other"); err != nil || g.ID != s2.ID {
		t.Errorf("FindByPrefix(other) = %v, %v, want s2 ok", g, err)
	}
}

func TestManager_SetLabelSummary(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	if err := mgr.SetLabel(sess.ID, "renamed"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := sess.Label; got != "renamed" {
		t.Errorf("Label = %q, want renamed", got)
	}
	if err := mgr.SetSummary(sess.ID, "new summary"); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	if sess.Summary != "new summary" {
		t.Errorf("Summary = %q, want new summary", sess.Summary)
	}
}

func TestManager_AppendRecentMessageAndShouldUpdate(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	sess, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})

	for i := 0; i < 3; i++ {
		mgr.AppendRecentMessage(sess.ID, "msg-i")
	}
	if sess.Summary == "" {
		t.Error("Summary should default to first message")
	}

	should, msgs := mgr.ShouldUpdateSummary(sess.ID, 3)
	if !should {
		t.Fatal("ShouldUpdateSummary returned false with 3 turns at interval 3")
	}
	if len(msgs) != 3 {
		t.Errorf("recent msgs = %d, want 3", len(msgs))
	}
	// Pending flag prevents further triggers until SetSummary
	if should2, _ := mgr.ShouldUpdateSummary(sess.ID, 3); should2 {
		t.Error("ShouldUpdateSummary should return false while pending")
	}
	_ = mgr.SetSummary(sess.ID, "final")
}

func TestManager_ImportIdleAndArchived(t *testing.T) {
	mgr, _ := newOwnedManager(t)

	idleID, err := mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "cli-idle-1",
		OwnerID:      "alice",
		Label:        "imported",
		WorkingDir:   "/tmp",
	})
	if err != nil {
		t.Fatalf("ImportIdleSession: %v", err)
	}
	if got, ok := mgr.Get(idleID); !ok || got.Status != StatusIdle {
		t.Errorf("imported session = %v ok=%v", got, ok)
	}

	archID, err := mgr.ImportArchivedSession(ImportOpts{
		CLISessionID: "cli-arch-1",
		OwnerID:      "alice",
		Label:        "old",
		WorkingDir:   "/tmp",
		ArchivedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("ImportArchivedSession: %v", err)
	}
	if got, _ := mgr.Get(archID); got.Status != StatusArchived {
		t.Error("imported archived has wrong status")
	}

	// Re-import dedup
	dup, _ := mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "cli-idle-1",
		OwnerID:      "alice",
	})
	if dup != idleID {
		t.Errorf("re-import should dedup to %s, got %s", idleID, dup)
	}
}

func TestManager_RemoveArchived(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	id, _ := mgr.ImportArchivedSession(ImportOpts{
		CLISessionID: "cli-rem-1",
		OwnerID:      "alice",
		WorkingDir:   "/tmp",
	})
	if err := mgr.RemoveArchived(id); err != nil {
		t.Fatalf("RemoveArchived: %v", err)
	}
	if _, ok := mgr.Get(id); ok {
		t.Error("session should be gone after RemoveArchived")
	}
}

func TestManager_SetFocus(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	s1, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})
	s2, _ := mgr.Create(context.Background(), CreateOpts{OwnerID: "alice"})

	// initial focus is on s2 (most recently created)
	cur, _ := mgr.FocusedSession("alice")
	if cur.ID != s2.ID {
		t.Errorf("initial focus = %s, want s2", cur.ID)
	}

	if err := mgr.SetFocus("alice", s1.ID); err != nil {
		t.Fatalf("SetFocus: %v", err)
	}
	cur, _ = mgr.FocusedSession("alice")
	if cur.ID != s1.ID {
		t.Errorf("after SetFocus = %s, want s1", cur.ID)
	}

	if err := mgr.SetFocus("bob", s1.ID); err == nil {
		t.Error("SetFocus on session owned by alice should error for bob")
	}
}

// TestManager_ReactivateCarriesThreadBinding locks in Bug A's fix:
// Reactivate generates a brand-new session record, but must copy ThreadID
// + RootMessageID from the old session so thread-routed plain text keeps
// reaching this session across reactivates.
func TestManager_ReactivateCarriesThreadBinding(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	idOld, err := mgr.ImportIdleSession(ImportOpts{
		CLISessionID:  "cli-thread",
		OwnerID:       "alice",
		Origin:        OriginFeishu,
		ThreadID:      "omt_persist",
		RootMessageID: "om_root_persist",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	newSess, err := mgr.Reactivate(context.Background(), idOld)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if newSess.ID == idOld {
		t.Errorf("Reactivate should generate a new gateway id, got same %s", idOld)
	}
	info := newSess.Info()
	if info.ThreadID != "omt_persist" {
		t.Errorf("ThreadID lost across Reactivate: %q", info.ThreadID)
	}
	if info.RootMessageID != "om_root_persist" {
		t.Errorf("RootMessageID lost across Reactivate: %q", info.RootMessageID)
	}
	// GetByThreadID must still find the new session.
	got, ok := mgr.GetByThreadID("omt_persist")
	if !ok || got.ID != newSess.ID {
		t.Errorf("GetByThreadID broke after Reactivate")
	}
}

func TestManager_BindAndGetByThreadID(t *testing.T) {
	mgr, _ := newOwnedManager(t)
	id, err := mgr.ImportIdleSession(ImportOpts{
		CLISessionID: "cli-x", OwnerID: "alice", Origin: OriginFeishu,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := mgr.GetByThreadID(""); ok {
		t.Error("empty threadID should not match anything")
	}
	if _, ok := mgr.GetByThreadID("omt_unknown"); ok {
		t.Error("unknown thread should not match")
	}

	if err := mgr.BindThread(id, "omt_abc", "om_root_1"); err != nil {
		t.Fatalf("BindThread: %v", err)
	}

	sess, ok := mgr.GetByThreadID("omt_abc")
	if !ok {
		t.Fatal("GetByThreadID after Bind returned ok=false")
	}
	info := sess.Info()
	if info.ThreadID != "omt_abc" || info.RootMessageID != "om_root_1" {
		t.Errorf("info = (thread=%q root=%q), want (omt_abc, om_root_1)", info.ThreadID, info.RootMessageID)
	}

	// Clear and ensure lookup misses
	if err := mgr.ClearThread(id); err != nil {
		t.Fatalf("ClearThread: %v", err)
	}
	if _, ok := mgr.GetByThreadID("omt_abc"); ok {
		t.Error("after ClearThread, GetByThreadID should not match")
	}
	if got := sess.Info(); got.ThreadID != "" || got.RootMessageID != "" {
		t.Errorf("after Clear: thread=%q root=%q, want empty", got.ThreadID, got.RootMessageID)
	}
}
