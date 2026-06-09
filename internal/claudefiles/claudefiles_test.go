package claudefiles

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeHome runs fn with $HOME set to a fresh tmpdir, then restores.
// Each scan function reads $HOME at call time, so this is the canonical
// way to isolate tests from the real user config.
func withFakeHome(t *testing.T, fn func(home string)) {
	t.Helper()
	tmp := t.TempDir()
	old := os.Getenv("HOME")
	t.Setenv("HOME", tmp)
	t.Cleanup(func() { _ = os.Setenv("HOME", old) })
	fn(tmp)
}

// writeFile is a tiny helper for setting up fixtures.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestParseFrontmatter_BasicAndQuoted(t *testing.T) {
	in := `name: foo
description: "with quotes"
tools: 'a, b'
empty:
# comment line
trailing: x`
	got := parseFrontmatter(in)
	want := map[string]string{
		"name":        "foo",
		"description": "with quotes",
		"tools":       "a, b",
		"empty":       "",
		"trailing":    "x",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %q, want %q", k, got[k], v)
		}
	}
}

func TestParseFrontmatter_BlockScalarFolded(t *testing.T) {
	in := `name: foo
description: >
  This is a long
  description that folds
  into one line.
tools: x`
	got := parseFrontmatter(in)
	if got["description"] != "This is a long description that folds into one line." {
		t.Errorf("folded scalar mishandled: %q", got["description"])
	}
	if got["tools"] != "x" {
		t.Errorf("scanner didn't resume after block scalar: tools=%q", got["tools"])
	}
}

// Anthropic docs show `tools:` both as an inline `Read, Grep` string AND as
// a YAML list. The agent card silently rendered empty tools for list form
// before — make sure both shapes produce equivalent comma-joined output.
func TestParseFrontmatter_YAMLListSyntax(t *testing.T) {
	t.Run("simple list", func(t *testing.T) {
		in := `name: foo
tools:
  - Read
  - Grep
  - Glob
description: agent`
		got := parseFrontmatter(in)
		if got["tools"] != "Read, Grep, Glob" {
			t.Errorf("list-form tools: got %q, want %q", got["tools"], "Read, Grep, Glob")
		}
		if got["description"] != "agent" {
			t.Errorf("scanner didn't resume after list: description=%q", got["description"])
		}
	})
	t.Run("quoted list items", func(t *testing.T) {
		in := `tools:
  - "Read"
  - 'Grep'`
		got := parseFrontmatter(in)
		if got["tools"] != "Read, Grep" {
			t.Errorf("quoted list items: got %q", got["tools"])
		}
	})
	t.Run("empty value without list also fine", func(t *testing.T) {
		in := `name: foo
empty:
next: bar`
		got := parseFrontmatter(in)
		if got["empty"] != "" || got["next"] != "bar" {
			t.Errorf("empty value handling broke; got %+v", got)
		}
	})
}

func TestSplitFrontmatterAndBody(t *testing.T) {
	t.Run("with frontmatter", func(t *testing.T) {
		in := "---\nname: foo\n---\nbody line"
		fm, body := splitFrontmatterAndBody(in)
		if fm["name"] != "foo" || body != "body line" {
			t.Errorf("got fm=%v body=%q", fm, body)
		}
	})
	t.Run("missing closing fence", func(t *testing.T) {
		in := "---\nname: foo\nno closer"
		fm, body := splitFrontmatterAndBody(in)
		// No proper terminator → treat the whole thing as body so we don't
		// silently swallow the file.
		if fm != nil {
			t.Errorf("expected nil fm, got %v", fm)
		}
		if body != in {
			t.Errorf("body should be entire content")
		}
	})
	t.Run("no frontmatter", func(t *testing.T) {
		fm, body := splitFrontmatterAndBody("plain markdown")
		if fm != nil || body != "plain markdown" {
			t.Errorf("got fm=%v body=%q", fm, body)
		}
	})
}
