package plan

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePlan(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTitle(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"# heading", "# Toolathlon 评测集接入\n\nbody", "Toolathlon 评测集接入"},
		{"## heading", "## Plan: HTTP gateway\n\nbody", "Plan: HTTP gateway"},
		{"heading after blank lines", "\n\n# 标题\n", "标题"},
		// First non-empty line is fallback only when there's no heading
		// anywhere in the file — heading wins regardless of position.
		{"plain line then heading", "Intro paragraph.\n\n# Real title\n", "Real title"},
		{"plain only, no heading", "Just some text\nmore text", "Just some text"},
		{"empty", "", "(untitled)"},
		{"only whitespace", "  \n\n  \n", "(untitled)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTitle([]byte(c.body))
			if got != c.want {
				t.Errorf("extractTitle(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestIndex_ListSortsByMTimeDesc(t *testing.T) {
	dir := t.TempDir()
	writePlan(t, dir, "a.md", "# Old plan")
	bPath := writePlan(t, dir, "b.md", "# Newer plan")
	cPath := writePlan(t, dir, "c.md", "# Newest plan")

	// Force mtimes: a = oldest, b = middle, c = newest
	old := time.Now().Add(-2 * time.Hour)
	mid := time.Now().Add(-1 * time.Hour)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "a.md"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bPath, mid, mid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cPath, now, now); err != nil {
		t.Fatal(err)
	}

	idx := NewIndex(dir)
	plans, err := idx.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("got %d, want 3", len(plans))
	}
	if plans[0].Filename != "c.md" || plans[1].Filename != "b.md" || plans[2].Filename != "a.md" {
		t.Errorf("order = %s, %s, %s; want c b a",
			plans[0].Filename, plans[1].Filename, plans[2].Filename)
	}
	if plans[0].Title != "Newest plan" {
		t.Errorf("title = %q, want 'Newest plan'", plans[0].Title)
	}
}

func TestIndex_ListSkipsNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	writePlan(t, dir, "real.md", "# Real")
	writePlan(t, dir, "ignored.txt", "# Not a plan")
	writePlan(t, dir, ".hidden.md", "# Hidden but parsed")

	plans, err := NewIndex(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Errorf("got %d, want 2 (.md only)", len(plans))
	}
}

func TestIndex_ListMissingDirIsEmpty(t *testing.T) {
	plans, err := NewIndex("/nonexistent/path/to/plans").List()
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("missing dir should return empty, got %d", len(plans))
	}
}

func TestIndex_GetRejectsPathTraversal(t *testing.T) {
	idx := NewIndex(t.TempDir())
	cases := []string{"../etc/passwd", "subdir/file.md", `..\windows`, ""}
	for _, c := range cases {
		if _, err := idx.Get(c); err == nil {
			t.Errorf("Get(%q) should reject path traversal", c)
		}
	}
}

func TestIndex_GetReturnsBodyAndMTime(t *testing.T) {
	dir := t.TempDir()
	writePlan(t, dir, "p.md", "# Title\n\nbody here\n")
	p, err := NewIndex(dir).Get("p.md")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Title" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Body == "" {
		t.Error("Body should be populated")
	}
	if p.MTime.IsZero() {
		t.Error("MTime should be set")
	}
	if p.Size == 0 {
		t.Error("Size should be > 0")
	}
}
