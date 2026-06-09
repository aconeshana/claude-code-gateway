package bridge

import (
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

func TestBuildAgentsCard_Empty(t *testing.T) {
	card := buildAgentsCard(nil, "/work")
	if !strings.Contains(card.Sections[0].Markdown, "未发现可用 agent") {
		t.Errorf("empty agents card should explain emptiness, got: %s", card.Sections[0].Markdown)
	}
	if !strings.Contains(card.Sections[0].Markdown, "/work/.claude/agents") {
		t.Errorf("empty hint should mention project path")
	}
}

func TestBuildAgentsCard_HeaderAndSourceTags(t *testing.T) {
	agents := []claudefiles.Agent{
		{Name: "alpha", Description: "first", Source: claudefiles.SourceProject, Path: "/p/.claude/agents/alpha.md"},
		{Name: "beta", Description: "second", Model: "opus", Tools: "Read, Grep",
			Source: claudefiles.SourceUser, Path: "/u/.claude/agents/beta.md"},
	}
	card := buildAgentsCard(agents, "/p")
	if !strings.Contains(card.Title, "Agents · 2") {
		t.Errorf("title should report total: %s", card.Title)
	}
	header := card.Sections[0].Markdown
	if !strings.Contains(header, "项目 1") || !strings.Contains(header, "用户 1") {
		t.Errorf("header counts wrong: %s", header)
	}
	// Each agent gets its own divider section.
	var foundAlpha, foundBeta bool
	for _, sec := range card.Sections {
		md := sec.Markdown
		if strings.Contains(md, "**alpha**") && strings.Contains(md, "[项目]") {
			foundAlpha = true
		}
		if strings.Contains(md, "**beta**") && strings.Contains(md, "[用户]") &&
			strings.Contains(md, "`opus`") && strings.Contains(md, "Read, Grep") {
			foundBeta = true
		}
	}
	if !foundAlpha || !foundBeta {
		t.Errorf("expected both agents rendered with source tags + metadata; alpha=%v beta=%v", foundAlpha, foundBeta)
	}
}

func TestBuildCommandsCard_NamespacedNameRendered(t *testing.T) {
	cmds := []claudefiles.Command{
		{Name: "ccpanes:browse", Description: "浏览 panes", Source: claudefiles.SourceUser, Path: "/u/.../ccpanes/browse.md"},
	}
	// An empty Bridge has no registered commands → no gateway conflict will
	// be flagged, which is the path we want to exercise here.
	b := &Bridge{}
	card := b.buildCommandsCard(cmds, "/p")
	for _, sec := range card.Sections {
		// The card must surface "/ccpanes:browse" — the leading slash and
		// the namespace separator are how the user actually invokes it.
		if strings.Contains(sec.Markdown, "`/ccpanes:browse`") {
			return
		}
	}
	t.Errorf("namespaced command not rendered with leading slash; sections: %+v", card.Sections)
}

func TestBuildCommandsCard_FlagsGatewayConflict(t *testing.T) {
	// Bridge with a single registered command — anything sharing that name
	// in claudefiles.ListCommands should be flagged as inert.
	b := &Bridge{commands: []Command{{Name: "/list"}}}
	cmds := []claudefiles.Command{
		{Name: "list", Source: claudefiles.SourceUser, Path: "/u/.claude/commands/list.md"},
		{Name: "fine", Source: claudefiles.SourceUser, Path: "/u/.claude/commands/fine.md"},
	}
	card := b.buildCommandsCard(cmds, "")
	full := ""
	for _, sec := range card.Sections {
		full += sec.Markdown + "\n"
	}
	// Find the section for "list" specifically — must carry the warning.
	if !strings.Contains(full, "`/list`") || !strings.Contains(full, "与网关命令冲突") {
		t.Errorf("expected gateway-conflict warning on /list; got: %s", full)
	}
	// "fine" must NOT carry the warning (sanity check that we didn't mass-flag).
	for _, sec := range card.Sections {
		if strings.Contains(sec.Markdown, "`/fine`") && strings.Contains(sec.Markdown, "与网关命令冲突") {
			t.Errorf("non-conflicting command got falsely flagged: %s", sec.Markdown)
		}
	}
}

func TestBuildMCPCard_HidesEnvValues(t *testing.T) {
	servers := []claudefiles.MCPServer{
		{
			Name: "secret-server", Type: "stdio", Command: "node",
			Args:   []string{"server.js"},
			Env:    map[string]string{"API_KEY": "sk-shouldnotappear", "REGION": "us"},
			Source: claudefiles.SourceUser, Path: "/u/.claude.json",
		},
	}
	card := buildMCPCard(servers, "")
	full := ""
	for _, sec := range card.Sections {
		full += sec.Markdown + "\n"
	}
	if strings.Contains(full, "sk-shouldnotappear") {
		t.Errorf("env values must not leak into card body")
	}
	// Keys are still rendered so the user knows the server has env-var setup.
	if !strings.Contains(full, "API_KEY") || !strings.Contains(full, "REGION") {
		t.Errorf("env keys should be visible: %s", full)
	}
}

func TestBuildMCPCard_HTTPVsStdio(t *testing.T) {
	servers := []claudefiles.MCPServer{
		{Name: "stdio-svr", Type: "stdio", Command: "node", Args: []string{"x.js"}, Source: claudefiles.SourceUser, Path: "/p"},
		{Name: "http-svr", Type: "http", URL: "http://example/mcp", Source: claudefiles.SourceUser, Path: "/p"},
	}
	card := buildMCPCard(servers, "")
	full := ""
	for _, sec := range card.Sections {
		full += sec.Markdown + "\n"
	}
	if !strings.Contains(full, "node x.js") {
		t.Errorf("stdio server should show command + args concatenated")
	}
	if !strings.Contains(full, "http://example/mcp") {
		t.Errorf("http server should show URL")
	}
}

func TestBuildHooksCard_RedactsSecrets(t *testing.T) {
	hooks := []claudefiles.Hook{
		// Webhook URL with secret in the path (Slack-style).
		{Event: "Notification", Type: "http",
			URL:    "https://hooks.slack.com/services/T0/B0/SECRET_TOKEN_xyz",
			Source: claudefiles.SourceUser, Path: "/u/.claude/settings.json"},
		// Command with bearer token in argv.
		{Event: "PostToolUse", Matcher: "Edit", Type: "command",
			Command: "curl -H 'Authorization: Bearer sk-shouldnotleak' https://api/x",
			Source:  claudefiles.SourceUser, Path: "/u/.claude/settings.json"},
		// Env-prefixed command — should peel ENV and show the real binary.
		{Event: "Stop", Type: "command",
			Command: "GITHUB_TOKEN=ghp_shouldnotleak node /tmp/hook.js",
			Source:  claudefiles.SourceUser, Path: "/u/.claude/settings.json"},
	}
	card := buildHooksCard(hooks, "")
	full := ""
	for _, sec := range card.Sections {
		full += sec.Markdown + "\n"
	}
	for _, leak := range []string{
		"SECRET_TOKEN_xyz",
		"sk-shouldnotleak",
		"ghp_shouldnotleak",
		"Authorization",
		"Bearer",
	} {
		if strings.Contains(full, leak) {
			t.Errorf("hook card leaked secret/header %q; full card:\n%s", leak, full)
		}
	}
	// Sanity: the redacted preview still surfaces enough info to identify
	// what runs (host for URL, binary for command).
	for _, expect := range []string{
		"hooks.slack.com",
		"curl …",
		"node …",
	} {
		if !strings.Contains(full, expect) {
			t.Errorf("hook card missing redacted preview %q; full card:\n%s", expect, full)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://hooks.slack.com/services/T/B/SECRET", "https://hooks.slack.com/…"},
		{"https://api.example.com", "https://api.example.com"},
		{"https://api.example.com/", "https://api.example.com"},
		{"https://api.example.com/?token=x", "https://api.example.com/…"},
		{"not a url", "[hidden]"},
		{"", "[hidden]"},
	}
	for _, c := range cases {
		if got := redactURL(c.in); got != c.want {
			t.Errorf("redactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bash", "bash"},
		{"bash -c 'foo'", "bash …"},
		{"/usr/local/bin/hook --arg x", "/usr/local/bin/hook …"},
		{"FOO=bar node script.js", "node …"},
		{"FOO=bar BAR=baz", "FOO=bar …"}, // unusual but don't infinite-peel
		{"", ""},
	}
	for _, c := range cases {
		if got := redactCommand(c.in); got != c.want {
			t.Errorf("redactCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildHooksCard_GroupsByEvent(t *testing.T) {
	hooks := []claudefiles.Hook{
		{Event: "PreToolUse", Matcher: "Bash", Type: "command", Command: "pre1", Source: claudefiles.SourceProject, Path: "/p"},
		{Event: "PreToolUse", Matcher: "Edit", Type: "command", Command: "pre2", Source: claudefiles.SourceProject, Path: "/p"},
		{Event: "Stop", Type: "http", URL: "http://x", Source: claudefiles.SourceUser, Path: "/u"},
	}
	card := buildHooksCard(hooks, "/proj")
	// Find both event headers; they appear once each, in insertion order.
	header := ""
	for _, sec := range card.Sections {
		header += sec.Markdown + "\n"
	}
	preIdx := strings.Index(header, "**PreToolUse** · 2")
	stopIdx := strings.Index(header, "**Stop** · 1")
	if preIdx == -1 || stopIdx == -1 {
		t.Fatalf("expected both event headers; got: %s", header)
	}
	if preIdx > stopIdx {
		t.Errorf("PreToolUse should come before Stop (input order preserved)")
	}
	// Empty matcher renders as "*" (match-all).
	if !strings.Contains(header, "matcher: `*`") {
		t.Errorf("empty matcher should render as *; full: %s", header)
	}
}

func TestSourceTag(t *testing.T) {
	if sourceTag(claudefiles.SourceProject) == "" {
		t.Errorf("SourceProject should render a non-empty tag")
	}
	if sourceTag(claudefiles.SourceUser) == "" {
		t.Errorf("SourceUser should render a non-empty tag")
	}
}

func TestTruncateForCard(t *testing.T) {
	// CJK runes count as one each — no surprise byte-cuts.
	in := strings.Repeat("中", 50)
	got := truncateForCard(in, 10)
	// 10 runes + ellipsis.
	if len([]rune(got)) != 11 {
		t.Errorf("expected 10 runes + ellipsis (11 total), got %d", len([]rune(got)))
	}
	if truncateForCard("short", 10) != "short" {
		t.Errorf("under-limit input should be returned as-is")
	}
}

// displayPath should hide the user's $HOME (which leaks the OS username)
// behind ~/. Anything outside $HOME passes through unchanged.
func TestDisplayPath(t *testing.T) {
	t.Setenv("HOME", "/Users/alice")
	cases := []struct{ in, want string }{
		{"/Users/alice/.claude/agents/x.md", "~/.claude/agents/x.md"},
		{"/Users/alice", "~"},
		{"/Users/alicia/something", "/Users/alicia/something"}, // adjacent username
		{"/etc/hosts", "/etc/hosts"},
	}
	for _, c := range cases {
		if got := displayPath(c.in); got != c.want {
			t.Errorf("displayPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
