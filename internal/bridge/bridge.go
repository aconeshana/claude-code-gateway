// Package bridge connects an IM channel (channel.Channel) to the session
// manager. It handles inbound user messages by either forwarding them to the
// focused session or dispatching slash commands; it subscribes to session
// events and renders them as outbound cards.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/channel/feishu"
	"github.com/anthropics/claude-code-gateway/internal/cron"
	"github.com/anthropics/claude-code-gateway/internal/plan"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

// Options carries everything Bridge needs to operate. Required fields:
// Manager, Channel, DefaultCWD. The rest have sensible defaults.
type Options struct {
	Manager    *session.Manager
	Channel    channel.Channel
	DefaultCWD string

	// EnvFilePath enables persistent /config changes. Empty disables.
	EnvFilePath string

	// AdminModel selects the model used for the admin helper (summary,
	// fuzzy matching). Empty disables admin features.
	AdminModel string

	// SummaryInterval is the number of user turns between automatic
	// summary regeneration. 0 disables.
	SummaryInterval int

	// Persister, when non-nil, is invoked after state-changing actions
	// (Create/Archive/Reactivate/Destroy/summary updates).
	Persister *persist.JSONStore

	// Discoverer, when non-nil, enables background scanning of on-disk
	// sessions belonging to the runtime (e.g. claude.NewDiscoverer scans
	// ~/.claude/projects/*.jsonl).
	Discoverer runtime.Discoverer

	// ShareExternal controls whether sessions found by the Discoverer but not
	// created via this gateway are surfaced in /list. Default false for
	// privacy in multi-user deployments.
	ShareExternal bool

	// DiscoveryWindowDays limits the discoverer to sessions modified within
	// the last N days. 0 disables the window. Default 7 if Discoverer is set.
	DiscoveryWindowDays int

	// RescanInterval controls how often the bridge re-runs discovery. 0
	// means scan once on Start and never again. Default 5 minutes.
	RescanInterval time.Duration

	// ApplyAllowedUsers, when set, is invoked when the user changes
	// FEISHU_ALLOWED_USER_IDS via /config so the channel's allowlist updates
	// without a restart.
	ApplyAllowedUsers func(ids []string)

	// ApplyCLIPath, when set, is invoked when CLAUDE_CLI_PATH changes so the
	// runtime spawns the new binary on subsequent Create calls.
	ApplyCLIPath func(path string)

	// CronStore, when non-nil, enables cron job management.
	CronStore cron.Store

	// CronRunLog, when non-nil, provides run history for cron jobs.
	CronRunLog *cron.RunLog
}

// Bridge wires together a channel.Channel and a session.Manager.
type Bridge struct {
	mgr *session.Manager
	ch  channel.Channel

	defaultCWD      string
	envFilePath     string
	summaryInterval int

	admin     *admin
	persister *persist.JSONStore
	commands  []Command
	worker    *summaryWorker
	plans     *plan.Index

	discoverer          runtime.Discoverer
	shareExternal       bool
	discoveryWindowDays int
	rescanInterval      time.Duration

	applyAllowedUsers func(ids []string)
	applyCLIPath      func(path string)

	cronStore     cron.Store
	cronRunLog    *cron.RunLog
	cronScheduler *cron.Scheduler

	mu                  sync.Mutex
	subscribed          map[string]bool
	pendingElicitations map[string]*PendingElicitation
	// terminating tracks session IDs the user explicitly /terminate-d, so
	// the CLI-exit handler can distinguish "user-initiated stop" (silent)
	// from an unexpected drop (notify).
	terminating    map[string]bool
	discoveryStats DiscoveryStats
	// manualProjects: per-owner set of directories the user explicitly
	// added via /project's directory picker but hasn't yet spawned a
	// session in. Transient; once a session is created in that dir the
	// projectsForUser fallback (deriving from session.WorkingDir) picks
	// it up permanently.
	manualProjects map[string]map[string]bool
}

// DiscoveryStats holds the latest scan progress (read by /status).
type DiscoveryStats struct {
	LastScanAt    time.Time
	LastScanTook  time.Duration
	WindowDays    int
	TotalOnDisk   int
	NewlyImported int
}

// New constructs a Bridge from the given options.
func New(opts Options) *Bridge {
	windowDays := opts.DiscoveryWindowDays
	if windowDays == 0 && opts.Discoverer != nil {
		windowDays = 7
	}
	rescan := opts.RescanInterval
	if rescan == 0 && opts.Discoverer != nil {
		rescan = 5 * time.Minute
	}
	b := &Bridge{
		mgr:                 opts.Manager,
		ch:                  opts.Channel,
		defaultCWD:          opts.DefaultCWD,
		envFilePath:         opts.EnvFilePath,
		summaryInterval:     opts.SummaryInterval,
		persister:           opts.Persister,
		discoverer:          opts.Discoverer,
		shareExternal:       opts.ShareExternal,
		discoveryWindowDays: windowDays,
		rescanInterval:      rescan,
		applyAllowedUsers:   opts.ApplyAllowedUsers,
		applyCLIPath:        opts.ApplyCLIPath,
		subscribed:          make(map[string]bool),
		pendingElicitations: make(map[string]*PendingElicitation),
		terminating:         make(map[string]bool),
		manualProjects:      make(map[string]map[string]bool),
	}
	if opts.AdminModel != "" {
		b.admin = newAdmin(opts.Manager, opts.DefaultCWD, opts.AdminModel)
		b.worker = newSummaryWorker(opts.Manager, b.admin, opts.Persister)
	}
	b.cronStore = opts.CronStore
	b.cronRunLog = opts.CronRunLog
	b.registerCommands()
	return b
}

// Start performs one-time initialization: restores persisted session state
// (if persister is set) and resubscribes to any active sessions so their
// streams are forwarded. When a Discoverer is configured, also starts a
// background goroutine that scans on-disk sessions and imports them.
func (b *Bridge) Start(ctx context.Context) {
	if b.persister != nil {
		if err := b.persister.Load(b.mgr); err != nil {
			log.Printf("[bridge] restore state failed: %v", err)
		}
		// Drop any admin-internal placeholders left over before we had a
		// detector — these polluted /list with "(自动化任务,无实质内容)" rows.
		b.cleanupAdminInternalLeftovers()
		b.saveStateIfPossible()
	}
	if b.worker != nil {
		go b.worker.Run(ctx)
	}
	if b.cronStore != nil {
		exec := newBridgeExecutor(b.mgr, b.defaultCWD)
		b.cronScheduler = cron.NewScheduler(b.cronStore, exec, b.cronRunLog, b.cronResultNotifier())
		go b.cronScheduler.Start(ctx)
		log.Printf("[bridge] cron scheduler started")
	}
	b.startDiscoveryLoop(ctx)
}

// Shutdown cleans up admin sessions and persists final state.
func (b *Bridge) Shutdown() {
	// transition all active sessions to idle so they can be resumed after
	// restart.
	infos := b.mgr.ListBy(session.Filter{Statuses: []session.Status{session.StatusActive}})
	for _, info := range infos {
		if info.OwnerID != "" && info.CLISessionID != "" {
			b.mgr.SetResumeHint(info.OwnerID, info.CLISessionID)
		}
		b.mgr.TransitionToIdle(info.ID)
	}
	if b.admin != nil {
		b.admin.destroy()
	}
	b.saveStateIfPossible()
}

// OnMessage dispatches an inbound user event to the appropriate handler.
// Implements channel.InboundHandler.
func (b *Bridge) OnMessage(ctx context.Context, m channel.InboundMessage) {
	switch m.Kind {
	case channel.InputText:
		b.handleText(ctx, m)
	case channel.InputImage, channel.InputBlocks:
		b.handleBlocks(ctx, m)
	case channel.InputCardAction:
		b.handleCardAction(ctx, m)
	default:
		log.Printf("[bridge] unsupported inbound kind: %s", m.Kind)
	}
}

func (b *Bridge) handleText(ctx context.Context, m channel.InboundMessage) {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}

	if strings.HasPrefix(text, "/") && len(text) > 1 {
		b.dispatchCommand(ctx, m, text)
		return
	}

	if strings.HasPrefix(text, "!") && len(text) > 1 {
		b.handleShell(ctx, m, strings.TrimSpace(text[1:]))
		return
	}

	// First-time setup: no default CWD configured yet.
	if b.needsSetup() {
		b.handleSetup(ctx, m, text)
		return
	}

	sess, ok := b.resolveOrCreateSession(ctx, m)
	if !ok {
		return
	}
	// Record where this message came from so streamSession can post the
	// bot's reply back to the same location (main chat vs Lark thread)
	// instead of the session's pinned thread.
	sess.SetLastInbound(inboundLocationFrom(m))
	b.ensureSubscribed(ctx, sess, m)

	b.markPendingOnIt(sess, m.MessageID)

	if err := sess.SendMessage(text); err != nil {
		b.replyText(ctx, m, "发送消息失败: "+err.Error())
		return
	}
	b.mgr.AppendRecentMessage(sess.ID, text)
	if b.summaryInterval > 0 {
		if should, msgs := b.mgr.ShouldUpdateSummary(sess.ID, b.summaryInterval); should {
			go b.updateSummary(sess.ID, m.UserID, msgs)
		}
	}
}

func (b *Bridge) handleBlocks(ctx context.Context, m channel.InboundMessage) {
	sess, ok := b.resolveOrCreateSession(ctx, m)
	if !ok {
		return
	}
	sess.SetLastInbound(inboundLocationFrom(m))
	b.ensureSubscribed(ctx, sess, m)

	b.markPendingOnIt(sess, m.MessageID)

	if err := sess.SendMessageBlocks(m.Blocks); err != nil {
		b.replyText(ctx, m, "发送图片消息失败: "+err.Error())
		return
	}
	// Record a marker for the list UI's LatestUserMessage fallback — we
	// don't have a clean text extraction from arbitrary block payloads, but
	// "[图片消息]" + any inline text the user typed is better than nothing.
	marker := "[图片消息]"
	if t := strings.TrimSpace(m.Text); t != "" {
		marker = marker + " " + t
	}
	b.mgr.AppendRecentMessage(sess.ID, marker)
	// Mirror handleText: a turn is a turn regardless of payload type, so
	// image messages should count toward SUMMARY_INTERVAL. Previously this
	// path was missing the trigger, which is why sessions that received a
	// lot of image-mixed messages never had their summary refreshed.
	if b.summaryInterval > 0 {
		if should, msgs := b.mgr.ShouldUpdateSummary(sess.ID, b.summaryInterval); should {
			go b.updateSummary(sess.ID, m.UserID, msgs)
		}
	}
}

// --- Session lookup / creation ---

func (b *Bridge) resolveOrCreateSession(ctx context.Context, m channel.InboundMessage) (*session.Session, bool) {
	// 0. Thread route (highest priority on platforms that support threads).
	// A message arriving with ThreadID set means the user is typing inside
	// a Lark thread; route to the session bound to that thread, creating /
	// binding one if this is the first message in a brand-new thread.
	if m.ThreadID != "" {
		// Snapshot main-chat focus BEFORE any Reactivate/Create — both
		// SetFocus to the new session, which would silently move main-chat
		// focus to this thread's session. Restore at the end so the user's
		// main-chat focus stays put when they type in a thread.
		priorFocus := b.snapshotFocus(m.UserID)
		restoreFocus := func(target *session.Session) {
			if priorFocus == nil || target == nil || priorFocus.ID == target.ID {
				return
			}
			_ = b.mgr.SetFocus(m.UserID, priorFocus.ID)
		}

		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok {
			if live, alive := b.mgr.Get(sess.ID); alive {
				if live.Info().Status == string(session.StatusActive) {
					return live, true
				}
				// Idle: reactivate so the next SendMessage works.
				newSess, err := b.mgr.Reactivate(ctx, live.ID)
				if err == nil {
					b.ensureSubscribed(ctx, newSess, m)
					restoreFocus(newSess)
					b.saveStateIfPossible()
					return newSess, true
				}
				log.Printf("[bridge] thread session %s reactivate failed: %v", displaySessionID(live), err)
				// fall through to new-thread logic below
			}
		}
		// Brand-new thread: bind it to the user's current focused session
		// (if that session isn't already pinned to another thread); otherwise
		// create a fresh session for this thread so each thread stays a
		// physically separate context.
		var target *session.Session
		if focused, ok := b.mgr.FocusedSession(m.UserID); ok && focused.Info().ThreadID == "" {
			target = focused
		} else {
			sess, err := b.mgr.Create(ctx, session.CreateOpts{
				WorkingDir:  b.defaultCWD,
				OwnerID:     m.UserID,
				ChatID:      m.ChatID,
				ChannelKind: m.ChannelKind,
				Origin:      channelKindToOrigin(m.ChannelKind),
			})
			if err != nil {
				b.replyText(ctx, m, "新建会话失败: "+err.Error())
				return nil, false
			}
			target = sess
		}
		rootID := m.RootID
		if rootID == "" {
			rootID = m.MessageID // first message in this thread is its root
		}
		_ = b.mgr.BindThread(target.ID, m.ThreadID, rootID)
		restoreFocus(target)
		b.saveStateIfPossible()
		return target, true
	}

	// 1. Main-chat route: focused session.
	// We don't gate on ThreadID anymore — a session can be focused in main
	// chat AND bound to a Lark thread. The two are independent UI entry
	// points to the same conversation; routing main-chat plain text to a
	// thread-bound focused session is intentional.
	if sess, ok := b.mgr.FocusedSession(m.UserID); ok {
		if live, alive := b.mgr.Get(sess.ID); alive {
			log.Printf("[bridge] route: focused session %s status=%s", displaySessionID(live), live.Info().Status)
			return live, true
		}
	} else {
		log.Printf("[bridge] route: no focused session for %s", shortID(m.UserID))
	}
	// 2. Resumable (idle preferred). Thread-bound sessions are valid
	// candidates here; they can serve both main chat and their thread.
	// Ghost sessions (CLI jsonl missing — typically /branch forks that were
	// never typed into) are auto-archived and skipped so they don't keep
	// catching auto-resume traffic.
	for {
		resumable := b.mgr.ResolveResumable(m.UserID)
		if resumable == nil {
			log.Printf("[bridge] route: ResolveResumable found nothing for %s", shortID(m.UserID))
			break
		}
		log.Printf("[bridge] route: ResolveResumable -> %s status=%s", displaySessionID(resumable), resumable.Info().Status)
		if !b.sessionAlive(resumable.Info()) {
			log.Printf("[bridge] auto-archiving ghost session %s (jsonl missing)", displaySessionID(resumable))
			if err := b.mgr.Archive(resumable.ID); err != nil {
				log.Printf("[bridge] archive ghost %s failed: %v", displaySessionID(resumable), err)
				break
			}
			b.saveStateIfPossible()
			continue
		}
		newSess, err := b.mgr.Reactivate(ctx, resumable.ID)
		if err == nil {
			b.replyText(ctx, m, "已自动恢复 session "+displaySessionID(newSess))
			b.saveStateIfPossible()
			return newSess, true
		}
		log.Printf("[bridge] auto-resume %s failed: %v", displaySessionID(resumable), err)
		break
	}
	// 3. Create new
	sess, err := b.mgr.Create(ctx, session.CreateOpts{
		WorkingDir:  b.defaultCWD,
		OwnerID:     m.UserID,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
		Origin:      channelKindToOrigin(m.ChannelKind),
	})
	if err != nil {
		b.replyText(ctx, m, "自动创建 session 失败: "+err.Error())
		return nil, false
	}
	b.saveStateIfPossible()
	return sess, true
}

// --- Subscribe / stream rendering ---

func (b *Bridge) ensureSubscribed(ctx context.Context, sess *session.Session, m channel.InboundMessage) {
	b.mu.Lock()
	if b.subscribed[sess.ID] {
		b.mu.Unlock()
		return
	}
	b.subscribed[sess.ID] = true
	b.mu.Unlock()

	log.Printf("[bridge] streamSession start: session=%s chat=%s thread=%s", displaySessionID(sess), shortID(m.ChatID), shortID(m.ThreadID))
	go b.streamSession(ctx, sess, m.ChatID)
}

// --- Outbound helpers ---

func (b *Bridge) sendText(ctx context.Context, chatID, text string) {
	if _, err := b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: chatID, Text: text}); err != nil {
		log.Printf("[bridge] sendText failed: %v", err)
	}
}

func (b *Bridge) sendCard(ctx context.Context, chatID string, card channel.Card) (string, error) {
	id, err := b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: chatID, Card: &card})
	if err != nil {
		log.Printf("[bridge] sendCard failed: %v", err)
	}
	return id, err
}

func (b *Bridge) updateCard(ctx context.Context, messageID string, card channel.Card) error {
	if err := b.ch.UpdateMessage(ctx, messageID, channel.OutboundMessage{Card: &card}); err != nil {
		log.Printf("[bridge] updateCard failed: %v", err)
		return err
	}
	return nil
}

// updateFinalCardForSession is the UpdateMessage counterpart of
// sendFinalCardForSession: same @-mention policy (group + known asker
// → @ the asker), used by the renderer when the Done card replaces an
// existing progress card via Lark's edit-in-place API rather than
// creating a brand-new message.
func (b *Bridge) updateFinalCardForSession(ctx context.Context, messageID string, sess *session.Session, card channel.Card) error {
	out := channel.OutboundMessage{Card: &card}
	if sess != nil {
		if loc := sess.LastInbound(); loc.IsGroup && loc.UserID != "" {
			out.MentionUserID = loc.UserID
		}
	}
	if err := b.ch.UpdateMessage(ctx, messageID, out); err != nil {
		log.Printf("[bridge] updateFinalCardForSession failed: %v (msg=%s mention=%s)",
			err, shortID(messageID), shortID(out.MentionUserID))
		return err
	}
	return nil
}

// --- Thread-aware outbound helpers ---
//
// Use replyText/replyCard for messages produced in response to an inbound
// event (command handlers, etc.). When the inbound arrived inside a Lark
// thread, the reply automatically lands in that same thread; otherwise it
// goes to the main chat. Use sendTextForSession/sendCardForSession for
// out-of-band messages tied to a specific session (streamSession output,
// etc.) — they anchor the reply to the session's bound thread root.
//
// Plain sendText/sendCard remain for messages that have no session/inbound
// context (independent notifications, discovery worker results).

func (b *Bridge) replyText(ctx context.Context, m channel.InboundMessage, text string) {
	out := channel.OutboundMessage{ChatID: m.ChatID, Text: text}
	if shouldUseReplyAPI(m) {
		out.ReplyToMessageID = m.MessageID
	}
	if _, err := b.ch.SendMessage(ctx, out); err != nil {
		// Anchor missing → drop the reply anchor and resend to the main chat.
		// Don't notify the user here (replyText is often used for error
		// messages itself; double-notification would be noisy).
		if errors.Is(err, feishu.ErrReplyAnchorMissing) {
			_, _ = b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: m.ChatID, Text: text})
			return
		}
		log.Printf("[bridge] replyText failed: %v", err)
	}
}

func (b *Bridge) replyCard(ctx context.Context, m channel.InboundMessage, card channel.Card) (string, error) {
	out := channel.OutboundMessage{ChatID: m.ChatID, Card: &card}
	if shouldUseReplyAPI(m) {
		out.ReplyToMessageID = m.MessageID
	}
	id, err := b.ch.SendMessage(ctx, out)
	if err != nil {
		if errors.Is(err, feishu.ErrReplyAnchorMissing) {
			id, err = b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: m.ChatID, Card: &card})
		}
		if err != nil {
			log.Printf("[bridge] replyCard failed: %v", err)
		}
	}
	return id, err
}

// threadAnchorFromInbound extracts the thread root message id for an inbound
// message inside a Lark thread, falling back to the message's own id when
// the platform didn't include root_id (which means this message IS the
// thread root). Returns "" when not in a thread, signaling main-chat output.
func threadAnchorFromInbound(m channel.InboundMessage) string {
	if m.ThreadID == "" {
		return ""
	}
	if m.RootID != "" {
		return m.RootID
	}
	return m.MessageID
}

// shouldUseReplyAPI decides whether replyCard/replyText should pin the
// outbound to the user's inbound message via the platform's Reply API.
//
// Two scenarios warrant this:
//   - The user spoke inside a Lark thread → reply must stay in the thread
//     (the Reply API is the only mechanism that does this).
//   - The user spoke in a group main chat → reply gets a quote bubble that
//     anchors bot output to a specific question (UX nicety; cluttered group
//     chats are unreadable without it).
//
// P2P / 1-on-1 chats are deliberately excluded — there is no ambiguity
// about who the bot is replying to, and the quote bubble adds visual noise.
//
// Note: replyCard/replyText deliberately do NOT set MentionUserID. They
// carry short command responses and system notifications ("auto-resumed
// session X") where an @ would be redundant push spam — the user just
// typed something and their attention is already on the chat. @-mention
// is reserved for the agent's Done card (handled by sendFinalCardForSession
// and updateFinalCardForSession), which can arrive minutes after the
// user walked away.
func shouldUseReplyAPI(m channel.InboundMessage) bool {
	if m.MessageID == "" {
		return false
	}
	return m.ThreadID != "" || m.IsGroup
}

// inboundLocationFrom projects an InboundMessage into the session-level
// routing context. Used wherever the bridge records "where this user spoke"
// for later streamed-output routing.
func inboundLocationFrom(m channel.InboundMessage) session.InboundLocation {
	return session.InboundLocation{
		ChatID:    m.ChatID,
		ThreadID:  m.ThreadID,
		RootMsgID: threadAnchorFromInbound(m),
		MsgID:     m.MessageID,
		UserID:    m.UserID,
		IsGroup:   m.IsGroup,
	}
}

// markPendingOnIt adds the "OnIt" reaction to the user's message and
// records the resulting reaction id on the session so the renderer can
// clear it when the agent finishes. If the session already has a pending
// reaction from a prior turn, that one is cleared first (1-slot LRU —
// users sending rapid-fire messages don't accumulate stale acknowledgers).
//
// Fires in BOTH P2P and group: OnIt is "I see you, working on it"
// feedback — orthogonal to the quote-reply / @-mention UX class which
// is group-only. P2P users equally rely on the visual cue, especially
// when the agent's first response is a long tool call (Bash, Grep)
// rather than immediate text — without OnIt, P2P users would see
// silence for minutes before the first assistant chunk lands.
func (b *Bridge) markPendingOnIt(sess *session.Session, msgID string) {
	if msgID == "" {
		return
	}
	if prevMsg, prevID := sess.PendingReaction(); prevMsg != "" && prevID != "" {
		_ = b.ch.RemoveReaction(prevMsg, prevID)
	}
	id, err := b.ch.AddReaction(msgID, "OnIt")
	if err != nil {
		log.Printf("[bridge] add OnIt failed (msg=%s): %v", shortID(msgID), err)
		sess.SetPendingReaction("", "")
		return
	}
	sess.SetPendingReaction(msgID, id)
}

// clearPendingReaction removes the pending OnIt reaction (if any) and
// resets the session's tracking. Called by the renderer after the Done
// card is dispatched.
func (b *Bridge) clearPendingReaction(sess *session.Session) {
	msgID, reactionID := sess.PendingReaction()
	if msgID == "" || reactionID == "" {
		return
	}
	if err := b.ch.RemoveReaction(msgID, reactionID); err != nil {
		log.Printf("[bridge] remove OnIt failed (msg=%s, rxn=%s): %v",
			shortID(msgID), shortID(reactionID), err)
	}
	sess.SetPendingReaction("", "")
}

func (b *Bridge) sendTextForSession(ctx context.Context, sess *session.Session, chatID, text string) {
	out := channel.OutboundMessage{ChatID: chatID, Text: text}
	if sess != nil {
		applyStreamingAnchor(&out, sess)
	}
	if _, err := b.ch.SendMessage(ctx, out); err != nil {
		if b.handleOutboundError(ctx, sess, out.ChatID, err) {
			_, _ = b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: out.ChatID, Text: text})
			return
		}
		log.Printf("[bridge] sendTextForSession failed: %v", err)
	}
}

func (b *Bridge) sendCardForSession(ctx context.Context, sess *session.Session, chatID string, card channel.Card) (string, error) {
	out := channel.OutboundMessage{ChatID: chatID, Card: &card}
	if sess != nil {
		applyStreamingAnchor(&out, sess)
	}
	id, err := b.ch.SendMessage(ctx, out)
	if err != nil {
		if b.handleOutboundError(ctx, sess, out.ChatID, err) {
			id, err = b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: out.ChatID, Card: &card})
		}
		if err != nil {
			log.Printf("[bridge] sendCardForSession failed: %v (session=%s anchor=%s)", err, displaySessionID(sess), shortID(out.ReplyToMessageID))
		}
	} else {
		log.Printf("[bridge] sendCardForSession ok: session=%s msgID=%s anchor=%s", displaySessionID(sess), shortID(id), shortID(out.ReplyToMessageID))
	}
	return id, err
}

// sendFinalCardForSession is a variant of sendCardForSession used for the
// last card of an agent turn (Done / Error). In group chats it additionally
// @-mentions the user who asked the question, so they get a strong
// notification on completion without spamming intermediate cards.
//
// All other behavior (quote anchoring, chat routing, error fallback) is
// identical to sendCardForSession.
func (b *Bridge) sendFinalCardForSession(ctx context.Context, sess *session.Session, chatID string, card channel.Card) (string, error) {
	out := channel.OutboundMessage{ChatID: chatID, Card: &card}
	if sess != nil {
		applyStreamingAnchor(&out, sess)
		if loc := sess.LastInbound(); loc.IsGroup && loc.UserID != "" {
			out.MentionUserID = loc.UserID
		}
	}
	id, err := b.ch.SendMessage(ctx, out)
	if err != nil {
		if b.handleOutboundError(ctx, sess, out.ChatID, err) {
			id, err = b.ch.SendMessage(ctx, channel.OutboundMessage{ChatID: out.ChatID, Card: &card})
		}
		if err != nil {
			log.Printf("[bridge] sendFinalCardForSession failed: %v (session=%s anchor=%s)", err, displaySessionID(sess), shortID(out.ReplyToMessageID))
		}
	} else {
		log.Printf("[bridge] sendFinalCardForSession ok: session=%s msgID=%s anchor=%s mention=%s",
			displaySessionID(sess), shortID(id), shortID(out.ReplyToMessageID), shortID(out.MentionUserID))
	}
	return id, err
}

// applyStreamingAnchor decides the routing target for a session-bound
// streamed output card/text. The contract is:
//
//   - Chat: follow the user's most recent location (so a thread-bound session
//     can still reply in main chat when the user types there). Fall back to
//     the session's pinned thread when there's no inbound history yet.
//   - Reply anchor: prefer the user's most recent message id (gives the
//     streamed card a quote bubble of the specific question being answered).
//     Fall back to the thread root for backward compat and pre-inbound cases.
//   - P2P: leave anchor empty — quote bubbles add noise in 1-on-1 chats.
//
// MentionUserID and IsGroup are stored on out so downstream Done-card
// rendering (sendFinalCardForSession) can decide whether to @ the asker.
func applyStreamingAnchor(out *channel.OutboundMessage, sess *session.Session) {
	loc := sess.LastInbound()
	if loc.ChatID != "" {
		out.ChatID = loc.ChatID
		switch {
		case loc.IsGroup && loc.MsgID != "":
			// Group chat: quote the user's most recent question.
			out.ReplyToMessageID = loc.MsgID
		case loc.ThreadID != "":
			// In a Lark thread (group or P2P): prefer latest msg, fall
			// back to thread root so the reply still lands in-thread.
			if loc.MsgID != "" {
				out.ReplyToMessageID = loc.MsgID
			} else {
				out.ReplyToMessageID = loc.RootMsgID
			}
		}
		// P2P main chat: leave anchor empty (Create API, no quote).
		return
	}
	// No inbound recorded yet — fall back to the session's pinned thread so
	// the thread entry card still sees the new reply.
	if anchor := sess.Info().RootMessageID; anchor != "" {
		out.ReplyToMessageID = anchor
	}
}

// handleOutboundError detects "thread anchor missing" failures from the Lark
// Reply API and clears the session's thread binding so subsequent messages
// fall back to the main chat. Returns true when a retry should be attempted
// via plain Create (caller responsibility).
func (b *Bridge) handleOutboundError(ctx context.Context, sess *session.Session, chatID string, err error) bool {
	if err == nil || sess == nil {
		return false
	}
	if !errors.Is(err, feishu.ErrReplyAnchorMissing) {
		return false
	}
	_ = b.mgr.ClearThread(sess.ID)
	b.saveStateIfPossible()
	b.sendText(ctx, chatID, "话题已失效（根消息被删），已切回主聊天。")
	return true
}

// --- Thread auto-open helpers ---

// openThreadForSession opens a Lark thread anchored at anchorMsgID and binds
// it to sess. Caller must already have a "framing" message in the main chat
// (typically a "✅ 已创建 session xxx" card returned by replyCard); this
// helper sends a follow-up reply that triggers the platform to spawn a new
// thread, then records the binding so future inbound thread events route
// back to sess.
//
// Reuse: if sess is already bound to a thread (e.g. surviving across a
// gateway restart or a /terminate + /resume cycle), this pings the original
// thread instead of opening a new one — keeps the main-chat free of
// duplicate 话题入口卡. When the original thread's root has been deleted
// the Reply API returns ErrReplyAnchorMissing; we clear the stale binding
// and fall through to OpenThread, producing a fresh thread.
//
// If the channel does not implement ThreadOpener (e.g. DingTalk), the
// session is left in the main chat and a fallback notification is logged.
func (b *Bridge) openThreadForSession(ctx context.Context, sess *session.Session, anchorMsgID, welcome string) error {
	info := sess.Info()
	if info.ThreadID != "" && info.RootMessageID != "" {
		// Reuse: reply into the existing thread. Lark will update the main
		// chat's 话题入口卡 with a "new reply" badge so the user notices.
		// Also patch the anchor card title so old "Session Created" entries
		// in the thread list get the project-name format.
		display := info.Label
		if display == "" {
			display = projectName(sess.WorkingDir)
		}
		anchorCard := channel.Card{
			Title:    "📂 " + display,
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: renderSessionHeader(info, "") + "\n" + renderSessionTitle(info)}},
		}
		if err := b.ch.UpdateMessage(ctx, info.RootMessageID, channel.OutboundMessage{Card: &anchorCard}); err != nil {
			log.Printf("[bridge] openThreadForSession: update anchor card title failed (non-fatal): %v", err)
		}
		_, err := b.ch.SendMessage(ctx, channel.OutboundMessage{
			ChatID:           info.ChatID,
			Text:             welcome,
			ReplyToMessageID: info.RootMessageID,
		})
		if err == nil {
			log.Printf("[bridge] reused thread %s for session %s", shortID(info.ThreadID), displaySessionID(sess))
			return nil
		}
		if !errors.Is(err, feishu.ErrReplyAnchorMissing) {
			return fmt.Errorf("ping existing thread: %w", err)
		}
		// Original anchor is gone (user deleted thread root). Clear the
		// stale binding and fall through to open a fresh thread.
		log.Printf("[bridge] existing thread %s root missing, clearing and re-opening", shortID(info.ThreadID))
		_ = b.mgr.ClearThread(sess.ID)
		b.saveStateIfPossible()
	}

	opener, ok := b.ch.(channel.ThreadOpener)
	if !ok {
		log.Printf("[bridge] channel %s does not support threads; leaving session %s in main chat", b.ch.Kind(), displaySessionID(sess))
		return nil
	}
	if anchorMsgID == "" {
		return fmt.Errorf("openThreadForSession: anchorMsgID required")
	}
	_, threadID, err := opener.OpenThread(ctx, anchorMsgID, channel.OutboundMessage{Text: welcome})
	if err != nil {
		return fmt.Errorf("open thread: %w", err)
	}
	if threadID == "" {
		return fmt.Errorf("open thread: platform returned empty thread_id")
	}
	if err := b.mgr.BindThread(sess.ID, threadID, anchorMsgID); err != nil {
		return fmt.Errorf("bind thread: %w", err)
	}
	b.saveStateIfPossible()
	log.Printf("[bridge] opened thread %s for session %s (anchor=%s)", shortID(threadID), displaySessionID(sess), shortID(anchorMsgID))
	return nil
}

// hasMainChatFocus returns the focused session for ownerID iff that session
// is currently active. Used to decide whether a newly created session should
// take the main-chat focus or be opened in a thread instead.
func (b *Bridge) hasMainChatFocus(ownerID string) bool {
	focused, ok := b.mgr.FocusedSession(ownerID)
	if !ok {
		return false
	}
	return focused.Info().Status == string(session.StatusActive)
}

// currentSession returns the session that "operate on the current
// conversation" commands (/rename /stop /terminate /archive no-arg, etc.)
// should target, following V2 routing rules:
//   - in a thread → the thread-bound session (so /rename inside a thread
//     names that session, not whatever the main-chat focus happens to be)
//   - in main chat → the focused session
//
// Returns (nil, false) when nothing is active in this context.
func (b *Bridge) currentSession(m channel.InboundMessage) (*session.Session, bool) {
	if m.ThreadID != "" {
		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok {
			return sess, true
		}
	}
	return b.mgr.FocusedSession(m.UserID)
}

// ensureCurrentSession resolves the session a command should act on, and
// optionally reactivates it when the command needs a live CLI process.
//
// Resolution chain (matches the in-text "auto-resume" UX so users don't
// need to remember the explicit /switch+/resume two-step):
//  1. currentSession(m) — thread-bound when in a thread, focused otherwise
//  2. mgr.ResolveResumable(userID) — pick any idle session, set focus so
//     follow-up commands stay on the same target
//  3. Give up with an instructive error
//
// When mustBeActive is true and the resolved session is idle, it gets
// reactivated transparently. Callers that only need session metadata
// (/rename setting custom title, /archive flipping status) pass false to
// skip the reactivate cost.
func (b *Bridge) ensureCurrentSession(ctx context.Context, m channel.InboundMessage, mustBeActive bool) (*session.Session, error) {
	sess, ok := b.currentSession(m)
	if !ok {
		resumable := b.mgr.ResolveResumable(m.UserID)
		if resumable == nil {
			return nil, fmt.Errorf("当前没有 session — 用 /new 创建,或 /list 选择一个")
		}
		sess = resumable
		// Only adopt main-chat focus when the user typed in main chat.
		// In a thread, "current session" comes from thread binding —
		// silently rewriting main-chat focus would surprise the user
		// (their /list focus marker would change without them asking).
		if m.ThreadID == "" {
			_ = b.mgr.SetFocus(m.UserID, sess.ID)
		}
	}
	if mustBeActive && sess.Info().Status != string(session.StatusActive) {
		newSess, err := b.mgr.Reactivate(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("自动恢复 session %s 失败: %w",
				displaySessionID(sess), err)
		}
		b.ensureSubscribed(ctx, newSess, m)
		b.saveStateIfPossible()
		sess = newSess
	}
	return sess, nil
}

// snapshotFocus captures the active main-chat focus BEFORE a /new /resume
// /branch creates or reactivates a session. Callers must capture this
// upfront because mgr.Create / mgr.Reactivate eagerly SetFocus to the new
// session, which clobbers the pre-existing focus.
func (b *Bridge) snapshotFocus(ownerID string) *session.Session {
	focused, ok := b.mgr.FocusedSession(ownerID)
	if !ok || focused == nil {
		return nil
	}
	if focused.Info().Status != string(session.StatusActive) {
		return nil
	}
	return focused
}

// afterCreateOrActivate applies the unified "no prior focus → take focus,
// has prior focus → open thread + restore" rule. priorFocus is the active
// main-chat focus captured BEFORE newSess was created (use snapshotFocus).
//
// When priorFocus is nil, newSess keeps the focus that mgr.Create already
// set. When priorFocus is non-nil, focus is restored to it and newSess is
// moved into a freshly opened thread anchored at anchorMsgID.
//
// forceThread=true unconditionally opens a thread (used by /branch — the
// fork is meant to be parallel, never the main-chat focus).
func (b *Bridge) afterCreateOrActivate(ctx context.Context, newSess *session.Session, ownerID, anchorMsgID, welcome string, priorFocus *session.Session, forceThread bool) {
	if priorFocus == nil && !forceThread {
		// V2 TOP principle: reuse existing thread outranks taking main-chat
		// focus. If newSess already carries a thread binding (preserved
		// across idle/restart by Reactivate), route into that thread —
		// don't silently hijack main-chat focus.
		if info := newSess.Info(); info.ThreadID != "" {
			b.mgr.ClearFocus(ownerID)
			if err := b.openThreadForSession(ctx, newSess, anchorMsgID, welcome); err != nil {
				log.Printf("[bridge] preserve-thread on resume %s failed: %v", displaySessionID(newSess), err)
			}
		}
		// else: newSess keeps the focus already set by mgr.Create / Reactivate
		// (default container = main chat, principle #2).
		b.saveStateIfPossible()
		return
	}
	// Restore prior focus (or, if forceThread without priorFocus, leave
	// focus where it is — but in practice forceThread is only used by
	// /branch, which always has a prior focus).
	if priorFocus != nil {
		_ = b.mgr.SetFocus(ownerID, priorFocus.ID)
	}
	if err := b.openThreadForSession(ctx, newSess, anchorMsgID, welcome); err != nil {
		log.Printf("[bridge] auto-open thread for session %s failed: %v", displaySessionID(newSess), err)
	}
	b.saveStateIfPossible()
}

// needsSetup returns true when the bridge hasn't been configured with a
// working directory yet (initial install).
func (b *Bridge) needsSetup() bool {
	return b.defaultCWD == "" || b.defaultCWD == "."
}

// sessionAlive returns false for sessions whose backing claude-code jsonl
// file is missing — typically `/branch` fork sessions that pre-assigned a
// CLI session id but never received a user message, so the CLI never wrote
// the transcript. Reviving such a session would fail with "No conversation
// found"; the gateway uses this check to filter ghosts out of /list views
// and to skip them in auto-resume routing.
//
// CLISessionID==""  → return true (session hasn't fully bootstrapped yet,
// not a ghost).
// Bridge-only / non-claude sessions return true (no jsonl to check).
func (b *Bridge) sessionAlive(info session.SessionInfo) bool {
	if info.CLISessionID == "" {
		return true
	}
	path := claudeRT.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
	if path == "" {
		return true
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// resolveSessionByPayload looks up a session by an id pulled out of a card
// action payload. We accept both gateway-internal session.ID and the
// runtime's CLI session id because mgr.Reactivate generates a NEW
// gateway-internal id on each reactivation — a card that was rendered
// before a thread message triggered a reactivate would otherwise carry a
// stale id and the action would fail with "session 不存在".
func (b *Bridge) resolveSessionByPayload(id string) (*session.Session, bool) {
	if id == "" {
		return nil, false
	}
	if sess, ok := b.mgr.Get(id); ok {
		return sess, true
	}
	if sess, ok := b.mgr.GetByCLISessionID(id); ok {
		return sess, true
	}
	return nil, false
}

// filterAliveSessions returns a new slice with ghost sessions removed.
// It also opportunistically backfills MessageCount for owned sessions that
// have a jsonl on disk but no count yet (the discoverer only fills it for
// external imports; owned feishu sessions go through state restore and
// would otherwise show no "· N 条" badge).
func (b *Bridge) filterAliveSessions(in []session.SessionInfo) []session.SessionInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]session.SessionInfo, 0, len(in))
	for _, info := range in {
		if !b.sessionAlive(info) {
			continue
		}
		if info.MessageCount == 0 && info.CLISessionID != "" {
			path := claudeRT.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
			if path != "" {
				if n := countUserTurnsInJSONL(path); n > 0 {
					info.MessageCount = n
					if sess, ok := b.mgr.Get(info.ID); ok {
						sess.SetMessageCount(n)
					}
				}
			}
		}
		out = append(out, info)
	}
	return out
}

// countUserTurnsInJSONL streams the jsonl file and counts user-authored
// conversation turns. Cheap (~50ms per 50MB file). Returns 0 on error.
func countUserTurnsInJSONL(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	const needle = `"type":"user"`
	buf := make([]byte, 64*1024)
	n := 0
	leftover := []byte{}
	for {
		nr, err := f.Read(buf)
		if nr > 0 {
			data := append(leftover, buf[:nr]...)
			start := 0
			for i := 0; i < len(data); i++ {
				if data[i] == '\n' {
					line := data[start:i]
					if strings.Contains(string(line), needle) {
						n++
					}
					start = i + 1
				}
			}
			leftover = data[start:]
		}
		if err != nil {
			break
		}
	}
	if len(leftover) > 0 && strings.Contains(string(leftover), needle) {
		n++
	}
	return n
}

func shortID(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// displaySessionID picks the user-facing 8-char id for a session. We prefer
// the Claude CLI session id (matches the on-disk jsonl filename and what
// shows up in /resume buttons). When the CLI hasn't emitted init yet and
// we don't know the resume id, fall back to the gateway-internal UUID
// prefixed with "gw:" so users can tell at a glance that this isn't a
// claude-side handle.
//
// Use this everywhere the UI shows "session XXX".
func displaySessionID(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return displayIDFromInfo(sess.Info())
}

// displayIDFromInfo is the SessionInfo-keyed form for templated lists.
func displayIDFromInfo(info session.SessionInfo) string {
	if info.CLISessionID != "" {
		return shortID(info.CLISessionID)
	}
	return "gw:" + shortID(info.ID)
}

// displayIDFromGatewayID resolves a gateway UUID to its display-form short id.
// Used by card action handlers that only receive the gateway id from the
// button payload — we look the session up to recover the CLI id.
func (b *Bridge) displayIDFromGatewayID(gatewayID string) string {
	if sess, ok := b.mgr.Get(gatewayID); ok {
		return displaySessionID(sess)
	}
	return "gw:" + shortID(gatewayID)
}

func projectName(dir string) string {
	return filepath.Base(dir)
}

func channelKindToOrigin(kind string) string {
	switch kind {
	case channel.KindFeishu:
		return session.OriginFeishu
	case channel.KindDingTalk:
		return session.OriginDingTalk
	default:
		return kind
	}
}
