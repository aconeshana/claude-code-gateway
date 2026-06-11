package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

func (b *Bridge) cmdExport(ctx context.Context, m channel.InboundMessage) {
	sess, ok := b.exportTargetSession(m)
	if !ok {
		b.replyText(ctx, m, "没有可导出的 session（需要有 focused 或 thread-bound session）")
		return
	}

	info := sess.Info()
	if info.CLISessionID == "" {
		b.replyText(ctx, m, "当前 session 尚无对话记录可导出")
		return
	}

	jsonlPath := persist.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
	_ = b.ch.Reaction(m.MessageID, "OnIt")
	turns, err := readAllTurns(jsonlPath)
	if err != nil {
		b.replyText(ctx, m, "读取对话记录失败: "+err.Error())
		return
	}
	if len(turns) == 0 {
		b.replyText(ctx, m, "当前 session 没有对话记录")
		return
	}

	content := renderConversationText(turns)
	filename := exportFilename(info)

	if fs, ok := b.ch.(channel.FileSender); ok {
		if _, err := fs.SendFile(ctx, m.ChatID, m.MessageID, filename, []byte(content)); err == nil {
			return
		}
		// SendFile failed — fall through to text fallback below.
	}

	// Fallback for channels without file support: send as text (truncated).
	const maxFallback = 4000
	text := content
	if len(text) > maxFallback {
		text = text[:maxFallback] + fmt.Sprintf("\n\n…（已截断，完整内容共 %d 字符）", len(content))
	}
	b.replyText(ctx, m, "```\n"+text+"\n```")
}

func (b *Bridge) exportTargetSession(m channel.InboundMessage) (*session.Session, bool) {
	if m.ThreadID != "" {
		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok {
			return sess, true
		}
	}
	if sess, ok := b.mgr.FocusedSession(m.UserID); ok {
		return sess, true
	}
	if sess := b.mgr.ResolveResumable(m.UserID); sess != nil {
		return sess, true
	}
	return nil, false
}

// renderConversationText formats turns as human-readable plain text.
// Each turn is prefixed with its role in brackets, separated by blank lines.
func renderConversationText(turns []recapTurn) string {
	var sb strings.Builder
	for i, t := range turns {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		switch t.Role {
		case "user":
			sb.WriteString("[User]\n")
		case "assistant":
			sb.WriteString("[Assistant]\n")
		default:
			sb.WriteString("[" + t.Role + "]\n")
		}
		sb.WriteString(t.Text)
	}
	return sb.String()
}

func exportFilename(info session.SessionInfo) string {
	ts := time.Now().Format("20060102-150405")
	label := info.Label
	if label == "" {
		label = projectName(info.WorkingDir)
	}
	// sanitize: keep letters (including CJK), digits, hyphens, underscores
	var clean strings.Builder
	for _, r := range label {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			clean.WriteRune(r)
		} else if r == ' ' {
			clean.WriteRune('-')
		}
	}
	slug := clean.String()
	if slug == "" {
		return fmt.Sprintf("conversation-%s.txt", ts)
	}
	return fmt.Sprintf("%s-%s.txt", ts, slug)
}
