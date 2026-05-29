package bridge

import (
	"context"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
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
