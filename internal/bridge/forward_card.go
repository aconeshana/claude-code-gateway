// Forward-mode permission request handling for ordinary tools (Bash,
// Read, Edit, Write, Grep, Glob, WebFetch, WebSearch, ...).
//
// Background: claude-code's --permission-prompt-tool stdio handshake
// makes the CLI send a control_request("can_use_tool") for every tool
// invocation it can't decide locally. For three high-ceremony tools
// (AskUserQuestion / EnterPlanMode / ExitPlanMode) we render bespoke
// cards. For all other tools, this file renders a generic approval card
// with three buttons:
//
//   [✓ 允许这次]       — one-shot allow for THIS tool_use only
//   [✗ 拒绝]            — deny
//   [📌 总是允许此模式] — allow this one + jump to permissions wizard
//                          step 2 (source picker) with a pre-filled
//                          rule pattern inferred from the tool input.
//
// "Always allow" reuses the existing add-rule wizard, so source choice
// (user/project/local) stays consistent with how /permissions adds a
// rule from scratch.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/permissionsfile"
	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// toolPermissionInput is the shape we care about inside the
// control_request("can_use_tool") payload. We parse locally rather than
// extending the protocol package because no other call site needs the
// Input field — keeping the protocol surface narrow.
type toolPermissionInput struct {
	Subtype   string                 `json:"subtype"`
	ToolName  string                 `json:"tool_name"`
	ToolUseID string                 `json:"tool_use_id"`
	Input     map[string]interface{} `json:"input"`
}

// handleToolPermission is the entrypoint for forward-mode permission
// cards covering any tool that does not have a dedicated renderer.
// Replaces the prior "unhandled control_request tool" log line that
// silently let the turn hang.
func (b *Bridge) handleToolPermission(ctx context.Context, sess *session.Session, chatID string, raw json.RawMessage, toolName string) {
	var req protocol.StdoutControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		log.Printf("[bridge] tool permission: parse outer: %v", err)
		return
	}
	var inner toolPermissionInput
	if err := json.Unmarshal(req.Request, &inner); err != nil {
		log.Printf("[bridge] tool permission: parse inner: %v", err)
		return
	}

	// Store the pending request so the button callbacks can look it up.
	// We reuse the pendingElicitations bucket — its lifecycle (10 min GC,
	// pop-on-respond) is identical to what this card needs.
	pendingID := b.storePendingElicitation(sess.ID, req.RequestID, inner.ToolUseID, inner.Input)

	card := b.buildToolPermissionCard(toolName, inner.Input, pendingID, sess)
	b.sendCardForSession(ctx, sess, chatID, card)
}

// buildToolPermissionCard composes the forward-mode approval card for a
// non-interactive tool call. Layout:
//
//	[Title]   工具批准 · <toolName>
//	[Subtitle] session display id + working dir hint
//	[Markdown] tool-specific highlight (e.g. command for Bash, file_path
//	           for Read/Edit/Write, url for WebFetch...) so the user can
//	           judge without expanding the raw input
//	[Buttons]  允许这次 · 拒绝 · 总是允许此模式
func (b *Bridge) buildToolPermissionCard(toolName string, input map[string]interface{}, pendingID string, sess *session.Session) channel.Card {
	highlight, suggestedPattern := summarizeToolInvocation(toolName, input)

	displayID := displaySessionID(sess)
	header := fmt.Sprintf("session **%s** · 请求执行 `%s`", displayID, toolName)

	sections := []channel.Section{
		{Markdown: header},
		{Markdown: highlight},
	}

	allowBtn := channel.Button{
		Label: "✓ 允许这次", Style: "primary",
		Action: map[string]string{
			"action": "tool_permission_allow", "pending_id": pendingID,
		},
	}
	denyBtn := channel.Button{
		Label: "✗ 拒绝", Style: "danger",
		Action: map[string]string{
			"action": "tool_permission_deny", "pending_id": pendingID,
		},
	}

	buttons := []channel.Button{allowBtn, denyBtn}
	// Only show [总是允许此模式] when we have a meaningful pattern to
	// suggest. For tools we can't model (e.g. unknown tool name) the
	// button would dump the user into an empty wizard, which is worse
	// than not offering it.
	if suggestedPattern != "" {
		buttons = append(buttons, channel.Button{
			Label: "📌 总是允许此模式", Style: "default",
			Action: map[string]string{
				"action":     "tool_permission_always_allow",
				"pending_id": pendingID,
				"pattern":    suggestedPattern,
			},
		})
		sections = append(sections, channel.Section{
			Markdown: "_点 `[📌 总是允许]` 会先允许这次,然后跳到规则添加(选 user/project/local 范围),建议模式: `" + suggestedPattern + "`_",
		})
	}

	sections = append(sections, channel.Section{Buttons: buttons, ButtonLayout: "fill"})

	return channel.Card{
		Title:    "工具批准 · " + toolName,
		Tone:     channel.ToneWarning,
		Sections: sections,
	}
}

// summarizeToolInvocation returns (markdown_for_card, suggested_rule_pattern)
// for a tool call. The first value is what the user sees in the card body
// — pick the most identity-defining fields (command for Bash, path for
// file tools, url for fetch). The second value is the rule string to
// pre-fill into the permissions wizard.
//
// We're conservative on pattern inference: when in doubt return "" for
// the pattern so the [📌 总是允许] button is hidden rather than offering
// a wrong rule that would silently mis-allow future calls.
//
// See claude-code/src/utils/permissions/permissionRuleParser.ts for the
// canonical rule grammar. Cliffs notes:
//   - Bash(<command>:*)       — allow command + any args
//   - Bash(<command> <args>)  — allow only that exact invocation
//   - Read(<path>)            — allow read of that path (supports globs)
//   - WebFetch(domain:host)   — allow fetches to that domain
//   - <ToolName>              — bare name = allow the entire tool
func summarizeToolInvocation(toolName string, input map[string]interface{}) (markdown, pattern string) {
	switch toolName {
	case "Bash":
		cmd, _ := input["command"].(string)
		desc, _ := input["description"].(string)
		md := "**命令**\n```bash\n" + truncateForCardBytes(cmd, 600) + "\n```"
		if desc != "" {
			md += "\n\n_" + desc + "_"
		}
		if firstWord := firstShellWord(cmd); firstWord != "" {
			pattern = fmt.Sprintf("Bash(%s:*)", firstWord)
		}
		return md, pattern

	case "Read":
		fp, _ := input["file_path"].(string)
		md := "**读取文件**\n`" + fp + "`"
		if off, ok := input["offset"].(float64); ok {
			md += fmt.Sprintf(" (offset=%.0f)", off)
		}
		if lim, ok := input["limit"].(float64); ok {
			md += fmt.Sprintf(" (limit=%.0f)", lim)
		}
		return md, inferFileRulePattern("Read", fp)

	case "Write":
		fp, _ := input["file_path"].(string)
		content, _ := input["content"].(string)
		md := "**写入文件**\n`" + fp + "`\n\n**内容预览**\n```\n" + truncateForCardBytes(content, 400) + "\n```"
		return md, inferFileRulePattern("Write", fp)

	case "Edit":
		fp, _ := input["file_path"].(string)
		oldStr, _ := input["old_string"].(string)
		newStr, _ := input["new_string"].(string)
		md := fmt.Sprintf("**编辑文件**\n`%s`\n\n**删除**\n```\n%s\n```\n\n**插入**\n```\n%s\n```",
			fp, truncateForCardBytes(oldStr, 200), truncateForCardBytes(newStr, 200))
		return md, inferFileRulePattern("Edit", fp)

	case "Grep":
		pat, _ := input["pattern"].(string)
		path, _ := input["path"].(string)
		md := fmt.Sprintf("**Grep**\n模式: `%s`", pat)
		if path != "" {
			md += "\n路径: `" + path + "`"
		}
		return md, "Grep" // grep is read-only, blanket allow makes sense

	case "Glob":
		pat, _ := input["pattern"].(string)
		return "**Glob**\n模式: `" + pat + "`", "Glob"

	case "WebFetch":
		urlStr, _ := input["url"].(string)
		prompt, _ := input["prompt"].(string)
		md := "**WebFetch**\nURL: `" + urlStr + "`"
		if prompt != "" {
			md += "\n\n_提取意图: " + truncateForCardBytes(prompt, 200) + "_"
		}
		if domain := extractDomain(urlStr); domain != "" {
			pattern = fmt.Sprintf("WebFetch(domain:%s)", domain)
		}
		return md, pattern

	case "WebSearch":
		query, _ := input["query"].(string)
		return "**WebSearch**\n查询: `" + query + "`", "WebSearch"
	}

	// Unknown tool — render raw input for transparency, suggest no
	// pattern (rather than guessing wrong).
	raw, _ := json.MarshalIndent(input, "", "  ")
	return "**输入**\n```json\n" + truncateForCardBytes(string(raw), 800) + "\n```", ""
}

// firstShellWord returns the first whitespace-delimited token of cmd —
// the canonical Bash rule pattern hinges on this. Empty input returns "".
//
// We deliberately don't try to parse quoted absolute paths (e.g.
// `"/Applications/IntelliJ IDEA.app/Contents/.../mvn" --version` would
// become `Bash("/Applications/IntelliJ:*)`, which is wrong) — for those
// the user can edit the pattern in the wizard's step 3 form before
// saving.
func firstShellWord(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	if idx := strings.IndexAny(cmd, " \t\n"); idx >= 0 {
		return cmd[:idx]
	}
	return cmd
}

// inferFileRulePattern picks a sensible default rule for a path-bearing
// tool. We use the directory glob (e.g. /a/b/c.txt → Read(/a/b/*)) so the
// rule covers neighbor files without being overly broad. Empty path
// returns "" so the [总是允许] button stays hidden.
//
// Users who want stricter (exact path) or broader (parent dir wildcard)
// rules can edit in the wizard's pattern form.
func inferFileRulePattern(toolName, filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	dir := filepath.Dir(filePath)
	if dir == "." || dir == "" {
		return fmt.Sprintf("%s(%s)", toolName, filePath)
	}
	return fmt.Sprintf("%s(%s/*)", toolName, dir)
}

// extractDomain returns the host portion of a URL, lowercased. Used for
// WebFetch rule inference where the canonical pattern is
// `WebFetch(domain:host)`. Returns "" on parse failure.
func extractDomain(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// truncateForCardBytes is a byte-budget cousin of truncateForCard. Used
// for embedding shell commands / file contents into card markdown —
// bytes are the right unit because the markdown body has a payload size
// limit and rune count understates the impact of CJK content. Cuts on a
// rune boundary so we never emit invalid UTF-8 mid-card.
func truncateForCardBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	out := s[:maxBytes]
	for !utf8.ValidString(out) && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out + fmt.Sprintf("\n…(已截断,共 %d 字节)", len(s))
}

// --- Card action handlers ---

// handleToolPermissionCardAction routes the three buttons (allow / deny
// / always-allow) to their respective behaviors. Returns true when claimed.
func (b *Bridge) handleToolPermissionCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action == nil {
		return false
	}
	switch m.Action.Name {
	case "tool_permission_allow":
		b.respondToolPermission(ctx, m, "allow")
	case "tool_permission_deny":
		b.respondToolPermission(ctx, m, "deny")
	case "tool_permission_always_allow":
		b.handleToolPermissionAlwaysAllow(ctx, m)
	default:
		return false
	}
	return true
}

// respondToolPermission sends behavior back to the CLI via
// session.RespondPermission, replaces the approval card with a short
// confirmation, and lets the turn continue.
func (b *Bridge) respondToolPermission(ctx context.Context, m channel.InboundMessage, behavior string) {
	pendingID := stringField(m.Action.Values, "pending_id")
	pending := b.popPendingElicitation(pendingID)
	if pending == nil {
		b.replyText(ctx, m, "该批准请求已过期或已处理")
		return
	}
	sess, ok := b.mgr.Get(pending.SessionID)
	if !ok {
		b.replyText(ctx, m, "session 已失效")
		return
	}
	if err := sess.RespondPermission(pending.RequestID, pending.ToolUseID, behavior, "", nil); err != nil {
		log.Printf("[bridge/tool-permission] respond %s failed: %v", behavior, err)
		b.replyText(ctx, m, "回复 CLI 失败: "+err.Error())
		return
	}
	tag := "✓ 已允许"
	tone := channel.ToneSuccess
	if behavior == "deny" {
		tag = "✗ 已拒绝"
		tone = channel.ToneNeutral
	}
	b.replyOrUpdateCard(ctx, m, channel.Card{
		Title:    "工具批准 · " + tag,
		Tone:     tone,
		Sections: []channel.Section{{Markdown: "本次请求已" + behaviorPastChinese(behavior) + ",会话继续。"}},
	})
}

// handleToolPermissionAlwaysAllow combines the "allow this one" action
// with a jump into the permissions wizard's source picker, behavior
// locked to allow and pattern pre-filled. The user then picks source
// (user/project/local) and confirms.
//
// We allow the in-flight tool_use FIRST (so the turn unblocks
// immediately) and only THEN open the wizard — if we waited, the user
// might cancel the wizard and leave the CLI hanging.
func (b *Bridge) handleToolPermissionAlwaysAllow(ctx context.Context, m channel.InboundMessage) {
	pendingID := stringField(m.Action.Values, "pending_id")
	pattern := stringField(m.Action.Values, "pattern")
	if pattern == "" {
		b.replyText(ctx, m, "无法推断规则模式 — 请手动用 /permissions allow <pattern> 添加")
		return
	}
	pending := b.popPendingElicitation(pendingID)
	if pending == nil {
		b.replyText(ctx, m, "该批准请求已过期或已处理")
		return
	}
	sess, ok := b.mgr.Get(pending.SessionID)
	if !ok {
		b.replyText(ctx, m, "session 已失效")
		return
	}
	if err := sess.RespondPermission(pending.RequestID, pending.ToolUseID, "allow", "", nil); err != nil {
		log.Printf("[bridge/tool-permission] always-allow respond failed: %v", err)
		b.replyText(ctx, m, "回复 CLI 失败: "+err.Error())
		return
	}
	// Open the wizard at step 2 with behavior=allow + pre-filled pattern.
	b.replyOrUpdateCard(ctx, m,
		b.buildPermissionsSourcePickerCard(permissionsfile.BehaviorAllow, pattern))
}

func behaviorPastChinese(b string) string {
	if b == "deny" {
		return "拒绝"
	}
	return "允许"
}
