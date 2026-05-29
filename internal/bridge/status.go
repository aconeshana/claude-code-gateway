package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// cmdStatus renders a multi-section card with discovery + worker + session
// stats. Designed to be extensible: each new gateway subsystem can add its own
// section without breaking the others.
func (b *Bridge) cmdStatus(ctx context.Context, m channel.InboundMessage) {
	var sections []channel.Section

	// Discovery
	stats := b.DiscoveryStatsSnapshot()
	if b.discoverer != nil {
		body := fmt.Sprintf("**🔍 Discovery**\n扫描窗口: 最近 %d 天\n本次扫描到 %d 个 session,新纳管 %d 个 external",
			stats.WindowDays, stats.TotalOnDisk, stats.NewlyImported)
		if !stats.LastScanAt.IsZero() {
			body += fmt.Sprintf("\n最近扫描: %s 前 · 用时 %v",
				humanAgo(time.Since(stats.LastScanAt)), stats.LastScanTook.Round(time.Millisecond))
			if b.rescanInterval > 0 {
				next := stats.LastScanAt.Add(b.rescanInterval)
				body += fmt.Sprintf("\n下次扫描: %s 后", humanAgo(time.Until(next)))
			}
		} else {
			body += "\n_等待首次扫描_"
		}
		sections = append(sections, channel.Section{Markdown: body})
	}

	// Summary worker — progress is driven by disk (the source of truth),
	// not the worker's in-memory counter. The counter resets across gateway
	// restarts; users want to see "how many of the current set are done"
	// regardless of when the worker last started.
	if b.worker != nil {
		total := len(b.mgr.ListBy(session.Filter{Origins: []string{session.OriginExternal}}))
		done := b.mgr.CountFreshExternalSummaries(SummaryPromptVersion)
		skipped := b.mgr.CountSkippedExternalSummaries(SummaryPromptVersion)
		ws := b.worker.Stats()
		body := "**📝 Summary Worker**\n"
		if total == 0 {
			body += "空闲(暂无 external session)"
		} else {
			processed := done + skipped
			remaining := total - processed
			if remaining < 0 {
				remaining = 0
			}
			pct := processed * 100 / total
			body += fmt.Sprintf("真实摘要: %d · 已跳过(meta): %d / %d (%d%%)\n剩余: %d 个 · prompt v%d · 本次失败: %d",
				done, skipped, total, pct, remaining, SummaryPromptVersion, ws.Failed)
		}
		if ws.LastError != "" {
			body += "\n最近错误: " + ws.LastError
		}
		sections = append(sections, channel.Section{Divider: true, Markdown: body})
	}

	// Sessions
	owned := b.mgr.ListActiveByOwner(m.UserID)
	archived := b.mgr.ListArchivedByOwner(m.UserID)
	active, idle := 0, 0
	for _, info := range owned {
		switch info.Status {
		case string(session.StatusActive):
			active++
		case string(session.StatusIdle):
			idle++
		}
	}
	body := fmt.Sprintf("**📊 Sessions**\n活跃: %d · Idle: %d · 归档: %d",
		active, idle, len(archived))
	if focused, ok := b.mgr.FocusedSession(m.UserID); ok {
		display := focused.Label
		if display == "" {
			display = displaySessionID(focused)
		}
		info := focused.Info()
		project := projectName(info.WorkingDir)
		body += fmt.Sprintf("\nFocused: %s · 项目: %s `%s`", display, project, info.WorkingDir)
	} else {
		body += "\nFocused: (无)"
	}
	body += fmt.Sprintf("\n项目数: %d (用 /project 查看)", len(b.projectsForUser(m.UserID)))
	if b.shareExternalEnabled() {
		extInfos := b.mgr.ListBy(session.Filter{Origins: []string{session.OriginExternal}})
		body += fmt.Sprintf("\nExternal (terminal): %d (已启用共享)", len(extInfos))
	}
	sections = append(sections, channel.Section{Divider: true, Markdown: body})

	b.replyCard(ctx, m, channel.Card{
		Title:    "Gateway Status",
		Tone:     channel.ToneInfo,
		Sections: sections,
	})
}

// humanAgo formats a duration as a short human string ("12 秒"/"3 分钟").
func humanAgo(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return "<1 秒"
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}
