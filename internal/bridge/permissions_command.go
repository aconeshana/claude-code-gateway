package bridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/permissionsfile"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// cmdPermissions handles the /permissions slash command.
//
// /permissions             → list all rules across user/project/local
// /permissions allow <pat> → fast-path: jump to source picker with
//                            behavior=allow and pattern pre-filled
// /permissions deny <pat>  → same but behavior=deny
// /permissions ask <pat>   → same but behavior=ask
//
// All other interactions happen through card buttons. See
// permissionsAddWizard* below for the three-step add flow.
func (b *Bridge) cmdPermissions(ctx context.Context, m channel.InboundMessage, args string) {
	args = strings.TrimSpace(args)
	if args == "" {
		b.replyPermissionsList(ctx, m)
		return
	}
	parts := strings.SplitN(args, " ", 2)
	first := strings.ToLower(parts[0])
	rest := ""
	if len(parts) == 2 {
		rest = strings.TrimSpace(parts[1])
	}
	if !permissionsfile.IsValidBehavior(permissionsfile.Behavior(first)) {
		b.replyText(ctx, m, "用法: `/permissions` 列出 · `/permissions [allow|deny|ask] <pattern>` 添加")
		return
	}
	// Skip the behavior step in the wizard since the user already typed it.
	b.replyCard(ctx, m, b.buildPermissionsSourcePickerCard(
		permissionsfile.Behavior(first), rest))
}

// replyPermissionsList renders the read view: rules grouped by source ×
// behavior, each row carrying a [删除] button. A trailing [➕ 添加规则]
// button starts the add wizard.
//
// When invoked from a card-action callback (m.Reply non-nil) the list
// replaces the source card in place — the user clicked [返回列表] /
// [刷新] / [➕ 添加规则]'s [取消] etc. on a wizard or success card and
// expects the navigation to mutate that card, not stack a new one in
// the chat. Slash-command invocations (m.Reply nil) fall through to a
// fresh outbound.
func (b *Bridge) replyPermissionsList(ctx context.Context, m channel.InboundMessage) {
	projectDir := b.currentProjectDir(m)
	rules, err := permissionsfile.Load(projectDir)
	if err != nil {
		b.replyText(ctx, m, "读取 permission 规则失败: "+err.Error())
		return
	}
	card := b.buildPermissionsListCard(rules, projectDir)
	b.replyOrUpdateCard(ctx, m, card)
}

// buildPermissionsListCard renders a snapshot of all rules. The
// projectDir parameter is shown in the header so users understand which
// project's settings.json files are being read.
func (b *Bridge) buildPermissionsListCard(rules []permissionsfile.Rule, projectDir string) channel.Card {
	header := "**当前项目:** "
	if projectDir == "" {
		header += "(无 — 仅读取 user 范围)"
	} else {
		header += "`" + displayPath(projectDir) + "`"
	}
	header += "\n\n规则按 user → project → local 顺序展示;后者覆盖前者。"

	sections := []channel.Section{{Markdown: header}}

	if len(rules) == 0 {
		sections = append(sections, channel.Section{
			Markdown: "_暂无规则_。点 [➕ 添加规则] 开始,或编辑 `~/.claude/settings.json` / `<project>/.claude/settings(.local).json` 的 `permissions` 字段手动写入。",
		})
	} else {
		// Group: source → behavior → []content
		grouped := groupRulesBySourceBehavior(rules)
		for _, src := range permissionsfile.AllSources {
			byBehavior, ok := grouped[src]
			if !ok || len(byBehavior) == 0 {
				continue
			}
			sections = append(sections, channel.Section{Divider: true,
				Markdown: "**" + sourceLabel(src) + "**"})
			for _, beh := range permissionsfile.AllBehaviors {
				list := byBehavior[beh]
				if len(list) == 0 {
					continue
				}
				sort.Strings(list)
				for _, content := range list {
					sections = append(sections, channel.Section{
						ButtonLayout: "trailing",
						Markdown:     behaviorIcon(beh) + " `" + content + "`",
						Buttons: []channel.Button{{
							Label: "删除",
							Style: "danger",
							Action: map[string]string{
								"action":   "permissions_remove",
								"source":   string(src),
								"behavior": string(beh),
								"content":  content,
							},
						}},
					})
				}
			}
		}
	}

	sections = append(sections, channel.Section{Divider: true,
		Buttons: []channel.Button{{
			Label:  "➕ 添加规则",
			Style:  "primary",
			Action: map[string]string{"action": "permissions_show_add"},
		}, {
			Label:  "🔄 刷新",
			Style:  "default",
			Action: map[string]string{"action": "permissions_show_list"},
		}},
		ButtonLayout: "fill",
	})

	return channel.Card{
		Title:    "Permissions",
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// buildPermissionsBehaviorPickerCard is wizard step 1.
func (b *Bridge) buildPermissionsBehaviorPickerCard() channel.Card {
	return channel.Card{
		Title: "添加规则 · 第 1 步 / 共 3 步",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: "选择规则**行为**:"},
			{Buttons: []channel.Button{
				{Label: "✓ 允许", Style: "primary",
					Action: map[string]string{"action": "permissions_pick_behavior", "behavior": "allow"}},
				{Label: "✗ 拒绝", Style: "danger",
					Action: map[string]string{"action": "permissions_pick_behavior", "behavior": "deny"}},
				{Label: "? 询问", Style: "default",
					Action: map[string]string{"action": "permissions_pick_behavior", "behavior": "ask"}},
			}, ButtonLayout: "fill"},
			{Markdown: "_- **允许** 命中即直接放行,不弹审批卡_\n_- **拒绝** 命中即拒(对 auto/forward 模式都生效)_\n_- **询问** 强制审批(对已配 auto-allow 的工具也会问)_"},
		},
	}
}

// buildPermissionsSourcePickerCard is wizard step 2. behavior is locked
// in from step 1 (or from a /permissions allow ... typed shortcut).
// prefilledPattern carries pattern from the forward card's "always allow
// this pattern" entry — empty otherwise.
func (b *Bridge) buildPermissionsSourcePickerCard(behavior permissionsfile.Behavior, prefilledPattern string) channel.Card {
	subtitle := "已选: **" + behaviorIcon(behavior) + " " + string(behavior) + "**"
	if prefilledPattern != "" {
		subtitle += " · 模式: `" + prefilledPattern + "`"
	}
	mkBtn := func(src permissionsfile.Source, label, style string) channel.Button {
		return channel.Button{
			Label: label, Style: style,
			Action: map[string]string{
				"action":   "permissions_pick_source",
				"behavior": string(behavior),
				"source":   string(src),
				"pattern":  prefilledPattern,
			},
		}
	}
	return channel.Card{
		Title: "添加规则 · 第 2 步 / 共 3 步",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: subtitle + "\n\n选择写入**范围**:"},
			{Buttons: []channel.Button{
				mkBtn(permissionsfile.SourceLocal, "📌 local · 仅本机本项目", "primary"),
				mkBtn(permissionsfile.SourceProject, "📁 project · 团队共享", "default"),
				mkBtn(permissionsfile.SourceUser, "🌐 user · 跨项目全局", "default"),
			}, ButtonLayout: "fill"},
			{Markdown: "_- **local** 写 `.claude/settings.local.json`(gitignored)_\n_- **project** 写 `.claude/settings.json`(随 git 提交,全队共享)_\n_- **user** 写 `~/.claude/settings.json`(影响所有项目)_"},
			{Buttons: []channel.Button{{
				Label: "← 返回", Style: "default",
				Action: map[string]string{"action": "permissions_show_add"},
			}}},
		},
	}
}

// buildPermissionsPatternFormCard is wizard step 3.
func (b *Bridge) buildPermissionsPatternFormCard(behavior permissionsfile.Behavior, source permissionsfile.Source, prefilledPattern string) channel.Card {
	return channel.Card{
		Title: "添加规则 · 第 3 步 / 共 3 步",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: fmt.Sprintf(
				"已选: **%s %s** · 范围: **%s**\n\n输入规则**模式**(如 `Bash(git push:*)` / `WebFetch(domain:example.com)` / `WebSearch`):",
				behaviorIcon(behavior), behavior, sourceLabel(source))},
			{Form: &channel.Form{
				FormID: "permissions_add_form",
				Fields: []channel.FormField{{Name: "pattern", Placeholder: "Bash(...)", Initial: prefilledPattern}},
				Submit: channel.Button{
					Label: "💾 保存", Style: "primary",
					Action: map[string]string{
						"action":   "permissions_add_submit",
						"behavior": string(behavior),
						"source":   string(source),
					},
				},
				SecondaryButtons: []channel.Button{{
					Label: "取消", Style: "default",
					Action: map[string]string{"action": "permissions_show_list"},
				}},
			}},
		},
	}
}

// handlePermissionsCardAction dispatches all permissions_* card actions.
// Returns true when claimed.
func (b *Bridge) handlePermissionsCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action == nil {
		return false
	}
	switch m.Action.Name {
	case "permissions_show_list":
		b.replyPermissionsList(ctx, m)
	case "permissions_show_add":
		b.replyOrUpdateCard(ctx, m, b.buildPermissionsBehaviorPickerCard())
	case "permissions_pick_behavior":
		beh := stringField(m.Action.Values, "behavior")
		if !permissionsfile.IsValidBehavior(permissionsfile.Behavior(beh)) {
			b.replyText(ctx, m, "无效的 behavior: "+beh)
			return true
		}
		b.replyOrUpdateCard(ctx, m,
			b.buildPermissionsSourcePickerCard(permissionsfile.Behavior(beh), ""))
	case "permissions_pick_source":
		beh := stringField(m.Action.Values, "behavior")
		src := stringField(m.Action.Values, "source")
		pattern := stringField(m.Action.Values, "pattern")
		if !permissionsfile.IsValidBehavior(permissionsfile.Behavior(beh)) ||
			!permissionsfile.IsValidSource(permissionsfile.Source(src)) {
			b.replyText(ctx, m, "无效的参数: behavior="+beh+" source="+src)
			return true
		}
		b.replyOrUpdateCard(ctx, m,
			b.buildPermissionsPatternFormCard(permissionsfile.Behavior(beh),
				permissionsfile.Source(src), pattern))
	case "permissions_add_submit":
		b.handlePermissionsAddSubmit(ctx, m)
	case "permissions_remove":
		b.handlePermissionsRemove(ctx, m)
	default:
		return false
	}
	return true
}

func (b *Bridge) handlePermissionsAddSubmit(ctx context.Context, m channel.InboundMessage) {
	beh := stringField(m.Action.Values, "behavior")
	src := stringField(m.Action.Values, "source")
	pattern := strings.TrimSpace(formField(m.Action, "pattern"))
	if pattern == "" {
		b.replyText(ctx, m, "规则模式不能为空")
		return
	}
	if !permissionsfile.IsValidBehavior(permissionsfile.Behavior(beh)) ||
		!permissionsfile.IsValidSource(permissionsfile.Source(src)) {
		b.replyText(ctx, m, "无效的参数: behavior="+beh+" source="+src)
		return
	}
	rule := permissionsfile.Rule{
		Source:   permissionsfile.Source(src),
		Behavior: permissionsfile.Behavior(beh),
		Content:  pattern,
	}
	projectDir := b.currentProjectDir(m)
	if rule.Source != permissionsfile.SourceUser && projectDir == "" {
		b.replyText(ctx, m, "当前没有聚焦项目 — 选 `user` 范围或先 /project 选项目后再加")
		return
	}

	err := permissionsfile.Add(rule, projectDir)
	switch {
	case err == nil, errors.Is(err, permissionsfile.ErrAlreadyExists):
		// Both treated as success: the desired state is reached.
		report := b.applySettingsRespawn(m.UserID, rule.Source, projectDir,
			fmt.Sprintf("permissions %s rule added", rule.Behavior))
		b.replyOrUpdateCard(ctx, m,
			b.buildPermissionsAddedCard(rule, err == nil, report))
	default:
		b.replyText(ctx, m, "添加规则失败: "+err.Error())
	}
}

func (b *Bridge) handlePermissionsRemove(ctx context.Context, m channel.InboundMessage) {
	beh := stringField(m.Action.Values, "behavior")
	src := stringField(m.Action.Values, "source")
	content := stringField(m.Action.Values, "content")
	if !permissionsfile.IsValidBehavior(permissionsfile.Behavior(beh)) ||
		!permissionsfile.IsValidSource(permissionsfile.Source(src)) ||
		content == "" {
		b.replyText(ctx, m, "无效的删除参数")
		return
	}
	rule := permissionsfile.Rule{
		Source: permissionsfile.Source(src), Behavior: permissionsfile.Behavior(beh),
		Content: content,
	}
	projectDir := b.currentProjectDir(m)
	if err := permissionsfile.Remove(rule, projectDir); err != nil {
		if errors.Is(err, permissionsfile.ErrNotFound) {
			b.replyText(ctx, m, "规则不存在(可能已被外部修改)")
			b.replyPermissionsList(ctx, m)
			return
		}
		b.replyText(ctx, m, "删除规则失败: "+err.Error())
		return
	}
	report := b.applySettingsRespawn(m.UserID, rule.Source, projectDir,
		fmt.Sprintf("permissions %s rule removed", rule.Behavior))
	rules, _ := permissionsfile.Load(projectDir)
	card := b.buildPermissionsListCard(rules, projectDir)
	if note := settingsRespawnNote(report); note != "" {
		// Surface the respawn outcome inline above the list so the user
		// sees what just happened without losing the navigation context.
		card.Sections = append([]channel.Section{
			{Markdown: "✓ 已删除 `" + content + "` (" + sourceLabel(rule.Source) + ")\n_" + note + "_"},
			{Divider: true},
		}, card.Sections...)
	}
	b.replyOrUpdateCard(ctx, m, card)
}

// buildPermissionsAddedCard renders the success card after an Add. dup
// distinguishes between a fresh write (true) and ErrAlreadyExists (false)
// so the user knows when their action was a no-op.
func (b *Bridge) buildPermissionsAddedCard(rule permissionsfile.Rule, fresh bool, report respawnReport) channel.Card {
	tag := "✓ 已添加"
	if !fresh {
		tag = "✓ 规则已存在(无需重复添加)"
	}
	body := fmt.Sprintf("%s\n\n- 行为: **%s %s**\n- 范围: **%s**\n- 模式: `%s`",
		tag, behaviorIcon(rule.Behavior), rule.Behavior,
		sourceLabel(rule.Source), rule.Content)
	if note := summarizeRespawn(report); note != "" {
		body += "\n\n**Session 影响**\n" + note
	}
	return channel.Card{
		Title: "Permissions · " + string(rule.Behavior),
		Tone:  channel.ToneSuccess,
		Sections: []channel.Section{
			{Markdown: body},
			{Buttons: []channel.Button{{
				Label: "返回列表", Style: "primary",
				Action: map[string]string{"action": "permissions_show_list"},
			}, {
				Label: "再加一条", Style: "default",
				Action: map[string]string{"action": "permissions_show_add"},
			}}, ButtonLayout: "fill"},
		},
	}
}

// groupRulesBySourceBehavior buckets rules for the list-view renderer.
func groupRulesBySourceBehavior(rules []permissionsfile.Rule) map[permissionsfile.Source]map[permissionsfile.Behavior][]string {
	out := map[permissionsfile.Source]map[permissionsfile.Behavior][]string{}
	for _, r := range rules {
		if out[r.Source] == nil {
			out[r.Source] = map[permissionsfile.Behavior][]string{}
		}
		out[r.Source][r.Behavior] = append(out[r.Source][r.Behavior], r.Content)
	}
	return out
}

// behaviorIcon picks a single-character glyph for each rule type so list
// rows scan visually. Stays in sync with the buttons in the wizard.
func behaviorIcon(b permissionsfile.Behavior) string {
	switch b {
	case permissionsfile.BehaviorAllow:
		return "✓"
	case permissionsfile.BehaviorDeny:
		return "✗"
	case permissionsfile.BehaviorAsk:
		return "?"
	}
	return "·"
}

func sourceLabel(s permissionsfile.Source) string {
	switch s {
	case permissionsfile.SourceUser:
		return "🌐 user"
	case permissionsfile.SourceProject:
		return "📁 project"
	case permissionsfile.SourceLocal:
		return "📌 local"
	}
	return string(s)
}

// stringField is a defensive accessor for action.Values — non-string
// values (rare, but possible if Lark deserialization shifts) become "".
func stringField(values map[string]interface{}, key string) string {
	if v, ok := values[key].(string); ok {
		return v
	}
	return ""
}

// formField extracts a value from action.FormValue (which is the channel
// abstraction over Lark's form_value payload). Returns "" when absent or
// when the underlying value is not a string (defensive — Lark always
// sends strings today, but the map is interface{}-typed).
func formField(action *channel.CardAction, key string) string {
	if action == nil {
		return ""
	}
	if v, ok := action.FormValue[key].(string); ok {
		return v
	}
	return ""
}

// _ ensures session import is exercised even if all uses move to other
// files later — keeps imports stable across refactors.
var _ = session.SessionInfo{}
