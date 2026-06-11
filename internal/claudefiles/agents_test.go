package claudefiles

import (
	"path/filepath"
	"testing"
)

func TestListAgents_UserAndProjectMerge(t *testing.T) {
	withFakeHome(t, func(home string) {
		// User-scope agent.
		writeFile(t, filepath.Join(home, ".claude", "agents", "alpha.md"),
			"---\nname: alpha\ndescription: user version\ntools: Read\nmodel: opus\n---\nbody")
		// Project-scope agent overrides "alpha" + adds "beta".
		project := t.TempDir()
		writeFile(t, filepath.Join(project, ".claude", "agents", "alpha.md"),
			"---\nname: alpha\ndescription: project version\n---\nbody")
		writeFile(t, filepath.Join(project, ".claude", "agents", "beta.md"),
			"---\nname: beta\ndescription: project only\n---\n")

		got := ListAgents(project)
		if len(got) != 2 {
			t.Fatalf("expected 2 agents (alpha overridden + beta), got %d: %+v", len(got), got)
		}
		// Project entries come first when sorted.
		if got[0].Source != SourceProject || got[1].Source != SourceProject {
			t.Errorf("expected both entries from project scope, got: %+v", got)
		}
		// Alpha must come from project file (override applied).
		var alpha Agent
		for _, a := range got {
			if a.Name == "alpha" {
				alpha = a
				break
			}
		}
		if alpha.Description != "project version" {
			t.Errorf("alpha override failed: desc=%q", alpha.Description)
		}
	})
}

func TestListAgents_MissingFrontmatterSkipped(t *testing.T) {
	withFakeHome(t, func(home string) {
		// File exists but no frontmatter — must be skipped (no name to surface).
		writeFile(t, filepath.Join(home, ".claude", "agents", "broken.md"), "just body, no frontmatter\n")
		writeFile(t, filepath.Join(home, ".claude", "agents", "ok.md"),
			"---\nname: ok\ndescription: fine\n---\n")
		got := ListAgents("")
		if len(got) != 1 || got[0].Name != "ok" {
			t.Errorf("expected only 'ok', got %+v", got)
		}
	})
}

func TestListAgents_NoHomeNoProject(t *testing.T) {
	t.Setenv("HOME", "")
	if got := ListAgents(""); len(got) != 0 {
		t.Errorf("expected empty slice, got %+v", got)
	}
}
