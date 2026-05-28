package persist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	rfake "github.com/anthropics/claude-code-gateway/internal/runtime/fake"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// newTestManager builds an in-process Manager backed by a fake runtime.
// Sessions imported via ImportIdleSession do not spawn processes, so the
// fake runtime is only required by the constructor.
func newTestManager(t *testing.T) *session.Manager {
	t.Helper()
	rt := rfake.NewRuntime(claude.Codec{})
	return session.NewManager(rt, t.TempDir(), "default", 32, 0, 0)
}

func TestPersist_OriginAndCustomTitleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewJSONStore(path)

	mgr := newTestManager(t)
	_, err := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-1",
		OwnerID:      "alice",
		Label:        "work",
		Summary:      "doing stuff",
		CustomTitle:  "my-renamed",
		Origin:       "external",
		WorkingDir:   "/tmp/proj",
		ChatID:       "chat-1",
		ChannelKind:  "feishu",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if err := store.Save(mgr); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reload into a fresh manager
	mgr2 := newTestManager(t)
	if err := store.Load(mgr2); err != nil {
		t.Fatalf("load: %v", err)
	}

	infos := mgr2.ListBy(session.Filter{OwnerID: "alice"})
	if len(infos) != 1 {
		t.Fatalf("got %d sessions, want 1", len(infos))
	}
	got := infos[0]
	if got.CLISessionID != "cli-1" {
		t.Errorf("CLISessionID = %q, want cli-1", got.CLISessionID)
	}
	if got.Origin != "external" {
		t.Errorf("Origin = %q, want external", got.Origin)
	}
	if got.CustomTitle != "my-renamed" {
		t.Errorf("CustomTitle = %q, want my-renamed", got.CustomTitle)
	}
}

func TestPersist_LegacyMissingOriginDefaultsToFeishu(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := PersistentState{
		Users: map[string]*PersistentUser{
			"alice": {
				FocusedCLIID: "cli-1",
				Sessions: []PersistentSession{
					{
						CLISessionID: "cli-1",
						Label:        "legacy",
						WorkingDir:   "/tmp/proj",
						Status:       "idle",
						// Origin intentionally empty (pre-Step-2 file)
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	mgr := newTestManager(t)
	store := NewJSONStore(path)
	if err := store.Load(mgr); err != nil {
		t.Fatalf("load: %v", err)
	}

	infos := mgr.ListBy(session.Filter{OwnerID: "alice"})
	if len(infos) != 1 {
		t.Fatalf("got %d sessions, want 1", len(infos))
	}
	if infos[0].Origin != "feishu" {
		t.Errorf("Origin = %q, want feishu (legacy default)", infos[0].Origin)
	}
}

func TestPersist_DefaultOriginHelper(t *testing.T) {
	if defaultOrigin("") != "feishu" {
		t.Error("empty → feishu expected")
	}
	if defaultOrigin("external") != "external" {
		t.Error("explicit value should pass through")
	}
	if defaultOrigin("ws") != "ws" {
		t.Error("ws should pass through")
	}
}

// TestPersist_ExternalSummaryRoundTrip guards against the regression where
// discovery re-enqueued every external session on restart, wasting summary
// API budget. Worker-generated summaries for unowned external sessions must
// survive Save/Load and be retrievable via ExternalAugmentationFor.
func TestPersist_ExternalSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewJSONStore(path)

	mgr := newTestManager(t)
	// Simulate worker writing an AI summary for an unowned external session.
	store.RecordExternalSummary("ext-1", ExternalAugmentation{
		Summary:       "重构 session manager",
		PromptVersion: 2,
	})

	if err := store.Save(mgr); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reload into a fresh store + manager
	store2 := NewJSONStore(path)
	mgr2 := newTestManager(t)
	if err := store2.Load(mgr2); err != nil {
		t.Fatalf("load: %v", err)
	}

	aug, ok := store2.ExternalAugmentationFor("ext-1")
	if !ok {
		t.Fatal("ExternalAugmentationFor(ext-1) not found after Save+Load")
	}
	if aug.Summary != "重构 session manager" {
		t.Errorf("Summary = %q, want %q", aug.Summary, "重构 session manager")
	}
	if aug.PromptVersion != 2 {
		t.Errorf("PromptVersion = %d, want 2", aug.PromptVersion)
	}
}

func TestPersist_ExternalSummarySkipsEmpty(t *testing.T) {
	// Only sessions for which the worker explicitly recorded an augmentation
	// get persisted — saving empty placeholders would waste file size.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewJSONStore(path)

	// Worker never called RecordExternalSummary for ext-untouched
	store.RecordExternalSummary("ext-named", ExternalAugmentation{
		CustomTitle: "my-work",
	})

	mgr := newTestManager(t)
	if err := store.Save(mgr); err != nil {
		t.Fatalf("save: %v", err)
	}
	store2 := NewJSONStore(path)
	if err := store2.Load(newTestManager(t)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := store2.ExternalAugmentationFor("ext-untouched"); ok {
		t.Error("untouched external session should NOT be persisted")
	}
	if aug, ok := store2.ExternalAugmentationFor("ext-named"); !ok || aug.CustomTitle != "my-work" {
		t.Errorf("recorded augmentation should persist: ok=%v aug=%+v", ok, aug)
	}
}

func TestPersist_ExternalSummaryMergesAcrossSaves(t *testing.T) {
	// Multiple worker runs across restarts must accumulate, not clobber.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	{
		store := NewJSONStore(path)
		store.RecordExternalSummary("ext-A", ExternalAugmentation{Summary: "topic A", PromptVersion: 1})
		if err := store.Save(newTestManager(t)); err != nil {
			t.Fatalf("save A: %v", err)
		}
	}

	// Second run: fresh store loads A from disk, then worker records B.
	{
		store := NewJSONStore(path)
		if err := store.Load(newTestManager(t)); err != nil {
			t.Fatalf("load: %v", err)
		}
		store.RecordExternalSummary("ext-B", ExternalAugmentation{Summary: "topic B", PromptVersion: 1})
		if err := store.Save(newTestManager(t)); err != nil {
			t.Fatalf("save B: %v", err)
		}
	}

	store := NewJSONStore(path)
	if err := store.Load(newTestManager(t)); err != nil {
		t.Fatalf("load final: %v", err)
	}
	if aug, ok := store.ExternalAugmentationFor("ext-A"); !ok || aug.Summary != "topic A" {
		t.Errorf("ext-A lost across saves: ok=%v aug=%+v", ok, aug)
	}
	if aug, ok := store.ExternalAugmentationFor("ext-B"); !ok || aug.Summary != "topic B" {
		t.Errorf("ext-B missing: ok=%v aug=%+v", ok, aug)
	}
}

// TestPersist_PromptVersionInvalidation: discovery uses PromptVersion to
// decide whether to re-enqueue. A summary from an older version should be
// readable but treated as stale by callers.
func TestPersist_PromptVersionInvalidation(t *testing.T) {
	store := NewJSONStore(filepath.Join(t.TempDir(), "state.json"))
	store.RecordExternalSummary("old", ExternalAugmentation{
		Summary:       "outdated",
		PromptVersion: 1,
	})
	store.RecordExternalSummary("new", ExternalAugmentation{
		Summary:       "fresh",
		PromptVersion: 2,
	})

	oldAug, _ := store.ExternalAugmentationFor("old")
	newAug, _ := store.ExternalAugmentationFor("new")

	if oldAug.PromptVersion >= 2 {
		t.Error("old version should compare less than current")
	}
	if newAug.PromptVersion < 2 {
		t.Error("new version should be >= current")
	}
}

// TestPersist_CountFreshExternalSummaries verifies the disk-source progress
// counter that /status uses. Must reflect what's actually persisted, ignore
// empty summaries (skip_meta placeholders shouldn't either, but here we
// only test non-empty), and respect the version gate.
func TestPersist_CountFreshExternalSummaries(t *testing.T) {
	store := NewJSONStore(filepath.Join(t.TempDir(), "state.json"))
	store.RecordExternalSummary("v1", ExternalAugmentation{Summary: "old", PromptVersion: 1})
	store.RecordExternalSummary("v3a", ExternalAugmentation{Summary: "fresh-a", PromptVersion: 3})
	store.RecordExternalSummary("v3b", ExternalAugmentation{Summary: "fresh-b", PromptVersion: 3})
	store.RecordExternalSummary("v5", ExternalAugmentation{Summary: "newest", PromptVersion: 5})
	store.RecordExternalSummary("empty", ExternalAugmentation{Summary: "", PromptVersion: 5})

	cases := []struct {
		minVersion int
		want       int
	}{
		{1, 4}, // v1 + v3a + v3b + v5 (empty skipped)
		{3, 3}, // v3a + v3b + v5
		{5, 1}, // v5 only
		{6, 0}, // none yet
	}
	for _, c := range cases {
		got := store.CountFreshExternalSummaries(c.minVersion)
		if got != c.want {
			t.Errorf("CountFreshExternalSummaries(%d) = %d, want %d", c.minVersion, got, c.want)
		}
	}
}

// TestPersist_CountSurvivesReload: progress data must be readable after a
// fresh Load — that's the whole reason /status reads from disk (worker
// counter resets on restart, disk doesn't).
func TestPersist_CountSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store := NewJSONStore(path)
	store.RecordExternalSummary("a", ExternalAugmentation{Summary: "x", PromptVersion: 6})
	store.RecordExternalSummary("b", ExternalAugmentation{Summary: "y", PromptVersion: 6})
	if err := store.Save(newTestManager(t)); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Fresh store reads from disk
	store2 := NewJSONStore(path)
	if err := store2.Load(newTestManager(t)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := store2.CountFreshExternalSummaries(6); got != 2 {
		t.Errorf("after reload: count = %d, want 2", got)
	}
}
