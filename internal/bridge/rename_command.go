package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// cmdRename sets a custom display title on the focused session and forwards
// the same command to the CLI so its internal session name stays in sync.
func (b *Bridge) cmdRename(ctx context.Context, m channel.InboundMessage, args string) {
	title := strings.TrimSpace(args)
	if title == "" {
		b.replyText(ctx, m, "用法: /rename <新名字>")
		return
	}

	// /rename only mutates session metadata (CustomTitle); the CLI doesn't
	// need to be live. Pass mustBeActive=false so a focused-idle (or
	// fallback resumable-idle) session is reused as-is without paying the
	// 10–30s reactivate cost just to update a display name.
	sess, err := b.ensureCurrentSession(ctx, m, false)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}

	if err := b.mgr.SetCustomTitle(sess.ID, title); err != nil {
		b.replyText(ctx, m, "重命名失败: "+err.Error())
		return
	}
	b.saveStateIfPossible()
	b.replyText(ctx, m, fmt.Sprintf("已重命名为 **%s** (session %s)",
		title, displaySessionID(sess)))
}
