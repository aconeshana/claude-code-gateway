package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// modelInfo enumerates the models exposed via /model. Name is the alias
// passed to `claude --model` so the CLI resolves it through env
// (ANTHROPIC_DEFAULT_<TIER>_MODEL) — never hard-code specific versions
// like "claude-opus-4-6" here, that bypasses the user's env and gets
// stale the moment Anthropic ships a new minor.
type modelInfo struct {
	Name  string
	Alias string
	Desc  string
}

var availableModels = []modelInfo{
	{"sonnet", "sonnet", "Best coding model, fast (resolves via $ANTHROPIC_DEFAULT_SONNET_MODEL)"},
	{"opus", "opus", "Deepest reasoning (resolves via $ANTHROPIC_DEFAULT_OPUS_MODEL)"},
	{"haiku", "haiku", "Fastest, lightweight (resolves via $ANTHROPIC_DEFAULT_HAIKU_MODEL)"},
}

func (b *Bridge) cmdModel(ctx context.Context, m channel.InboundMessage, args string) {
	modelName := strings.TrimSpace(args)
	if modelName == "" {
		b.showModelMenu(ctx, m)
		return
	}
	sess, err := b.ensureCurrentSession(ctx, m, true)
	if err != nil {
		// Error after a button click: replace the menu in place with the
		// failure card so the user doesn't end up with a stale menu plus
		// a separate error message.
		b.replyModelResult(ctx, m, channel.Card{
			Title:    "Switch Model · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: err.Error()}},
		})
		return
	}
	if err := sess.SwitchModel(modelName); err != nil {
		b.replyModelResult(ctx, m, channel.Card{
			Title:    "Switch Model · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: "切换模型失败: " + err.Error()}},
		})
		return
	}
	b.replyModelResult(ctx, m, channel.Card{
		Title: "Switch Model · ✓ " + modelName,
		Tone:  channel.ToneSuccess,
		Sections: []channel.Section{{
			Markdown: fmt.Sprintf("已切换 session **%s** 的模型为 `%s`",
				displaySessionID(sess), modelName),
		}},
	})
}

// replyModelResult posts a /model result card. When the inbound came
// from a card-action click (m.Reply non-nil), the original menu is
// replaced in place to keep the chat dense. Falls back to a fresh
// outbound for typed `/model <name>` invocations.
func (b *Bridge) replyModelResult(ctx context.Context, m channel.InboundMessage, card channel.Card) {
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// showModelMenu renders the Switch Model card. The card header surfaces
// the target session so users don't have to guess what the buttons will
// act on — and so "no session at all" turns into a clear instruction
// instead of a pointless menu followed by an error on click.
func (b *Bridge) showModelMenu(ctx context.Context, m channel.InboundMessage) {
	target := b.modelMenuTarget(m)

	var subtitle string
	switch {
	case target == nil:
		// No session anywhere — menu is meaningless because there's
		// nothing to switch. Skip the buttons entirely.
		b.replyText(ctx, m, "暂无可切换的 session。先 /new 创建一个,然后再 /model。")
		return
	case target.Info().Status == string(session.StatusActive):
		subtitle = fmt.Sprintf("将作用于 session **%s** (active)", displaySessionID(target))
	default:
		subtitle = fmt.Sprintf("将作用于 session **%s** (idle,点击按钮会自动恢复)",
			displaySessionID(target))
	}

	targetInfo := target.Info()
	var btns []channel.Button
	for _, mi := range availableModels {
		btns = append(btns, channel.Button{
			Label:  mi.Name,
			Style:  "default",
			Action: map[string]string{"action": "switch_model", "model": mi.Name, "session_id": sessionPayloadID(targetInfo)},
		})
	}
	b.replyCard(ctx, m, channel.Card{
		Title: "Switch Model",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: subtitle},
			{Buttons: btns},
		},
	})
}

// modelMenuTarget picks the session a /model button will act on. Mirrors
// the lookup chain used by ensureCurrentSession so the menu subtitle
// stays truthful: thread-bound > main-chat focus > any resumable idle
// session.
func (b *Bridge) modelMenuTarget(m channel.InboundMessage) *session.Session {
	if sess, ok := b.currentSession(m); ok {
		return sess
	}
	return b.mgr.ResolveResumable(m.UserID)
}

// handleModelCardAction handles the switch_model button on the model menu.
// Returns true when claimed.
func (b *Bridge) handleModelCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action.Name != "switch_model" {
		return false
	}
	model, _ := m.Action.Values["model"].(string)
	if model == "" {
		return true
	}
	sessID, _ := m.Action.Values["session_id"].(string)
	if sess, ok := b.resolveSessionByPayload(sessID); ok {
		// Session pinned at menu-render time — apply directly without
		// re-running ensureCurrentSession (avoids TOCTOU if focus changed).
		if err := sess.SwitchModel(model); err != nil {
			b.replyModelResult(ctx, m, channel.Card{
				Title:    "Switch Model · 失败",
				Tone:     channel.ToneWarning,
				Sections: []channel.Section{{Markdown: "切换模型失败: " + err.Error()}},
			})
			return true
		}
		b.replyModelResult(ctx, m, channel.Card{
			Title: "Switch Model · ✓ " + model,
			Tone:  channel.ToneSuccess,
			Sections: []channel.Section{{
				Markdown: fmt.Sprintf("已切换 session **%s** 的模型为 `%s`",
					displaySessionID(sess), model),
			}},
		})
	} else {
		// Fallback: session no longer exists (e.g. archived between menu
		// render and click) — let cmdModel surface the error gracefully.
		b.cmdModel(ctx, m, model)
	}
	return true
}
