package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// handleCardAction is the central dispatch for all interactive card-button
// callbacks. It runs after channel.OnCardAction has unmarshaled the
// platform-specific payload into m.Action.
//
// Each domain (cron, skill, project, diff, model, config, plan, session)
// owns a handleXxxCardAction(m) bool entry that returns true once it claims
// the action. This dispatcher walks the chain in priority order and stops
// at the first claim — the chain order doesn't matter for correctness as
// long as every action.Name is owned by exactly one domain, but keeping
// frequently-used domains (session) earlier shaves a couple of switch
// trips off the hot path.
//
// To add a new card action, append the case to the relevant domain's
// handleXxxCardAction; to introduce a new domain, register it here.
func (b *Bridge) handleCardAction(ctx context.Context, m channel.InboundMessage) {
	if m.Action == nil {
		return
	}
	handlers := []func(context.Context, channel.InboundMessage) bool{
		b.handleCronCardAction,
		b.handleSessionCardAction,
		b.handleProjectCardAction,
		b.handleSkillCardAction,
		b.handleDiffCardAction,
		b.handleModelCardAction,
		b.handleEffortCardAction,
		b.handlePermissionsCardAction,
		b.handleToolPermissionCardAction,
		b.handleConfigCardAction,
		b.handlePlanCardAction,
		b.handleMemoryCardAction,
	}
	for _, h := range handlers {
		if h(ctx, m) {
			return
		}
	}
	log.Printf("[bridge] unhandled card action: %s", m.Action.Name)
}

// handleSessionCardAction owns session-lifecycle and elicitation buttons
// (switch / archive / resume / refresh / rename / answer_elicitation).
// Returns true when claimed.
func (b *Bridge) handleSessionCardAction(ctx context.Context, m channel.InboundMessage) bool {
	switch m.Action.Name {
	case "switch_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return true
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return true
		}
		b.switchFocusTo(ctx, m, sess)
	case "archive_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return true
		}
		// External sessions have no owner; archive without claiming first
		// would orphan them — neither ListArchivedByOwner nor any view
		// would surface them again. Claim then archive.
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return true
		}
		info := sess.Info()
		if info.Origin == session.OriginExternal && info.OwnerID == "" {
			if err := b.mgr.ClaimExternal(sess.ID, m.UserID, m.ChatID, m.ChannelKind); err != nil {
				b.replyOrText(ctx, m, "纳管失败: "+err.Error())
				return true
			}
		}
		if err := b.mgr.Archive(sess.ID); err != nil {
			b.replyOrText(ctx, m, "归档失败: "+err.Error())
			return true
		}
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
	case "resume_session":
		id, _ := m.Action.Values["session_id"].(string)
		if id == "" {
			return true
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return true
		}
		info := sess.Info()
		if info.Status == string(session.StatusActive) {
			// Already active — treat the button as a switch.
			b.switchFocusTo(ctx, m, sess)
			return true
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
	case "show_archived":
		archived := b.mgr.ListArchivedByOwner(m.UserID)
		if len(archived) == 0 {
			b.replyOrText(ctx, m, "没有归档 session")
			return true
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
			return true
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return true
		}
		newSess, err := b.mgr.Reactivate(ctx, sess.ID)
		if err != nil {
			b.replyOrText(ctx, m, "恢复失败: "+err.Error())
			return true
		}
		b.ensureSubscribed(ctx, newSess, m)
		b.saveStateIfPossible()
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
			return true
		}
		sess, exists := b.resolveSessionByPayload(id)
		if !exists {
			b.replyOrText(ctx, m, "session 不存在")
			return true
		}
		if err := b.mgr.RemoveArchived(sess.ID); err != nil {
			b.replyOrText(ctx, m, "删除失败: "+err.Error())
			return true
		}
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
	case "answer_elicitation":
		pendingID, _ := m.Action.Values["pending_id"].(string)
		question, _ := m.Action.Values["question"].(string)
		answer, _ := m.Action.Values["answer"].(string)
		if pendingID != "" && question != "" && answer != "" {
			b.handleElicitationAnswer(ctx, m.ChatID, pendingID, question, answer)
		}
	default:
		return false
	}
	return true
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

// --- Project card (drill-in view backing show_project / show_project_archived) ---

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
		sections := buildArchivedSectionsWithDir(archived, dir, returnTo)
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

// --- refresh_summary action: regenerate one session's summary on demand ---

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
	jsonlPath := claudeRT.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
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

// --- Rename session via card form (rename_session / rename_save actions) ---

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
