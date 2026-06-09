package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/protocol"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

const (
	maxCardTextLen     = 15000
	cardUpdateInterval = 1500 * time.Millisecond
)

// streamSession subscribes to a session's events and renders them as
// outbound cards. Runs until the session's subscriber channel is closed.
func (b *Bridge) streamSession(ctx context.Context, sess *session.Session, chatID string) {
	subID := "bridge-" + sess.ID
	ch := sess.Subscribe(subID)
	defer sess.Unsubscribe(subID)

	state := &streamState{
		sess:         sess,
		project:      projectName(sess.WorkingDir),
		sessionShort: displaySessionID(sess),
		gitBranch:    getGitBranch(sess.WorkingDir),
		contextPct:   0,
	}

	for {
		select {
		case <-ctx.Done():
			state.flush()
			return
		case raw, ok := <-ch:
			if !ok {
				state.flush()
				b.handleCLIExit(ctx, sess, chatID)
				return
			}
			b.handleSessionEvent(ctx, sess, chatID, state, raw)
		}
	}
}

func (b *Bridge) handleCLIExit(ctx context.Context, sess *session.Session, chatID string) {
	cliID := sess.Info().CLISessionID
	// If the user explicitly /terminate-d this session, the command itself
	// already replied with a tailored message. Suppress the generic
	// "session idle (发消息自动恢复)" so the two don't contradict each other.
	b.mu.Lock()
	userTerminated := b.terminating[sess.ID]
	delete(b.terminating, sess.ID)
	b.mu.Unlock()

	if cliID != "" {
		b.mgr.TransitionToIdle(sess.ID)
		if !userTerminated {
			b.sendTextForSession(ctx, sess, chatID, "Session "+displaySessionID(sess)+" 已变为 idle(发消息自动恢复)")
		}
	} else {
		b.sendTextForSession(ctx, sess, chatID, "Session "+displaySessionID(sess)+" 已断开(CLI 未返回 session ID,无法保留)")
		_ = b.mgr.Destroy(sess.ID)
	}
	b.mu.Lock()
	delete(b.subscribed, sess.ID)
	b.mu.Unlock()
}

func (b *Bridge) handleSessionEvent(ctx context.Context, sess *session.Session, chatID string, state *streamState, raw json.RawMessage) {
	if gwEvent, ok := extractGatewayEvent(raw); ok {
		switch gwEvent {
		case "turn_status", "session_exit":
			return
		}
	}

	msgType, _, err := protocol.ParseType(raw)
	if err != nil {
		return
	}

	// Stamp activity for every recognized event so the HUD's elapsed /
	// "since last event" reflect real CLI activity, not just text emissions.
	state.noteEvent()

	switch msgType {
	case protocol.MsgTypeSystem:
		var sysMsg protocol.SystemInitMessage
		if json.Unmarshal(raw, &sysMsg) == nil && sysMsg.Subtype == protocol.SubtypeInit {
			state.mu.Lock()
			state.model = sysMsg.Model
			// init carries the CLI session id; refresh sessionShort so cards
			// stop showing the placeholder gw:xxxx id.
			state.sessionShort = displaySessionID(sess)
			state.mu.Unlock()
			// Messages sent while the CLI was still starting up (StateStarting)
			// have been sitting in the OS pipe buffer; show an early progress
			// card now so the user sees "Processing…" instead of silence for
			// the ~40 s between ✅ reaction and the eventual Done card.
			if sess.Info().PendingTurns > 0 {
				state.ensureProgressCard(ctx, b, chatID)
			}
		}
	case protocol.MsgTypeAssistant:
		// Extract usage from the last API response to track context window usage.
		// result.Usage is the sum of ALL turns in the invocation and overcounts
		// cache_read tokens when there are multiple turns — the assistant message's
		// inner usage reflects the actual last-call context size.
		updateContextPctFromAssistant(state, raw)
		// Capture the latest tool_use (if any) so the HUD can surface
		// "running Bash · tcpdump…" instead of a generic "processing".
		// A text-only assistant message means the model just spoke without
		// invoking a tool — clear the indicator so the HUD doesn't show
		// a stale tool name during the model's monologue.
		toolLabel := extractCurrentTool(raw)
		if toolLabel != "" {
			state.setCurrentTool(toolLabel)
		} else if hasOnlyText(raw) {
			state.setCurrentTool("")
		}
		if items := extractTodoItems(raw); items != nil {
			state.mu.Lock()
			state.todos = items
			state.mu.Unlock()
		}
		text := extractAssistantText(raw)
		if text != "" {
			state.appendText(ctx, b, chatID, text)
			return
		}
		// No user-visible text in this turn-step yet — the model may be
		// thinking and going straight to a tool. Without an empty-text
		// shortcut the user would see nothing for as long as the tool
		// runs (Bash with multi-minute commands is the canonical case).
		// Render a tool-only progress card so the HUD ("🔧 Bash · …",
		// elapsed clock, heartbeat ticker) starts moving immediately.
		if toolLabel != "" {
			state.ensureProgressCard(ctx, b, chatID)
		}
	case protocol.MsgTypeResult:
		var result protocol.ResultMessage
		if err := json.Unmarshal(raw, &result); err != nil {
			return
		}
		state.finalize(ctx, b, chatID, &result)
	case protocol.MsgTypeControlRequest:
		b.handleControlRequest(ctx, sess, chatID, raw)
	}
}

// streamState batches assistant chunks and re-renders one card per interval.
type streamState struct {
	mu           sync.Mutex
	sess         *session.Session // owning session — used to anchor outbound to its thread (if any)
	messageID    string
	project      string
	sessionShort string
	summary      string
	model        string
	gitBranch    string
	contextPct   int // 0 = unknown/first turn; updated after each assistant message
	textBuf      strings.Builder
	lastUpdate   time.Time
	dirty        bool
	timer        *time.Timer
	finalized    bool

	// Per-turn lifetime — reset on finalize so a long-lived session doesn't
	// inherit elapsed counters from previous turns.
	turnStart     time.Time // first event of the current turn
	lastEvent     time.Time // most recent event (text/tool/etc.) for "Ys ago"
	currentTool   string    // e.g. "Bash · tcpdump…" (empty between tools)
	heartbeatStop chan struct{}

	// Session-level todo list; updated on every TodoWrite call and persists
	// across turns so the Done card always shows the latest state.
	todos []todoItem
}

// startHeartbeat ensures a background goroutine is re-rendering the
// processing card on a fixed cadence (in addition to event-driven updates).
// Without it, a turn that triggers a single long-running tool can leave the
// HUD frozen for many minutes — the user can't tell whether the agent is
// still alive. Called by appendText after the first card lands; safe to
// call repeatedly (subsequent calls are no-ops while one is already running).
//
// Concurrency: caller MUST hold s.mu. The heartbeatStop field is read and
// written here without taking the mutex; relying on the caller's lock keeps
// startHeartbeat / stopHeartbeat / appendText / finalize / flush serialized.
func (s *streamState) startHeartbeat(ctx context.Context, b *Bridge, chatID string) {
	if s.heartbeatStop != nil {
		return
	}
	stop := make(chan struct{})
	s.heartbeatStop = stop
	go func() {
		// 45s strikes a balance — frequent enough that "still alive" is
		// obvious in the UI, but well under Lark's update API throttling
		// and far less chatty than per-second.
		t := time.NewTicker(45 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.renderHeartbeat(ctx, b, chatID)
			}
		}
	}()
}

// stopHeartbeat signals the heartbeat goroutine to exit. Idempotent.
// Concurrency: caller MUST hold s.mu (see startHeartbeat).
func (s *streamState) stopHeartbeat() {
	if s.heartbeatStop == nil {
		return
	}
	close(s.heartbeatStop)
	s.heartbeatStop = nil
}

// renderHeartbeat re-issues the processing card with refreshed timestamps
// in the HUD. No content change — just a beat so the user sees the clock
// move and (when present) the current tool name stays accurate.
func (s *streamState) renderHeartbeat(ctx context.Context, b *Bridge, chatID string) {
	s.mu.Lock()
	if s.finalized || s.messageID == "" {
		s.mu.Unlock()
		return
	}
	msgID := s.messageID
	content := truncate(s.textBuf.String())
	if content == "" {
		content = "_(thinking · running tool…)_"
	}
	card := b.processingCardWithProgress(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, content, s.currentTool, s.todos, s.turnStart, s.lastEvent)
	s.mu.Unlock()
	_ = b.updateCard(ctx, msgID, card)
}

// noteEvent stamps the per-turn timestamps. Called by handleSessionEvent
// for every CLI event so elapsed / "Ys ago" reflect real activity rather
// than just text emissions.
func (s *streamState) noteEvent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.turnStart.IsZero() {
		s.turnStart = now
	}
	s.lastEvent = now
}

// ensureProgressCard sends an initial Processing card when the model has
// committed to a tool call but produced no user-visible text yet.
// Without this, a turn that opens with a long-running tool (Bash with a
// minute-long find / WebFetch / multi-step Grep) leaves the user
// staring at silence — the existing appendText-driven card creation
// only fires on text events.
//
// Called from the MsgTypeAssistant branch when text=="" but a tool_use
// exists. Subsequent appendText calls reuse the same messageID and the
// heartbeat ticker keeps the card alive.
func (s *streamState) ensureProgressCard(ctx context.Context, b *Bridge, chatID string) {
	s.mu.Lock()
	if s.finalized || s.messageID != "" {
		s.mu.Unlock()
		return
	}
	if strings.HasPrefix(s.sessionShort, "gw:") {
		s.sessionShort = displaySessionID(s.sess)
	}
	// Empty body — the HUD note carries everything user-visible
	// (current tool, elapsed). The body fills in once assistant text
	// arrives.
	card := b.processingCardWithProgress(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, "_(thinking · running tool…)_", s.currentTool, s.todos, s.turnStart, s.lastEvent)
	s.mu.Unlock()

	msgID, err := b.sendCardForSession(ctx, s.sess, chatID, card)
	if err != nil {
		return
	}

	s.mu.Lock()
	// Re-check finalized: could have raced with a concurrent finalize.
	if s.finalized {
		s.mu.Unlock()
		return
	}
	s.messageID = msgID
	s.lastUpdate = time.Now()
	s.startHeartbeat(ctx, b, chatID)
	s.mu.Unlock()
}

// setCurrentTool updates the visible tool indicator. Pass "" to clear
// (e.g. when a tool_result arrives signalling completion).
func (s *streamState) setCurrentTool(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentTool = label
}

func (s *streamState) appendText(ctx context.Context, b *Bridge, chatID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}
	// Defensive refresh: if sessionShort was captured before the CLI emitted
	// init (assistant text race), it would still be the "gw:xxx" placeholder.
	// Re-resolve from the live session so cards never show stale ids.
	if strings.HasPrefix(s.sessionShort, "gw:") {
		s.sessionShort = displaySessionID(s.sess)
	}
	s.textBuf.WriteString(text)
	s.dirty = true

	if s.messageID == "" {
		content := truncate(s.textBuf.String())
		card := b.processingCardWithProgress(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, content, s.currentTool, s.todos, s.turnStart, s.lastEvent)
		msgID, err := b.sendCardForSession(ctx, s.sess, chatID, card)
		if err != nil {
			return
		}
		s.messageID = msgID
		s.lastUpdate = time.Now()
		s.dirty = false
		// Now that there's a card to refresh, start the background
		// heartbeat. Calls are idempotent so the cost of misfiring is
		// nil — only one goroutine ever runs per turn.
		s.startHeartbeat(ctx, b, chatID)
		return
	}

	if s.timer != nil {
		return
	}
	elapsed := time.Since(s.lastUpdate)
	delay := cardUpdateInterval - elapsed
	if delay < 0 {
		delay = 0
	}
	s.timer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.finalized || !s.dirty || s.messageID == "" {
			s.timer = nil
			return
		}
		s.timer = nil
		content := truncate(s.textBuf.String())
		card := b.processingCardWithProgress(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, content, s.currentTool, s.todos, s.turnStart, s.lastEvent)
		_ = b.updateCard(ctx, s.messageID, card)
		s.lastUpdate = time.Now()
		s.dirty = false
	})
}

func (s *streamState) finalize(ctx context.Context, b *Bridge, chatID string, result *protocol.ResultMessage) {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.finalized = true
	s.stopHeartbeat()
	msgID := s.messageID
	buffered := s.textBuf.String()
	project := s.project
	sessionShort := s.sessionShort
	summary := s.summary
	model := s.model
	gitBranch := s.gitBranch
	contextPct := s.contextPct
	todos := s.todos
	s.mu.Unlock()

	if result.IsError && msgID == "" && buffered == "" {
		// no user-visible output; suppress
		s.mu.Lock()
		s.messageID = ""
		s.textBuf.Reset()
		s.dirty = false
		s.finalized = false
		s.mu.Unlock()
		return
	}

	content := result.Result
	if content == "" {
		content = buffered
	}
	content = truncate(content)

	// If the user just /stop-ed this session, the CLI exits with IsError=true
	// even though that's exactly what they asked for. Consume the flag here
	// so resultCardWithID can render a neutral "Stopped" card instead of a
	// scary red "Error".
	interrupted := s.sess != nil && s.sess.ConsumeInterruptedFlag()

	card := b.resultCardWithIDAndInterrupt(project, sessionShort, summary, model, gitBranch, contextPct, content, todos, result, interrupted)
	if msgID != "" {
		if err := b.updateFinalCardForSession(ctx, msgID, s.sess, card); err != nil {
			_, _ = b.sendFinalCardForSession(ctx, s.sess, chatID, card)
		}
	} else {
		_, _ = b.sendFinalCardForSession(ctx, s.sess, chatID, card)
	}
	b.clearPendingReaction(s.sess)

	s.mu.Lock()
	s.messageID = ""
	s.textBuf.Reset()
	s.dirty = false
	s.finalized = false
	// Per-turn lifetime ends here — clear so the next turn's heartbeat
	// reports a fresh elapsed counter and doesn't carry stale tool labels.
	s.turnStart = time.Time{}
	s.lastEvent = time.Time{}
	s.currentTool = ""
	s.mu.Unlock()
}

func (s *streamState) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	// stop the heartbeat goroutine too — flush() is the catch-all
	// shutdown path (ctx cancellation, subscriber channel close, session
	// reactivate), and a lingering ticker would keep firing renders
	// against a finalized state forever.
	s.stopHeartbeat()
}

func (b *Bridge) processingCard(project, summary, model, gitBranch string, contextPct int, content string) channel.Card {
	return b.processingCardWithID(project, "", summary, model, gitBranch, contextPct, content)
}

// processingCardWithID variant that suffixes the session display id to the
// card title (e.g. "Processing: claude-code-gateway · 6befadec"). The id
// suffix lets users running multiple parallel sessions identify which card
// belongs to which session at a glance, and copy the id for /switch.
func (b *Bridge) processingCardWithID(project, sessionShort, summary, model, gitBranch string, contextPct int, content string) channel.Card {
	return b.processingCardWithProgress(project, sessionShort, summary, model, gitBranch, contextPct, content, "", nil, time.Time{}, time.Time{})
}

// processingCardWithProgress is the full-fat variant used by streamSession
// — same shape as processingCardWithID but with the "still alive" signals
// (current tool, turn-start, last-event) woven into the HUD note so a
// user staring at the card for 17 minutes can tell whether the agent is
// still working or stuck.
func (b *Bridge) processingCardWithProgress(project, sessionShort, summary, model, gitBranch string, contextPct int, content, currentTool string, todos []todoItem, turnStart, lastEvent time.Time) channel.Card {
	title := "Processing"
	if project != "" {
		title += ": " + project
	}
	if sessionShort != "" {
		title += " · " + sessionShort
	}
	hud := buildProcessingHUDNote(model, gitBranch, contextPct, currentTool, turnStart, lastEvent)
	note := hud
	if summary != "" {
		note = summary + " | " + hud
	}
	var sections []channel.Section
	if content != "" {
		sections = append(sections, channel.Section{Markdown: content})
	}
	if md := renderTodosMarkdown(todos); md != "" {
		sections = append(sections, channel.Section{Markdown: md})
	}
	sections = append(sections, channel.Section{Note: note})
	return channel.Card{
		Title:    title,
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func (b *Bridge) resultCard(project, summary, model, gitBranch string, contextPct int, content string, result *protocol.ResultMessage) channel.Card {
	return b.resultCardWithID(project, "", summary, model, gitBranch, contextPct, content, result)
}

func (b *Bridge) resultCardWithID(project, sessionShort, summary, model, gitBranch string, contextPct int, content string, result *protocol.ResultMessage) channel.Card {
	return b.resultCardWithIDAndInterrupt(project, sessionShort, summary, model, gitBranch, contextPct, content, nil, result, false)
}

// resultCardWithIDAndInterrupt is the parameterized form used by streamSession.
// interrupted=true means /stop fired before the CLI exited; render as a neutral
// "Stopped" card instead of a red "Error" card so the UX matches user intent.
func (b *Bridge) resultCardWithIDAndInterrupt(project, sessionShort, summary, model, gitBranch string, contextPct int, content string, todos []todoItem, result *protocol.ResultMessage, interrupted bool) channel.Card {
	title := "Done"
	tone := channel.ToneSuccess
	switch {
	case interrupted:
		title = "Stopped"
		tone = channel.ToneNeutral
	case result.IsError:
		title = "Error"
		tone = channel.ToneError
	}
	if project != "" {
		title += ": " + project
	}
	if sessionShort != "" {
		title += " · " + sessionShort
	}
	duration := fmt.Sprintf("%.1fs", float64(result.DurationMS)/1000)
	cost := fmt.Sprintf("$%.4f", result.TotalCostUSD)
	stats := fmt.Sprintf("%s | %d turns | %s", duration, result.NumTurns, cost)
	hudCtxPct := contextPct
	if result.NumTurns == 0 {
		hudCtxPct = -1
	}
	hud := buildHUDNote(model, gitBranch, hudCtxPct, stats)
	note := hud
	if summary != "" {
		note = summary + " | " + hud
	}
	var sections []channel.Section
	if content != "" {
		sections = append(sections, channel.Section{Markdown: content})
	}
	if md := renderTodosMarkdown(todos); md != "" {
		sections = append(sections, channel.Section{Markdown: md})
	}
	sections = append(sections, channel.Section{Divider: true, Note: note})
	return channel.Card{
		Title:    title,
		Tone:     tone,
		Sections: sections,
	}
}

// updateContextPctFromAssistant reads the Anthropic API usage embedded in an
// assistant message and updates state.contextPct. Using the per-message usage
// (rather than result.Usage which sums all turns) gives the actual context
// window fill for the last API call.
func updateContextPctFromAssistant(state *streamState, raw json.RawMessage) {
	var msg struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	var apiMsg struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(msg.Message, &apiMsg) != nil {
		return
	}
	total := apiMsg.Usage.InputTokens + apiMsg.Usage.CacheReadInputTokens + apiMsg.Usage.CacheCreationInputTokens
	if total == 0 {
		return
	}
	state.mu.Lock()
	model := state.model
	state.mu.Unlock()
	window := contextWindowForModel(model)
	if window > 0 {
		pct := total * 100 / window
		if pct > 100 {
			pct = 100
		}
		state.mu.Lock()
		state.contextPct = pct
		state.mu.Unlock()
	}
}

func buildHUDNote(model, gitBranch string, contextPct int, suffix string) string {
	var parts []string
	if model != "" {
		parts = append(parts, "["+shortModelName(model)+"]")
	}
	if gitBranch != "" {
		parts = append(parts, "git:("+gitBranch+")")
	}
	if contextPct >= 0 {
		parts = append(parts, hudContextBar(contextPct))
	}
	hud := strings.Join(parts, " │ ")
	if suffix == "" {
		return hud
	}
	if hud == "" {
		return suffix
	}
	return hud + " | " + suffix
}

// buildProcessingHUDNote extends the standard HUD with two "still alive"
// indicators consumed by the in-progress card:
//   - currentTool: e.g. "🔧 Bash · tcpdump…" when known; falls back to
//     "处理中..." so the suffix is always present.
//   - turnStart / lastEvent: produces "已运行 17m · 30s ago" so the user
//     can tell whether the card is fresh or stale at a glance.
//
// All extras are appended as additional " | "-separated cells onto the
// HUD line so the layout stays one line — flagged as the preferred
// design in the test card sent to the user.
func buildProcessingHUDNote(model, gitBranch string, contextPct int, currentTool string, turnStart, lastEvent time.Time) string {
	suffix := "处理中..."
	if currentTool != "" {
		suffix = "🔧 " + currentTool
	}
	if !turnStart.IsZero() {
		elapsed := time.Since(turnStart)
		suffix += " | 已运行 " + formatShortDuration(elapsed)
		if !lastEvent.IsZero() && time.Since(lastEvent) >= 5*time.Second {
			suffix += " · " + formatShortDuration(time.Since(lastEvent)) + " ago"
		}
	}
	return buildHUDNote(model, gitBranch, contextPct, suffix)
}

// formatShortDuration renders a duration in the most compact form that
// still conveys the right magnitude: "30s", "5m", "1h12m". Avoids the
// noisy "1h12m34s" you get from time.Duration.String().
func formatShortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// todoItem mirrors a single entry from Claude Code's TodoWrite tool output.
type todoItem struct {
	Content string
	Status  string // "pending", "in_progress", "completed"
}

// extractTodoItems returns the todo list written by a TodoWrite tool_use block
// in an assistant message, or nil when no TodoWrite call is present.
func extractTodoItems(raw json.RawMessage) []todoItem {
	var msg struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var apiMsg struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msg.Message, &apiMsg); err != nil {
		return nil
	}
	for i := len(apiMsg.Content) - 1; i >= 0; i-- {
		c := apiMsg.Content[i]
		if c.Type != "tool_use" || c.Name != "TodoWrite" {
			continue
		}
		var input struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		if err := json.Unmarshal(c.Input, &input); err != nil {
			return nil
		}
		items := make([]todoItem, 0, len(input.Todos))
		for _, t := range input.Todos {
			items = append(items, todoItem{Content: t.Content, Status: t.Status})
		}
		return items
	}
	return nil
}

// renderTodosMarkdown converts a todo list to a Markdown string using emoji
// status indicators. Returns "" when the slice is empty.
func renderTodosMarkdown(todos []todoItem) string {
	if len(todos) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, t := range todos {
		switch t.Status {
		case "completed":
			fmt.Fprintf(&sb, "✅ ~~%s~~\n", t.Content)
		case "in_progress":
			fmt.Fprintf(&sb, "⏳ **%s**\n", t.Content)
		default:
			fmt.Fprintf(&sb, "🔲 %s\n", t.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// extractCurrentTool finds the most recent tool_use block in an assistant
// stream-json message and returns a short, HUD-friendly label.
// Example outputs:
//
//	Bash · tcpdump -i en0 …
//	Edit · cards.go
//	Read · /Users/xmly/…/renderer.go
//
// Returns "" when no tool_use block is present (pure text turn) so the
// caller can decide whether to clear or leave the previous label.
func extractCurrentTool(raw json.RawMessage) string {
	var msg struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var apiMsg struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msg.Message, &apiMsg); err != nil {
		return ""
	}
	for i := len(apiMsg.Content) - 1; i >= 0; i-- {
		c := apiMsg.Content[i]
		if c.Type != "tool_use" || c.Name == "" {
			continue
		}
		return c.Name + summarizeToolInput(c.Name, c.Input)
	}
	return ""
}

// summarizeToolInput pulls a brief, single-line preview out of a tool's
// input bag — picks the field most useful for telling tools apart at a
// glance (Bash → command, Edit/Read/Write → file_path, etc.). Returns
// "" when there's nothing useful to add (caller renders just the name).
func summarizeToolInput(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	var preview string
	switch toolName {
	case "Bash":
		preview = pick("command")
	case "Edit", "Write", "Read", "NotebookEdit":
		preview = pick("file_path", "notebook_path")
		// strip leading directories so the HUD shows a recognizable basename
		if idx := strings.LastIndex(preview, "/"); idx >= 0 {
			preview = preview[idx+1:]
		}
	case "Grep", "Glob":
		preview = pick("pattern", "query")
	case "WebFetch", "WebSearch":
		preview = pick("url", "query")
	default:
		// Best-effort: try the most common keys.
		preview = pick("command", "file_path", "pattern", "query", "url", "description")
	}
	if preview == "" {
		return ""
	}
	const cap = 32
	if len([]rune(preview)) > cap {
		preview = string([]rune(preview)[:cap]) + "…"
	}
	preview = strings.ReplaceAll(preview, "\n", " ")
	return " · " + preview
}

// hasOnlyText reports whether an assistant stream-json message contains
// only text content blocks (no tool_use). When true, the model spoke
// without invoking a tool — caller should clear any lingering tool
// indicator since the previous tool round-trip has completed and the
// model has resumed talking.
func hasOnlyText(raw json.RawMessage) bool {
	var msg struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}
	var apiMsg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msg.Message, &apiMsg); err != nil {
		return false
	}
	if len(apiMsg.Content) == 0 {
		return false
	}
	for _, c := range apiMsg.Content {
		if c.Type != "text" {
			return false
		}
	}
	return true
}

// shortModelName strips the "claude-" prefix for display brevity.
// "claude-sonnet-4-6" → "sonnet-4-6"; "us.amazon.claude-opus-4-7" → "opus-4-7"
func shortModelName(model string) string {
	if idx := strings.Index(model, "claude-"); idx >= 0 {
		return model[idx+len("claude-"):]
	}
	return model
}

// hudContextBar renders a 10-char bar plus percentage: "████░░░░░░ 45%"
// Pass pct=-1 to render an empty bar with "compacted" label instead of a number.
func hudContextBar(pct int) string {
	if pct == -1 {
		return strings.Repeat("░", 10) + " compacted"
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct / 10
	return strings.Repeat("█", filled) + strings.Repeat("░", 10-filled) + fmt.Sprintf(" %d%%", pct)
}

// contextWindowForModel returns the context window size in tokens for a given
// model name. All current Claude models use a 200k-token context window.
func contextWindowForModel(model string) int {
	if strings.Contains(model, "claude") {
		return 200_000
	}
	return 0
}

// getGitBranch returns the current git branch for the given directory,
// or an empty string if git is unavailable or not in a repository.
func getGitBranch(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", workingDir, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handleControlRequest converts a runtime permission request into a card
// asking the user for approval. For Phase 4 the only handled case is the
// plan-mode / elicitation flow; full coverage matches feishu/renderer.go.
func (b *Bridge) handleControlRequest(ctx context.Context, sess *session.Session, chatID string, raw json.RawMessage) {
	inner, err := protocol.ParseControlRequestInner(raw)
	if err != nil {
		log.Printf("[bridge] failed to parse control_request: %v", err)
		return
	}
	switch inner.ToolName {
	case "AskUserQuestion":
		b.handleElicitation(ctx, sess, chatID, raw)
	case "EnterPlanMode", "ExitPlanMode":
		b.handlePlanPermission(ctx, sess, chatID, raw, inner.ToolName)
	default:
		// In auto mode the session layer already responded allow to the CLI;
		// the broadcast exists only for WS-transport observers. Skip the card.
		if sess.Info().PermissionMode == claudeRT.PermissionAuto {
			return
		}
		// Forward-mode: render a generic approval card with allow/deny/always-allow
		// buttons. Without this branch the CLI hangs in StateWaitingPermission.
		b.handleToolPermission(ctx, sess, chatID, raw, inner.ToolName)
	}
}

func (b *Bridge) handleElicitation(ctx context.Context, sess *session.Session, chatID string, raw json.RawMessage) {
	elicitation, err := protocol.ParseElicitation(raw)
	if err != nil || len(elicitation.Questions) == 0 {
		return
	}
	pendingID := b.storePendingElicitation(sess.ID, elicitation.RequestID, elicitation.ToolUseID, elicitation.Input)

	var sections []channel.Section
	for _, q := range elicitation.Questions {
		if q.Header != "" {
			sections = append(sections, channel.Section{Markdown: "**" + q.Header + "**"})
		}
		sections = append(sections, channel.Section{Markdown: q.Question})

		if q.MultiSelect {
			var lines []string
			for i, opt := range q.Options {
				label := opt.Label
				if opt.Description != "" {
					label += " — " + opt.Description
				}
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, label))
			}
			lines = append(lines, "_多选题,请直接输入选项(如 \"1, 3\")_")
			sections = append(sections, channel.Section{Markdown: strings.Join(lines, "\n")})
			continue
		}

		var btns []channel.Button
		for _, opt := range q.Options {
			label := opt.Label
			if opt.Description != "" && len([]rune(label+" — "+opt.Description)) <= 30 {
				label = opt.Label + " — " + opt.Description
			}
			btns = append(btns, channel.Button{
				Label: truncateOption(label),
				Style: "default",
				Action: map[string]string{
					"action":     "answer_elicitation",
					"pending_id": pendingID,
					"question":   q.Question,
					"answer":     opt.Label,
				},
			})
		}
		if len(btns) > 0 {
			sections = append(sections, channel.Section{Buttons: btns})
		}
	}
	b.sendCardForSession(ctx, sess, chatID, channel.Card{
		Title:    "Question",
		Tone:     channel.ToneInfo,
		Sections: sections,
	})
}

func truncateOption(s string) string {
	runes := []rune(s)
	if len(runes) > 25 {
		return string(runes[:25]) + "..."
	}
	return s
}

func (b *Bridge) handlePlanPermission(ctx context.Context, sess *session.Session, chatID string, raw json.RawMessage, toolName string) {
	var req protocol.StdoutControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	var inner struct {
		ToolUseID string                 `json:"tool_use_id"`
		Input     map[string]interface{} `json:"input"`
	}
	_ = json.Unmarshal(req.Request, &inner)

	pendingID := b.storePendingElicitation(sess.ID, req.RequestID, inner.ToolUseID, nil)

	title := "Plan Mode"
	desc := "Claude 请求进入**规划模式**,将先制定方案再执行。"
	if toolName == "ExitPlanMode" {
		title = "Plan Ready"
		// Claude's ExitPlanMode tool passes the plan body via input.plan as
		// markdown text. Render it directly so the user can actually see what
		// they're approving — without this the card just said "please review"
		// with no review material.
		if planText, _ := inner.Input["plan"].(string); planText != "" {
			desc = planText
		} else {
			desc = "Claude 已完成规划,请审批后开始执行。"
		}
	}

	b.sendCardForSession(ctx, sess, chatID, channel.Card{
		Title: title,
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{{
			Markdown: desc,
			Buttons: []channel.Button{
				{Label: "批准", Style: "primary",
					Action: map[string]string{"action": "plan_response", "pending_id": pendingID, "result": "allow"}},
				{Label: "拒绝", Style: "danger",
					Action: map[string]string{"action": "plan_response", "pending_id": pendingID, "result": "deny"}},
			},
		}},
	})
}

func (b *Bridge) storePendingElicitation(sessionID, requestID, toolUseID string, original map[string]interface{}) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	// gc old entries (>10min)
	for k, p := range b.pendingElicitations {
		if now.Sub(p.CreatedAt) > 10*time.Minute {
			delete(b.pendingElicitations, k)
		}
	}
	b.pendingElicitations[requestID] = &PendingElicitation{
		SessionID:     sessionID,
		RequestID:     requestID,
		ToolUseID:     toolUseID,
		OriginalInput: original,
		CreatedAt:     now,
	}
	return requestID
}

func (b *Bridge) popPendingElicitation(id string) *PendingElicitation {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pendingElicitations[id]
	if !ok {
		return nil
	}
	delete(b.pendingElicitations, id)
	return p
}

// handleElicitationAnswer is invoked when a user clicks an option in an
// AskUserQuestion card; it sends the answer back to the runtime as an
// approved permission response carrying the answer in updated_input.
func (b *Bridge) handleElicitationAnswer(ctx context.Context, chatID, pendingID, question, answer string) {
	pending := b.popPendingElicitation(pendingID)
	if pending == nil {
		b.sendText(ctx, chatID, "该问题已过期或已回答")
		return
	}
	sess, ok := b.mgr.Get(pending.SessionID)
	if !ok {
		b.sendText(ctx, chatID, "session 已失效")
		return
	}
	updatedInput := make(map[string]interface{})
	for k, v := range pending.OriginalInput {
		updatedInput[k] = v
	}
	updatedInput["answers"] = map[string]interface{}{question: answer}
	if err := sess.RespondPermission(pending.RequestID, pending.ToolUseID, "allow", "", updatedInput); err != nil {
		b.sendTextForSession(ctx, sess, chatID, "回复失败: "+err.Error())
		return
	}
	b.sendTextForSession(ctx, sess, chatID, "已选择: "+answer)
}

// handlePlanResponse is invoked for EnterPlanMode / ExitPlanMode permissions.
func (b *Bridge) handlePlanResponse(ctx context.Context, chatID, pendingID, result string) {
	pending := b.popPendingElicitation(pendingID)
	if pending == nil {
		b.sendText(ctx, chatID, "该请求已过期或已处理")
		return
	}
	sess, ok := b.mgr.Get(pending.SessionID)
	if !ok {
		b.sendText(ctx, chatID, "session 已失效")
		return
	}
	behavior := "allow"
	if result == "deny" {
		behavior = "deny"
	}
	if err := sess.RespondPermission(pending.RequestID, pending.ToolUseID, behavior, "", nil); err != nil {
		b.sendTextForSession(ctx, sess, chatID, "回复失败: "+err.Error())
		return
	}
	title := "Plan Mode (Approved)"
	tone := channel.ToneSuccess
	body := "已批准,继续执行。"
	if behavior == "deny" {
		title = "Plan Mode (Denied)"
		tone = channel.ToneNeutral
		body = "已拒绝。"
	}
	b.sendCardForSession(ctx, sess, chatID, channel.Card{
		Title:    title,
		Tone:     tone,
		Sections: []channel.Section{{Markdown: body}},
	})
}

func extractAssistantText(raw json.RawMessage) string {
	var msg struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var apiMsg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msg.Message, &apiMsg); err != nil {
		return ""
	}
	var texts []string
	for _, c := range apiMsg.Content {
		if c.Type == "text" && c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "")
}

func extractGatewayEvent(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	eventRaw, ok := m["_gateway_event"]
	if !ok {
		return "", false
	}
	var event string
	if err := json.Unmarshal(eventRaw, &event); err != nil {
		return "", false
	}
	return event, true
}

func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= maxCardTextLen {
		return s
	}
	return string(runes[:maxCardTextLen]) + "\n\n... (truncated)"
}
