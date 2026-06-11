package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// cmdAddDir handles the /add-dir slash command. With no argument it shows the
// session's current extra allowed directories. With a path argument it adds
// the directory, respawning the CLI if the session is active.
func (b *Bridge) cmdAddDir(ctx context.Context, m channel.InboundMessage, args string) {
	dir := strings.TrimSpace(args)
	if dir == "" {
		b.showAddDirStatus(ctx, m)
		return
	}

	// Expand ~ and make absolute.
	if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		b.replyText(ctx, m, fmt.Sprintf("无效路径: %s", dir))
		return
	}

	fi, err := os.Stat(absDir)
	if err != nil {
		b.replyText(ctx, m, fmt.Sprintf("目录不存在: %s", absDir))
		return
	}
	if !fi.IsDir() {
		b.replyText(ctx, m, fmt.Sprintf("不是目录: %s", absDir))
		return
	}

	sess, err := b.ensureCurrentSession(ctx, m, false)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}

	wasActive := sess.Info().Status == string(session.StatusActive)

	if err := b.mgr.AddSessionDir(sess.ID, absDir); err != nil {
		b.replyCard(ctx, m, channel.Card{
			Title:    "Add Dir · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: "添加目录失败: " + err.Error()}},
		})
		return
	}
	b.saveStateIfPossible()

	var detail string
	if wasActive {
		detail = fmt.Sprintf("CLI 已重启以生效。")
	} else {
		detail = fmt.Sprintf("下次激活时生效。")
	}
	b.replyCard(ctx, m, channel.Card{
		Title: "Add Dir · ✓",
		Tone:  channel.ToneSuccess,
		Sections: []channel.Section{{
			Markdown: fmt.Sprintf("已将 `%s` 加入 session **%s** 的允许目录列表。\n\n%s",
				absDir, displaySessionID(sess), detail),
		}},
	})
}

// showAddDirStatus renders the current ExtraAddDirs for the focused session.
func (b *Bridge) showAddDirStatus(ctx context.Context, m channel.InboundMessage) {
	sess, err := b.ensureCurrentSession(ctx, m, false)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}
	info := sess.Info()

	var md string
	if len(info.ExtraAddDirs) == 0 {
		md = fmt.Sprintf("**%s** 没有额外添加的目录。\n\n使用 `/add-dir <路径>` 添加", displaySessionID(sess))
	} else {
		lines := []string{fmt.Sprintf("**%s** 的额外允许目录:", displaySessionID(sess))}
		for _, d := range info.ExtraAddDirs {
			lines = append(lines, "- `"+d+"`")
		}
		md = strings.Join(lines, "\n")
	}
	b.replyCard(ctx, m, channel.Card{
		Title:    "Add Dir · 目录列表",
		Tone:     channel.ToneInfo,
		Sections: []channel.Section{{Markdown: md}},
	})
}
