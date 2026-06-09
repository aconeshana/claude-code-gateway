package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// skillEntry describes one skill discovered from a SKILL.md file.
//
// Discovery mirrors Claude Code's own walk: start at the session's working
// dir, climb to git root (or $HOME, whichever comes first), collecting
// .claude/skills/<name>/SKILL.md; then add ~/.claude/skills/ as the global
// fallback. Project-level skills override globals when they share a name —
// matches how the CLI itself resolves them.
type skillEntry struct {
	Name        string // directory basename = command id
	Description string // from frontmatter or body's first line
	Source      string // absolute path to SKILL.md (display only)
	Body        string // SKILL.md body after frontmatter — passed through verbatim
}

// scanSkills returns the deduped, sorted list of skills visible from
// workingDir. Order: project-level (closest dir first), then ~/.claude/skills/.
// Same name later in the list is ignored (first one wins → project overrides global).
func scanSkills(workingDir string) []skillEntry {
	dirs := skillDirs(workingDir)
	seen := map[string]bool{}
	var out []skillEntry
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if seen[name] {
				continue
			}
			path := filepath.Join(d, name, "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			s := parseSkillMD(name, string(data), path)
			if s == nil {
				continue
			}
			seen[name] = true
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// skillDirs returns the ordered list of skill directories to scan, near-to-far.
func skillDirs(workingDir string) []string {
	var dirs []string
	add := func(p string) {
		for _, existing := range dirs {
			if existing == p {
				return
			}
		}
		dirs = append(dirs, p)
	}

	home := homeDir()
	if workingDir != "" {
		current := filepath.Clean(workingDir)
		stopAt := findSkillsGitRoot(current)
		for {
			if home != "" && samePath(current, home) {
				break
			}
			add(filepath.Join(current, ".claude", "skills"))
			if stopAt != "" && samePath(current, stopAt) {
				break
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	if home != "" {
		add(filepath.Join(home, ".claude", "skills"))
	}
	return dirs
}

func findSkillsGitRoot(start string) string {
	current := filepath.Clean(start)
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// parseSkillMD pulls name/description from optional YAML frontmatter,
// otherwise falls back to the dir name and first body line. Supports YAML
// block scalar indicators ('>', '|', '>-', '|-') for multi-line description.
func parseSkillMD(dirName, raw, source string) *skillEntry {
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil
	}

	body := content
	var fm map[string]string
	if strings.HasPrefix(content, "---") {
		rest := content[3:]
		// frontmatter block ends at the next line containing only "---".
		end := strings.Index(rest, "\n---")
		if end >= 0 {
			fm = parseFrontmatter(rest[:end])
			body = strings.TrimSpace(rest[end+4:])
		}
	}
	if body == "" {
		return nil
	}

	desc := ""
	if fm != nil {
		desc = fm["description"]
	}
	if desc == "" {
		first, _, _ := strings.Cut(body, "\n")
		desc = strings.TrimSpace(first)
	}

	return &skillEntry{
		Name:        dirName,
		Description: desc,
		Source:      source,
		Body:        body,
	}
}

func parseFrontmatter(block string) map[string]string {
	m := map[string]string{}
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Block scalar indicators: read indented continuation lines.
		if val == ">" || val == "|" || val == ">-" || val == "|-" {
			folded := val == ">" || val == ">-"
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				cont := lines[j]
				if strings.TrimSpace(cont) == "" {
					if !folded {
						parts = append(parts, "")
					}
					continue
				}
				// Must be indented relative to the key — assume any leading
				// whitespace counts (frontmatter rarely nests).
				if cont[0] != ' ' && cont[0] != '\t' {
					i = j - 1
					break
				}
				parts = append(parts, strings.TrimSpace(cont))
				i = j
			}
			if folded {
				val = strings.Join(parts, " ")
			} else {
				val = strings.Join(parts, "\n")
			}
		}

		// Trim surrounding quotes if present.
		val = strings.TrimSpace(val)
		if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
			(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
			val = val[1 : len(val)-1]
		}
		m[key] = val
	}
	return m
}

// --- /skills command ---

func (b *Bridge) cmdSkills(ctx context.Context, m channel.InboundMessage) {
	card := b.buildSkillsCard(m)
	b.replyCard(ctx, m, card)
}

func (b *Bridge) skillsWorkingDir(m channel.InboundMessage) string {
	if wd := b.currentProjectDir(m); wd != "" {
		return wd
	}
	// Skills scan tolerates a missing project context: fall back to $HOME
	// so the user always sees their global skills even when no session is
	// focused and no defaultCWD is set.
	return homeDir()
}

func (b *Bridge) buildSkillsCard(m channel.InboundMessage) channel.Card {
	wd := b.skillsWorkingDir(m)
	skills := scanSkills(wd)
	globalPrefix := filepath.Join(homeDir(), ".claude", "skills") + string(filepath.Separator)

	sections := make([]channel.Section, 0, len(skills)+1)
	if len(skills) == 0 {
		sections = append(sections, channel.Section{
			Markdown: "_未发现可用 skill_。把 SKILL.md 放到 `.claude/skills/<name>/SKILL.md` 即可被识别。",
		})
	} else {
		for _, s := range skills {
			scope := "项目"
			if strings.HasPrefix(s.Source, globalPrefix) {
				scope = "全局"
			}
			// Two visual rows per skill in a single section: label + 详情 on top,
			// then [input] [执行] below via an inline form. Lark renders the
			// form on its own line, which gives the user space to type without
			// forcing a 2nd-level menu.
			sections = append(sections, channel.Section{
				Markdown: fmt.Sprintf("[%s] **/%s**", scope, s.Name),
				Form: &channel.Form{
					FormID: "skill_run_" + s.Name,
					Fields: []channel.FormField{{
						// Lark requires input.name to be unique per card — same
						// /skills list shows N forms, so namespace per skill.
						Name:        "user_input_" + s.Name,
						Placeholder: "(可选) 自然语言信息",
					}},
					LeadingButtons: []channel.Button{{
						Label: "详情", Style: "default",
						Action: map[string]string{"action": "show_skill", "key": s.Name},
					}},
					Submit: channel.Button{
						Label: "执行", Style: "primary",
						Action: map[string]string{"action": "run_skill", "key": s.Name},
					},
				},
			})
		}
	}
	return channel.Card{
		Title:    fmt.Sprintf("Skills (%d)", len(skills)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// --- action: show_skill (二级菜单) ---

func (b *Bridge) showSkillDetail(ctx context.Context, m channel.InboundMessage) {
	name, _ := m.Action.Values["key"].(string)
	if name == "" {
		b.replyOrText(ctx, m, "skill 名称缺失")
		return
	}
	wd := b.skillsWorkingDir(m)
	var found *skillEntry
	for _, s := range scanSkills(wd) {
		if s.Name == name {
			s := s
			found = &s
			break
		}
	}
	if found == nil {
		b.replyOrText(ctx, m, fmt.Sprintf("skill `%s` 不存在或已移除", name))
		return
	}

	card := b.buildSkillDetailCard(*found, "")
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// buildSkillDetailCard renders the per-skill detail view. initialInput
// pre-fills the Form field (used when re-rendering after a failed execute).
func (b *Bridge) buildSkillDetailCard(s skillEntry, initialInput string) channel.Card {
	desc := s.Description
	if desc == "" {
		desc = "_无描述_"
	}
	header := fmt.Sprintf("**/%s**\n%s\n\n_source_: `%s`", s.Name, desc, s.Source)

	sections := []channel.Section{
		{Markdown: header},
		{Divider: true},
		{Markdown: s.Body}, // 原文透传,飞书超限再说
		{Divider: true},
		{Form: &channel.Form{
			FormID: "skill_run_" + s.Name,
			Fields: []channel.FormField{{
				Name:        "user_input",
				Label:       "可选输入",
				Placeholder: "(可选) 自然语言信息",
				Initial:     initialInput,
			}},
			Submit: channel.Button{
				Label: "执行", Style: "primary",
				Action: map[string]string{"action": "run_skill", "key": s.Name},
			},
		}},
		{Buttons: []channel.Button{{
			Label: "← 返回", Style: "default",
			Action: map[string]string{"action": "back_to_skills"},
		}}},
	}
	return channel.Card{
		Title:    "Skill · " + s.Name,
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// --- action: run_skill ---

func (b *Bridge) runSkill(ctx context.Context, m channel.InboundMessage) {
	name, _ := m.Action.Values["key"].(string)
	if name == "" {
		b.replyOrText(ctx, m, "skill 名称缺失")
		return
	}

	// Form input name is namespaced per skill on the /skills list ("user_input_<name>")
	// but kept plain ("user_input") on the detail card. Accept either.
	userInput, _ := m.Action.FormValue["user_input_"+name].(string)
	if userInput == "" {
		userInput, _ = m.Action.FormValue["user_input"].(string)
	}
	userInput = strings.TrimSpace(userInput)

	// Find or auto-create a session: focused → use it; no focus → fall back
	// to the gateway's default cwd (whatever /new without focus would have used).
	sess, ok := b.mgr.FocusedSession(m.UserID)
	if !ok {
		created, err := b.mgr.Create(ctx, session.CreateOpts{
			WorkingDir:  b.defaultCWD,
			OwnerID:     m.UserID,
			ChatID:      m.ChatID,
			ChannelKind: m.ChannelKind,
			Origin:      channelKindToOrigin(m.ChannelKind),
		})
		if err != nil {
			card := b.buildSkillExecutedCard(name, userInput, false,
				"自动创建 session 失败: "+err.Error())
			if m.Reply != nil {
				m.Reply(card)
				return
			}
			b.replyCard(ctx, m, card)
			return
		}
		b.ensureSubscribed(ctx, created, m)
		_ = b.mgr.SetFocus(m.UserID, created.ID)
		sess = created
	}

	prompt := "/" + name
	if userInput != "" {
		prompt += "\n" + userInput
	}
	sess.SetLastInbound(inboundLocationFrom(m))
	if err := sess.SendMessage(prompt); err != nil {
		card := b.buildSkillExecutedCard(name, userInput, false,
			"发送到 session 失败: "+err.Error())
		if m.Reply != nil {
			m.Reply(card)
			return
		}
		b.replyCard(ctx, m, card)
		return
	}
	b.mgr.AppendRecentMessage(sess.ID, prompt)

	card := b.buildSkillExecutedCard(name, userInput, true,
		fmt.Sprintf("已注入到 session `%s`,bot 的回复会出现在%s",
			displaySessionID(sess), inboundLocationLabel(m)))
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

func inboundLocationLabel(m channel.InboundMessage) string {
	if m.ThreadID != "" {
		return "本话题"
	}
	return "主聊天"
}

func (b *Bridge) buildSkillExecutedCard(name, userInput string, success bool, note string) channel.Card {
	tone := channel.ToneSuccess
	title := "Skill · " + name + " · 已执行"
	if !success {
		tone = channel.ToneWarning
		title = "Skill · " + name + " · 执行失败"
	}
	promptPreview := "/" + name
	if userInput != "" {
		promptPreview += "\n" + userInput
	}
	sections := []channel.Section{
		{Markdown: fmt.Sprintf("**注入的 prompt:**\n```\n%s\n```", promptPreview)},
		{Markdown: note},
	}
	return channel.Card{Title: title, Tone: tone, Sections: sections}
}

// --- action: back_to_skills ---

func (b *Bridge) replyWithSkillsCard(ctx context.Context, m channel.InboundMessage) {
	card := b.buildSkillsCard(m)
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// handleSkillCardAction is the dispatcher entry for skill-related card buttons.
// Returns true when m.Action.Name was claimed by this domain.
func (b *Bridge) handleSkillCardAction(ctx context.Context, m channel.InboundMessage) bool {
	switch m.Action.Name {
	case "show_skill":
		b.showSkillDetail(ctx, m)
	case "run_skill":
		b.runSkill(ctx, m)
	case "back_to_skills":
		b.replyWithSkillsCard(ctx, m)
	default:
		return false
	}
	return true
}
