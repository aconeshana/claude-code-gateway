package bridge

import (
	"context"
	"log"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

// cmdRecap handles the /recap slash command. It generates a 2-3 sentence
// summary of the focused session — what the user is working on and what the
// next step is — mirroring the official Claude Code /recap behaviour.
func (b *Bridge) cmdRecap(ctx context.Context, m channel.InboundMessage) {
	if b.admin == nil {
		b.replyText(ctx, m, "/recap 需要 admin AI session — 未配置 ADMIN_MODEL")
		return
	}

	sess, ok := b.recapTargetSession(m)
	if !ok {
		b.replyText(ctx, m, "没有聚焦的 session，请先 /new 或 /resume")
		return
	}
	info := sess.Info()
	jsonlPath := persist.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
	if jsonlPath == "" {
		b.replyText(ctx, m, "session 还没有对话记录")
		return
	}

	prompt := buildRecapPrompt(jsonlPath)
	if prompt == "" {
		b.replyText(ctx, m, "session 还没有对话记录")
		return
	}

	_ = b.ch.Reaction(m.MessageID, "OnIt")

	result, err := b.admin.query(ctx, prompt)
	if err != nil {
		log.Printf("[bridge/recap] query failed for %s: %v", shortID(sess.ID), err)
		b.replyText(ctx, m, "生成摘要失败: "+err.Error())
		return
	}

	summary := cleanSummaryOutput(result)
	if summary == "" || summary == "_skip_meta_" {
		b.replyText(ctx, m, "_(当前 session 内容较少，无法生成摘要)_")
		return
	}
	b.replyText(ctx, m, "📋 **Session "+displaySessionID(sess)+" 摘要**\n\n"+summary)
}

// recapTargetSession resolves the session to recap: thread-bound session
// recapTargetSession resolves the session to recap: thread-bound session
// when inside a thread, focused session in main chat, or the best resumable
// candidate when there is no current focus.
func (b *Bridge) recapTargetSession(m channel.InboundMessage) (*session.Session, bool) {
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
