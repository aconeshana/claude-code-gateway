package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

func writeSkill(t *testing.T, base, name, fm, body string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var content string
	if fm != "" {
		content = "---\n" + fm + "\n---\n" + body
	} else {
		content = body
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withHome forces homeDir() to return tmp for this test.
func withHome(t *testing.T, dir string) {
	t.Helper()
	old := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", old) })
}

func TestScanSkills_LayeredOverride(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	workDir := filepath.Join(home, "code", "proj")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mark git root at workDir so walk stops there.
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	projSkills := filepath.Join(workDir, ".claude", "skills")
	globalSkills := filepath.Join(home, ".claude", "skills")

	writeSkill(t, projSkills, "foo", "name: foo\ndescription: project foo", "project foo body")
	writeSkill(t, projSkills, "proj-only", "description: project-only skill", "body p")
	writeSkill(t, globalSkills, "foo", "description: global foo", "global foo body")
	writeSkill(t, globalSkills, "global-only", "description: global-only skill", "body g")

	got := scanSkills(workDir)
	names := map[string]skillEntry{}
	for _, s := range got {
		names[s.Name] = s
	}

	if want := 3; len(names) != want {
		t.Fatalf("skill count = %d, want %d (names=%v)", len(names), want, names)
	}
	if got := names["foo"].Description; !strings.Contains(got, "project") {
		t.Errorf("foo description = %q, want project-level to win", got)
	}
	if _, ok := names["proj-only"]; !ok {
		t.Errorf("proj-only missing")
	}
	if _, ok := names["global-only"]; !ok {
		t.Errorf("global-only missing")
	}
}

func TestScanSkills_GitRootStopsWalk(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "inside", "description: inside repo", "body")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "outside", "description: outside repo", "body")

	workDir := filepath.Join(repo, "sub")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanSkills(workDir)
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["inside"] || !names["outside"] {
		t.Errorf("expected both inside+outside, got %v", names)
	}
}

func TestParseSkillMD_BlockScalarDescription(t *testing.T) {
	raw := `---
name: agent-reach
description: >
  Use the internet: search, read, and interact with 13+ platforms.
  Triggers when user asks about Twitter, YouTube, GitHub.
---
The body content here.
More body.`
	s := parseSkillMD("agent-reach", raw, "/tmp/agent-reach/SKILL.md")
	if s == nil {
		t.Fatal("nil skill")
	}
	if !strings.Contains(s.Description, "Use the internet") {
		t.Errorf("desc missing folded content: %q", s.Description)
	}
	if !strings.Contains(s.Description, "Twitter, YouTube, GitHub") {
		t.Errorf("desc missing continuation: %q", s.Description)
	}
	if !strings.Contains(s.Body, "The body content here") {
		t.Errorf("body wrong: %q", s.Body)
	}
}

func TestParseSkillMD_NoFrontmatterUsesBodyFirstLine(t *testing.T) {
	raw := "Just a short skill that does X.\nSecond line."
	s := parseSkillMD("plain", raw, "/tmp/plain/SKILL.md")
	if s == nil {
		t.Fatal("nil skill")
	}
	if s.Description != "Just a short skill that does X." {
		t.Errorf("desc = %q", s.Description)
	}
}

func TestBridge_SkillsCommand_RendersDiscoveredSkill(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	writeSkill(t, filepath.Join(home, ".claude", "skills"), "my-skill",
		"description: a test skill", "do the thing")

	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputText, Text: "/skills",
	})

	out := ch.Outbound()
	if len(out) != 1 || out[0].Card == nil {
		t.Fatalf("expected 1 card, got %+v", out)
	}
	found := false
	for _, sec := range out[0].Card.Sections {
		if strings.Contains(sec.Markdown, "my-skill") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /my-skill in card sections, got %+v", out[0].Card.Sections)
	}
}

func TestBridge_RunSkill_InjectsPromptAndShowsReceipt(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	writeSkill(t, filepath.Join(home, ".claude", "skills"), "do-thing",
		"description: does the thing", "body")

	b, ch, mgr := newTestBridge(t)
	sess, err := mgr.Create(context.Background(), session.CreateOpts{
		OwnerID: "alice", WorkingDir: home, ChatID: "c1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetFocus("alice", sess.ID); err != nil {
		t.Fatal(err)
	}

	beforeCount := sess.Info().MessageCount

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputCardAction,
		Action: &channel.CardAction{
			Name:      "run_skill",
			Values:    map[string]interface{}{"key": "do-thing"},
			FormValue: map[string]interface{}{"user_input": "find the bug"},
		},
	})

	if got := sess.Info().MessageCount; got != beforeCount+1 {
		t.Errorf("MessageCount = %d, want %d (SendMessage should have fired)", got, beforeCount+1)
	}

	out := ch.Outbound()
	if len(out) == 0 {
		t.Fatalf("expected outbound receipt card, got none")
	}
	last := out[len(out)-1]
	if last.Card == nil {
		t.Fatalf("last outbound has no card: %+v", last)
	}
	if !strings.Contains(last.Card.Title, "已执行") {
		t.Errorf("expected '已执行' in title, got %q", last.Card.Title)
	}
	hasPrompt := false
	for _, sec := range last.Card.Sections {
		if strings.Contains(sec.Markdown, "/do-thing") && strings.Contains(sec.Markdown, "find the bug") {
			hasPrompt = true
		}
	}
	if !hasPrompt {
		t.Errorf("receipt missing prompt preview, sections=%+v", last.Card.Sections)
	}
}

func TestBridge_RunSkill_NoFocusAutoCreatesSession(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "x", "description: x", "body")

	b, ch, mgr := newTestBridge(t)
	if _, ok := mgr.FocusedSession("alice"); ok {
		t.Fatal("precondition: expected no focus")
	}

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID: "alice", ChatID: "c1", Kind: channel.InputCardAction,
		Action: &channel.CardAction{
			Name:   "run_skill",
			Values: map[string]interface{}{"key": "x"},
		},
	})

	// Auto-created and focused.
	sess, ok := mgr.FocusedSession("alice")
	if !ok {
		t.Fatal("expected auto-created session to be focused")
	}
	if got := sess.Info().MessageCount; got != 1 {
		t.Errorf("MessageCount = %d, want 1 (skill prompt should have been sent)", got)
	}

	out := ch.Outbound()
	if len(out) == 0 {
		t.Fatalf("expected outbound receipt card, got none")
	}
	last := out[len(out)-1]
	if last.Card == nil || !strings.Contains(last.Card.Title, "已执行") {
		t.Errorf("expected '已执行' receipt, got %+v", last.Card)
	}
}
