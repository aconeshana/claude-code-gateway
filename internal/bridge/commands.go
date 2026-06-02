package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// registerCommands populates b.commands with all built-in slash commands.
// To add a new command: append a Command literal here and implement the handler.
func (b *Bridge) registerCommands() {
	b.commands = []Command{
		{
			Name:    "/new",
			Usage:   "/new",
			Desc:    "创建新 session(无 focus 时弹项目选择;有 focus 时在当前项目下新建)",
			Handler: b.cmdNew,
		},
		{
			Name:    "/list",
			Aliases: []string{"/sessions"},
			Usage:   "/list",
			Desc:    "查看所有 session(活跃 + 归档,带切换/归档按钮)",
			Handler: b.wrapNoArgs(b.cmdList),
		},
		{
			Name:    "/switch",
			Usage:   "/switch [prefix]",
			Desc:    "切换到 active session(无参显示菜单)。idle/归档请用 /resume",
			Handler: b.cmdSwitch,
		},
		{
			Name:    "/archive",
			Aliases: []string{"/destroy"},
			Usage:   "/archive [prefix]",
			Desc:    "归档 session(默认归档 active;归档后不会自动加载)",
			Handler: b.cmdArchive,
		},
		{
			Name:    "/resume",
			Usage:   "/resume [prefix]",
			Desc:    "恢复 session(idle/归档/external 都行,跟 claude --resume 一致)",
			Handler: b.cmdResume,
		},
		{
			Name:    "/branch",
			Aliases: []string{"/fork"},
			Usage:   "/branch [名字]",
			Desc:    "在当前对话历史上创建分支 session,可选名字",
			Handler: b.cmdBranch,
		},
		{
			Name:    "/model",
			Usage:   "/model [name]",
			Desc:    "查看或切换模型(haiku, sonnet, opus)",
			Handler: b.cmdModel,
		},
		{
			Name:    "/diff",
			Usage:   "/diff",
			Desc:    "查看工作目录未提交 git 变更",
			Handler: b.wrapNoArgs(b.cmdDiff),
		},
		{
			Name:    "/config",
			Usage:   "/config [set <KEY> <VAL>]",
			Desc:    "查看/修改配置(/config set <KEY> <VAL> 直接改)",
			Handler: b.cmdConfig,
		},
		{
			Name:    "/rename",
			Usage:   "/rename <新名字>",
			Desc:    "重命名当前 session(更新网关显示标题并透传给 Claude CLI)",
			Handler: b.cmdRename,
		},
		{
			Name:    "/plan",
			Usage:   "/plan [description]",
			Desc:    "进入 plan 模式(透传给 Claude CLI)",
			Handler: b.forwardToCLI,
		},
		{
			Name:    "/plan-list",
			Usage:   "/plan-list",
			Desc:    "浏览 ~/.claude/plans 下已有的 plan 文件(按最近修改)",
			Handler: b.wrapNoArgs(b.cmdPlanList),
		},
		{
			Name:    "/project",
			Aliases: []string{"/projects"},
			Usage:   "/project",
			Desc:    "查看/添加项目(working directory),从中选 session",
			Handler: b.wrapNoArgs(b.cmdProject),
		},
		{
			Name:    "/stop",
			Usage:   "/stop [prefix]",
			Desc:    "打断当前会话正在执行的任务(等价 CLI 里的 ESC)",
			Handler: b.cmdStop,
		},
		{
			Name:    "/terminate",
			Usage:   "/terminate [prefix]",
			Desc:    "停止 CLI 子进程,会话变为 idle(再发消息会自动恢复)",
			Handler: b.cmdTerminate,
		},
		{
			Name:    "/status",
			Usage:   "/status",
			Desc:    "查看 gateway 状态(发现进度、摘要 worker、活跃 session)",
			Handler: b.wrapNoArgs(b.cmdStatus),
		},
		{
			Name:    "/skills",
			Usage:   "/skills",
			Desc:    "列出可用 skills(项目 + 全局 .claude/skills/)",
			Handler: b.wrapNoArgs(b.cmdSkills),
		},
		{
			Name:    "/help",
			Usage:   "/help",
			Desc:    "显示此帮助",
			Handler: b.wrapNoArgs(b.cmdHelp),
		},
		{
			Name:    "/cron",
			Usage:   "/cron [list|add|remove|enable|disable|history]",
			Desc:    "管理定时任务(无参数显示管理卡片)",
			Handler: b.cmdCron,
		},
	}
}

// wrapNoArgs adapts a no-args handler to the CommandHandler signature.
func (b *Bridge) wrapNoArgs(fn func(ctx context.Context, m channel.InboundMessage)) CommandHandler {
	return func(ctx context.Context, m channel.InboundMessage, _ string) {
		fn(ctx, m)
	}
}

// sessionPayloadID returns the stable identifier used in card-action
// payloads. We prefer the CLI session id because mgr.Reactivate replaces
// the gateway-internal session.ID — a card rendered before a reactivate
// would otherwise carry a stale id that mgr.Get can't find.
func sessionPayloadID(info session.SessionInfo) string {
	if info.CLISessionID != "" {
		return info.CLISessionID
	}
	return info.ID
}

// --- /new ---

func (b *Bridge) cmdNew(ctx context.Context, m channel.InboundMessage, args string) {
	if m.ThreadID != "" {
		b.replyText(ctx, m, "请回主聊天创建新 session(/new 在话题里没有意义)")
		return
	}
	// V2 /new: no label, no dir arg. The working dir is implicit —
	//   - if a focus exists, the new session lives in the focused project
	//   - if not, we route to /project so the user picks a project first
	if strings.TrimSpace(args) != "" {
		b.replyText(ctx, m, "/new 不再接受参数;在 /project 里挑项目,或在已有 focus 时直接 /new")
		return
	}

	priorFocus := b.snapshotFocus(m.UserID)

	if priorFocus == nil {
		// No focus → user needs to pick a project first.
		card := b.buildProjectsCard(m.UserID)
		b.replyCard(ctx, m, card)
		return
	}

	// Focus exists → inherit its working dir; new session becomes a parallel
	// session in the same project, opened in a thread (afterCreateOrActivate
	// will handle the focus/thread split).
	workingDir := priorFocus.WorkingDir
	if workingDir == "" {
		workingDir = b.defaultCWD
	}

	sess, err := b.mgr.Create(ctx, session.CreateOpts{
		WorkingDir:  workingDir,
		OwnerID:     m.UserID,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
		Origin:      channelKindToOrigin(m.ChannelKind),
	})
	if err != nil {
		b.replyText(ctx, m, "创建 session 失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, sess, m)

	display := projectName(workingDir)
	sid := displaySessionID(sess)
	body := fmt.Sprintf("%s · %s · 已创建 · 进入话题发送消息", display, sid)
	msgID, err := b.replyCard(ctx, m, channel.Card{
		Title:    "📂 " + display,
		Tone:     channel.ToneSuccess,
		Sections: []channel.Section{{Markdown: body}},
	})
	if err != nil {
		log.Printf("[bridge] cmdNew: response card send failed: %v", err)
		return
	}
	welcome := fmt.Sprintf("👋 话题 [`%s`] · %s 已创建\n\n在当前对话框继续沟通", sid, display)
	b.afterCreateOrActivate(ctx, sess, m.UserID, msgID, welcome, priorFocus, false)
}

// --- /list (alias for /project — single canonical project picker) ---
//
// /list and /project both render the same merged Projects card via
// buildProjectsCard. The card includes:
//   - one section per project (with focus marker + session counts)
//   - per-project [进入][新建会话] buttons
//   - footer [➕ 添加项目][归档对话 (N)] entries
//
// Card-action drill-in flows (show_project / back_to_list / show_projects)
// all route back to this same card so the user stays inside one Reply chain.

func (b *Bridge) cmdList(ctx context.Context, m channel.InboundMessage) {
	b.cmdProject(ctx, m)
}

// replyWithProjectsCard re-renders the merged projects card via Reply (edits
// the original card in place) — used by "← 返回" buttons in drill-in views.
func (b *Bridge) replyWithProjectsCard(ctx context.Context, m channel.InboundMessage) {
	card := b.buildProjectsCard(m.UserID)
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// buildArchivedSections renders archived sessions with resume/delete buttons.
// Used by both the global archive view and per-project drill-down.
func buildArchivedSections(archived []session.SessionInfo) []channel.Section {
	return buildArchivedSectionsWithDir(archived, "")
}

// buildSessionListSections renders the per-session card for one project
// (drill-down view) or for an arbitrary filtered slice. Two-line layout:
//
//	id: <short> · <status> · [tag] · <relative time>
//	[<custom-title>] <summary>
//
// sortSessionsForList orders sessions for the user-facing list view:
//   - active sessions first (running, immediately switchable)
//   - then idle (resumable but cold)
//   - everything else (external/archived) last
//   - within each tier: most recently active first
//
// Returns a new slice; the input is not mutated.
func sortSessionsForList(sessions []session.SessionInfo) []session.SessionInfo {
	out := make([]session.SessionInfo, len(sessions))
	copy(out, sessions)
	rank := func(s session.SessionInfo) int {
		switch s.Status {
		case string(session.StatusActive):
			return 0
		case string(session.StatusIdle):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		ti := parseRFC3339(out[i].LastActivity)
		tj := parseRFC3339(out[j].LastActivity)
		return ti.After(tj)
	})
	return out
}

// Buttons depend on status; tags indicate provenance.
func buildSessionListSections(sessions []session.SessionInfo, focusedID string) []channel.Section {
	sorted := sortSessionsForList(sessions)
	sections := make([]channel.Section, 0, len(sorted))
	for _, info := range sorted {
		isActive := info.Status == string(session.StatusActive)

		header := renderSessionHeader(info, focusedID)
		body := renderSessionTitle(info)
		md := header + "\n" + body

		btns := []channel.Button{}
		switch {
		case info.ID == focusedID:
			// Already focused — no primary action.
		case isActive:
			btns = append(btns, channel.Button{
				Label: "切换", Style: "primary",
				Action: map[string]string{"action": "switch_session", "session_id": sessionPayloadID(info)},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": sessionPayloadID(info)},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": sessionPayloadID(info)},
		})
		btns = append(btns, channel.Button{
			Label: "刷新摘要", Style: "default",
			Action: map[string]string{"action": "refresh_summary", "session_id": sessionPayloadID(info)},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns, ButtonLayout: "fill"})
	}
	return sections
}

// buildArchivedSectionsWithDir is like buildArchivedSections but includes
// working_dir in button payloads so the handler can navigate back to the
// project view after resume/delete.
func buildArchivedSectionsWithDir(archived []session.SessionInfo, dir string) []channel.Section {
	sections := make([]channel.Section, 0, len(archived))
	for _, info := range archived {
		header := renderSessionHeader(info, "")
		body := renderSessionTitle(info)
		md := header + "\n" + body
		sections = append(sections, channel.Section{
			Markdown:     md,
			ButtonLayout: "fill",
			Buttons: []channel.Button{
				{Label: "恢复", Style: "primary", Action: map[string]string{"action": "resume_archived", "session_id": sessionPayloadID(info), "working_dir": dir}},
				{Label: "删除", Style: "danger", Action: map[string]string{"action": "remove_archived", "session_id": sessionPayloadID(info), "working_dir": dir}},
			},
		})
	}
	return sections
}

// buildSessionListSectionsWithDir is like buildSessionListSections but
// includes working_dir in button payloads for project-scoped navigation.
func buildSessionListSectionsWithDir(sessions []session.SessionInfo, focusedID string, dir string) []channel.Section {
	sorted := sortSessionsForList(sessions)
	sections := make([]channel.Section, 0, len(sorted))
	for _, info := range sorted {
		isActive := info.Status == string(session.StatusActive)

		header := renderSessionHeader(info, focusedID)
		body := renderSessionTitle(info)
		md := header + "\n" + body

		btns := []channel.Button{}
		switch {
		case info.ID == focusedID:
			// Already focused — no primary action.
		case isActive:
			btns = append(btns, channel.Button{
				Label: "切换", Style: "primary",
				Action: map[string]string{"action": "switch_session", "session_id": sessionPayloadID(info), "working_dir": dir},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": sessionPayloadID(info), "working_dir": dir},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		btns = append(btns, channel.Button{
			Label: "刷新摘要", Style: "default",
			Action: map[string]string{"action": "refresh_summary", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		btns = append(btns, channel.Button{
			Label: "重命名", Style: "default",
			Action: map[string]string{"action": "rename_session", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns, ButtonLayout: "fill"})
	}
	return sections
}

// focusedID may be empty (e.g. archive view); when set, the focused session
// gets a ★ marker.
func renderSessionHeader(info session.SessionInfo, focusedID string) string {
	idStr := shortID(info.CLISessionID)
	if idStr == "" {
		idStr = "gw:" + shortID(info.ID)
	}
	parts := []string{fmt.Sprintf("`%s`", idStr)}

	status := info.Status
	if status == "" {
		status = info.State
	}
	if info.ID == focusedID {
		status += " ★"
	}
	parts = append(parts, status)

	switch info.Origin {
	case session.OriginFeishu:
		parts = append(parts, "`[💬feishu]`")
	case session.OriginDingTalk:
		parts = append(parts, "`[💬dingtalk]`")
	}

	if t := parseRFC3339(info.LastActivity); !t.IsZero() {
		parts = append(parts, humanAgo(time.Since(t)))
	}

	if info.MessageCount > 0 {
		parts = append(parts, fmt.Sprintf("%d 条", info.MessageCount))
	}

	return strings.Join(parts, " · ")
}

// renderSessionTitle renders the second line: `[custom] summary` or just
// `summary` (or short id when both are empty).
func renderSessionTitle(info session.SessionInfo) string {
	switch {
	case info.CustomTitle != "" && info.Summary != "":
		return fmt.Sprintf("**[%s]** %s", info.CustomTitle, info.Summary)
	case info.CustomTitle != "":
		return fmt.Sprintf("**[%s]**", info.CustomTitle)
	case info.Summary != "":
		return info.Summary
	default:
		// Worker classified this session as too short / meta-like and
		// didn't generate a summary. Fall back to the latest user message
		// so /list still gives the user enough signal to pick the right
		// session — multiple short parallel sessions are otherwise
		// indistinguishable.
		if info.LatestUserMessage != "" {
			return "_(短对话)_ " + info.LatestUserMessage
		}
		return "_(短对话,无摘要)_"
	}
}

// buildResumeWelcome constructs the first message sent into a thread when a
// session is resumed. Includes the session summary so the user knows what
// was being worked on without opening the full history.
func buildResumeWelcome(info session.SessionInfo, display string) string {
	sid := shortID(info.CLISessionID)
	if sid == "" {
		sid = shortID(info.ID)
	}
	footer := fmt.Sprintf("👋 话题 [`%s`] · %s 已恢复\n\n在当前对话框继续沟通", sid, display)
	summary := renderSessionTitle(info)
	if summary != "" && summary != "_(短对话,无摘要)_" {
		return "📝 " + summary + "\n\n" + footer
	}
	return footer
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// --- /switch (active-only) ---
//
// Strictly switches focus to an active session. Idle/external/archived
// sessions are rejected with a hint to use /resume — matches the verb's
// "this is alive, switch to it" mental model.
//
// `/switch <prefix>` switches directly (no menu); `/switch` with no args
// renders a compact list of active sessions only, so the user can pick
// without wading through the two-level /list project menu.

func (b *Bridge) cmdSwitch(ctx context.Context, m channel.InboundMessage, args string) {
	if m.ThreadID != "" {
		b.replyText(ctx, m, "请回主聊天 /switch(切换主聊天 focus 在话题里没有意义)")
		return
	}
	prefix := strings.TrimSpace(args)
	if prefix == "" {
		b.renderActiveSessionsCard(ctx, m)
		return
	}

	sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}
	// V2: /switch on an idle target auto-resumes — same outcome as the
	// [恢复] button. Avoids the previous "请用 /resume" round-trip.
	if sess.Info().Status != string(session.StatusActive) {
		b.resumeSessionFlow(ctx, m, sess, "")
		return
	}
	b.switchFocusTo(ctx, m, sess)
}

// resumeSessionFlow runs the synchronous V2 resume sequence: ClaimExternal
// if needed, snapshot prior focus, Reactivate, then sendResumedCardAndOpenThread.
// Shared between cmdSwitch (idle fallback) and the [恢复] card button (the
// latter wraps this in a goroutine + placeholder card for Lark's 3s callback
// timeout — see resume_session action handler).
//
// editMsgID, when non-empty, makes the "Session Resumed" card replace that
// message in place (used by the [恢复] card action to update its placeholder
// instead of posting a 2nd card).
func (b *Bridge) resumeSessionFlow(ctx context.Context, m channel.InboundMessage, sess *session.Session, editMsgID string) {
	info := sess.Info()
	if info.Origin == session.OriginExternal && info.OwnerID == "" {
		if err := b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind); err != nil {
			b.replyOrText(ctx, m, "纳管失败: "+err.Error())
			return
		}
	}
	priorFocus := b.snapshotFocus(m.UserID)
	newSess, err := b.mgr.Reactivate(ctx, sess.ID)
	if err != nil {
		b.replyOrText(ctx, m, "恢复失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, newSess, m)
	b.sendResumedCardAndOpenThread(ctx, m, newSess, priorFocus, editMsgID)
}

// switchFocusTo applies the V2 /switch flow to a pre-resolved session: stow
// the previous focus into a thread (if it had none yet) and promote target
// to main-chat focus. Shared between cmdSwitch (prefix-based) and the
// switch_session card-action button so both routes behave identically.
func (b *Bridge) switchFocusTo(ctx context.Context, m channel.InboundMessage, sess *session.Session) {
	if sess.Info().Status != string(session.StatusActive) {
		b.replyText(ctx, m, fmt.Sprintf(
			"session %s 当前是 %s 状态,不能 /switch。请用 /resume 恢复",
			displaySessionID(sess), sess.Info().Status))
		return
	}
	priorFocus := b.snapshotFocus(m.UserID)
	if priorFocus != nil && priorFocus.ID == sess.ID {
		b.replyText(ctx, m, "已经在 session "+displaySessionID(sess)+" 了")
		return
	}

	// Stow the previous focus into a thread (if it doesn't have one yet) so
	// it remains reachable via the 话题入口卡 once focus moves to sess. Use
	// sendCard (new message in main chat) instead of replyCard so we keep
	// m.Reply available for re-rendering the /switch card at the end.
	if priorFocus != nil {
		pInfo := priorFocus.Info()
		if pInfo.ThreadID == "" {
			// First time stowing — open a new thread anchored at a fresh card.
			stowSID := displaySessionID(priorFocus)
			stowBody := fmt.Sprintf("**%s** · %s\n已收纳到话题。点下方话题入口继续聊。", stowSID, priorFocus.Label)
			anchorMsgID, err := b.sendCard(ctx, m.ChatID, channel.Card{
				Title:    "📦 " + projectName(priorFocus.WorkingDir),
				Tone:     channel.ToneInfo,
				Sections: []channel.Section{{Markdown: stowBody}},
			})
			if err != nil {
				log.Printf("[bridge] switchFocusTo: stow card send failed: %v", err)
			} else {
				welcome := fmt.Sprintf("📌 话题 [`%s`] · %s 已收纳\n\n在当前对话框继续沟通", stowSID, priorFocus.Label)
				if err := b.openThreadForSession(ctx, priorFocus, anchorMsgID, welcome); err != nil {
					log.Printf("[bridge] switchFocusTo: stow thread open failed: %v", err)
				} else {
					info := priorFocus.Info()
					if info.ThreadID != "" && info.RootMessageID != "" {
						priorFocus.SetLastInbound(info.ChatID, info.ThreadID, info.RootMessageID)
					}
				}
			}
		} else if pInfo.RootMessageID != "" {
			// Already in a thread — redirect output back into it (so anything
			// the user just typed in main chat that's still in-flight goes
			// to the thread on the next bot reply) and ping the thread so
			// the main-chat 话题入口卡 surfaces a "new reply" indicator,
			// letting the user know that session is alive somewhere else.
			priorFocus.SetLastInbound(pInfo.ChatID, pInfo.ThreadID, pInfo.RootMessageID)
			pingText := fmt.Sprintf("📌 主聊天 focus 已切走,在这里继续 [`%s`] · %s", displaySessionID(priorFocus), priorFocus.Label)
			_, _ = b.ch.SendMessage(ctx, channel.OutboundMessage{
				ChatID:           pInfo.ChatID,
				Text:             pingText,
				ReplyToMessageID: pInfo.RootMessageID,
			})
		}
	}

	if err := b.mgr.SetFocus(m.UserID, sess.ID); err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}
	b.saveStateIfPossible()

	// Re-render the /switch card in place so the user sees the new focus
	// star, the [切换] button on what was the old focus, and the updated
	// "当前: <sid>" header — instead of just a stale card with a text
	// confirmation underneath.
	newCard, empty := b.buildActiveSessionsCard(m.UserID)
	if empty {
		b.replyOrText(ctx, m, "已切换到 "+displaySessionID(sess))
		return
	}
	if m.Reply != nil {
		m.Reply(newCard)
		return
	}
	b.replyCard(ctx, m, newCard)
}

// renderActiveSessionsCard shows only Active sessions in a flat list — what
// /switch needs. Idle/archived are deliberately omitted (use /list for the
// full project-grouped view).
func (b *Bridge) renderActiveSessionsCard(ctx context.Context, m channel.InboundMessage) {
	card, empty := b.buildActiveSessionsCard(m.UserID)
	if empty {
		b.replyText(ctx, m, "当前没有 active session。用 /list 查看所有 session,或 /resume <id> 唤醒 idle session")
		return
	}
	b.replyCard(ctx, m, card)
}

// replyWithActiveSessionsCard re-renders the active-sessions card via the
// inbound's Reply hook so the original card is edited in place. Falls back
// to sending a fresh card when Reply isn't available (e.g. text command
// path) or there are no active sessions.
func (b *Bridge) replyWithActiveSessionsCard(ctx context.Context, m channel.InboundMessage) {
	card, empty := b.buildActiveSessionsCard(m.UserID)
	if empty {
		b.replyOrText(ctx, m, "已切换,但当前没有 active session 可显示")
		return
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// replyOrText prefers the inbound's Reply hook (edits the originating card
// in place — keeps the chat compact) and falls back to a new text message.
func (b *Bridge) replyOrText(ctx context.Context, m channel.InboundMessage, text string) {
	if m.Reply != nil {
		m.Reply(channel.Card{
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: text}},
		})
		return
	}
	b.replyText(ctx, m, text)
}

// buildActiveSessionsCard renders the /switch card content. Returns
// (zeroCard, true) when the user has no active sessions so the caller can
// decide whether to send an empty-state text or skip silently.
func (b *Bridge) buildActiveSessionsCard(userID string) (channel.Card, bool) {
	owned := b.filterAliveSessions(b.mgr.ListActiveByOwner(userID))
	var actives []session.SessionInfo
	for _, s := range owned {
		if s.Status == string(session.StatusActive) {
			actives = append(actives, s)
		}
	}
	if len(actives) == 0 {
		return channel.Card{}, true
	}
	focusedID := ""
	if f, ok := b.mgr.FocusedSession(userID); ok {
		focusedID = f.ID
	}
	actives = sortSessionsForList(actives)
	header := fmt.Sprintf("**%d 个 active session** · 点击切换,或发送 `/switch <id前缀>`", len(actives))
	if len(actives) == 1 && actives[0].ID == focusedID {
		header = "**当前唯一 active session**"
	}
	sections := []channel.Section{
		{Markdown: header},
	}
	for _, info := range actives {
		hdr := renderSessionHeader(info, focusedID)
		if proj := projectName(info.WorkingDir); proj != "" {
			hdr = fmt.Sprintf("`[%s]` · ", proj) + hdr
		}
		title := renderSessionTitle(info)
		md := hdr + "\n" + title
		sec := channel.Section{Divider: true, Markdown: md}
		if info.ID != focusedID {
			sec.Buttons = []channel.Button{{
				Label:  "切换",
				Style:  "primary",
				Action: map[string]string{"action": "switch_session", "session_id": sessionPayloadID(info)},
			}}
		}
		sections = append(sections, sec)
	}
	return channel.Card{
		Title:    "Switch Session · 当前: " + focusedDisplay(b.mgr, focusedID),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}, false
}

// focusedDisplay renders the short id of the focused session for the
// /switch card title — gives the user an unambiguous "you're here now"
// signal at the top of the card (the ★ marker can scroll out of view).
func focusedDisplay(mgr *session.Manager, focusedID string) string {
	if focusedID == "" {
		return "无"
	}
	if sess, ok := mgr.Get(focusedID); ok {
		return displaySessionID(sess)
	}
	return "gw:" + shortID(focusedID)
}

// --- /archive ---

func (b *Bridge) cmdArchive(ctx context.Context, m channel.InboundMessage, args string) {
	prefix := strings.TrimSpace(args)
	var sessionID string
	if prefix == "" {
		focused, ok := b.currentSession(m)
		if !ok {
			b.replyText(ctx, m, "没有 active session")
			return
		}
		sessionID = focused.ID
	} else {
		sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
		if err != nil {
			b.replyText(ctx, m, err.Error())
			return
		}
		sessionID = sess.ID
	}
	if err := b.mgr.Archive(sessionID); err != nil {
		b.replyText(ctx, m, "归档失败: "+err.Error())
		return
	}
	b.replyText(ctx, m, "Session "+b.displayIDFromGatewayID(sessionID)+" 已归档")
}

// --- /stop ---
//
// Aborts the current turn for an active session — sends an "interrupt"
// control message to the CLI, equivalent to pressing ESC in the standalone
// claude CLI. The session itself stays active and ready for the next
// message; only the in-flight assistant response is canceled.

func (b *Bridge) cmdStop(ctx context.Context, m channel.InboundMessage, args string) {
	sess, ok := b.resolveActiveTarget(ctx, m, args, "/stop")
	if !ok {
		return
	}
	// Don't gate on session.State — our state lags the CLI: we mark Ready
	// the moment a `result` event arrives, but the CLI may already be back
	// in a follow-up tool round-trip. Just forward the interrupt; the CLI
	// is the authority on whether there's a turn to cancel and silently
	// no-ops if not.
	if err := sess.SendControl(json.RawMessage(`{"subtype":"interrupt"}`)); err != nil {
		b.replyText(ctx, m, "打断失败: "+err.Error())
		return
	}
	b.replyText(ctx, m, "已发送中断信号到 session "+displaySessionID(sess))
}

// --- /terminate ---
//
// Stops the CLI subprocess for a session, dropping the session into idle.
// Useful for freeing CLI slots without losing the conversation — the next
// message in this session will auto-reactivate it via --resume.

func (b *Bridge) cmdTerminate(ctx context.Context, m channel.InboundMessage, args string) {
	sess, ok := b.resolveActiveTarget(ctx, m, args, "/terminate")
	if !ok {
		return
	}
	display := displaySessionID(sess)
	// Mark "user-initiated termination" so the bridge's CLI-exit handler
	// (renderer.go::handleCLIExit) doesn't fire its own "session idle"
	// notice — that one is for unexpected exits and contradicts the
	// terminate reply.
	b.mu.Lock()
	if b.terminating == nil {
		b.terminating = make(map[string]bool)
	}
	b.terminating[sess.ID] = true
	b.mu.Unlock()

	if err := b.mgr.Terminate(sess.ID); err != nil {
		b.replyText(ctx, m, "终止失败: "+err.Error())
		return
	}
	// After termination there may be another active session worth pointing
	// the user at. SetFocus to the next-most-recent active so /switch with
	// no args works and the user isn't left in "no focused session" limbo.
	var nextFocus string
	for _, info := range sortSessionsForList(b.mgr.ListActiveByOwner(m.UserID)) {
		if info.ID != sess.ID && info.Status == string(session.StatusActive) {
			nextFocus = info.ID
			break
		}
	}
	tail := ""
	if nextFocus != "" {
		_ = b.mgr.SetFocus(m.UserID, nextFocus)
		if next, ok := b.mgr.Get(nextFocus); ok {
			tail = ",已切换到 " + displaySessionID(next)
		}
	}
	b.replyText(ctx, m, fmt.Sprintf(
		"Session %s 已终止(用 /resume %s 可恢复)%s", display, display, tail))
}

// resolveActiveTarget picks an active session — focused if no prefix, or by
// prefix lookup. Used by /stop and /terminate which only act on active
// (process running) sessions. Returns false (and sends a user-visible
// error) when no suitable target is found.
func (b *Bridge) resolveActiveTarget(ctx context.Context, m channel.InboundMessage, args, cmd string) (*session.Session, bool) {
	prefix := strings.TrimSpace(args)
	if prefix == "" {
		focused, ok := b.currentSession(m)
		if !ok {
			b.replyText(ctx, m, "没有 focused session。用 "+cmd+" <id前缀> 指定")
			return nil, false
		}
		if focused.Info().Status != string(session.StatusActive) {
			b.replyText(ctx, m, fmt.Sprintf(
				"Focused session %s 不是 active 状态,无法 %s",
				displaySessionID(focused), cmd))
			return nil, false
		}
		return focused, true
	}
	sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return nil, false
	}
	if sess.Info().Status != string(session.StatusActive) {
		b.replyText(ctx, m, fmt.Sprintf(
			"Session %s 当前是 %s 状态,不是 active",
			displaySessionID(sess), sess.Info().Status))
		return nil, false
	}
	return sess, true
}

// --- /resume (claim + reactivate) ---
//
// Behaves like `claude --resume`: bring any non-active session back to life.
// Supports idle (own), archived (own), and external (terminal-created, when
// share-external is enabled and matched by prefix).
//
// External sessions are claimed for the user (OwnerID set, Origin → "feishu")
// before reactivation so they appear in the user's normal list afterward.

func (b *Bridge) cmdResume(ctx context.Context, m channel.InboundMessage, args string) {
	prefix := strings.TrimSpace(args)
	if prefix == "" {
		b.cmdList(ctx, m)
		return
	}

	// Find: own (active+idle) → own archived → external (when shared)
	sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
	if err != nil {
		if archived, archErr := b.mgr.FindArchivedByPrefix(m.UserID, prefix); archErr == nil {
			sess = archived
			err = nil
		}
	}
	if err != nil && b.shareExternalEnabled() {
		if ext := b.findExternalByPrefix(prefix); ext != nil {
			sess = ext
			err = nil
		}
	}
	if err != nil && b.admin != nil {
		if matched, aiErr := b.matchSessionByQuery(ctx, m.UserID, prefix); aiErr == nil {
			if mSess, ok := b.mgr.Get(matched); ok {
				sess = mSess
				err = nil
			}
		}
	}
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}

	info := sess.Info()
	if info.Status == string(session.StatusActive) {
		b.replyText(ctx, m, fmt.Sprintf(
			"session %s 已经是 active,无需 /resume(用 /switch)", displaySessionID(sess)))
		return
	}

	// Claim external before reactivate so the new session starts as owned.
	if info.Origin == session.OriginExternal && info.OwnerID == "" {
		if err := b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind); err != nil {
			b.replyText(ctx, m, "纳管失败: "+err.Error())
			return
		}
	}

	// Capture prior focus BEFORE Reactivate (which SetFocus's the new session).
	priorFocus := b.snapshotFocus(m.UserID)

	newSess, err := b.mgr.Reactivate(ctx, sess.ID)
	if err != nil {
		b.replyText(ctx, m, "恢复失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, newSess, m)
	b.sendResumedCardAndOpenThread(ctx, m, newSess, priorFocus, "")
}

// sendResumedCardAndOpenThread emits the "Session Resumed" card to the main
// chat and runs the V2 afterCreateOrActivate rule (keep focus on
// priorFocus + open a thread for the resumed session, or set focus when
// priorFocus is nil). Shared between cmdResume and the resume_session
// card-action button.
//
// editMsgID, when non-empty, identifies a card message to UpdateMessage in
// place (e.g. the "正在恢复…" placeholder dropped by the [恢复] card action).
// When empty, a fresh card is posted via replyCard. Either way the resulting
// message id is used as the thread anchor for afterCreateOrActivate.
func (b *Bridge) sendResumedCardAndOpenThread(ctx context.Context, m channel.InboundMessage, newSess *session.Session, priorFocus *session.Session, editMsgID string) {
	info := newSess.Info()
	display := newSess.Label
	if display == "" {
		display = projectName(newSess.WorkingDir)
	}
	hasThread := info.ThreadID != ""

	// V2 routing principle: reuse existing thread > use main chat. When sess
	// has a thread, the full detail (header/summary/welcome) goes INTO that
	// thread; the main-chat card shrinks to a short ack so the user knows
	// the click registered. When sess has no thread, the main-chat card is
	// the only surface and gets the full detail.
	var resumedCard channel.Card
	title := "▶ " + display + " · 已恢复"
	if hasThread {
		resumedCard = channel.Card{
			Title:    title,
			Tone:     channel.ToneSuccess,
			Sections: []channel.Section{{Markdown: "_详情已发往话题_"}},
		}
	} else {
		body := renderSessionHeader(info, "") + "\n" + renderSessionTitle(info)
		if priorFocus != nil {
			body += "\n_进入话题发送消息_"
		}
		resumedCard = channel.Card{
			Title:    title,
			Tone:     channel.ToneSuccess,
			Sections: []channel.Section{{Markdown: body}},
		}
	}

	var msgID string
	if editMsgID != "" {
		// In-place update: keeps everything on one message (no extra card).
		if err := b.ch.UpdateMessage(ctx, editMsgID, channel.OutboundMessage{Card: &resumedCard}); err != nil {
			log.Printf("[bridge] sendResumedCardAndOpenThread: UpdateMessage %s failed: %v — falling back to new card", shortID(editMsgID), err)
			editMsgID = ""
		} else {
			msgID = editMsgID
		}
	}
	if editMsgID == "" {
		newMsgID, cardErr := b.replyCard(ctx, m, resumedCard)
		if cardErr != nil {
			log.Printf("[bridge] sendResumedCardAndOpenThread: response card send failed: %v", cardErr)
			return
		}
		msgID = newMsgID
	}
	welcome := buildResumeWelcome(newSess.Info(), display)
	b.afterCreateOrActivate(ctx, newSess, m.UserID, msgID, welcome, priorFocus, false)
}

// findExternalByPrefix searches all external (unowned) sessions for one whose
// CLISessionID or working_dir basename starts with prefix. Used by /resume
// when share-external is enabled.
func (b *Bridge) findExternalByPrefix(prefix string) *session.Session {
	lower := strings.ToLower(prefix)
	infos := b.mgr.ListBy(session.Filter{Origins: []string{session.OriginExternal}})
	for _, info := range infos {
		if info.OwnerID != "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(info.CLISessionID), lower) ||
			strings.HasPrefix(strings.ToLower(projectName(info.WorkingDir)), lower) {
			if sess, ok := b.mgr.Get(info.ID); ok {
				return sess
			}
		}
	}
	return nil
}

// --- /help ---

func (b *Bridge) cmdHelp(ctx context.Context, m channel.InboundMessage) {
	var sb strings.Builder
	sb.WriteString("**命令列表:**\n")
	for _, cmd := range b.commands {
		sb.WriteString(fmt.Sprintf("`%s` — %s\n", cmd.Usage, cmd.Desc))
	}
	sb.WriteString("\n**其他 `/xxx` 命令** 透传给 Claude CLI(如 /commit, /compact, /review)。\n")
	sb.WriteString("**`!<cmd>`** — 在工作目录执行 shell 命令(30s 超时)\n")
	sb.WriteString("**普通消息** 自动发送到 active session;无 session 时自动创建或从 idle/归档恢复。")
	b.replyCard(ctx, m, channel.Card{
		Title:    "Claude Code Gateway Help",
		Tone:     channel.ToneInfo,
		Sections: []channel.Section{{Markdown: sb.String()}},
	})
}

// --- Card action handler ---

func (b *Bridge) handleCardAction(ctx context.Context, m channel.InboundMessage) {
	if m.Action == nil {
		return
	}
	// Delegate cron-specific actions first.
	if b.handleCronCardAction(ctx, m) {
		return
	}
	switch m.Action.Name {
	case "switch_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return
		}
		b.switchFocusTo(ctx, m, sess)
	case "archive_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		// External sessions have no owner; archive without claiming first
		// would orphan them — neither ListArchivedByOwner nor any view
		// would surface them again. Claim then archive.
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return
		}
		info := sess.Info()
		if info.Origin == session.OriginExternal && info.OwnerID == "" {
			_ = b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind)
		}
		_ = b.mgr.Archive(sess.ID)
		b.saveStateIfPossible()
		// Stay in the originating card: project view when working_dir is
		// known (drill-in from /list), switch-card otherwise. Falls back
		// to a plain text ack if neither applies.
		if dir, _ := m.Action.Values["working_dir"].(string); dir != "" {
			returnTo, _ := m.Action.Values["return_to"].(string)
			b.replyWithProjectCard(ctx, m, dir, false, returnTo)
		} else {
			b.replyOrText(ctx, m, "已归档 "+b.displayIDFromGatewayID(id))
		}
	case "refresh_summary":
		id, _ := m.Action.Values["session_id"].(string)
		dir, _ := m.Action.Values["working_dir"].(string)
		b.refreshSummaryNow(ctx, m, id, dir)
	case "rename_session":
		id, _ := m.Action.Values["session_id"].(string)
		dir, _ := m.Action.Values["working_dir"].(string)
		b.showRenameForm(ctx, m, id, dir)
	case "rename_save":
		// Form submit: action.Values["key"] is the encoded session_id|dir;
		// new name is in action.FormValue["new_name"].
		b.handleRenameSave(ctx, m)
	case "pick_dir":
		b.handlePickDir(ctx, m)
	case "pick_dir_confirm":
		b.handlePickDirConfirm(ctx, m)
	case "show_projects":
		card := b.buildProjectsCard(m.UserID)
		if m.Reply != nil {
			m.Reply(card)
		} else {
			b.replyCard(ctx, m, card)
		}
	case "new_session_in":
		dir, _ := m.Action.Values["working_dir"].(string)
		b.newSessionInDir(ctx, m, dir)
	case "cmd_new":
		// Top-level "新建会话" button on the /list card — focus-aware:
		// has focus → create in focus dir + open thread; no focus → projects picker.
		b.cmdNew(ctx, m, "")
	case "show_skill":
		b.showSkillDetail(ctx, m)
	case "run_skill":
		b.runSkill(ctx, m)
	case "back_to_skills":
		b.replyWithSkillsCard(ctx, m)
	case "show_project":
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir == "" {
			return
		}
		returnTo, _ := m.Action.Values["return_to"].(string)
		b.replyWithProjectCard(ctx, m, dir, false, returnTo)
	case "show_project_archived":
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir == "" {
			return
		}
		returnTo, _ := m.Action.Values["return_to"].(string)
		b.replyWithProjectCard(ctx, m, dir, true, returnTo)
	case "back_to_list":
		// Back-button payloads on cards rendered before the /list and /project
		// merge still use "back_to_list" — route to the merged projects card.
		b.replyWithProjectsCard(ctx, m)
	case "resume_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return
		}
		info := sess.Info()
		if info.Status == string(session.StatusActive) {
			// Already active — treat the button as a switch.
			b.switchFocusTo(ctx, m, sess)
			return
		}
		// BUG-8: give immediate feedback before the slow Reactivate. Lark's
		// card-action callback times out at ~3s (would otherwise show
		// code:100000), and CLI cold start is 10-30s; without a placeholder
		// the user has no signal that the click registered.
		if m.Reply != nil {
			m.Reply(channel.Card{
				Title: "正在恢复 session " + displaySessionID(sess),
				Tone:  channel.ToneInfo,
				Sections: []channel.Section{{
					Markdown: "CLI 冷启动需 10-30 秒,完成后会在主聊天弹出 Session Resumed 卡片。",
				}},
			})
		}
		// Detach: the handler returns immediately so Lark gets its ack, and the
		// actual resume + result card happen asynchronously. The placeholder's
		// message id is reused as the in-place edit target so the final
		// "Session Resumed" card replaces it (no extra message).
		mCopy := m
		editID := m.MessageID
		go func() {
			b.resumeSessionFlow(context.Background(), mCopy, sess, editID)
		}()
	case "show_plan":
		if filename, ok := m.Action.Values["filename"].(string); ok {
			// plan-list embeds thread context into the button payload because
			// Lark card-action events don't carry it; restore it onto m so
			// replyCard posts the detail card back into the originating
			// thread instead of leaking to the main chat.
			if tid, _ := m.Action.Values["thread_id"].(string); tid != "" {
				m.ThreadID = tid
				if rid, _ := m.Action.Values["root_id"].(string); rid != "" {
					m.RootID = rid
					m.MessageID = rid
				}
			}
			b.showPlanDetail(ctx, m, filename)
		}
	case "show_archived":
		archived := b.mgr.ListArchivedByOwner(m.UserID)
		if len(archived) == 0 {
			b.replyOrText(ctx, m, "没有归档 session")
			return
		}
		card := channel.Card{
			Title:    "归档对话",
			Tone:     channel.ToneNeutral,
			Sections: appendBackButton(buildArchivedSections(archived), "back_to_list", nil),
		}
		if m.Reply != nil {
			m.Reply(card)
		} else {
			b.replyCard(ctx, m, card)
		}
	case "resume_archived":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		newSess, err := b.mgr.Reactivate(ctx, id)
		if err != nil {
			b.replyOrText(ctx, m, "恢复失败: "+err.Error())
			return
		}
		b.ensureSubscribed(ctx, newSess, m)
		b.saveStateIfPossible()
		// Bounce back to the project view so the user sees the row move
		// out of archived. If we don't know the project, fall back to a
		// short ack via Reply (still in-place).
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir != "" {
			returnTo, _ := m.Action.Values["return_to"].(string)
			b.replyWithProjectCard(ctx, m, dir, false, returnTo)
		} else {
			b.replyOrText(ctx, m, "已恢复 "+displaySessionID(newSess))
		}
	case "remove_archived":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		_ = b.mgr.RemoveArchived(id)
		b.saveStateIfPossible()
		// Redraw the archived view (or fall back to top list when this was
		// the last archived session in the project).
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir != "" {
			returnTo, _ := m.Action.Values["return_to"].(string)
			b.replyWithProjectCard(ctx, m, dir, true, returnTo)
		} else {
			b.replyOrText(ctx, m, "已删除归档 "+b.displayIDFromGatewayID(id))
		}
	case "switch_model":
		if model, ok := m.Action.Values["model"].(string); ok {
			b.cmdModel(ctx, m, model)
		}
	case "edit_config":
		if key, ok := m.Action.Values["key"].(string); ok {
			field, found := FindConfigField(key)
			if !found {
				b.replyText(ctx, m, "未知配置项")
				return
			}
			values := b.currentConfigValues()
			b.replyCard(ctx, m, buildConfigEditCard(field, values[key]))
		}
	case "save_config":
		key, _ := m.Action.Values["key"].(string)
		if key == "" {
			return
		}
		value := ""
		if v, ok := m.Action.FormValue["config_value"]; ok {
			if s, ok := v.(string); ok {
				value = s
			}
		}
		b.persistConfigChange(ctx, m, key, value)
	case "save_config_value":
		// Used by bool/enum buttons that carry the new value inline.
		key, _ := m.Action.Values["key"].(string)
		value, _ := m.Action.Values["value"].(string)
		if key == "" {
			return
		}
		b.persistConfigChange(ctx, m, key, value)
	case "answer_elicitation":
		pendingID, _ := m.Action.Values["pending_id"].(string)
		question, _ := m.Action.Values["question"].(string)
		answer, _ := m.Action.Values["answer"].(string)
		if pendingID != "" && question != "" && answer != "" {
			b.handleElicitationAnswer(ctx, m.ChatID, pendingID, question, answer)
		}
	case "plan_response":
		pendingID, _ := m.Action.Values["pending_id"].(string)
		result, _ := m.Action.Values["result"].(string)
		if pendingID != "" && result != "" {
			b.handlePlanResponse(ctx, m.ChatID, pendingID, result)
		}
	default:
		log.Printf("[bridge] unhandled card action: %s", m.Action.Name)
	}
}

func buildConfigEditCard(field ConfigField, currentValue string) channel.Card {
	desc := fmt.Sprintf("**%s** (`%s`)", field.Label, field.EnvKey)
	if currentValue != "" {
		display := currentValue
		if field.Sensitive {
			display = formatConfigValue(currentValue, field)
		}
		desc += fmt.Sprintf("\n当前值: `%s`", display)
	}
	if field.Default != "" {
		desc += fmt.Sprintf("\n默认值: `%s`", field.Default)
	}
	if field.Mutable {
		desc += "\n修改后立即生效"
	} else {
		desc += "\n修改后需重启生效"
	}

	// Bool / enum: render one button per choice, no form input needed.
	switch field.Type {
	case "bool":
		return channel.Card{
			Title:    "修改配置",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: desc, Buttons: boolButtons(field.EnvKey, currentValue)}},
		}
	case "enum":
		return channel.Card{
			Title:    "修改配置",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: desc, Buttons: enumButtons(field.EnvKey, field.EnumValues, currentValue)}},
		}
	}

	// Default: free-form text via a form.
	return channel.Card{
		Title: "修改配置",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{{
			Markdown: desc,
			Form: &channel.Form{
				FormID: "config_form",
				Fields: []channel.FormField{{Name: "config_value", Placeholder: "请输入新值"}},
				Submit: channel.Button{
					Label:  "保存",
					Style:  "primary",
					Action: map[string]string{"action": "save_config", "key": field.EnvKey},
				},
			},
		}},
	}
}

func boolButtons(key, current string) []channel.Button {
	current = strings.ToLower(strings.TrimSpace(current))
	makeBtn := func(label, value, style string, active bool) channel.Button {
		if active {
			label = "✓ " + label
		}
		return channel.Button{
			Label:  label,
			Style:  style,
			Action: map[string]string{"action": "save_config_value", "key": key, "value": value},
		}
	}
	return []channel.Button{
		makeBtn("开启", "true", "primary", current == "true"),
		makeBtn("关闭", "false", "default", current != "true"),
	}
}

func enumButtons(key string, values []string, current string) []channel.Button {
	btns := make([]channel.Button, 0, len(values))
	for _, v := range values {
		label := v
		style := "default"
		if v == current {
			label = "✓ " + v
			style = "primary"
		}
		btns = append(btns, channel.Button{
			Label:  label,
			Style:  style,
			Action: map[string]string{"action": "save_config_value", "key": key, "value": v},
		})
	}
	return btns
}

func looksLikePath(s string) bool {
	return strings.Contains(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".")
}

// persistConfigChange writes the new value to .env, applies it at runtime
// when the field is Mutable, and reports the outcome back to the user.
//
// When the user came from an edit card (m.MessageID set), the card is updated
// in place to show the new value and disable further submissions — this
// prevents accidental double-clicks and gives clear "saved" feedback.
func (b *Bridge) persistConfigChange(ctx context.Context, m channel.InboundMessage, key, value string) {
	if key == "GATEWAY_PERMISSION_MODE" {
		value = NormalizePermissionMode(value)
	}
	field, found := FindConfigField(key)
	if !found {
		b.replyConfigSave(ctx, m, "未知配置项", channel.ToneWarning)
		return
	}
	if b.envFilePath == "" {
		b.replyConfigSave(ctx, m, "未配置 .env,无法保存", channel.ToneWarning)
		return
	}
	if err := WriteEnvFile(b.envFilePath, map[string]string{key: value}); err != nil {
		b.replyConfigSave(ctx, m, "写入配置失败: "+err.Error(), channel.ToneWarning)
		return
	}
	hint := "已写入 .env,重启后生效"
	if field.Mutable {
		b.applyConfigChange(key, value)
		hint = "已写入 .env(已运行时生效)"
	}
	body := fmt.Sprintf("✅ **%s**\n`%s` = `%s`\n%s", field.Label, key, value, hint)
	b.replyConfigSave(ctx, m, body, channel.ToneSuccess)
}

// replyConfigSave updates the originating edit card. Uses the synchronous
// Reply hook when available (preferred — atomic with the form-submit
// response) and falls back to UpdateMessage / new card otherwise.
func (b *Bridge) replyConfigSave(ctx context.Context, m channel.InboundMessage, body string, tone channel.Tone) {
	card := channel.Card{
		Title:    "配置已保存",
		Tone:     tone,
		Sections: []channel.Section{{Markdown: body}},
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	if m.MessageID != "" {
		if err := b.updateCard(ctx, m.MessageID, card); err == nil {
			return
		}
	}
	b.replyCard(ctx, m, card)
}

// claimAndReactivate is the shared logic behind the "恢复" button: if the
// session is external (unowned), claim it for the current user first, then
// reactivate. After this point the session is treated as feishu-created.
//
// projectDir, when non-empty, tells us which project view to redraw via
// Reply after reactivation — keeps the user in the same card. Empty means
// fall back to a plain text ack.
func (b *Bridge) claimAndReactivate(ctx context.Context, m channel.InboundMessage, sessionID, projectDir string) {
	sess, ok := b.resolveSessionByPayload(sessionID)
	if !ok {
		b.replyOrText(ctx, m, "session 不存在")
		return
	}
	info := sess.Info()
	if info.Origin == session.OriginExternal && info.OwnerID == "" {
		if err := b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind); err != nil {
			b.replyOrText(ctx, m, "纳管失败: "+err.Error())
			return
		}
	}
	newSess, err := b.mgr.Reactivate(ctx, sess.ID)
	if err != nil {
		b.replyOrText(ctx, m, "恢复失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, newSess, m)
	b.saveStateIfPossible()
	if projectDir != "" {
		b.replyWithProjectCard(ctx, m, projectDir, false, "list")
		return
	}
	b.replyOrText(ctx, m, "已恢复 "+displaySessionID(newSess))
}

// replyWithProjectCard renders the drill-in view (sessions inside a single
// project) and Reply-replaces the originating card. When archivedOnly is
// true, renders only the project's archived sessions; otherwise renders
// active/idle sessions with an "项目归档 (N)" entrypoint and a back button.
func (b *Bridge) replyWithProjectCard(ctx context.Context, m channel.InboundMessage, dir string, archivedOnly bool, returnTo string) {
	card, ok := b.buildProjectCard(m.UserID, dir, archivedOnly, returnTo)
	if !ok {
		if archivedOnly {
			b.replyWithProjectCard(ctx, m, dir, false, returnTo)
			return
		}
		b.replyOrText(ctx, m, "项目下没有 session")
		return
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// buildProjectCard constructs the drill-in card without sending it. Shared
// between replyWithProjectCard (inbound-triggered) and refreshSummaryNow
// (background goroutine that needs to UpdateMessage with the same shape).
// returnTo controls the "← 返回" target so the back button closes the loop
// of whichever menu opened this card ("list" → /list one-level menu;
// "projects" → /project picker; default "list" for compat).
func (b *Bridge) buildProjectCard(userID, dir string, archivedOnly bool, returnTo string) (channel.Card, bool) {
	if returnTo == "" {
		returnTo = "list"
	}
	backAction := "back_to_list"
	if returnTo == "projects" {
		backAction = "show_projects"
	}
	if archivedOnly {
		var archived []session.SessionInfo
		for _, info := range b.mgr.ListArchivedByOwner(userID) {
			if info.WorkingDir == dir {
				archived = append(archived, info)
			}
		}
		if len(archived) == 0 {
			return channel.Card{}, false
		}
		sections := buildArchivedSectionsWithDir(archived, dir)
		// archived view goes back to the project view (same returnTo) so the
		// outer loop is preserved.
		sections = appendBackButton(sections, "show_project", map[string]string{"working_dir": dir, "return_to": returnTo})
		return channel.Card{
			Title:    projectName(dir) + " · 归档",
			Tone:     channel.ToneNeutral,
			Sections: sections,
		}, true
	}
	visible := b.filterAliveSessions(b.mgr.ListDiscoverableByOwner(userID, b.shareExternalEnabled()))
	var inProj []session.SessionInfo
	for _, info := range visible {
		if info.WorkingDir == dir {
			inProj = append(inProj, info)
		}
	}
	var archivedInProj []session.SessionInfo
	for _, info := range b.mgr.ListArchivedByOwner(userID) {
		if info.WorkingDir == dir {
			archivedInProj = append(archivedInProj, info)
		}
	}
	if len(inProj) == 0 && len(archivedInProj) == 0 {
		// Empty project — still give the user actions so the card isn't a
		// dead end (新建 + 返回).
		sections := []channel.Section{
			{Markdown: fmt.Sprintf("📁 **%s**\n_项目下没有 session_", dir)},
		}
		sections = appendNewAndBackButtons(sections, dir, backAction, nil)
		return channel.Card{
			Title:    projectName(dir),
			Tone:     channel.ToneInfo,
			Sections: sections,
		}, true
	}
	var focusedID string
	if sess, ok := b.mgr.FocusedSession(userID); ok {
		focusedID = sess.ID
	}
	sections := buildSessionListSectionsWithDir(inProj, focusedID, dir)
	if len(archivedInProj) > 0 {
		sections = append(sections, channel.Section{
			Divider: true,
			Buttons: []channel.Button{{
				Label:  fmt.Sprintf("项目归档 (%d)", len(archivedInProj)),
				Style:  "default",
				Action: map[string]string{"action": "show_project_archived", "working_dir": dir, "return_to": returnTo},
			}},
		})
	}
	sections = appendNewAndBackButtons(sections, dir, backAction, nil)
	return channel.Card{
		Title:    projectName(dir),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}, true
}

// appendBackButton suffixes a divider + "← 返回" button section. action is
// the action.Name; extras merges into the button payload (e.g. working_dir).
func appendBackButton(sections []channel.Section, action string, extras map[string]string) []channel.Section {
	payload := map[string]string{"action": action}
	for k, v := range extras {
		payload[k] = v
	}
	return append(sections, channel.Section{
		Divider: true,
		Buttons: []channel.Button{{
			Label:  "← 返回",
			Style:  "default",
			Action: payload,
		}},
	})
}

// appendNewAndBackButtons suffixes a divider + [+ 新建会话][← 返回] row.
// dir is the project working_dir for the new-session button; backAction +
// extras follow the same convention as appendBackButton.
func appendNewAndBackButtons(sections []channel.Section, dir, backAction string, extras map[string]string) []channel.Section {
	backPayload := map[string]string{"action": backAction}
	for k, v := range extras {
		backPayload[k] = v
	}
	return append(sections, channel.Section{
		Divider: true,
		Buttons: []channel.Button{
			{Label: "+ 新建会话", Style: "primary",
				Action: map[string]string{"action": "new_session_in", "working_dir": dir}},
			{Label: "← 返回", Style: "default", Action: backPayload},
		},
	})
}

// PendingElicitation captures an outstanding question awaiting a user reply.
type PendingElicitation struct {
	SessionID string
	RequestID string
	ToolUseID string
	CardID    string
	CreatedAt time.Time

	OriginalInput map[string]interface{}
}

// refreshSummaryNow regenerates the summary synchronously and updates the
// originating card in place. Flow:
//  1. Lark gets a synchronous Reply (patches the originating card to show
//     "🔄 刷新中..." — replaces the buttons so the user sees a loading state).
//  2. After Reply returns, we run admin.query in the SAME goroutine but
//     with a 30s budget. The card-action HTTP response has already been
//     sent to Lark via m.Reply, so blocking here is fine.
//  3. When admin finishes, we rebuild the project / top-level list card
//     with the new summary and UpdateMessage the originating card id —
//     the user sees the same card refresh in place, no new chat message.
func (b *Bridge) refreshSummaryNow(ctx context.Context, m channel.InboundMessage, sessionID, dir string) {
	if b.worker == nil {
		b.replyOrText(ctx, m, "摘要 worker 未启用,无法刷新")
		return
	}
	sess, ok := b.resolveSessionByPayload(sessionID)
	if !ok {
		b.replyOrText(ctx, m, "session 不存在")
		return
	}
	info := sess.Info()
	sid := displaySessionID(sess)
	jsonlPath := claudeSessionJSONLPath(info.WorkingDir, info.CLISessionID)
	if jsonlPath == "" {
		b.replyOrText(ctx, m, "无法定位 session jsonl,刷新失败")
		return
	}
	cardMsgID := m.MessageID

	// Step 1: synchronous Reply replaces the originating card with a
	// loading placeholder. Done before any blocking work so Lark's
	// card-action callback returns immediately.
	if m.Reply != nil {
		m.Reply(channel.Card{
			Title:    "刷新摘要中",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: fmt.Sprintf("🔄 正在为 `%s` 重新生成摘要,最长 30 秒...", sid)}},
		})
	}

	// Step 2+3 in a goroutine — we don't want the inbound handler stuck
	// for 30s (and we already returned the synchronous Reply above).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Use sess.ID (gateway-internal) for worker which keys SetSummary
		// on the manager id. The payload may have been a CLI session id;
		// resolveSessionByPayload already normalized to the live sess.
		err := b.worker.RefreshNow(bgCtx, sess.ID, jsonlPath)
		if err != nil {
			log.Printf("[bridge] refreshSummaryNow %s failed: %v", sid, err)
		}
		// Rebuild the card the user was on and patch the original message.
		var newCard channel.Card
		var ok bool
		if dir != "" {
			returnTo, _ := m.Action.Values["return_to"].(string)
			newCard, ok = b.buildProjectCard(m.UserID, dir, false, returnTo)
		} else {
			// Top-level card (no project context) → merged projects card.
			newCard = b.buildProjectsCard(m.UserID)
			ok = true
		}
		if !ok || cardMsgID == "" {
			return
		}
		if uerr := b.ch.UpdateMessage(context.Background(), cardMsgID, channel.OutboundMessage{Card: &newCard}); uerr != nil {
			log.Printf("[bridge] refreshSummaryNow update card %s failed: %v", shortID(cardMsgID), uerr)
		}
	}()
}

// claudeSessionJSONLPath is a thin proxy that avoids the import cycle —
// internal/runtime/claude already re-exports this; keep a small inline copy
// so commands.go doesn't need to import the runtime package directly.
func claudeSessionJSONLPath(workingDir, cliSessionID string) string {
	if cliSessionID == "" || workingDir == "" {
		return ""
	}
	// Mirror runtime/claude.SessionJSONLPath behavior: ~/.claude/projects/<escaped>/<id>.jsonl
	escaped := strings.ReplaceAll(workingDir, "/", "-")
	if !strings.HasPrefix(escaped, "-") {
		escaped = "-" + escaped
	}
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects", escaped, cliSessionID+".jsonl")
}

// Unused import guard.
// --- Rename session via card form ---

// showRenameForm renders a card with a text input + 保存/取消 buttons.
// The submit button's Name field carries the routing context
// ("rename_save:<session_id>|<working_dir>") so handleRenameSave can recover
// it from action.Values["key"] (channel/feishu decodes the colon prefix).
func (b *Bridge) showRenameForm(ctx context.Context, m channel.InboundMessage, sessionID, dir string) {
	sess, ok := b.resolveSessionByPayload(sessionID)
	if !ok {
		b.replyOrText(ctx, m, "session 不存在")
		return
	}
	info := sess.Info()
	sid := displaySessionID(sess)
	current := info.CustomTitle

	// Encode session_id and working_dir into the submit button Name. The
	// pipe-separated key roundtrips through channel/feishu's form-submit
	// dispatch (it splits on the first colon to set action, then leaves
	// the rest in values["key"]).
	key := sessionID + "|" + dir
	body := fmt.Sprintf("**%s** · %s\n当前名字: `%s`", projectName(info.WorkingDir), sid, valueOrDash(current))

	card := channel.Card{
		Title: "重命名 session",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: body},
			{Form: &channel.Form{
				FormID: "rename_form_" + shortID(sessionID),
				Fields: []channel.FormField{{
					Name: "new_name", Label: "新名字", Placeholder: "起个有意义的名字", Initial: current,
				}},
				Submit: channel.Button{
					Label: "保存", Style: "primary",
					Action: map[string]string{
						"action": "rename_save",
						"key":    key,
					},
				},
				SecondaryButtons: []channel.Button{{
					Label: "取消", Style: "default",
					Action: map[string]string{"action": "show_project", "working_dir": dir, "return_to": "projects"},
				}},
			}},
		},
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

func (b *Bridge) handleRenameSave(ctx context.Context, m channel.InboundMessage) {
	key, _ := m.Action.Values["key"].(string)
	parts := strings.SplitN(key, "|", 2)
	if len(parts) < 1 || parts[0] == "" {
		b.replyOrText(ctx, m, "重命名失败: 缺少 session 标识")
		return
	}
	sessionID := parts[0]
	dir := ""
	if len(parts) == 2 {
		dir = parts[1]
	}
	newName, _ := m.Action.FormValue["new_name"].(string)
	newName = strings.TrimSpace(newName)
	if newName == "" {
		b.replyOrText(ctx, m, "名字为空,未更新")
		return
	}
	// resolveSessionByPayload handles both gateway-internal and CLI id;
	// payload now ships CLI id so older cards still work after reactivate.
	sess, ok := b.resolveSessionByPayload(sessionID)
	if !ok {
		b.replyOrText(ctx, m, "重命名失败: session 不存在")
		return
	}
	if err := b.mgr.SetCustomTitle(sess.ID, newName); err != nil {
		b.replyOrText(ctx, m, "重命名失败: "+err.Error())
		return
	}
	b.saveStateIfPossible()

	// Return to the project card the user came from. /project entry → projects,
	// /list entry would need another path but we don't expose rename from /list yet.
	if dir != "" {
		b.replyWithProjectCard(ctx, m, dir, false, "projects")
		return
	}
	b.replyOrText(ctx, m, "已重命名")
}

func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

var _ = json.Marshal
