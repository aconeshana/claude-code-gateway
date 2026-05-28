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
			b.sendText(ctx, chatID, "Session "+displaySessionID(sess)+" 已变为 idle(发消息自动恢复)")
		}
	} else {
		b.sendText(ctx, chatID, "Session "+displaySessionID(sess)+" 已断开(CLI 未返回 session ID,无法保留)")
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

	switch msgType {
	case protocol.MsgTypeSystem:
		var sysMsg protocol.SystemInitMessage
		if json.Unmarshal(raw, &sysMsg) == nil && sysMsg.Subtype == protocol.SubtypeInit {
			state.mu.Lock()
			state.model = sysMsg.Model
			state.mu.Unlock()
		}
	case protocol.MsgTypeAssistant:
		// Extract usage from the last API response to track context window usage.
		// result.Usage is the sum of ALL turns in the invocation and overcounts
		// cache_read tokens when there are multiple turns — the assistant message's
		// inner usage reflects the actual last-call context size.
		updateContextPctFromAssistant(state, raw)
		text := extractAssistantText(raw)
		if text == "" {
			return
		}
		state.appendText(ctx, b, chatID, text)
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
}

func (s *streamState) appendText(ctx context.Context, b *Bridge, chatID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}
	s.textBuf.WriteString(text)
	s.dirty = true

	if s.messageID == "" {
		content := truncate(s.textBuf.String())
		msgID, err := b.sendCard(ctx, chatID, b.processingCardWithID(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, content))
		if err != nil {
			return
		}
		s.messageID = msgID
		s.lastUpdate = time.Now()
		s.dirty = false
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
		_ = b.updateCard(ctx, s.messageID, b.processingCardWithID(s.project, s.sessionShort, s.summary, s.model, s.gitBranch, s.contextPct, content))
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
	msgID := s.messageID
	buffered := s.textBuf.String()
	project := s.project
	sessionShort := s.sessionShort
	summary := s.summary
	model := s.model
	gitBranch := s.gitBranch
	contextPct := s.contextPct
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

	card := b.resultCardWithID(project, sessionShort, summary, model, gitBranch, contextPct, content, result)
	if msgID != "" {
		if err := b.updateCard(ctx, msgID, card); err != nil {
			_, _ = b.sendCard(ctx, chatID, card)
		}
	} else {
		_, _ = b.sendCard(ctx, chatID, card)
	}

	s.mu.Lock()
	s.messageID = ""
	s.textBuf.Reset()
	s.dirty = false
	s.finalized = false
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
}

func (b *Bridge) processingCard(project, summary, model, gitBranch string, contextPct int, content string) channel.Card {
	return b.processingCardWithID(project, "", summary, model, gitBranch, contextPct, content)
}

// processingCardWithID variant that suffixes the session display id to the
// card title (e.g. "Processing: claude-code-gateway · 6befadec"). The id
// suffix lets users running multiple parallel sessions identify which card
// belongs to which session at a glance, and copy the id for /switch.
func (b *Bridge) processingCardWithID(project, sessionShort, summary, model, gitBranch string, contextPct int, content string) channel.Card {
	title := "Processing"
	if project != "" {
		title += ": " + project
	}
	if sessionShort != "" {
		title += " · " + sessionShort
	}
	hud := buildHUDNote(model, gitBranch, contextPct, "处理中...")
	note := hud
	if summary != "" {
		note = summary + " | " + hud
	}
	return channel.Card{
		Title: title,
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: content},
			{Note: note},
		},
	}
}

func (b *Bridge) resultCard(project, summary, model, gitBranch string, contextPct int, content string, result *protocol.ResultMessage) channel.Card {
	return b.resultCardWithID(project, "", summary, model, gitBranch, contextPct, content, result)
}

func (b *Bridge) resultCardWithID(project, sessionShort, summary, model, gitBranch string, contextPct int, content string, result *protocol.ResultMessage) channel.Card {
	title := "Done"
	tone := channel.ToneSuccess
	if result.IsError {
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
	hud := buildHUDNote(model, gitBranch, contextPct, stats)
	note := hud
	if summary != "" {
		note = summary + " | " + hud
	}
	return channel.Card{
		Title: title,
		Tone:  tone,
		Sections: []channel.Section{
			{Markdown: content},
			{Divider: true, Note: note},
		},
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

// shortModelName strips the "claude-" prefix for display brevity.
// "claude-sonnet-4-6" → "sonnet-4-6"; "us.amazon.claude-opus-4-7" → "opus-4-7"
func shortModelName(model string) string {
	if idx := strings.Index(model, "claude-"); idx >= 0 {
		return model[idx+len("claude-"):]
	}
	return model
}

// hudContextBar renders a 10-char bar plus percentage: "████░░░░░░ 45%"
func hudContextBar(pct int) string {
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
		log.Printf("[bridge] unhandled control_request tool: %s", inner.ToolName)
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
	b.sendCard(ctx, chatID, channel.Card{
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
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal(req.Request, &inner)

	pendingID := b.storePendingElicitation(sess.ID, req.RequestID, inner.ToolUseID, nil)

	title := "Plan Mode"
	desc := "Claude 请求进入**规划模式**,将先制定方案再执行。"
	if toolName == "ExitPlanMode" {
		title = "Plan Ready"
		desc = "Claude 已完成规划,请审批后开始执行。"
	}

	b.sendCard(ctx, chatID, channel.Card{
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
		b.sendText(ctx, chatID, "回复失败: "+err.Error())
		return
	}
	b.sendText(ctx, chatID, "已选择: "+answer)
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
		b.sendText(ctx, chatID, "回复失败: "+err.Error())
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
	b.sendCard(ctx, chatID, channel.Card{
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
