package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/google/uuid"
)

const defaultSummaryWindow = 5

// ListBy returns sessions that match the filter. Returns SessionInfo snapshots
// (not live pointers) so callers can iterate without holding the manager lock.
func (m *Manager) ListBy(f Filter) []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if sess == nil {
			continue
		}
		if f.matches(sess) {
			out = append(out, sess.Info())
		}
	}
	return out
}

// ListActiveByOwner returns sessions for ownerID with status active or idle.
func (m *Manager) ListActiveByOwner(ownerID string) []SessionInfo {
	return m.ListBy(Filter{OwnerID: ownerID, Statuses: []Status{StatusActive, StatusIdle}})
}

// ListArchivedByOwner returns archived sessions for ownerID.
func (m *Manager) ListArchivedByOwner(ownerID string) []SessionInfo {
	return m.ListBy(Filter{OwnerID: ownerID, Statuses: []Status{StatusArchived}})
}

// ListDiscoverableByOwner returns sessions the user can see: their own
// active/idle sessions plus, when shareExternal is true, all unowned external
// sessions (created via terminal etc., discovered from disk).
func (m *Manager) ListDiscoverableByOwner(ownerID string, shareExternal bool) []SessionInfo {
	owned := m.ListActiveByOwner(ownerID)
	if !shareExternal {
		return owned
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]SessionInfo{}, owned...)
	for _, s := range m.sessions {
		if s == nil {
			continue
		}
		if s.OwnerID == "" && s.Origin == OriginExternal && s.Status != StatusArchived {
			out = append(out, s.Info())
		}
	}
	return out
}

// GetByRuntimeID returns the (first) session whose RuntimeID matches.
func (m *Manager) GetByRuntimeID(runtimeID string) (*Session, bool) {
	if runtimeID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sess := range m.sessions {
		if sess != nil && sess.CLISessionID == runtimeID {
			return sess, true
		}
	}
	return nil, false
}

// SetLabel updates the session's user-facing label.
func (m *Manager) SetLabel(sessionID, label string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.Label = label
	sess.mu.Unlock()
	return nil
}

// SetCustomTitle sets the user-assigned display name for a session.
func (m *Manager) SetCustomTitle(sessionID, title string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.CustomTitle = title
	sess.mu.Unlock()
	return nil
}

// GetByThreadID returns the (first) session bound to the given Lark thread.
// Used by the bridge to route inbound thread messages to their session.
// Returns ok=false when no session is bound to that thread.
func (m *Manager) GetByThreadID(threadID string) (*Session, bool) {
	if threadID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sess := range m.sessions {
		if sess == nil {
			continue
		}
		sess.mu.Lock()
		match := sess.ThreadID == threadID
		sess.mu.Unlock()
		if match {
			return sess, true
		}
	}
	return nil, false
}

// BindThread binds a Lark thread to the session. rootMessageID is the thread
// root that future bot replies will anchor at. Idempotent — re-binding the
// same values is a no-op. Returns an error if the session does not exist.
func (m *Manager) BindThread(sessionID, threadID, rootMessageID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.ThreadID = threadID
	sess.RootMessageID = rootMessageID
	sess.mu.Unlock()
	return nil
}

// ClearThread removes the thread binding from a session. Used by the bridge
// when a Reply API call fails with anchor-missing (user deleted the thread
// root), so the next bot message falls back to the main chat.
func (m *Manager) ClearThread(sessionID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.ThreadID = ""
	sess.RootMessageID = ""
	sess.mu.Unlock()
	return nil
}

// SetCLISessionID eagerly stamps the CLI-assigned session ID on a gateway session.
// Used by /branch after pre-assigning a UUID via --session-id so the display
// is correct before the first KindInit event arrives.
func (m *Manager) SetCLISessionID(sessionID, cliSessionID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.CLISessionID = cliSessionID
	sess.mu.Unlock()
	return nil
}

// SetSummary updates the session's user-facing summary.
func (m *Manager) SetSummary(sessionID, summary string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.Summary = summary
	sess.turnsSinceSummary = 0
	sess.summaryPending = false
	sess.mu.Unlock()
	return nil
}

// SetChatID updates the session's transport-side channel identifier.
func (m *Manager) SetChatID(sessionID, chatID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.ChatID = chatID
	sess.mu.Unlock()
	return nil
}

// AppendRecentMessage records a user-sent message for summary computation.
// Truncates the content to 200 runes and keeps the most recent 5.
func (m *Manager) AppendRecentMessage(sessionID, content string) {
	sess, ok := m.Get(sessionID)
	if !ok {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	truncated := truncateRunes(content, 200)
	sess.recentMessages = append(sess.recentMessages, truncated)
	if len(sess.recentMessages) > defaultSummaryWindow {
		sess.recentMessages = sess.recentMessages[len(sess.recentMessages)-defaultSummaryWindow:]
	}
	sess.turnsSinceSummary++
	// LatestUserMessage is a UI decision-aid: when Summary is empty (short
	// session, worker hasn't run, or worker classified as _skip_meta_), the
	// list view shows this so the user can tell sessions apart by what they
	// last asked. Truncate to 80 runes to keep cards compact.
	sess.LatestUserMessage = truncateRunes(content, 80)
	if sess.Summary == "" {
		sess.Summary = truncateRunes(content, 30)
	}
}

// ShouldUpdateSummary returns true (and a snapshot of recent messages) if the
// session has accumulated enough turns to warrant a summary refresh, given
// the requested interval. Sets the "pending" flag so subsequent calls return
// false until SetSummary is invoked.
func (m *Manager) ShouldUpdateSummary(sessionID string, interval int) (bool, []string) {
	sess, ok := m.Get(sessionID)
	if !ok || interval <= 0 {
		return false, nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.summaryPending || len(sess.recentMessages) == 0 {
		return false, nil
	}
	needsUpdate := sess.turnsSinceSummary >= interval ||
		(sess.turnsSinceSummary >= 1 && sess.Summary == truncateRunes(sess.recentMessages[0], 30))
	if !needsUpdate {
		return false, nil
	}
	sess.summaryPending = true
	out := make([]string, len(sess.recentMessages))
	copy(out, sess.recentMessages)
	return true, out
}

// ClearSummaryPending releases the in-flight flag set by ShouldUpdateSummary
// when the caller's summary attempt failed. Without this, a single failure
// (network blip, admin error, etc.) permanently prevents future
// ShouldUpdateSummary calls from returning true for this session.
func (m *Manager) ClearSummaryPending(sessionID string) {
	sess, ok := m.Get(sessionID)
	if !ok {
		return
	}
	sess.mu.Lock()
	sess.summaryPending = false
	sess.mu.Unlock()
}

// SetFocus marks sessionID as the focused (active conversation) session for
// owner. Empty sessionID clears focus.
func (m *Manager) SetFocus(ownerID, sessionID string) error {
	if ownerID == "" {
		return fmt.Errorf("ownerID required")
	}
	if sessionID != "" {
		sess, ok := m.Get(sessionID)
		if !ok {
			return fmt.Errorf("session %s not found", sessionID)
		}
		if sess.OwnerID != ownerID {
			return fmt.Errorf("session %s does not belong to owner %s", sessionID, ownerID)
		}
		if sess.Status == StatusArchived {
			return fmt.Errorf("session %s is archived", sessionID)
		}
	}
	m.idx.setFocus(ownerID, sessionID)
	if sessionID != "" {
		if sess, ok := m.Get(sessionID); ok && sess.CLISessionID != "" {
			m.idx.setResumeHint(ownerID, sess.CLISessionID)
		}
	}
	return nil
}

// ClearFocus drops the focused-session pointer for owner without removing
// the session itself.
func (m *Manager) ClearFocus(ownerID string) {
	m.idx.setFocus(ownerID, "")
}

// FocusedSession returns the currently focused session for owner.
func (m *Manager) FocusedSession(ownerID string) (*Session, bool) {
	v := m.idx.view(ownerID)
	if v.FocusedID == "" {
		return nil, false
	}
	sess, ok := m.Get(v.FocusedID)
	if !ok || sess.Status == StatusArchived {
		return nil, false
	}
	return sess, true
}

// ResolveResumable picks the best session to auto-resume for owner.
// Preference order:
//  1. Any idle session (most recently created)
//  2. ResumeHint pointing to an idle/archived session (used after restart)
//  3. The most recently archived session
//
// Returns nil if no candidate exists.
// ClaimExternal converts an Origin="external" session into a managed one for
// the given owner. Used when a user explicitly resumes an external (terminal)
// session via the bridge UI — from that point on it's treated like any
// feishu-created session (gets a [feishu created] tag, appears in their
// per-owner list, etc.).
func (m *Manager) ClaimExternal(sessionID, ownerID, chatID, channelKind string) error {
	if ownerID == "" {
		return fmt.Errorf("ownerID required")
	}
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.mu.Lock()
	sess.OwnerID = ownerID
	switch channelKind {
	case "dingtalk":
		sess.Origin = OriginDingTalk
	default:
		sess.Origin = OriginFeishu
	}
	if chatID != "" {
		sess.ChatID = chatID
	}
	if channelKind != "" {
		sess.ChannelKind = channelKind
	}
	sess.mu.Unlock()
	m.idx.addSession(ownerID, sessionID)
	return nil
}

func (m *Manager) ResolveResumable(ownerID string) *Session {
	v := m.idx.view(ownerID)

	// External sessions (no owner, Origin=external) must NEVER be auto-resumed:
	// they belong to a terminal/SDK user and silently hijacking them would be
	// surprising. Admin sessions (Origin=admin) are gateway plumbing — also
	// never resumable on the user path. Thread-bound sessions ARE resumable
	// here (a session can be focused in main chat AND bound to a thread; the
	// thread is just an additional UI entry point, not an exclusive owner).
	isResumable := func(s *Session) bool {
		return s != nil && s.Origin != OriginExternal && s.Origin != OriginAdmin
	}

	// Prefer idle (process exited cleanly but state preserved in memory)
	for _, id := range v.SessionIDs {
		sess, ok := m.Get(id)
		if ok && sess.Status == StatusIdle && isResumable(sess) {
			return sess
		}
	}

	// Then ResumeHint (used after gateway restart when state is restored)
	if v.ResumeHintID != "" {
		for _, id := range v.SessionIDs {
			sess, ok := m.Get(id)
			if !ok || !isResumable(sess) {
				continue
			}
			if sess.CLISessionID == v.ResumeHintID && (sess.Status == StatusIdle || sess.Status == StatusArchived) {
				return sess
			}
		}
	}

	// Fallback: latest archived
	var latest *Session
	for _, id := range v.SessionIDs {
		sess, ok := m.Get(id)
		if !ok || sess.Status != StatusArchived || !isResumable(sess) {
			continue
		}
		if latest == nil || sess.ArchivedAt.After(latest.ArchivedAt) {
			latest = sess
		}
	}
	return latest
}

// ResumeHint returns the CLI session id recorded as the auto-resume hint.
func (m *Manager) ResumeHint(ownerID string) string {
	return m.idx.view(ownerID).ResumeHintID
}

// SetResumeHint overrides the auto-resume hint (used during state restore).
func (m *Manager) SetResumeHint(ownerID, cliSessionID string) {
	m.idx.setResumeHint(ownerID, cliSessionID)
}

// FindByPrefix looks up a session for owner by either:
//   - the leading characters of its gateway UUID (sess.ID)
//   - the leading characters of its CLI session id (sess.CLISessionID) —
//     this is what the UI displays (jsonl filename), so a user copying the
//     id from a card and pasting it into `/switch <id>` should just work
//   - exact-case-insensitive match on Label
//   - substring-case-insensitive match on Label
//
// Archived sessions are excluded.
func (m *Manager) FindByPrefix(ownerID, prefix string) (*Session, error) {
	lower := strings.ToLower(prefix)
	v := m.idx.view(ownerID)

	var exact, contains []*Session
	for _, id := range v.SessionIDs {
		sess, ok := m.Get(id)
		if !ok || sess.Status == StatusArchived {
			continue
		}
		idMatch := strings.HasPrefix(strings.ToLower(sess.ID), lower) ||
			(sess.CLISessionID != "" && strings.HasPrefix(strings.ToLower(sess.CLISessionID), lower))
		if idMatch || strings.EqualFold(sess.Label, prefix) {
			exact = append(exact, sess)
		} else if sess.Label != "" && strings.Contains(strings.ToLower(sess.Label), lower) {
			contains = append(contains, sess)
		}
	}

	switch len(exact) {
	case 1:
		return exact[0], nil
	case 0:
		// fall through
	default:
		return nil, fmt.Errorf("'%s' 匹配了 %d 个 session,请使用更长的前缀", prefix, len(exact))
	}

	switch len(contains) {
	case 0:
		return nil, fmt.Errorf("没有匹配 '%s' 的 session", prefix)
	case 1:
		return contains[0], nil
	default:
		return nil, fmt.Errorf("'%s' 匹配了 %d 个 session,请使用更精确的描述", prefix, len(contains))
	}
}

// FindArchivedByPrefix mirrors FindByPrefix but only considers archived sessions.
func (m *Manager) FindArchivedByPrefix(ownerID, prefix string) (*Session, error) {
	lower := strings.ToLower(prefix)
	v := m.idx.view(ownerID)

	var exact, contains []*Session
	for _, id := range v.SessionIDs {
		sess, ok := m.Get(id)
		if !ok || sess.Status != StatusArchived {
			continue
		}
		if strings.HasPrefix(strings.ToLower(sess.CLISessionID), lower) || strings.EqualFold(sess.Label, prefix) {
			exact = append(exact, sess)
		} else if sess.Label != "" && strings.Contains(strings.ToLower(sess.Label), lower) {
			contains = append(contains, sess)
		}
	}

	switch len(exact) {
	case 1:
		return exact[0], nil
	case 0:
	default:
		return nil, fmt.Errorf("'%s' 匹配了 %d 个归档 session,请使用更长的前缀", prefix, len(exact))
	}

	switch len(contains) {
	case 0:
		return nil, fmt.Errorf("没有匹配 '%s' 的归档 session", prefix)
	case 1:
		return contains[0], nil
	default:
		return nil, fmt.Errorf("'%s' 匹配了 %d 个归档 session,请使用更精确的描述", prefix, len(contains))
	}
}

// Archive marks a session as archived. If the session has an active runtime,
// it is stopped first. The session record itself is retained.
func (m *Manager) Archive(sessionID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	if sess.Status == StatusActive {
		// Best-effort stop (don't fail Archive on graceful-stop error)
		_ = sess.Close()
	}

	sess.mu.Lock()
	sess.Status = StatusArchived
	sess.ArchivedAt = time.Now()
	owner := sess.OwnerID
	sess.mu.Unlock()

	if owner != "" {
		v := m.idx.view(owner)
		if v.FocusedID == sessionID {
			m.idx.setFocus(owner, "")
		}
	}
	log.Printf("[manager] archived session %s", sessionID)
	return nil
}

// Reactivate restores an idle or archived session to active by spawning a new
// runtime instance that resumes the previous CLI session.
//
// Returns the (now-active) session; the returned pointer may differ from
// any prior reference since reactivation creates a fresh session record.
func (m *Manager) Reactivate(ctx context.Context, sessionID string) (*Session, error) {
	old, ok := m.Get(sessionID)
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	if old.Status == StatusActive {
		return old, nil
	}
	if old.CLISessionID == "" {
		return nil, fmt.Errorf("session %s has no runtime id, cannot resume", sessionID)
	}

	m.mu.Lock()
	if m.countActiveLocked() >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("max sessions (%d) reached", m.maxSessions)
	}
	placeholderID := "placeholder-" + fmt.Sprint(time.Now().UnixNano())
	m.sessions[placeholderID] = nil
	m.mu.Unlock()

	old.mu.Lock()
	label := old.Label
	summary := old.Summary
	customTitle := old.CustomTitle
	origin := old.Origin
	workingDir := old.WorkingDir
	chatID := old.ChatID
	channelKind := old.ChannelKind
	ownerID := old.OwnerID
	cliID := old.CLISessionID
	threadID := old.ThreadID
	rootMessageID := old.RootMessageID
	old.mu.Unlock()

	req := runtime.SpawnRequest{
		WorkingDir: workingDir,
		Config:     claude.Config{PermissionMode: m.defaultPermMode},
		ResumeID:   cliID,
	}
	sess, spawnErr := NewSession(m.rt, req, m.defaultPermMode, m.keepAliveInterval)

	m.mu.Lock()
	delete(m.sessions, placeholderID)
	if spawnErr != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("spawn for reactivate: %w", spawnErr)
	}
	sess.OwnerID = ownerID
	sess.Label = label
	sess.Summary = summary
	sess.CustomTitle = customTitle
	sess.Origin = origin
	sess.ChatID = chatID
	sess.ChannelKind = channelKind
	// Carry over the Lark thread binding so the reactivated session keeps
	// answering in the same thread. Without this, thread plain text would
	// stop routing here after a single reactivate (GetByThreadID would miss)
	// and main-chat /resume would gratuitously open a new thread instead of
	// pinging the existing one.
	sess.ThreadID = threadID
	sess.RootMessageID = rootMessageID
	// CLISessionID is normally populated when the runtime emits its init
	// message, but for resume we already know it (it's what we passed to
	// --resume). Set it eagerly so operations that need it — SwitchModel,
	// persistence, /status — don't race the CLI's startup. The init
	// callback will overwrite with the same value when it arrives.
	sess.CLISessionID = cliID
	m.sessions[sess.ID] = sess

	// Remove the old idle/archived placeholder
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if ownerID != "" {
		m.idx.removeSession(ownerID, sessionID)
		m.idx.addSession(ownerID, sess.ID)
		m.idx.setFocus(ownerID, sess.ID)
		m.idx.setResumeHint(ownerID, cliID)
	}

	log.Printf("[manager] reactivated session %s → %s (owner=%s, runtime_session=%s)", sessionID, sess.ID, ownerID, cliID)
	return sess, nil
}

// TransitionToIdle is called when a session's runtime exits cleanly. The
// session record is retained and switched to StatusIdle so it can be
// reactivated later.
func (m *Manager) TransitionToIdle(sessionID string) {
	sess, ok := m.Get(sessionID)
	if !ok {
		return
	}
	sess.mu.Lock()
	if sess.Status != StatusActive {
		sess.mu.Unlock()
		return
	}
	sess.Status = StatusIdle
	owner := sess.OwnerID
	cliID := sess.CLISessionID
	sess.mu.Unlock()

	if owner != "" {
		v := m.idx.view(owner)
		if v.FocusedID == sessionID {
			m.idx.setFocus(owner, "")
		}
		if cliID != "" {
			m.idx.setResumeHint(owner, cliID)
		}
	}
	log.Printf("[manager] session %s → idle", sessionID)
}

// ImportIdleSession adds an idle session record without spawning any runtime.
// Used by Restore() during startup. Returns the new session.ID.
func (m *Manager) ImportIdleSession(opts ImportOpts) (string, error) {
	return m.importSession(opts, StatusIdle)
}

// ImportArchivedSession adds an archived session record without spawning any
// runtime. Used by Restore() during startup.
func (m *Manager) ImportArchivedSession(opts ImportOpts) (string, error) {
	return m.importSession(opts, StatusArchived)
}

// ImportOpts carries the fields needed to import an idle/archived session
// from persistent storage. CLISessionID is required so the session can be
// reactivated later.
type ImportOpts struct {
	CLISessionID      string
	OwnerID           string
	Label             string
	Summary           string
	CustomTitle       string
	LatestUserMessage string
	Origin            string
	WorkingDir        string
	ChatID            string
	ChannelKind       string
	ThreadID          string
	RootMessageID     string
	MessageCount      int
	CreatedAt         time.Time
	ArchivedAt        time.Time
	LastActivity      time.Time
}

func (m *Manager) importSession(opts ImportOpts, status Status) (string, error) {
	if opts.CLISessionID == "" {
		return "", fmt.Errorf("CLISessionID required for import")
	}
	// If an existing session for this owner already has the same
	// CLISessionID, update its metadata (label/summary/workingDir/chatID)
	// from the new opts and return the existing ID. This matches the
	// behavior of the legacy map-based UserState which let later writes
	// override earlier ones.
	if opts.OwnerID != "" {
		v := m.idx.view(opts.OwnerID)
		for _, id := range v.SessionIDs {
			sess, ok := m.Get(id)
			if !ok || sess.CLISessionID != opts.CLISessionID {
				continue
			}
			sess.mu.Lock()
			if opts.Label != "" {
				sess.Label = opts.Label
			}
			if opts.Summary != "" {
				sess.Summary = opts.Summary
			}
			if opts.CustomTitle != "" {
				sess.CustomTitle = opts.CustomTitle
			}
			if opts.LatestUserMessage != "" {
				sess.LatestUserMessage = opts.LatestUserMessage
			}
			if opts.Origin != "" {
				sess.Origin = opts.Origin
			}
			if opts.WorkingDir != "" {
				sess.WorkingDir = opts.WorkingDir
			}
			if opts.ChatID != "" {
				sess.ChatID = opts.ChatID
			}
			if opts.ChannelKind != "" {
				sess.ChannelKind = opts.ChannelKind
			}
			if opts.ThreadID != "" {
				sess.ThreadID = opts.ThreadID
			}
			if opts.RootMessageID != "" {
				sess.RootMessageID = opts.RootMessageID
			}
			if opts.MessageCount > 0 {
				sess.MessageCount = opts.MessageCount
			}
			sess.Status = status
			if !opts.ArchivedAt.IsZero() {
				sess.ArchivedAt = opts.ArchivedAt
			}
			sess.mu.Unlock()
			return id, nil
		}
	}

	sess := &Session{
		ID:                uuid.New().String(),
		CLISessionID:      opts.CLISessionID,
		OwnerID:           opts.OwnerID,
		Label:             opts.Label,
		Summary:           opts.Summary,
		CustomTitle:       opts.CustomTitle,
		LatestUserMessage: opts.LatestUserMessage,
		Origin:            opts.Origin,
		WorkingDir:        opts.WorkingDir,
		ChatID:            opts.ChatID,
		ChannelKind:       opts.ChannelKind,
		ThreadID:          opts.ThreadID,
		RootMessageID:     opts.RootMessageID,
		MessageCount:      opts.MessageCount,
		Status:            status,
		CreatedAt:         coalesceTime(opts.CreatedAt),
		ArchivedAt:        opts.ArchivedAt,
		lastActivity:      coalesceTime(opts.LastActivity),
		state:             StateStopped,
		subscribers:       make(map[string]*Subscriber),
	}

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	if opts.OwnerID != "" {
		m.idx.addSession(opts.OwnerID, sess.ID)
	}
	return sess.ID, nil
}

// RemoveArchived deletes an archived session record permanently.
func (m *Manager) RemoveArchived(sessionID string) error {
	sess, ok := m.Get(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if sess.Status != StatusArchived {
		return fmt.Errorf("session %s is not archived (status=%s)", sessionID, sess.Status)
	}
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if sess.OwnerID != "" {
		m.idx.removeSession(sess.OwnerID, sessionID)
	}
	return nil
}

// AllOwners returns the set of owner IDs known to the manager.
func (m *Manager) AllOwners() []string {
	return m.idx.allOwners()
}

// truncateRunes returns s truncated to maxRunes runes plus "..." if it was cut.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func coalesceTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
