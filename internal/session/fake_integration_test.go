package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/runtime/fake"
)

// newFakeManager builds a Manager backed by a fake runtime that uses the
// claude codec so session.handleCLIMessage can recognize emitted events.
func newFakeManager(t *testing.T, maxSessions int) (*Manager, *fake.Runtime) {
	t.Helper()
	rt := fake.NewRuntime(claude.Codec{})
	mgr := NewManager(rt, t.TempDir(), "default", maxSessions, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	return mgr, rt
}

func TestManagerWithFake_CreatePassesConfig(t *testing.T) {
	mgr, rt := newFakeManager(t, 4)

	_, err := mgr.Create(context.Background(), CreateOpts{
		Model:    "sonnet-4",
		Effort:   "high",
		Betas:    []string{"beta-a", "beta-b"},
		MaxTurns: 7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	last, ok := rt.LastSpawn()
	if !ok {
		t.Fatal("no spawn recorded")
	}
	if last.Request.Config == nil {
		t.Fatal("spawn request has no config")
	}
	if last.Request.Config.RuntimeName() != "claude-code" {
		t.Errorf("config kind = %q, want claude-code", last.Request.Config.RuntimeName())
	}
}

func TestManagerWithFake_ResumePassesResumeID(t *testing.T) {
	mgr, rt := newFakeManager(t, 4)

	_, err := mgr.Resume(context.Background(), ResumeOpts{
		CLISessionID: "prior-12345",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	last, _ := rt.LastSpawn()
	if last.Request.ResumeID != "prior-12345" {
		t.Errorf("ResumeID = %q, want prior-12345", last.Request.ResumeID)
	}
}

func TestManagerWithFake_SpawnFailurePropagates(t *testing.T) {
	rt := fake.NewRuntime(claude.Codec{})
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		return nil, errors.New("boom")
	})
	mgr := NewManager(rt, t.TempDir(), "default", 4, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	_, err := mgr.Create(context.Background(), CreateOpts{})
	if err == nil {
		t.Fatal("Create should propagate spawn error")
	}

	if got := len(mgr.List()); got != 0 {
		t.Errorf("List after failed Create = %d, want 0", got)
	}
}

func TestSessionWithFake_DriveInitAndResult(t *testing.T) {
	rt := fake.NewRuntime(claude.Codec{})
	var proc *fake.Process
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		proc = fake.NewProcess(cb)
		go func() {
			time.Sleep(10 * time.Millisecond)
			proc.EmitInit("fake-init-1")
		}()
		return proc, nil
	})

	sess, err := NewSession(rt, runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Config:     fake.Config{},
	}, "default", 0)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		proc.Exit(nil)
		sess.ForceClose()
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sess.CurrentState() == StateReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := sess.CurrentState(); got != StateReady {
		t.Fatalf("state = %s, want ready", got)
	}
	if got := sess.Info().CLISessionID; got != "fake-init-1" {
		t.Errorf("CLISessionID = %q, want fake-init-1", got)
	}
}

func TestSessionWithFake_SendMessageWritesEncoded(t *testing.T) {
	rt := fake.NewRuntime(claude.Codec{})
	var proc *fake.Process
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		proc = fake.NewProcess(cb)
		go func() { time.Sleep(5 * time.Millisecond); proc.EmitInit("s-1") }()
		return proc, nil
	})

	sess, err := NewSession(rt, runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Config:     fake.Config{},
	}, "default", 0)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { proc.Exit(nil); sess.ForceClose() })

	// wait for ready
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && sess.CurrentState() != StateReady {
		time.Sleep(10 * time.Millisecond)
	}

	if err := sess.SendMessage("ping"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	written := proc.Written()
	if len(written) == 0 {
		t.Fatal("no bytes written to fake process")
	}
	// claude codec marshals the user message as JSON; verify the payload contains "ping"
	var msg map[string]interface{}
	if err := json.Unmarshal(written[0], &msg); err != nil {
		t.Fatalf("written[0] is not JSON: %v (%q)", err, string(written[0]))
	}
	inner, _ := msg["message"].(map[string]interface{})
	if got, _ := inner["content"].(string); got != "ping" {
		t.Errorf("message.content = %q, want ping", got)
	}
}

func TestSessionWithFake_ProcessExitTransitionsToStopped(t *testing.T) {
	rt := fake.NewRuntime(claude.Codec{})
	var proc *fake.Process
	rt.OnSpawn(func(req runtime.SpawnRequest, cb runtime.Callbacks) (*fake.Process, error) {
		proc = fake.NewProcess(cb)
		return proc, nil
	})

	sess, err := NewSession(rt, runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Config:     fake.Config{},
	}, "default", 0)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.ForceClose() })

	ch := sess.Subscribe("c1")
	proc.Exit(nil)

	// expect session_exit gateway event before channel closes
	gotExit := false
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case raw, ok := <-ch:
			if !ok {
				if !gotExit {
					t.Error("channel closed before session_exit event")
				}
				return
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) == nil {
				if ev, ok := m["_gateway_event"]; ok {
					var evStr string
					_ = json.Unmarshal(ev, &evStr)
					if evStr == "session_exit" {
						gotExit = true
					}
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !gotExit {
		t.Error("did not receive session_exit event")
	}
}
