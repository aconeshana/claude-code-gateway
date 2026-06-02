package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// cmdStatus renders a 3-section card:
//  1. 当前会话 (focused session details)
//  2. 会话总览 (active/idle/archived counts)
//  3. 摘要进度 (combined discovery + worker; users don't need to know the
//     two are separate subsystems)
func (b *Bridge) cmdStatus(ctx context.Context, m channel.InboundMessage) {
	var sections []channel.Section

	// --- 1. 当前会话 ---
	sections = append(sections, channel.Section{Markdown: b.statusCurrentSection(m)})

	// --- 2. 会话总览 (no "External" — that's an implementation detail) ---
	sections = append(sections, channel.Section{
		Divider:  true,
		Markdown: b.statusOverviewSection(m),
	})

	// --- 3. 摘要进度 (merged: discovery scan + summary worker) ---
	if b.discoverer != nil || b.worker != nil {
		sections = append(sections, channel.Section{
			Divider:  true,
			Markdown: b.statusSummarySection(),
		})
	}

	b.replyCard(ctx, m, channel.Card{
		Title:    "Gateway Status",
		Tone:     channel.ToneInfo,
		Sections: sections,
	})
}

// statusCurrentSection renders the focused session at the top so the user
// sees "where am I" first. Falls back to a hint when nothing is focused.
func (b *Bridge) statusCurrentSection(m channel.InboundMessage) string {
	sess, ok := b.currentSession(m)
	if !ok {
		return "**📍 当前会话**\n_无 focused session_ · 用 /new 创建或 /list 选一个"
	}
	info := sess.Info()
	project := projectName(info.WorkingDir)
	if project == "" {
		project = "(未知项目)"
	}
	sid := displaySessionID(sess)
	title := info.Label
	if title == "" {
		title = renderSessionTitle(info)
	}

	parts := []string{fmt.Sprintf("`%s`", sid)}
	if t := parseRFC3339(info.LastActivity); !t.IsZero() {
		parts = append(parts, humanAgo(time.Since(t)))
	}
	if info.MessageCount > 0 {
		parts = append(parts, fmt.Sprintf("%d 条", info.MessageCount))
	}
	meta := ""
	if len(parts) > 0 {
		meta = " · " + joinDot(parts)
	}

	return fmt.Sprintf("**📍 当前会话**\n**%s**%s\n%s", project, meta, title)
}

// statusOverviewSection renders active/idle/archived counts. External is
// hidden — users don't distinguish ownership origin.
func (b *Bridge) statusOverviewSection(m channel.InboundMessage) string {
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
	body := fmt.Sprintf("**📊 会话总览**\n活跃: %d · 空闲: %d · 归档: %d",
		active, idle, len(archived))
	body += fmt.Sprintf("\n项目数: %d (用 /project 查看)", len(b.projectsForUser(m.UserID)))
	return body
}

// statusSummarySection merges discovery scan progress and summary worker
// progress into one user-facing "摘要进度" section. The two are separate
// subsystems internally but the user just wants to know "are summaries
// being generated for my sessions".
//
// Progress is bounded — we cap visible counts at the current discoverable
// set size so the percentage stays in [0, 100]. Historical summary records
// for sessions that have been claimed/archived are excluded from progress
// math but still contribute to the underlying store.
func (b *Bridge) statusSummarySection() string {
	body := "**📝 摘要进度**\n"

	// Scope: current discoverable external sessions (need summaries).
	externals := b.mgr.ListBy(session.Filter{Origins: []string{session.OriginExternal}})
	total := len(externals)

	if total == 0 {
		body += "暂无待处理的对话"
	} else {
		// Count fresh/skipped only within the CURRENT external set so
		// progress matches the visible inventory (no 1081% nonsense).
		current := make(map[string]bool, total)
		for _, info := range externals {
			if info.CLISessionID != "" {
				current[info.CLISessionID] = true
			}
		}
		done := b.mgr.CountFreshExternalSummariesIn(SummaryPromptVersion, current)
		skipped := b.mgr.CountSkippedExternalSummariesIn(SummaryPromptVersion, current)
		processed := done + skipped
		if processed > total {
			processed = total // defensive: shouldn't happen with scoped counts
		}
		remaining := total - processed
		pct := 0
		if total > 0 {
			pct = processed * 100 / total
		}
		body += fmt.Sprintf("已完成: %d / %d (%d%%) · 剩余: %d", processed, total, pct, remaining)
		if skipped > 0 {
			body += fmt.Sprintf("\n_其中 %d 个是短对话/工具会话,无需摘要_", skipped)
		}
	}

	// Scan timing (was the Discovery section)
	stats := b.DiscoveryStatsSnapshot()
	if !stats.LastScanAt.IsZero() {
		body += fmt.Sprintf("\n最近扫描: %s 前", humanAgo(time.Since(stats.LastScanAt)))
		if b.rescanInterval > 0 {
			next := stats.LastScanAt.Add(b.rescanInterval)
			body += fmt.Sprintf(" · 下次: %s 后", humanAgo(time.Until(next)))
		}
	}

	if b.worker != nil {
		ws := b.worker.Stats()
		if ws.Failed > 0 {
			body += fmt.Sprintf("\n本次失败: %d 次", ws.Failed)
		}
		if ws.LastError != "" {
			body += "\n最近错误: " + ws.LastError
		}
	}
	return body
}

func joinDot(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " · "
		}
		out += p
	}
	return out
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
