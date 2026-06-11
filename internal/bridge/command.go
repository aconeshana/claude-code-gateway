package bridge

import (
	"context"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// CommandHandler is the function signature for all slash command implementations.
type CommandHandler func(ctx context.Context, m channel.InboundMessage, args string)

// Command describes a single slash command exposed to users.
type Command struct {
	// Name is the primary slash-prefixed name, e.g. "/new".
	Name string

	// Aliases are alternative names, e.g. ["/sessions"] for "/list".
	Aliases []string

	// Usage shows the invocation syntax, e.g. "/new [label] [dir]".
	Usage string

	// Desc is a single-line Chinese description shown in /help.
	Desc string

	// Handler is the implementation. args is the text after the command name,
	// trimmed of leading/trailing whitespace.
	Handler CommandHandler
}

// findCommand looks up a Command by name or alias. Returns nil when not found.
func (b *Bridge) findCommand(name string) *Command {
	for i := range b.commands {
		if b.commands[i].Name == name {
			return &b.commands[i]
		}
		for _, a := range b.commands[i].Aliases {
			if a == name {
				return &b.commands[i]
			}
		}
	}
	return nil
}

// dispatchCommand routes a slash command to its handler.
// If no registered command matches, the text is forwarded to the focused CLI
// session as-is (unknown /xxx is a Claude CLI slash command).
func (b *Bridge) dispatchCommand(ctx context.Context, m channel.InboundMessage, text string) {
	name, args := splitCommand(text)
	if cmd := b.findCommand(name); cmd != nil {
		cmd.Handler(ctx, m, args)
		return
	}
	// Unknown /command → forward to focused CLI session.
	b.forwardToCLI(ctx, m, "")
}

// forwardToCLI sends the full original slash command to the focused CLI
// session. Used for commands handled natively by Claude CLI (e.g. /plan).
// The args parameter is unused — we always forward m.Text verbatim so the CLI
// receives the original command syntax including the leading slash.
func (b *Bridge) forwardToCLI(ctx context.Context, m channel.InboundMessage, _ string) {
	text := strings.TrimSpace(m.Text)
	sess, ok := b.resolveOrCreateSession(ctx, m)
	if !ok {
		return
	}
	b.ensureSubscribed(ctx, sess, m)
	_ = b.ch.Reaction(m.MessageID, "OnIt")
	if err := sess.SendMessage(text); err != nil {
		b.replyText(ctx, m, "发送命令失败: "+err.Error())
	}
}

func splitCommand(text string) (cmd, rest string) {
	idx := strings.IndexAny(text, " \t\n")
	if idx == -1 {
		return text, ""
	}
	return text[:idx], strings.TrimSpace(text[idx+1:])
}

// valueOrDash returns "—" for empty strings — useful when rendering
// "current value: <x>" in card forms where blank looks broken.
func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// currentProjectDir returns the working dir of the user's current
// project context, walking the precedence chain:
//
//  1. Thread-bound session — when m.ThreadID is set, the user is
//     speaking inside a specific session's thread, so the "current
//     project" must follow that session (not the user's main-chat
//     focus, which may point at a completely different working dir).
//  2. Main-chat focused session — when no thread context, the focused
//     session's wd is the canonical answer.
//  3. b.defaultCWD — final fallback when the user has no sessions at
//     all (e.g. first-ever interaction).
//
// Used by /agents, /mcp, /commands, /hooks, /skills, /permissions to
// scope project-level configuration files. Returns "" only when even
// defaultCWD is unset.
//
// Critical for /permissions: writing a rule to <projectDir>/.claude/
// settings.local.json and reading it back from the same path must
// agree. Without thread awareness, a wizard launched from a thread's
// forward card would write to (e.g.) ~/weflow/foo/.claude/... while
// the subsequent /permissions list (from the same thread, but going
// through FocusedSession=nil → defaultCWD) would read from ~/weflow/...
// and show "no rules" — a silent inconsistency.
func (b *Bridge) currentProjectDir(m channel.InboundMessage) string {
	if m.ThreadID != "" {
		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok {
			if d := sess.Info().WorkingDir; d != "" {
				return d
			}
		}
	}
	if sess, ok := b.mgr.FocusedSession(m.UserID); ok {
		if d := sess.Info().WorkingDir; d != "" {
			return d
		}
	}
	if b.defaultCWD != "" && b.defaultCWD != "." {
		return b.defaultCWD
	}
	return ""
}

// replyOrUpdateCard prefers in-place card replacement when the inbound
// came from a button click — keeps the chat dense by avoiding stacks of
// stale wizard cards. When the inbound is a slash command (no m.Reply
// callback), it falls through to a fresh outbound card. Used by any
// multi-step flow (permissions wizard, tool-permission allow/deny,
// effort/model picker) where the same handler serves both entry paths.
func (b *Bridge) replyOrUpdateCard(ctx context.Context, m channel.InboundMessage, card channel.Card) {
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// sourceTag renders a Source enum as a short Markdown chip used in the
// agents/commands/mcp/hooks card sections — keeps the visual language
// consistent across all four commands.
//
// SourceLocal gets a 🔒 marker because settings.local.json is gitignored
// by convention and typically contains per-machine secrets users wouldn't
// want broadcast to a group chat.
func sourceTag(s claudefiles.Source) string {
	switch s {
	case claudefiles.SourceLocal:
		return "`[🔒 本地]`"
	case claudefiles.SourceProject:
		return "`[项目]`"
	case claudefiles.SourceUser:
		return "`[用户]`"
	default:
		return ""
	}
}

// truncateForCard caps a single-line value at maxRunes (counted in runes,
// not bytes — important for CJK content). Appends an ellipsis when cut.
// Use for descriptions / commands / args inside listing cards.
func truncateForCard(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// displayPath collapses the user's $HOME prefix to `~` for display in
// cards posted to a group chat. The absolute path is informationally
// equivalent for the user but leaks the OS username; the tilde form keeps
// the path useful while removing the PII.
func displayPath(p string) string {
	home := homeDir()
	if home == "" || !strings.HasPrefix(p, home) {
		return p
	}
	rest := p[len(home):]
	if rest == "" {
		return "~"
	}
	if rest[0] != '/' {
		// e.g. home="/Users/abc" but p="/Users/abcd/x" — not actually inside HOME.
		return p
	}
	return "~" + rest
}
