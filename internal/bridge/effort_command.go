package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// effortInfo enumerates the effort levels exposed via /effort. Level is the
// raw string passed to claude --effort. The "auto" pseudo-level is a
// sentinel for "clear the override and let claude-code's settings.json
// effortLevel (or the model default) decide" — it maps to an empty string
// at the runtime config layer.
//
// Per claude-code/src/utils/effort.ts:
//   - 'low' / 'medium' / 'high' are universal (subject to model support)
//   - 'max' is Opus 4.6 only on public models; 3P providers may differ
//
// We do not gate 'max' here because modelSupportsMaxEffort can shift over
// time; if the user's current model rejects it, claude-code surfaces the
// error in the next turn.
type effortInfo struct {
	Name string
	Desc string
}

var availableEfforts = []effortInfo{
	{"low", "更快、更省 token,适合简单任务"},
	{"medium", "默认平衡档"},
	{"high", "更深推理,适合复杂任务(关键档)"},
	{"max", "最深推理(仅 Opus 4.6 等支持)"},
	{"auto", "清除 session 级覆盖,让 settings.json / 模型默认决定"},
}

func (b *Bridge) cmdEffort(ctx context.Context, m channel.InboundMessage, args string) {
	level := strings.ToLower(strings.TrimSpace(args))
	if level == "" {
		b.showEffortMenu(ctx, m)
		return
	}
	if !isValidEffortName(level) {
		b.replyText(ctx, m, "无效的 effort 等级: "+level+"。可选: low / medium / high / max / auto")
		return
	}
	sess, err := b.ensureCurrentSession(ctx, m, true)
	if err != nil {
		b.replyEffortResult(ctx, m, channel.Card{
			Title:    "Switch Effort · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: err.Error()}},
		})
		return
	}
	b.applyEffortSwitch(ctx, m, sess, level)
}

// applyEffortSwitch is shared between typed `/effort <level>` and
// switch_effort card-action callbacks so the success/failure surface stays
// identical. "auto" is translated to the empty-string sentinel before
// reaching SwitchEffort.
func (b *Bridge) applyEffortSwitch(ctx context.Context, m channel.InboundMessage, sess *session.Session, level string) {
	wire := effortNameToWire(level)
	if err := sess.SwitchEffort(wire); err != nil {
		b.replyEffortResult(ctx, m, channel.Card{
			Title:    "Switch Effort · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: "切换 effort 失败: " + err.Error()}},
		})
		return
	}
	b.replyEffortResult(ctx, m, channel.Card{
		Title: "Switch Effort · ✓ " + level,
		Tone:  channel.ToneSuccess,
		Sections: []channel.Section{{
			Markdown: fmt.Sprintf("已将 session **%s** 的 effort 设为 `%s`",
				displaySessionID(sess), level),
		}},
	})
}

// replyEffortResult mirrors replyModelResult: a card click replaces the
// menu in place, a typed command posts a fresh card.
func (b *Bridge) replyEffortResult(ctx context.Context, m channel.InboundMessage, card channel.Card) {
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

func (b *Bridge) showEffortMenu(ctx context.Context, m channel.InboundMessage) {
	target := b.modelMenuTarget(m) // same lookup chain as /model
	if target == nil {
		b.replyText(ctx, m, "暂无可切换的 session。先 /new 创建一个,然后再 /effort。")
		return
	}

	targetInfo := target.Info()
	var subtitle string
	switch targetInfo.Status {
	case string(session.StatusActive):
		subtitle = fmt.Sprintf("将作用于 session **%s** (active)", displaySessionID(target))
	default:
		subtitle = fmt.Sprintf("将作用于 session **%s** (idle,点击按钮会自动恢复)",
			displaySessionID(target))
	}

	var btns []channel.Button
	for _, ei := range availableEfforts {
		btns = append(btns, channel.Button{
			Label:  ei.Name,
			Style:  "default",
			Action: map[string]string{"action": "switch_effort", "level": ei.Name, "session_id": sessionPayloadID(targetInfo)},
		})
	}

	// Show a one-line description per level above the buttons so users can
	// pick without leaving the chat for docs.
	var descLines []string
	for _, ei := range availableEfforts {
		descLines = append(descLines, fmt.Sprintf("- `%s` — %s", ei.Name, ei.Desc))
	}

	b.replyCard(ctx, m, channel.Card{
		Title: "Switch Effort",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: subtitle},
			{Markdown: strings.Join(descLines, "\n")},
			{Buttons: btns, ButtonLayout: "fill"},
		},
	})
}

// handleEffortCardAction handles the switch_effort button on the menu.
// Returns true when claimed.
func (b *Bridge) handleEffortCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action.Name != "switch_effort" {
		return false
	}
	level, _ := m.Action.Values["level"].(string)
	if level == "" || !isValidEffortName(level) {
		return true
	}
	sessID, _ := m.Action.Values["session_id"].(string)
	if sess, ok := b.resolveSessionByPayload(sessID); ok {
		b.applyEffortSwitch(ctx, m, sess, level)
	} else {
		// Session disappeared between menu render and click — fall through
		// to the typed command path so cmdEffort surfaces a clean error.
		b.cmdEffort(ctx, m, level)
	}
	return true
}

func isValidEffortName(name string) bool {
	for _, ei := range availableEfforts {
		if ei.Name == name {
			return true
		}
	}
	return false
}

// effortNameToWire translates the user-facing level name into the raw
// value buildArgs feeds to --effort. The "auto" sentinel becomes "" so
// claude-code falls back to settings.json / model default.
func effortNameToWire(name string) string {
	if name == "auto" {
		return ""
	}
	return name
}
