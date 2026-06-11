package claudefiles

import (
	"path/filepath"
	"testing"
)

func TestListCommands_TopLevelAndNamespaced(t *testing.T) {
	withFakeHome(t, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", "commands", "build-fix.md"),
			"# Build and Fix\n\nIncrementally fix TypeScript errors.")
		writeFile(t, filepath.Join(home, ".claude", "commands", "ccpanes", "browse.md"),
			"---\ndescription: 浏览终端\n---\nbody")

		got := ListCommands("")
		if len(got) != 2 {
			t.Fatalf("expected 2 commands, got %d: %+v", len(got), got)
		}
		var top, ns Command
		for _, c := range got {
			switch c.Name {
			case "build-fix":
				top = c
			case "ccpanes:browse":
				ns = c
			}
		}
		if top.Description != "Build and Fix" {
			t.Errorf("top command desc fallback (heading) failed: %q", top.Description)
		}
		if ns.Description != "浏览终端" {
			t.Errorf("namespaced command frontmatter desc lost: %q", ns.Description)
		}
		if ns.Source != SourceUser {
			t.Errorf("expected SourceUser, got %v", ns.Source)
		}
	})
}

func TestListCommands_ProjectOverridesUser(t *testing.T) {
	withFakeHome(t, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", "commands", "shared.md"),
			"# user-version")
		project := t.TempDir()
		writeFile(t, filepath.Join(project, ".claude", "commands", "shared.md"),
			"# project-version")
		got := ListCommands(project)
		if len(got) != 1 {
			t.Fatalf("expected 1 (project overrides user), got %d", len(got))
		}
		if got[0].Description != "project-version" || got[0].Source != SourceProject {
			t.Errorf("expected project override, got %+v", got[0])
		}
	})
}

func TestFirstHeadingOrLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# Title\nbody", "Title"},
		{"\n\n## Sub\nstuff", "Sub"},
		{"plain first line\n# heading later", "plain first line"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstHeadingOrLine(c.in); got != c.want {
			t.Errorf("firstHeadingOrLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
