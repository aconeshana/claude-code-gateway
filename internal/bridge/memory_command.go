package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// cmdMemory lists Claude Code memory files as a card with one button per
// file. Clicking a button drills into a detail card showing the full content.
func (b *Bridge) cmdMemory(ctx context.Context, m channel.InboundMessage) {
	wd := b.currentProjectDir(m)
	files := claudefiles.ListMemoryFiles(wd)
	b.replyCard(ctx, m, buildMemoryListCard(files, wd))
}

func buildMemoryListCard(files []claudefiles.MemoryFile, projectDir string) channel.Card {
	if len(files) == 0 {
		hint := "_未发现 memory 文件_。\n可在以下位置创建:\n- 用户: `~/.claude/CLAUDE.md` 或 `~/.claude/rules/<name>.md`"
		if projectDir != "" {
			hint += fmt.Sprintf("\n- 项目: `%s/CLAUDE.md` 或 `%s/.claude/rules/<name>.md`", projectDir, projectDir)
		}
		return channel.Card{
			Title:    "Memory",
			Tone:     channel.ToneNeutral,
			Sections: []channel.Section{{Markdown: hint}},
		}
	}

	var userCount, projectCount, rulesCount, localCount int
	for _, f := range files {
		switch f.Type {
		case claudefiles.MemoryTypeUser:
			userCount++
		case claudefiles.MemoryTypeProject:
			projectCount++
		case claudefiles.MemoryTypeRules:
			rulesCount++
		case claudefiles.MemoryTypeLocal:
			localCount++
		}
	}

	var summary []string
	if userCount > 0 {
		summary = append(summary, fmt.Sprintf("用户 %d", userCount))
	}
	if projectCount > 0 {
		summary = append(summary, fmt.Sprintf("项目 %d", projectCount))
	}
	if rulesCount > 0 {
		summary = append(summary, fmt.Sprintf("rules %d", rulesCount))
	}
	if localCount > 0 {
		summary = append(summary, fmt.Sprintf("local %d", localCount))
	}

	sections := []channel.Section{{
		Markdown: fmt.Sprintf("**%d 个 memory 文件** · %s", len(files), strings.Join(summary, " · ")),
	}}

	for i, f := range files {
		typeTag := memoryTypeTag(f.Type)
		label := fmt.Sprintf("%s %s · %s", typeTag, f.Label, displayPath(f.Path))
		sections = append(sections, channel.Section{
			Buttons: []channel.Button{{
				Label: label,
				Style: "default",
				Action: map[string]string{
					"action": "memory_file_detail",
					"index":  fmt.Sprintf("%d", i),
					"wd":     projectDir,
				},
			}},
		})
	}

	sections = append(sections, channel.Section{
		Divider: true,
		Note:    "点击文件查看内容 · 如需修改请直接编辑对应文件",
	})

	return channel.Card{
		Title:    fmt.Sprintf("Memory · %d", len(files)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func buildMemoryDetailCard(f claudefiles.MemoryFile, index int, projectDir string) channel.Card {
	content := strings.TrimSpace(f.Content)
	if content == "" {
		content = "_(空文件)_"
	} else {
		content = truncateForCard(content, 2000)
	}

	sections := []channel.Section{
		{Markdown: fmt.Sprintf("**%s**\n`%s`", f.Label, f.Path)},
		{Divider: true, Markdown: content},
		{
			Divider: true,
			Buttons: []channel.Button{{
				Label: "← 返回列表",
				Style: "default",
				Action: map[string]string{
					"action": "memory_list",
					"wd":     projectDir,
				},
			}},
		},
	}

	return channel.Card{
		Title:    "Memory: " + f.Label,
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// handleMemoryCardAction handles memory_file_detail and memory_list card actions.
// Returns true when claimed.
func (b *Bridge) handleMemoryCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action == nil {
		return false
	}
	switch m.Action.Name {
	case "memory_file_detail":
		b.handleMemoryFileDetail(ctx, m)
	case "memory_list":
		b.handleMemoryList(ctx, m)
	default:
		return false
	}
	return true
}

func (b *Bridge) handleMemoryFileDetail(ctx context.Context, m channel.InboundMessage) {
	wd, _ := m.Action.Values["wd"].(string)
	indexStr, _ := m.Action.Values["index"].(string)
	index := 0
	fmt.Sscanf(indexStr, "%d", &index)

	files := claudefiles.ListMemoryFiles(wd)
	if index < 0 || index >= len(files) {
		return
	}
	card := buildMemoryDetailCard(files[index], index, wd)
	if m.Reply != nil {
		m.Reply(card)
	} else {
		b.replyCard(ctx, m, card)
	}
}

func (b *Bridge) handleMemoryList(ctx context.Context, m channel.InboundMessage) {
	wd, _ := m.Action.Values["wd"].(string)
	files := claudefiles.ListMemoryFiles(wd)
	card := buildMemoryListCard(files, wd)
	if m.Reply != nil {
		m.Reply(card)
	} else {
		b.replyCard(ctx, m, card)
	}
}

func memoryTypeTag(t claudefiles.MemoryType) string {
	switch t {
	case claudefiles.MemoryTypeUser:
		return "👤"
	case claudefiles.MemoryTypeProject:
		return "📁"
	case claudefiles.MemoryTypeRules:
		return "📋"
	case claudefiles.MemoryTypeLocal:
		return "🔒"
	default:
		return "📄"
	}
}
