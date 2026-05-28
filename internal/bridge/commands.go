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
			Usage:   "/new [label] [dir]",
			Desc:    "创建新 session(label 可选,dir 可选,默认当前工作目录)",
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
			Name:    "/help",
			Usage:   "/help",
			Desc:    "显示此帮助",
			Handler: b.wrapNoArgs(b.cmdHelp),
		},
	}
}

// wrapNoArgs adapts a no-args handler to the CommandHandler signature.
func (b *Bridge) wrapNoArgs(fn func(ctx context.Context, m channel.InboundMessage)) CommandHandler {
	return func(ctx context.Context, m channel.InboundMessage, _ string) {
		fn(ctx, m)
	}
}

// --- /new ---

func (b *Bridge) cmdNew(ctx context.Context, m channel.InboundMessage, args string) {
	label := ""
	workingDir := b.defaultCWD
	parts := strings.Fields(args)
	switch len(parts) {
	case 1:
		arg := parts[0]
		if looksLikePath(arg) {
			workingDir = arg
		} else {
			label = arg
			// If projectRoot/<label> is a directory, use it as the working dir.
			// This lets users type "/new deepgate" to mean "/new deepgate $projectRoot/deepgate".
			if b.projectRoot != "" {
				candidate := filepath.Join(b.projectRoot, arg)
				if info, err := os.Stat(candidate); err == nil && info.IsDir() {
					workingDir = candidate
				}
			}
		}
	case 2:
		label = parts[0]
		workingDir = parts[1]
	}

	sess, err := b.mgr.Create(ctx, session.CreateOpts{
		WorkingDir:  workingDir,
		Label:       label,
		OwnerID:     m.UserID,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
		Origin:      session.OriginFeishu,
	})
	if err != nil {
		b.sendText(ctx, m.ChatID, "创建 session 失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, sess, m)

	display := label
	if display == "" {
		display = workingDir
	}
	body := fmt.Sprintf("**%s** %s\nWorking dir: `%s`\n\n已设为 active session,直接发消息开始对话。",
		displaySessionID(sess), display, workingDir)
	b.sendCard(ctx, m.ChatID, channel.Card{
		Title:    "Session Created",
		Tone:     channel.ToneSuccess,
		Sections: []channel.Section{{Markdown: body}},
	})
}

// --- /list (two-level menu: projects → sessions) ---

func (b *Bridge) cmdList(ctx context.Context, m channel.InboundMessage) {
	card, empty := b.buildListCard(m.UserID)
	if empty {
		b.sendCard(ctx, m.ChatID, channel.Card{
			Title:    "Sessions",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: "暂无 session,发送消息或使用 `/new` 创建"}},
		})
		return
	}
	b.sendCard(ctx, m.ChatID, card)
}

// buildListCard renders the top-level /list view (project grouping). Reused
// by the card-action handlers below so the user's drill-in / resume flow
// stays inside a single card via Reply — saving chat real estate compared
// to posting a new card for every step.
func (b *Bridge) buildListCard(userID string) (channel.Card, bool) {
	visible := b.mgr.ListDiscoverableByOwner(userID, b.shareExternalEnabled())
	archived := b.mgr.ListArchivedByOwner(userID)
	if len(visible) == 0 && len(archived) == 0 {
		return channel.Card{}, true
	}
	var focusedID string
	if sess, ok := b.mgr.FocusedSession(userID); ok {
		focusedID = sess.ID
	}
	sections := buildProjectListSections(visible, archived, focusedID)
	return channel.Card{
		Title:    "Sessions · 按项目分组",
		Tone:     channel.ToneInfo,
		Sections: sections,
	}, false
}

// replyWithListCard re-renders the top-level project list via Reply (edits
// the original card in place) — used by "← 返回" buttons in drill-in views.
func (b *Bridge) replyWithListCard(ctx context.Context, m channel.InboundMessage) {
	card, empty := b.buildListCard(m.UserID)
	if empty {
		b.replyOrText(ctx, m, "暂无 session,发送消息或使用 `/new` 创建")
		return
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.sendCard(ctx, m.ChatID, card)
}

// buildProjectListSections groups sessions by their WorkingDir and renders one
// section per project. Each section is expandable via the show_project card
// action (drills into per-session view).
func buildProjectListSections(visible, archived []session.SessionInfo, focusedID string) []channel.Section {
	type projAgg struct {
		WorkingDir string
		Sessions   []session.SessionInfo
		Active     int
		HasFocused bool
	}
	groups := make(map[string]*projAgg)
	order := []string{}
	for _, info := range visible {
		dir := info.WorkingDir
		if dir == "" {
			dir = "(unknown)"
		}
		g, ok := groups[dir]
		if !ok {
			g = &projAgg{WorkingDir: dir}
			groups[dir] = g
			order = append(order, dir)
		}
		g.Sessions = append(g.Sessions, info)
		if info.Status == string(session.StatusActive) {
			g.Active++
		}
		if info.ID == focusedID {
			g.HasFocused = true
		}
	}

	sections := make([]channel.Section, 0, len(order)+1)
	for _, dir := range order {
		g := groups[dir]
		focusMark := ""
		if g.HasFocused {
			focusMark = " ★"
		}
		line := fmt.Sprintf("**%s** · %d sessions · %d active%s",
			projectName(dir), len(g.Sessions), g.Active, focusMark)
		sections = append(sections, channel.Section{
			Markdown: line,
			Buttons: []channel.Button{{
				Label: "展开", Style: "primary",
				Action: map[string]string{"action": "show_project", "working_dir": dir},
			}},
		})
	}

	if len(visible) == 0 {
		sections = append(sections, channel.Section{Markdown: "暂无活跃 session"})
	}

	if len(archived) > 0 {
		sections = append(sections, channel.Section{
			Divider: true,
			Buttons: []channel.Button{{
				Label:  fmt.Sprintf("归档对话 (%d)", len(archived)),
				Style:  "default",
				Action: map[string]string{"action": "show_archived"},
			}},
		})
	}
	return sections
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
				Action: map[string]string{"action": "switch_session", "session_id": info.ID},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": info.ID},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": info.ID},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns})
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
			Markdown: md,
			Buttons: []channel.Button{
				{Label: "恢复", Style: "primary", Action: map[string]string{"action": "resume_archived", "session_id": info.ID, "working_dir": dir}},
				{Label: "删除", Style: "danger", Action: map[string]string{"action": "remove_archived", "session_id": info.ID, "working_dir": dir}},
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
				Action: map[string]string{"action": "switch_session", "session_id": info.ID, "working_dir": dir},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": info.ID, "working_dir": dir},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": info.ID, "working_dir": dir},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns})
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

	if info.Origin == session.OriginFeishu {
		parts = append(parts, "`[💬feishu created]`")
	}

	if t := parseRFC3339(info.LastActivity); !t.IsZero() {
		parts = append(parts, humanAgo(time.Since(t)))
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
	prefix := strings.TrimSpace(args)
	if prefix == "" {
		b.renderActiveSessionsCard(ctx, m)
		return
	}

	sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
	if err != nil {
		b.sendText(ctx, m.ChatID, err.Error())
		return
	}
	if sess.Info().Status != string(session.StatusActive) {
		b.sendText(ctx, m.ChatID, fmt.Sprintf(
			"session %s 当前是 %s 状态,不能 /switch。请用 /resume 恢复",
			displaySessionID(sess), sess.Info().Status))
		return
	}
	if err := b.mgr.SetFocus(m.UserID, sess.ID); err != nil {
		b.sendText(ctx, m.ChatID, err.Error())
		return
	}
	b.sendText(ctx, m.ChatID, "已切换到 session "+displaySessionID(sess))
}

// renderActiveSessionsCard shows only Active sessions in a flat list — what
// /switch needs. Idle/archived are deliberately omitted (use /list for the
// full project-grouped view).
func (b *Bridge) renderActiveSessionsCard(ctx context.Context, m channel.InboundMessage) {
	card, empty := b.buildActiveSessionsCard(m.UserID)
	if empty {
		b.sendText(ctx, m.ChatID, "当前没有 active session。用 /list 查看所有 session,或 /resume <id> 唤醒 idle session")
		return
	}
	b.sendCard(ctx, m.ChatID, card)
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
	b.sendCard(ctx, m.ChatID, card)
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
	b.sendText(ctx, m.ChatID, text)
}

// buildActiveSessionsCard renders the /switch card content. Returns
// (zeroCard, true) when the user has no active sessions so the caller can
// decide whether to send an empty-state text or skip silently.
func (b *Bridge) buildActiveSessionsCard(userID string) (channel.Card, bool) {
	owned := b.mgr.ListActiveByOwner(userID)
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
				Action: map[string]string{"action": "switch_session", "session_id": info.ID},
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
		focused, ok := b.mgr.FocusedSession(m.UserID)
		if !ok {
			b.sendText(ctx, m.ChatID, "没有 active session")
			return
		}
		sessionID = focused.ID
	} else {
		sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
		if err != nil {
			b.sendText(ctx, m.ChatID, err.Error())
			return
		}
		sessionID = sess.ID
	}
	if err := b.mgr.Archive(sessionID); err != nil {
		b.sendText(ctx, m.ChatID, "归档失败: "+err.Error())
		return
	}
	b.sendText(ctx, m.ChatID, "Session "+b.displayIDFromGatewayID(sessionID)+" 已归档")
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
		b.sendText(ctx, m.ChatID, "打断失败: "+err.Error())
		return
	}
	b.sendText(ctx, m.ChatID, "已发送中断信号到 session "+displaySessionID(sess))
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
		b.sendText(ctx, m.ChatID, "终止失败: "+err.Error())
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
	b.sendText(ctx, m.ChatID, fmt.Sprintf(
		"Session %s 已终止(用 /resume %s 可恢复)%s", display, display, tail))
}

// resolveActiveTarget picks an active session — focused if no prefix, or by
// prefix lookup. Used by /stop and /terminate which only act on active
// (process running) sessions. Returns false (and sends a user-visible
// error) when no suitable target is found.
func (b *Bridge) resolveActiveTarget(ctx context.Context, m channel.InboundMessage, args, cmd string) (*session.Session, bool) {
	prefix := strings.TrimSpace(args)
	if prefix == "" {
		focused, ok := b.mgr.FocusedSession(m.UserID)
		if !ok {
			b.sendText(ctx, m.ChatID, "没有 focused session。用 "+cmd+" <id前缀> 指定")
			return nil, false
		}
		if focused.Info().Status != string(session.StatusActive) {
			b.sendText(ctx, m.ChatID, fmt.Sprintf(
				"Focused session %s 不是 active 状态,无法 %s",
				displaySessionID(focused), cmd))
			return nil, false
		}
		return focused, true
	}
	sess, err := b.mgr.FindByPrefix(m.UserID, prefix)
	if err != nil {
		b.sendText(ctx, m.ChatID, err.Error())
		return nil, false
	}
	if sess.Info().Status != string(session.StatusActive) {
		b.sendText(ctx, m.ChatID, fmt.Sprintf(
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
		b.sendText(ctx, m.ChatID, err.Error())
		return
	}

	info := sess.Info()
	if info.Status == string(session.StatusActive) {
		b.sendText(ctx, m.ChatID, fmt.Sprintf(
			"session %s 已经是 active,无需 /resume(用 /switch)", displaySessionID(sess)))
		return
	}

	// Claim external before reactivate so the new session starts as owned.
	if info.Origin == session.OriginExternal && info.OwnerID == "" {
		if err := b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind); err != nil {
			b.sendText(ctx, m.ChatID, "纳管失败: "+err.Error())
			return
		}
	}

	newSess, err := b.mgr.Reactivate(ctx, sess.ID)
	if err != nil {
		b.sendText(ctx, m.ChatID, "恢复失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, newSess, m)
	b.saveStateIfPossible()
	b.sendText(ctx, m.ChatID, "已恢复 session "+displaySessionID(newSess))
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
	b.sendCard(ctx, m.ChatID, channel.Card{
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
	switch m.Action.Name {
	case "switch_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		if err := b.mgr.SetFocus(m.UserID, id); err != nil {
			b.replyOrText(ctx, m, "切换失败: "+err.Error())
			return
		}
		// Re-render in place. Which card we redraw depends on the origin
		// of the click: the /list two-level menu passes working_dir so we
		// stay in the project view; the /switch flat menu omits it so we
		// stay in the cross-project active list. Without this distinction
		// every /list click would silently land on the /switch card.
		if dir, _ := m.Action.Values["working_dir"].(string); dir != "" {
			b.replyWithProjectCard(ctx, m, dir, false)
		} else {
			b.replyWithActiveSessionsCard(ctx, m)
		}
	case "archive_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		// External sessions have no owner; archive without claiming first
		// would orphan them — neither ListArchivedByOwner nor any view
		// would surface them again. Claim then archive.
		if sess, exists := b.mgr.Get(id); exists {
			info := sess.Info()
			if info.Origin == session.OriginExternal && info.OwnerID == "" {
				_ = b.mgr.ClaimExternal(id, m.UserID, m.ChatID, m.ChannelKind)
			}
		}
		_ = b.mgr.Archive(id)
		b.saveStateIfPossible()
		// Stay in the originating card: project view when working_dir is
		// known (drill-in from /list), switch-card otherwise. Falls back
		// to a plain text ack if neither applies.
		if dir, _ := m.Action.Values["working_dir"].(string); dir != "" {
			b.replyWithProjectCard(ctx, m, dir, false)
		} else {
			b.replyOrText(ctx, m, "已归档 "+b.displayIDFromGatewayID(id))
		}
	case "show_project":
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir == "" {
			return
		}
		b.replyWithProjectCard(ctx, m, dir, false)
	case "show_project_archived":
		dir, _ := m.Action.Values["working_dir"].(string)
		if dir == "" {
			return
		}
		b.replyWithProjectCard(ctx, m, dir, true)
	case "back_to_list":
		b.replyWithListCard(ctx, m)
	case "resume_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return
		}
		// Resume + re-render the originating project card so the user
		// stays in context (sees the row's status flip active, with the
		// "切换" button replacing "恢复"). dir comes through on the action
		// payload so we know which project view to redraw.
		dir, _ := m.Action.Values["working_dir"].(string)
		b.claimAndReactivate(ctx, m, id, dir)
	case "show_plan":
		if filename, ok := m.Action.Values["filename"].(string); ok {
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
			b.sendCard(ctx, m.ChatID, card)
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
			b.replyWithProjectCard(ctx, m, dir, false)
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
			b.replyWithProjectCard(ctx, m, dir, true)
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
				b.sendText(ctx, m.ChatID, "未知配置项")
				return
			}
			values := b.currentConfigValues()
			b.sendCard(ctx, m.ChatID, buildConfigEditCard(field, values[key]))
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
	b.sendCard(ctx, m.ChatID, card)
}

// claimAndReactivate is the shared logic behind the "恢复" button: if the
// session is external (unowned), claim it for the current user first, then
// reactivate. After this point the session is treated as feishu-created.
//
// projectDir, when non-empty, tells us which project view to redraw via
// Reply after reactivation — keeps the user in the same card. Empty means
// fall back to a plain text ack.
func (b *Bridge) claimAndReactivate(ctx context.Context, m channel.InboundMessage, sessionID, projectDir string) {
	sess, ok := b.mgr.Get(sessionID)
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
		b.replyWithProjectCard(ctx, m, projectDir, false)
		return
	}
	b.replyOrText(ctx, m, "已恢复 "+displaySessionID(newSess))
}

// replyWithProjectCard renders the drill-in view (sessions inside a single
// project) and Reply-replaces the originating card. When archivedOnly is
// true, renders only the project's archived sessions; otherwise renders
// active/idle sessions with an "项目归档 (N)" entrypoint and a back button.
func (b *Bridge) replyWithProjectCard(ctx context.Context, m channel.InboundMessage, dir string, archivedOnly bool) {
	if archivedOnly {
		var archived []session.SessionInfo
		for _, info := range b.mgr.ListArchivedByOwner(m.UserID) {
			if info.WorkingDir == dir {
				archived = append(archived, info)
			}
		}
		if len(archived) == 0 {
			// No archived left — bounce back to active view so the user
			// isn't stranded on an empty card.
			b.replyWithProjectCard(ctx, m, dir, false)
			return
		}
		sections := buildArchivedSectionsWithDir(archived, dir)
		sections = appendBackButton(sections, "show_project", map[string]string{"working_dir": dir})
		card := channel.Card{
			Title:    projectName(dir) + " · 归档",
			Tone:     channel.ToneNeutral,
			Sections: sections,
		}
		if m.Reply != nil {
			m.Reply(card)
		} else {
			b.sendCard(ctx, m.ChatID, card)
		}
		return
	}

	visible := b.mgr.ListDiscoverableByOwner(m.UserID, b.shareExternalEnabled())
	var inProj []session.SessionInfo
	for _, info := range visible {
		if info.WorkingDir == dir {
			inProj = append(inProj, info)
		}
	}
	var archivedInProj []session.SessionInfo
	for _, info := range b.mgr.ListArchivedByOwner(m.UserID) {
		if info.WorkingDir == dir {
			archivedInProj = append(archivedInProj, info)
		}
	}
	if len(inProj) == 0 && len(archivedInProj) == 0 {
		b.replyOrText(ctx, m, "项目下没有 session")
		return
	}
	var focusedID string
	if sess, ok := b.mgr.FocusedSession(m.UserID); ok {
		focusedID = sess.ID
	}
	sections := buildSessionListSectionsWithDir(inProj, focusedID, dir)
	if len(archivedInProj) > 0 {
		sections = append(sections, channel.Section{
			Divider: true,
			Buttons: []channel.Button{{
				Label:  fmt.Sprintf("项目归档 (%d)", len(archivedInProj)),
				Style:  "default",
				Action: map[string]string{"action": "show_project_archived", "working_dir": dir},
			}},
		})
	}
	sections = appendBackButton(sections, "back_to_list", nil)
	card := channel.Card{
		Title:    projectName(dir),
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
	if m.Reply != nil {
		m.Reply(card)
	} else {
		b.sendCard(ctx, m.ChatID, card)
	}
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

// PendingElicitation captures an outstanding question awaiting a user reply.
type PendingElicitation struct {
	SessionID string
	RequestID string
	ToolUseID string
	CardID    string
	CreatedAt time.Time

	OriginalInput map[string]interface{}
}

// Unused import guard.
var _ = json.Marshal
