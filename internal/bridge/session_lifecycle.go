package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

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
						// Restore prior focus's thread routing from saved info; we
						// don't have the user's latest msg id here, so streamed
						// output falls back to anchoring at the thread root.
						// Preserve existing MsgID/UserID/IsGroup so in-flight
						// group replies keep their @mention and quote anchor.
						prior := priorFocus.LastInbound()
						priorFocus.SetLastInbound(session.InboundLocation{
							ChatID:    info.ChatID,
							ThreadID:  info.ThreadID,
							RootMsgID: info.RootMessageID,
							MsgID:     prior.MsgID,
							UserID:    m.UserID,
							IsGroup:   m.IsGroup,
						})
					}
				}
			}
		} else if pInfo.RootMessageID != "" {
			// Already in a thread — redirect output back into it (so anything
			// the user just typed in main chat that's still in-flight goes
			// to the thread on the next bot reply) and ping the thread so
			// the main-chat 话题入口卡 surfaces a "new reply" indicator,
			// letting the user know that session is alive somewhere else.
			// Preserve MsgID so in-flight group replies keep their quote anchor.
			prior := priorFocus.LastInbound()
			priorFocus.SetLastInbound(session.InboundLocation{
				ChatID:    pInfo.ChatID,
				ThreadID:  pInfo.ThreadID,
				RootMsgID: pInfo.RootMessageID,
				MsgID:     prior.MsgID,
				UserID:    m.UserID,
				IsGroup:   m.IsGroup,
			})
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
		// /archive without args targets "the current session". Use the
		// shared helper so idle/no-focus cases still resolve a target
		// (archive is a metadata op — no need to spin the CLI up).
		focused, err := b.ensureCurrentSession(ctx, m, false)
		if err != nil {
			b.replyText(ctx, m, err.Error())
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
	// Mark BEFORE sending the interrupt so the renderer wins the race —
	// CLI exit can fire its result event in microseconds and we don't
	// want it processed as a genuine error on the way out.
	sess.MarkInterrupted()
	// Don't gate on session.State — our state lags the CLI: we mark Ready
	// the moment a `result` event arrives, but the CLI may already be back
	// in a follow-up tool round-trip. Just forward the interrupt; the CLI
	// is the authority on whether there's a turn to cancel and silently
	// no-ops if not.
	if err := sess.SendControl(json.RawMessage(`{"subtype":"interrupt"}`)); err != nil {
		// SendControl never reached the CLI — the interrupt didn't
		// happen, so consume the flag back out. Otherwise the next
		// unrelated CLI error (could be much later) would render as a
		// neutral "Stopped" card and mask the real failure.
		_ = sess.ConsumeInterruptedFlag()
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
