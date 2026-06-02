package cron

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mockExecutor records calls and returns a configurable result.
type mockExecutor struct {
	mu   sync.Mutex
	runs []ExecRequest
	res  ExecResult
}

func (m *mockExecutor) Name() string { return "mock" }
func (m *mockExecutor) Execute(_ context.Context, req ExecRequest) ExecResult {
	m.mu.Lock()
	m.runs = append(m.runs, req)
	res := m.res
	m.mu.Unlock()
	return res
}
func (m *mockExecutor) Runs() []ExecRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ExecRequest, len(m.runs))
	copy(out, m.runs)
	return out
}

func TestScheduler_FiresJob(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONStore(filepath.Join(dir, "jobs.json"))
	rl := NewRunLog(filepath.Join(dir, "history.json"), 10)

	exec := &mockExecutor{res: ExecResult{Summary: "ok"}}

	var cbMu sync.Mutex
	var callbacks []string
	cb := func(j Job, r ExecResult) {
		cbMu.Lock()
		callbacks = append(callbacks, j.ID)
		cbMu.Unlock()
	}

	// Create a job that fires every minute.
	j := NewJob("p", "u", "c", "* * * * *", "hello", "/tmp", "test")
	_ = store.Add(j)

	sched := NewScheduler(store, exec, rl, cb)

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Start(ctx)

	// Wait up to 90 seconds for at least one execution.
	deadline := time.After(90 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			t.Fatal("timeout waiting for job to fire")
		case <-time.After(500 * time.Millisecond):
			if len(exec.Runs()) > 0 {
				cancel()
				// Verify the run was recorded.
				h := rl.History(j.ID)
				if len(h) == 0 {
					t.Fatal("expected run history entry")
				}
				cbMu.Lock()
				if len(callbacks) == 0 {
					t.Fatal("expected callback")
				}
				cbMu.Unlock()
				return
			}
		}
	}
}

func TestScheduler_DisabledJobSkipped(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONStore(filepath.Join(dir, "jobs.json"))
	rl := NewRunLog(filepath.Join(dir, "history.json"), 10)
	exec := &mockExecutor{}

	j := NewJob("p", "u", "c", "* * * * *", "hello", "/tmp", "disabled")
	j.Enabled = false
	_ = store.Add(j)

	sched := NewScheduler(store, exec, rl, nil)
	stats := sched.Stats()
	// Before start, stats are zero.
	if stats.Active != 0 {
		t.Fatal("expected 0 active before start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Start(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()

	s := sched.Stats()
	if s.Active != 0 {
		t.Errorf("expected 0 active, got %d", s.Active)
	}
	if s.Disabled != 1 {
		t.Errorf("expected 1 disabled, got %d", s.Disabled)
	}
	if len(exec.Runs()) != 0 {
		t.Fatal("disabled job should not have run")
	}
}

func TestScheduler_Reload(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONStore(filepath.Join(dir, "jobs.json"))
	rl := NewRunLog(filepath.Join(dir, "history.json"), 10)
	exec := &mockExecutor{}

	sched := NewScheduler(store, exec, rl, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Start(ctx)
	defer cancel()

	time.Sleep(100 * time.Millisecond)

	// Add a job while scheduler is running.
	j := NewJob("p", "u", "c", "* * * * *", "dynamic", "/tmp", "dyn")
	_ = store.Add(j)
	sched.Reload()

	time.Sleep(100 * time.Millisecond)
	s := sched.Stats()
	if s.Active != 1 {
		t.Errorf("expected 1 active after reload, got %d", s.Active)
	}
}
