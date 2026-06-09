package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

func (b *Bridge) cmdHelp(ctx context.Context, m channel.InboundMessage) {
	var sb strings.Builder
	sb.WriteString("**命令列表:**\n")
	for _, cmd := range b.commands {
		sb.WriteString(fmt.Sprintf("`%s` — %s\n", cmd.Usage, cmd.Desc))
	}
	sb.WriteString("\n**其他 `/xxx` 命令** 透传给 Claude CLI(如 /commit, /compact, /review)。\n")
	sb.WriteString("**`!<cmd>`** — 在工作目录执行 shell 命令(30s 超时)\n")
	sb.WriteString("**普通消息** 自动发送到 active session;无 session 时自动创建或从 idle/归档恢复。")
	b.replyCard(ctx, m, channel.Card{
		Title:    "Claude Code Gateway Help",
		Tone:     channel.ToneInfo,
		Sections: []channel.Section{{Markdown: sb.String()}},
	})
}
