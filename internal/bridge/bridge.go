// Package bridge connects an IM channel (channel.Channel) to the session
// manager. It handles inbound user messages by either forwarding them to the
// focused session or dispatching slash commands; it subscribes to session
// events and renders them as outbound cards.
package bridge

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/plan"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

// Options carries everything Bridge needs to operate. Required fields:
// Manager, Channel, DefaultCWD. The rest have sensible defaults.
type Options struct {
	Manager     *session.Manager
	Channel     channel.Channel
	DefaultCWD  string
	ProjectRoot string

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
}

// Bridge wires together a channel.Channel and a session.Manager.
type Bridge struct {
	mgr *session.Manager
	ch  channel.Channel

	defaultCWD      string
	projectRoot     string
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

	mu                  sync.Mutex
	subscribed          map[string]bool
	pendingElicitations map[string]*PendingElicitation
	// terminating tracks session IDs the user explicitly /terminate-d, so
	// the CLI-exit handler can distinguish "user-initiated stop" (silent)
	// from an unexpected drop (notify).
	terminating    map[string]bool
	discoveryStats DiscoveryStats
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
	if opts.ProjectRoot == "" {
		opts.ProjectRoot = opts.DefaultCWD
	}
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
		projectRoot:         opts.ProjectRoot,
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
	}
	if opts.AdminModel != "" {
		b.admin = newAdmin(opts.Manager, opts.DefaultCWD, opts.AdminModel)
		b.worker = newSummaryWorker(opts.Manager, b.admin, opts.Persister)
	}
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
	b.ensureSubscribed(ctx, sess, m)

	_ = b.ch.Reaction(m.MessageID, "OnIt")

	if err := sess.SendMessage(text); err != nil {
		b.sendText(ctx, m.ChatID, "发送消息失败: "+err.Error())
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
	b.ensureSubscribed(ctx, sess, m)

	_ = b.ch.Reaction(m.MessageID, "OnIt")

	if err := sess.SendMessageBlocks(m.Blocks); err != nil {
		b.sendText(ctx, m.ChatID, "发送图片消息失败: "+err.Error())
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
	// 1. Focused session active in memory
	if sess, ok := b.mgr.FocusedSession(m.UserID); ok {
		if live, alive := b.mgr.Get(sess.ID); alive {
			return live, true
		}
	}
	// 2. Resumable session (idle preferred)
	if resumable := b.mgr.ResolveResumable(m.UserID); resumable != nil {
		newSess, err := b.mgr.Reactivate(ctx, resumable.ID)
		if err == nil {
			b.sendText(ctx, m.ChatID, "已自动恢复 session "+displaySessionID(newSess))
			b.saveStateIfPossible()
			return newSess, true
		}
		log.Printf("[bridge] auto-resume %s failed: %v", displaySessionID(resumable), err)
	}
	// 3. Create new
	sess, err := b.mgr.Create(ctx, session.CreateOpts{
		WorkingDir:  b.defaultCWD,
		OwnerID:     m.UserID,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
		Origin:      session.OriginFeishu,
	})
	if err != nil {
		b.sendText(ctx, m.ChatID, "自动创建 session 失败: "+err.Error())
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

// needsSetup returns true when the bridge hasn't been configured with a
// working directory yet (initial install).
func (b *Bridge) needsSetup() bool {
	return b.defaultCWD == "" || b.defaultCWD == "."
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
