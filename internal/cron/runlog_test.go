package cron

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRunLog_AppendAndHistory(t *testing.T) {
	dir := t.TempDir()
	rl := NewRunLog(filepath.Join(dir, "history.json"), 3)

	for i := 0; i < 5; i++ {
		rl.Append(RunRecord{
			JobID:     "j1",
			StartedAt: time.Now().Add(time.Duration(i) * time.Second),
			DurationS: 1.0,
			Status:    "ok",
			Summary:   "run",
		})
	}

	// Cap is 3 — only the last 3 should remain.
	h := rl.History("j1")
	if len(h) != 3 {
		t.Fatalf("expected 3 records, got %d", len(h))
	}
}

func TestRunLog_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")

	rl := NewRunLog(path, 10)
	rl.Append(RunRecord{
		JobID:     "j1",
		StartedAt: time.Now(),
		DurationS: 2.5,
		Status:    "ok",
		Summary:   "done",
	})

	rl2 := NewRunLog(path, 10)
	h := rl2.History("j1")
	if len(h) != 1 {
		t.Fatalf("expected 1 record after reload, got %d", len(h))
	}
	if h[0].Summary != "done" {
		t.Fatalf("wrong summary: %s", h[0].Summary)
	}
}

func TestRunLog_Purge(t *testing.T) {
	dir := t.TempDir()
	rl := NewRunLog(filepath.Join(dir, "h.json"), 10)
	rl.Append(RunRecord{JobID: "j1", Status: "ok"})
	rl.Append(RunRecord{JobID: "j2", Status: "ok"})

	rl.Purge("j1")
	if len(rl.History("j1")) != 0 {
		t.Fatal("expected purged")
	}
	if len(rl.History("j2")) != 1 {
		t.Fatal("j2 should be unaffected")
	}
}

func TestRunLog_AllHistory(t *testing.T) {
	dir := t.TempDir()
	rl := NewRunLog(filepath.Join(dir, "h.json"), 10)

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)

	rl.Append(RunRecord{JobID: "j1", StartedAt: t1, Status: "ok"})
	rl.Append(RunRecord{JobID: "j2", StartedAt: t3, Status: "ok"})
	rl.Append(RunRecord{JobID: "j1", StartedAt: t2, Status: "error"})

	all := rl.AllHistory(2)
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
	// Newest first.
	if all[0].StartedAt != t3 {
		t.Error("expected newest first")
	}
}
