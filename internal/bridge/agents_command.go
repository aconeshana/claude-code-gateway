package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// cmdAgents lists user-defined Claude Code agents discovered in
// ~/.claude/agents/ and <project>/.claude/agents/. Read-only — Phase 1
// doesn't support add / edit / disable from the bot.
func (b *Bridge) cmdAgents(ctx context.Context, m channel.InboundMessage) {
	wd := b.currentProjectDir(m)
	agents := claudefiles.ListAgents(wd)
	b.replyCard(ctx, m, buildAgentsCard(agents, wd))
}

func buildAgentsCard(agents []claudefiles.Agent, projectDir string) channel.Card {
	if len(agents) == 0 {
		return channel.Card{
			Title: "Agents",
			Tone:  channel.ToneNeutral,
			Sections: []channel.Section{{
				Markdown: emptyAgentsHint(projectDir),
			}},
		}
	}

	var projectCount, userCount int
	for _, a := range agents {
		if a.Source == claudefiles.SourceProject {
			projectCount++
		} else {
			userCount++
		}
	}

	// Header summarises the count by scope so the user sees totals before
	// scrolling through the list.
	header := fmt.Sprintf("**%d 个 agent** · 项目 %d · 用户 %d", len(agents), projectCount, userCount)
	sections := []channel.Section{{Markdown: header}}

	for _, a := range agents {
		sections = append(sections, agentSection(a))
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    "新增 / 修改请编辑对应 .md 文件 · `/agents` 命令仅展示",
	})
	return channel.Card{
		Title:    fmt.Sprintf("Agents · %d", len(agents)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func agentSection(a claudefiles.Agent) channel.Section {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** %s", a.Name, sourceTag(a.Source))
	if a.Model != "" {
		fmt.Fprintf(&b, " · `%s`", a.Model)
	}
	if a.Description != "" {
		fmt.Fprintf(&b, "\n%s", truncateForCard(a.Description, 240))
	}
	if a.Tools != "" {
		fmt.Fprintf(&b, "\n_tools_: `%s`", truncateForCard(a.Tools, 80))
	}
	fmt.Fprintf(&b, "\n_path_: `%s`", displayPath(a.Path))
	return channel.Section{Divider: true, Markdown: b.String()}
}

func emptyAgentsHint(projectDir string) string {
	if projectDir == "" {
		return "_未发现可用 agent_。把 markdown 文件放到 `~/.claude/agents/<name>.md` 即可被识别。"
	}
	return fmt.Sprintf(
		"_未发现可用 agent_。\n位置:\n- 用户: `~/.claude/agents/<name>.md`\n- 项目: `%s/.claude/agents/<name>.md`",
		projectDir,
	)
}
