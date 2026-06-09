package bridge

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/claudefiles"
)

// cmdHooks lists configured hooks from ~/.claude/settings.json and
// <project>/.claude/settings.json + settings.local.json. Hooks can run
// arbitrary commands or HTTP calls — Phase 1 only displays them; editing
// stays in the JSON files for safety.
func (b *Bridge) cmdHooks(ctx context.Context, m channel.InboundMessage) {
	wd := b.currentProjectDir(m)
	hooks := claudefiles.ListHooks(wd)
	b.replyCard(ctx, m, buildHooksCard(hooks, wd))
}

func buildHooksCard(hooks []claudefiles.Hook, projectDir string) channel.Card {
	if len(hooks) == 0 {
		return channel.Card{
			Title: "Hooks",
			Tone:  channel.ToneNeutral,
			Sections: []channel.Section{{
				Markdown: emptyHooksHint(projectDir),
			}},
		}
	}

	var projectCount, userCount, localCount int
	for _, h := range hooks {
		switch h.Source {
		case claudefiles.SourceLocal:
			localCount++
		case claudefiles.SourceProject:
			projectCount++
		default:
			userCount++
		}
	}

	header := fmt.Sprintf("**%d 个 hook** · 本地 %d · 项目 %d · 用户 %d", len(hooks), localCount, projectCount, userCount)
	sections := []channel.Section{{Markdown: header}}

	// Group by event so users see "all PreToolUse hooks together" instead of
	// having to scan a flat heterogeneous list.
	for _, group := range groupHooksByEvent(hooks) {
		sections = append(sections, channel.Section{
			Divider:  true,
			Markdown: fmt.Sprintf("**%s** · %d", group.event, len(group.entries)),
		})
		for _, h := range group.entries {
			sections = append(sections, hookSection(h))
		}
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    "新增 / 修改请编辑 settings.json `hooks` 字段 · `/hooks` 命令仅展示",
	})
	return channel.Card{
		Title:    fmt.Sprintf("Hooks · %d", len(hooks)),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

type hookGroup struct {
	event   string
	entries []claudefiles.Hook
}

// groupHooksByEvent preserves the input slice's pre-sorted order within
// each event bucket; events themselves come out in first-seen order.
func groupHooksByEvent(hooks []claudefiles.Hook) []hookGroup {
	idx := map[string]int{}
	var groups []hookGroup
	for _, h := range hooks {
		if i, ok := idx[h.Event]; ok {
			groups[i].entries = append(groups[i].entries, h)
			continue
		}
		idx[h.Event] = len(groups)
		groups = append(groups, hookGroup{event: h.Event, entries: []claudefiles.Hook{h}})
	}
	return groups
}

func hookSection(h claudefiles.Hook) channel.Section {
	var b strings.Builder
	matcher := h.Matcher
	if matcher == "" {
		matcher = "*"
	}
	fmt.Fprintf(&b, "matcher: `%s` %s", matcher, sourceTag(h.Source))

	hookType := h.Type
	if hookType == "" {
		hookType = "command"
	}
	switch {
	case h.URL != "":
		fmt.Fprintf(&b, "\n`%s` → `%s`", hookType, redactURL(h.URL))
	case h.Command != "":
		fmt.Fprintf(&b, "\n`%s` → `%s`", hookType, redactCommand(h.Command))
	}

	var meta []string
	if h.Timeout > 0 {
		meta = append(meta, fmt.Sprintf("timeout %ds", h.Timeout))
	}
	if h.Async {
		meta = append(meta, "async")
	}
	if len(meta) > 0 {
		fmt.Fprintf(&b, " · %s", strings.Join(meta, " · "))
	}

	fmt.Fprintf(&b, "\n_path_: `%s`", displayPath(h.Path))
	return channel.Section{Markdown: b.String()}
}

func emptyHooksHint(projectDir string) string {
	if projectDir == "" {
		return "_未发现 hook_。\n编辑 `~/.claude/settings.json` 的 `hooks` 字段添加。"
	}
	return fmt.Sprintf(
		"_未发现 hook_。\n位置:\n- 用户: `~/.claude/settings.json` `hooks`\n- 项目: `%s/.claude/settings.json` `hooks`",
		projectDir,
	)
}

// redactURL strips path, query, and fragment from a URL, keeping only
// scheme + host + a trailing "/…" marker when there was anything beyond.
//
// Hook URLs are typically webhooks (Slack/Discord/Teams) where the URL
// itself encodes a bearer-style secret in the path or query — leaking the
// full URL into a group chat is equivalent to leaking the credential.
// Unparseable input gets a flat "[hidden]" so we fail closed.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[hidden]"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		out += "/…"
	}
	return out
}

// redactCommand keeps only the first whitespace-separated token of a shell
// command (the executable name) and appends " …" when arguments follow.
//
// Hook commands are arbitrary shell, and the body almost always carries
// secrets — `-H "Authorization: Bearer ..."`, `TOKEN=... node x.js`, etc.
// Showing just the binary is enough for a "what runs here?" overview
// without leaking; users who need the exact command should open the
// settings.json file (path is rendered separately).
func redactCommand(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	idx := strings.IndexAny(trimmed, " \t")
	if idx < 0 {
		return trimmed
	}
	first := trimmed[:idx]
	// Heuristic: peel a single leading `ENV=val` assignment so users see the
	// real binary instead of just `FOO=bar`. Only peel when the NEXT token
	// looks like a real command (no '=') — otherwise we risk hiding values
	// inside a `KEY1=v1 KEY2=v2` chain that we shouldn't be unwrapping.
	if looksLikeEnvAssign(first) {
		rest := strings.TrimLeft(trimmed[idx:], " \t")
		if rest != "" {
			nextEnd := strings.IndexAny(rest, " \t")
			nextTok := rest
			if nextEnd >= 0 {
				nextTok = rest[:nextEnd]
			}
			if !looksLikeEnvAssign(nextTok) {
				if nextEnd >= 0 {
					return nextTok + " …"
				}
				return nextTok
			}
		}
	}
	return first + " …"
}

// looksLikeEnvAssign reports whether tok matches the shell `KEY=value`
// convention. We deliberately reject tokens containing '/' or '.' to
// avoid mistaking paths (e.g. `./script.sh`) for env assignments.
func looksLikeEnvAssign(tok string) bool {
	return strings.Contains(tok, "=") && !strings.ContainsAny(tok, "/.")
}
