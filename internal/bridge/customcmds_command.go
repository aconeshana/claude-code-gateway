package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// cmdCommands lists user-defined Claude Code slash commands discovered in
// ~/.claude/commands/ and <project>/.claude/commands/. Subdirectories
// produce namespaced names (foo/bar.md → /foo:bar).
func (b *Bridge) cmdCommands(ctx context.Context, m channel.InboundMessage) {
	wd := b.currentProjectDir(m)
	cmds := claudefiles.ListCommands(wd)
	b.replyCard(ctx, m, b.buildCommandsCard(cmds, wd))
}

func (b *Bridge) buildCommandsCard(cmds []claudefiles.Command, projectDir string) channel.Card {
	if len(cmds) == 0 {
		return channel.Card{
			Title: "Custom Commands",
			Tone:  channel.ToneNeutral,
			Sections: []channel.Section{{
				Markdown: emptyCommandsHint(projectDir),
			}},
		}
	}

	var projectCount, userCount int
	for _, c := range cmds {
		if c.Source == claudefiles.SourceProject {
			projectCount++
		} else {
			userCount++
		}
	}

	header := fmt.Sprintf("**%d 个 custom command** · 项目 %d · 用户 %d", len(cmds), projectCount, userCount)
	sections := []channel.Section{{Markdown: header}}

	for _, c := range cmds {
		// Custom commands whose name collides with a gateway-registered
		// command (`/list`, `/new`, etc.) will never reach the CLI — the
		// bridge dispatcher claims them first. Flag the conflict so the
		// user doesn't wonder why their .md file appears inert.
		conflict := b.findCommand("/"+c.Name) != nil
		sections = append(sections, commandSection(c, conflict))
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    "新增 / 修改请编辑对应 .md 文件 · 在主聊天发送 `/<name>` 即可调用",
	})
	return channel.Card{
		Title:    fmt.Sprintf("Custom Commands · %d", len(cmds)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func commandSection(c claudefiles.Command, gatewayConflict bool) channel.Section {
	var b strings.Builder
	fmt.Fprintf(&b, "**`/%s`** %s", c.Name, sourceTag(c.Source))
	if gatewayConflict {
		// Red highlight is what the user notices first — keeps it from being
		// mistaken for a routine source tag.
		fmt.Fprintf(&b, " <font color='red'>⚠ 与网关命令冲突,不会被调用</font>")
	}
	if c.Description != "" {
		fmt.Fprintf(&b, "\n%s", truncateForCard(c.Description, 240))
	}
	fmt.Fprintf(&b, "\n_path_: `%s`", displayPath(c.Path))
	return channel.Section{Divider: true, Markdown: b.String()}
}

func emptyCommandsHint(projectDir string) string {
	if projectDir == "" {
		return "_未发现自定义命令_。把 markdown 文件放到 `~/.claude/commands/<name>.md` 即可被识别。"
	}
	return fmt.Sprintf(
		"_未发现自定义命令_。\n位置:\n- 用户: `~/.claude/commands/<name>.md`\n- 项目: `%s/.claude/commands/<name>.md`",
		projectDir,
	)
}
