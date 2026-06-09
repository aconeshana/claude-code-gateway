package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/plan"
)

// planIndex lazy-initialized on first /plan-list invocation. Plans dir
// rarely changes between calls; we don't cache the file list because
// disk-scan is cheap.
func (b *Bridge) planIndex() *plan.Index {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.plans == nil {
		b.plans = plan.NewIndex("")
	}
	return b.plans
}

// cmdPlanList shows recent plan files from ~/.claude/plans/. Most-recent
// first. Each entry exposes a "查看" button that drills into the full
// markdown via the show_plan card action.
//
// To create a new plan, the user runs `/plan` (which is forwarded to
// Claude CLI verbatim and triggers plan mode there). /plan-list is only
// for browsing/reviewing what's already on disk.
func (b *Bridge) cmdPlanList(ctx context.Context, m channel.InboundMessage) {
	plans, err := b.planIndex().List()
	if err != nil {
		b.replyText(ctx, m, "读取 plan 目录失败: "+err.Error())
		return
	}
	if len(plans) == 0 {
		b.replyCard(ctx, m, channel.Card{
			Title:    "Plans",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: "暂无 plan。用 `/plan <description>` 进入 plan mode 让 Claude 生成第一份。"}},
		})
		return
	}
	sections := make([]channel.Section, 0, len(plans)+1)
	// Lark card-action events don't carry thread context, so embed it into
	// the button action and let handleCardAction restore m.ThreadID before
	// dispatching. Without this, [查看] in a thread would silently post the
	// plan detail to the main chat.
	threadID := m.ThreadID
	rootMsgID := threadAnchorFromInbound(m)
	for _, p := range plans {
		title := p.Title
		if title == "" {
			title = "(untitled)"
		}
		body := fmt.Sprintf("**%s**\n%s · %s",
			title, humanAgo(time.Since(p.MTime)), humanSize(p.Size))
		action := map[string]string{"action": "show_plan", "filename": p.Filename}
		if threadID != "" {
			action["thread_id"] = threadID
			action["root_id"] = rootMsgID
		}
		sections = append(sections, channel.Section{
			Markdown: body,
			Buttons: []channel.Button{{
				Label: "查看", Style: "primary",
				Action: action,
			}},
		})
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    fmt.Sprintf("Plans 目录: %s · 共 %d 份", b.planIndex().Dir(), len(plans)),
	})
	b.replyCard(ctx, m, channel.Card{
		Title:    "Plans · 按最近修改",
		Tone:     channel.ToneInfo,
		Sections: sections,
	})
}

// showPlanDetail renders one plan's full markdown body. Feishu cards
// accept several KB of markdown without complaint (the client folds long
// sections in the UI), so we send the whole file. If we ever hit a hard
// size limit, see ~/weflow/docs/lark-file-send-research.md for the file
// upload path.
func (b *Bridge) showPlanDetail(ctx context.Context, m channel.InboundMessage, filename string) {
	p, err := b.planIndex().Get(filename)
	if err != nil {
		b.replyText(ctx, m, "读取 plan 失败: "+err.Error())
		return
	}
	header := fmt.Sprintf("**%s**\n_%s · %s · %s_",
		p.Title,
		p.Filename,
		humanAgo(time.Since(p.MTime)),
		humanSize(p.Size),
	)
	b.replyCard(ctx, m, channel.Card{
		Title: "Plan",
		Tone:  channel.ToneNeutral,
		Sections: []channel.Section{
			{Markdown: header},
			{Divider: true, Markdown: p.Body},
		},
	})
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// handlePlanCardAction dispatches show_plan (drill-in from /plan-list) and
// plan_response (allow/deny on an ExitPlanMode prompt). Returns true when
// the action was claimed.
func (b *Bridge) handlePlanCardAction(ctx context.Context, m channel.InboundMessage) bool {
	switch m.Action.Name {
	case "show_plan":
		if filename, ok := m.Action.Values["filename"].(string); ok {
			// plan-list embeds thread context into the button payload because
			// Lark card-action events don't carry it; restore it onto m so
			// replyCard posts the detail card back into the originating
			// thread instead of leaking to the main chat.
			if tid, _ := m.Action.Values["thread_id"].(string); tid != "" {
				m.ThreadID = tid
				if rid, _ := m.Action.Values["root_id"].(string); rid != "" {
					m.RootID = rid
					m.MessageID = rid
				}
			}
			b.showPlanDetail(ctx, m, filename)
		}
	case "plan_response":
		pendingID, _ := m.Action.Values["pending_id"].(string)
		result, _ := m.Action.Values["result"].(string)
		if pendingID != "" && result != "" {
			b.handlePlanResponse(ctx, m.ChatID, pendingID, result)
		}
	default:
		return false
	}
	return true
}
