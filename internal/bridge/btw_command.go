package bridge

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// cmdBTW handles `/btw <question>` — "by the way", a side question that
// references the current session's context but doesn't write into its
// transcript.
//
// Modeled after Claude Code's TUI /btw command. The TUI version runs
// in-process with the live conversation already loaded; we don't have that,
// so we point the shared admin AI session at the focused session's jsonl
// via jq, ask the question, and post the answer as a card. The user's
// main session stays untouched.
//
// Differences vs the TUI command:
//   - We can only see what's been persisted to jsonl — assistant text from
//     the very-latest in-flight turn may be missing.
//   - Admin has tool access; the prompt biases hard against any tool use
//     beyond the single jq read but cannot enforce it client-side.
//   - Output is a card in the chat, not a transient TUI overlay.
func (b *Bridge) cmdBTW(ctx context.Context, m channel.InboundMessage, args string) {
	question := strings.TrimSpace(args)
	if question == "" {
		b.replyText(ctx, m, "用法: `/btw <问题>` — 基于当前会话上下文问一个旁支问题(不写入主对话)")
		return
	}
	if b.admin == nil {
		b.replyText(ctx, m, "/btw 需要 admin AI session — 未配置 ADMIN_MODEL")
		return
	}

	sess, ok := b.btwTargetSession(m)
	if !ok {
		b.replyText(ctx, m, "/btw 需要一个已聚焦的会话 — 先用 /new 或 /switch 选一个")
		return
	}
	info := sess.Info()
	jsonlPath := claudeRT.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
	if jsonlPath == "" {
		b.replyText(ctx, m, "当前会话还没有持久化历史 — 先和 Claude 说几句再 /btw")
		return
	}

	_ = b.ch.Reaction(m.MessageID, "OnIt")

	prompt := buildBTWPrompt(jsonlPath, question)
	reply, err := b.admin.query(ctx, prompt)
	if err != nil {
		log.Printf("[bridge] /btw admin query failed: %v", err)
		b.replyText(ctx, m, "/btw 调用失败: "+err.Error())
		return
	}

	answer := cleanBTWAnswer(reply)
	if answer == "" {
		// Admin sometimes drops the anchor tag when confused; fall back to the
		// raw reply so the user at least sees what was produced rather than a
		// silent empty card.
		answer = strings.TrimSpace(reply)
		if answer == "" {
			answer = "_(admin 没有给出有效回答)_"
		}
	}

	displayID := displaySessionID(sess)
	_, _ = b.replyCard(ctx, m, channel.Card{
		Title: "🤔 BTW · " + displayID,
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: "**问题**\n" + question},
			{Divider: true, Markdown: "**回答**\n" + answer},
			{Note: "基于当前会话最近 25 条历史 · 不写入主对话 · 仅展示参考"},
		},
	})
}

// btwTargetSession picks the session this /btw should reference. Threads
// route to their bound session; otherwise we use the user's main-chat
// focused session. Returns ok=false when neither resolves.
func (b *Bridge) btwTargetSession(m channel.InboundMessage) (*session.Session, bool) {
	if m.ThreadID != "" {
		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok {
			if live, alive := b.mgr.Get(sess.ID); alive {
				return live, true
			}
		}
	}
	return b.mgr.FocusedSession(m.UserID)
}

// buildBTWPrompt assembles the prompt sent to the admin AI for a /btw
// question. The marker at the top is the canonical admin-session
// fingerprint (see summary_worker.AdminSessionMarker) — keeping it here
// means the admin's own jsonl gets correctly identified as gateway-internal
// and won't accidentally surface in /list.
//
// The jq filter is identical to the summary worker's. We hand it to the
// model pre-built so it doesn't waste rounds exploring the jsonl format.
func buildBTWPrompt(jsonlPath, question string) string {
	const jqFilter = `select((.type == "user" or .type == "assistant") and (.isMeta != true)) | "[" + (.message.role // .type) + "] " + (.message.content | if type == "string" then . elif type == "array" then map(select(.type == "text") | .text) | join(" ") else "" end)`
	return AdminSessionMarker + `
你在协助一个 claude-code 用户回答关于他当前 session 的旁支问题(/btw)。

【第 1 步:取上下文】 跑下面这一条命令拿到最近 25 条对话(其它探索一律不要,会浪费 token):

  jq -r '` + jqFilter + `' ` + jsonlPath + ` 2>/dev/null | grep -v '^\[[^]]*\] $' | tail -25

输出长这样:
  [user] 真实用户消息文本
  [assistant] 助手回复文本
  ...

【第 2 步:回答】 基于上下文回答用户的问题。

用户问题: ` + fmt.Sprintf("%q", question) + `

回答约束:
- **简洁** — 一两段足够,不要长篇大论
- **中文回答**
- 如果上下文里没有相关信息,**明说"上下文里没看到这部分"** —— 不要凭训练知识瞎猜
- 不要重复用户原话
- **绝对不要**执行任何修改类工具(Edit/Write/Bash 之外的写动作) —— 只允许 jq 读取那一条命令
- 不要 echo/cat 整个 jsonl —— 太长会爆 context

【输出格式】 思考过程在前,最终回答 MUST 用 <answer>...</answer> 包裹,整个回复**只出现 1 次**此标签。

【示例】
  ...分析过程...
  <answer>你之前提到的配置文件是 internal/bridge/config.go,第 37 行定义了 ADMIN_MODEL 字段。</answer>

文件: ` + jsonlPath
}

// btwAnswerPattern extracts the user-facing answer between <answer>…</answer>.
// We DELIBERATELY match the FIRST occurrence — the prompt contract says
// admin must emit the tag exactly once; if more leak through (rare model
// glitch), the first one is the canonical answer.
var btwAnswerPattern = regexp.MustCompile(`(?s)<answer>(.*?)</answer>`)

// cleanBTWAnswer pulls the answer string out of admin's raw reply. Returns
// an empty string when the anchor tag is missing — the caller treats that
// as a soft failure and falls back to displaying the raw reply.
func cleanBTWAnswer(s string) string {
	m := btwAnswerPattern.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	v := strings.TrimSpace(m[1])
	v = strings.Trim(v, "\"'`")
	return strings.TrimSpace(v)
}
