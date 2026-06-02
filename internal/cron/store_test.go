package cron

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJSONStore_CRUD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}

	j := NewJob("proj", "u1", "c1", "*/5 * * * *", "check build", "/tmp", "build check")

	// Add
	if err := s.Add(j); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 1 {
		t.Fatal("expected 1 job")
	}

	// Duplicate add
	if err := s.Add(j); err == nil {
		t.Fatal("expected duplicate error")
	}

	// Get
	got, ok := s.Get(j.ID)
	if !ok {
		t.Fatal("expected to find job")
	}
	if got.Prompt != "check build" {
		t.Fatalf("wrong prompt: %s", got.Prompt)
	}

	// Update
	j.Prompt = "deploy"
	if err := s.Update(j); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(j.ID)
	if got.Prompt != "deploy" {
		t.Fatal("update not reflected")
	}

	// SetEnabled
	if err := s.SetEnabled(j.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(j.ID)
	if got.Enabled {
		t.Fatal("expected disabled")
	}

	// MarkRun
	if err := s.MarkRun(j.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(j.ID)
	if got.LastRun == nil {
		t.Fatal("expected LastRun set")
	}
	if got.LastError != "" {
		t.Fatal("expected empty LastError")
	}

	// Remove
	if err := s.Remove(j.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected 0 jobs")
	}
}

func TestJSONStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	s, _ := NewJSONStore(path)
	j := NewJob("p", "u", "c", "0 9 * * *", "morning", "/tmp", "morning check")
	_ = s.Add(j)

	// Reload from disk.
	s2, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 1 {
		t.Fatal("expected 1 job after reload")
	}
	got, _ := s2.Get(j.ID)
	if got.Prompt != "morning" {
		t.Fatalf("wrong prompt after reload: %s", got.Prompt)
	}
}

func TestJSONStore_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty store")
	}
}

func TestJSONStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(path, []byte("{invalid json"), 0600)

	_, err := NewJSONStore(path)
	if err == nil {
		t.Fatal("expected error on corrupt file")
	}
}
