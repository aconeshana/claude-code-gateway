package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// cmdMCP lists configured MCP (Model Context Protocol) servers from
// ~/.claude.json (top-level + per-project block) and <project>/.mcp.json.
func (b *Bridge) cmdMCP(ctx context.Context, m channel.InboundMessage) {
	wd := b.currentProjectDir(m)
	servers := claudefiles.ListMCPServers(wd)
	b.replyCard(ctx, m, buildMCPCard(servers, wd))
}

func buildMCPCard(servers []claudefiles.MCPServer, projectDir string) channel.Card {
	if len(servers) == 0 {
		return channel.Card{
			Title: "MCP Servers",
			Tone:  channel.ToneNeutral,
			Sections: []channel.Section{{
				Markdown: emptyMCPHint(projectDir),
			}},
		}
	}

	var projectCount, userCount int
	for _, s := range servers {
		if s.Source == claudefiles.SourceProject {
			projectCount++
		} else {
			userCount++
		}
	}

	header := fmt.Sprintf("**%d 个 MCP server** · 项目 %d · 用户 %d", len(servers), projectCount, userCount)
	sections := []channel.Section{{Markdown: header}}

	for _, s := range servers {
		sections = append(sections, mcpServerSection(s))
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    "新增 / 修改使用 `claude mcp add` 命令 · `/mcp` 命令仅展示",
	})
	return channel.Card{
		Title:    fmt.Sprintf("MCP Servers · %d", len(servers)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func mcpServerSection(s claudefiles.MCPServer) channel.Section {
	var b strings.Builder
	transport := s.Type
	if transport == "" {
		transport = "stdio"
	}
	fmt.Fprintf(&b, "**%s** %s · `%s`", s.Name, sourceTag(s.Source), transport)

	switch {
	case s.URL != "":
		fmt.Fprintf(&b, "\n`%s`", s.URL)
	case s.Command != "":
		invocation := s.Command
		if len(s.Args) > 0 {
			invocation += " " + strings.Join(s.Args, " ")
		}
		fmt.Fprintf(&b, "\n`%s`", truncateForCard(invocation, 200))
	}

	if len(s.Env) > 0 {
		// Env keys are revealing enough; values may carry secrets — hide them.
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		fmt.Fprintf(&b, "\n_env_: `%s`", strings.Join(keys, ", "))
	}

	fmt.Fprintf(&b, "\n_path_: `%s`", displayPath(s.Path))
	return channel.Section{Divider: true, Markdown: b.String()}
}

func emptyMCPHint(projectDir string) string {
	if projectDir == "" {
		return "_未发现 MCP server_。\n用 `claude mcp add` 添加,或编辑 `~/.claude.json` 的 `mcpServers` 字段。"
	}
	return fmt.Sprintf(
		"_未发现 MCP server_。\n位置:\n- 用户: `~/.claude.json` `mcpServers`\n- 项目: `%s/.mcp.json`",
		projectDir,
	)
}
