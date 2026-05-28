package session

import (
	"context"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session/sessiontest"
)

func newTestManager(t *testing.T, maxSessions int) *Manager {
	t.Helper()
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	mgr := NewManager(rt, t.TempDir(), "default", maxSessions, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	return mgr
}

func TestManager_CreateAndGet(t *testing.T) {
	mgr := newTestManager(t, 4)
	sess, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Error("session.ID is empty")
	}
	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned not found")
	}
	if got.ID != sess.ID {
		t.Errorf("Get.ID = %q, want %q", got.ID, sess.ID)
	}
}

func TestManager_CreateAppliesDefaults(t *testing.T) {
	mgr := newTestManager(t, 4)
	sess, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.WorkingDir == "" {
		t.Error("WorkingDir should default to manager default")
	}
	if sess.PermissionMode != "default" {
		t.Errorf("PermissionMode = %q, want default", sess.PermissionMode)
	}
}

func TestManager_CreateRespectsExplicitOpts(t *testing.T) {
	mgr := newTestManager(t, 4)
	dir := t.TempDir()
	sess, err := mgr.Create(context.Background(), CreateOpts{
		WorkingDir:     dir,
		PermissionMode: "auto",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.WorkingDir != dir {
		t.Errorf("WorkingDir = %q, want %q", sess.WorkingDir, dir)
	}
	if sess.PermissionMode != "auto" {
		t.Errorf("PermissionMode = %q, want auto", sess.PermissionMode)
	}
}

func TestManager_CreateRespectsEnvVars(t *testing.T) {
	mgr := newTestManager(t, 4)
	sess, err := mgr.Create(context.Background(), CreateOpts{
		EnvVars: map[string]string{
			"FAKE_CLI_SESSION_ID": "envsession-1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if got := sess.Info().CLISessionID; got != "envsession-1" {
		t.Errorf("CLISessionID = %q, want envsession-1", got)
	}
}

func TestManager_CreateMaxSessionsLimit(t *testing.T) {
	mgr := newTestManager(t, 2)
	s1, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	s2, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	if _, err := mgr.Create(context.Background(), CreateOpts{}); err == nil {
		t.Fatal("Create #3 should fail when at limit, got nil err")
	}
	// Destroy one and try again — should succeed
	if err := mgr.Destroy(s1.ID); err != nil {
		t.Fatalf("Destroy s1: %v", err)
	}
	if _, err := mgr.Create(context.Background(), CreateOpts{}); err != nil {
		t.Fatalf("Create after destroy: %v", err)
	}
	_ = s2
}

func TestManager_DestroyRemovesSession(t *testing.T) {
	mgr := newTestManager(t, 4)
	sess, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Destroy(sess.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Error("Get after Destroy returned ok=true")
	}
}

func TestManager_DestroyUnknownReturnsError(t *testing.T) {
	mgr := newTestManager(t, 4)
	if err := mgr.Destroy("does-not-exist"); err == nil {
		t.Error("Destroy unknown returned nil error")
	}
}

func TestManager_List(t *testing.T) {
	mgr := newTestManager(t, 4)
	_, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	_, err = mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	infos := mgr.List()
	if len(infos) != 2 {
		t.Errorf("List len = %d, want 2", len(infos))
	}
}

func TestManager_Resume(t *testing.T) {
	mgr := newTestManager(t, 4)
	sess, err := mgr.Resume(context.Background(), ResumeOpts{
		CLISessionID: "prior-cli-session",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if sess.ID == "" {
		t.Error("Resume session ID is empty")
	}
	if got, ok := mgr.Get(sess.ID); !ok || got != sess {
		t.Error("Resumed session not retrievable via Get")
	}
}

func TestManager_ShutdownClosesAll(t *testing.T) {
	mgr := newTestManager(t, 4)
	s1, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s2, err := mgr.Create(context.Background(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	c1 := sessiontest.NewEventCollector(s1.Subscribe("a"))
	c2 := sessiontest.NewEventCollector(s2.Subscribe("b"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr.Shutdown(ctx)

	if !c1.WaitDone(3 * time.Second) {
		t.Error("s1 subscribers not closed after Shutdown")
	}
	if !c2.WaitDone(3 * time.Second) {
		t.Error("s2 subscribers not closed after Shutdown")
	}
	if got := mgr.List(); len(got) != 0 {
		t.Errorf("List after Shutdown = %d, want 0", len(got))
	}
}

func TestManager_AddAllowedBaseDir(t *testing.T) {
	mgr := newTestManager(t, 4)
	mgr.AddAllowedBaseDir(t.TempDir())
	mgr.AddAllowedBaseDir("") // no-op
}
