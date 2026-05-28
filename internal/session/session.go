package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/google/uuid"
)

type State int

const (
	StateStarting State = iota
	StateReady
	StateProcessing
	StateWaitingPermission
	StateIdle
	StateStopped
	StateError
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateProcessing:
		return "processing"
	case StateWaitingPermission:
		return "waiting_permission"
	case StateIdle:
		return "idle"
	case StateStopped:
		return "stopped"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

type Subscriber struct {
	Ch     chan json.RawMessage
	mu     sync.Mutex
	closed bool
}

// trySend attempts to send msg without blocking. Safe to call concurrently
// with safeClose; returns false if the subscriber has been closed or its
// buffer is full.
func (sub *Subscriber) trySend(msg json.RawMessage) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return false
	}
	select {
	case sub.Ch <- msg:
		return true
	default:
		return false
	}
}

// safeClose closes the channel exactly once. Safe to call concurrently with
// trySend.
func (sub *Subscriber) safeClose() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if !sub.closed {
		sub.closed = true
		close(sub.Ch)
	}
}

// Status describes a session's user-facing lifecycle state. The runtime State
// (StateStarting/StateReady/...) captures the underlying CLI process phase
// independently — Status is only meaningful at the user/manager level.
type Status string

const (
	// StatusActive: a runtime process is currently spawned for this session.
	StatusActive Status = "active"
	// StatusIdle: the runtime process has exited cleanly; the session can be
	// reactivated on demand without losing context (CLI persists its state on
	// disk).
	StatusIdle Status = "idle"
	// StatusArchived: the user explicitly archived this session. No runtime
	// process; surfaced separately in the UI.
	StatusArchived Status = "archived"
)

// Origin classifies how a session entered the manager. The user-facing
// surfaces (channel UI, /list, /switch, /resume, summary worker) use Origin
// to gate which sessions appear in which views — the manager itself is
// origin-agnostic and tracks all of them uniformly.
const (
	// OriginFeishu — created by the Feishu (Lark) channel via /new or by an
	// auto-resolve when the user sent a plain message.
	OriginFeishu = "feishu"
	// OriginWS — created via the WebSocket gateway transport.
	OriginWS = "ws"
	// OriginExternal — discovered on disk (~/.claude/projects/*.jsonl) but
	// not created via this gateway. Typically terminal / SDK / VS Code
	// invocations. Visibility gated by GATEWAY_SHARE_EXTERNAL_SESSIONS.
	OriginExternal = "external"
	// OriginAdmin — spawned by the gateway's own summary worker / admin
	// helper (cwd = /tmp/claude-code-gateway-admin, or fingerprint matched).
	// Never shown to users; never receives auto-summary work.
	OriginAdmin = "admin"
)

type Session struct {
	ID             string
	CLISessionID   string
	WorkingDir     string
	PermissionMode string
	CreatedAt      time.Time

	// User-facing metadata (managed by Manager).
	OwnerID     string
	Label       string
	Summary     string
	CustomTitle string // /rename-style name; takes display precedence over Summary
	// LatestUserMessage is the most recent user-sent text (truncated). Used
	// as a decision aid in the list UI when Summary is empty (short session)
	// — shows the user what they actually said last, regardless of Origin.
	LatestUserMessage string
	Origin            string // see OriginXxx constants — provenance of the session
	ChatID      string
	ChannelKind string
	Status      Status
	ArchivedAt  time.Time

	rt           runtime.Runtime
	codec        runtime.Codec
	keepAliveInt time.Duration

	lastActivity time.Time
	state        State
	pendingTurns int
	process      runtime.Process
	subscribers  map[string]*Subscriber
	mu           sync.Mutex
	cancelKeep   context.CancelFunc
	spawnReq     runtime.SpawnRequest
	switching    bool

	// Summary-related runtime state (used by manager.AppendRecentMessage /
	// ShouldUpdateSummary). Guarded by mu.
	turnsSinceSummary int
	recentMessages    []string
	summaryPending    bool
}

type SessionInfo struct {
	ID             string `json:"id"`
	CLISessionID   string `json:"cli_session_id"`
	State          string `json:"state"`
	WorkingDir     string `json:"working_dir"`
	PendingTurns   int    `json:"pending_turns"`
	PermissionMode string `json:"permission_mode"`
	CreatedAt      string `json:"created_at"`
	LastActivity   string `json:"last_activity"`
	OwnerID        string `json:"owner_id,omitempty"`
	Label          string `json:"label,omitempty"`
	Summary        string `json:"summary,omitempty"`
	CustomTitle    string `json:"custom_title,omitempty"`
	LatestUserMessage string `json:"latest_user_message,omitempty"`
	Origin         string `json:"origin,omitempty"`
	ChatID         string `json:"chat_id,omitempty"`
	ChannelKind    string `json:"channel_kind,omitempty"`
	Status         string `json:"status,omitempty"`
	ArchivedAt     string `json:"archived_at,omitempty"`
}

// NewSession spawns a runtime instance and wraps it in a Session.
//
// permissionMode controls session-level auto-allow behavior for non-interactive
// tools — it is the manager-level permission mode, distinct from any
// runtime-specific permission flags inside req.Config.
func NewSession(rt runtime.Runtime, req runtime.SpawnRequest, permissionMode string, keepAliveInterval time.Duration) (*Session, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID:             uuid.New().String(),
		WorkingDir:     req.WorkingDir,
		PermissionMode: permissionMode,
		Status:         StatusActive,
		CreatedAt:      time.Now(),
		lastActivity:   time.Now(),
		state:          StateStarting,
		subscribers:    make(map[string]*Subscriber),
		cancelKeep:     cancel,
		rt:             rt,
		codec:          rt.Codec(),
		spawnReq:       req,
		keepAliveInt:   keepAliveInterval,
	}

	process, err := rt.Spawn(context.Background(), req, runtime.Callbacks{
		OnMessage: s.handleCLIMessage,
		OnExit:    s.handleCLIExit,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	s.process = process

	go s.keepAliveLoop(ctx, keepAliveInterval)

	return s, nil
}

func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:             s.ID,
		CLISessionID:   s.CLISessionID,
		State:          s.state.String(),
		WorkingDir:     s.WorkingDir,
		PendingTurns:   s.pendingTurns,
		PermissionMode: s.PermissionMode,
		CreatedAt:      s.CreatedAt.Format(time.RFC3339),
		LastActivity:   s.lastActivity.Format(time.RFC3339),
		OwnerID:        s.OwnerID,
		Label:          s.Label,
		Summary:        s.Summary,
		CustomTitle:    s.CustomTitle,
		LatestUserMessage: s.LatestUserMessage,
		Origin:         s.Origin,
		ChatID:         s.ChatID,
		ChannelKind:    s.ChannelKind,
		Status:         string(s.Status),
	}
	if !s.ArchivedAt.IsZero() {
		info.ArchivedAt = s.ArchivedAt.Format(time.RFC3339)
	}
	return info
}

func (s *Session) SendMessage(content string) error {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateError {
		s.mu.Unlock()
		return fmt.Errorf("session %s is %s", s.ID, s.state)
	}
	s.pendingTurns++
	if s.state == StateReady || s.state == StateIdle {
		s.state = StateProcessing
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()

	raw, err := s.codec.EncodeUserText(content, uuid.New().String())
	if err != nil {
		return fmt.Errorf("encode user message: %w", err)
	}
	if err := s.process.Write(raw); err != nil {
		return fmt.Errorf("write user message: %w", err)
	}

	s.broadcastTurnStatus()
	return nil
}

func (s *Session) SendMessageBlocks(blocks []interface{}) error {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateError {
		s.mu.Unlock()
		return fmt.Errorf("session %s is %s", s.ID, s.state)
	}
	s.pendingTurns++
	if s.state == StateReady || s.state == StateIdle {
		s.state = StateProcessing
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()

	raw, err := s.codec.EncodeUserBlocks(blocks, uuid.New().String())
	if err != nil {
		return fmt.Errorf("encode user blocks: %w", err)
	}
	if err := s.process.Write(raw); err != nil {
		return fmt.Errorf("write user message blocks: %w", err)
	}

	s.broadcastTurnStatus()
	return nil
}

func (s *Session) RespondPermission(requestID, toolUseID, behavior, message string, updatedInput map[string]interface{}) error {
	raw, err := s.codec.EncodeControlResponse(requestID, toolUseID, behavior, message, updatedInput)
	if err != nil {
		return fmt.Errorf("encode permission response: %w", err)
	}
	if err := s.process.Write(raw); err != nil {
		return fmt.Errorf("write permission response: %w", err)
	}

	s.mu.Lock()
	if s.state == StateWaitingPermission {
		s.state = StateProcessing
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()

	return nil
}

func (s *Session) SendControl(payload json.RawMessage) error {
	raw, err := s.codec.EncodeControl(payload)
	if err != nil {
		return fmt.Errorf("encode control: %w", err)
	}
	return s.process.Write(raw)
}

func (s *Session) Subscribe(clientID string) <-chan json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan json.RawMessage, 64)
	s.subscribers[clientID] = &Subscriber{Ch: ch}
	return ch
}

func (s *Session) Unsubscribe(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := s.subscribers[clientID]; ok {
		sub.safeClose()
		delete(s.subscribers, clientID)
	}
}

func (s *Session) Close() error {
	if s.cancelKeep != nil {
		s.cancelKeep()
	}
	if s.process == nil {
		return nil
	}
	return s.process.GracefulStop(5 * time.Second)
}

func (s *Session) ForceClose() {
	if s.cancelKeep != nil {
		s.cancelKeep()
	}
	if s.process == nil {
		return
	}
	_ = s.process.Kill()
}

func (s *Session) handleCLIMessage(raw json.RawMessage) {
	ev, err := s.codec.ParseEvent(raw)
	if err != nil {
		log.Printf("[session %s] parse message type error: %v", s.ID, err)
		s.broadcast(raw)
		return
	}

	log.Printf("[session %s] msg: kind=%d subtype=%s len=%d", s.ID, ev.Kind, ev.Subtype, len(raw))

	s.mu.Lock()
	s.lastActivity = time.Now()

	switch ev.Kind {
	case runtime.KindInit:
		if ev.RuntimeID != "" {
			s.CLISessionID = ev.RuntimeID
		}
		if s.state == StateStarting {
			s.state = StateReady
		}
	case runtime.KindResult:
		if s.pendingTurns > 0 {
			s.pendingTurns--
		}
		if s.pendingTurns == 0 {
			s.state = StateIdle
		}
	case runtime.KindControlRequest:
		// Permission requests are parsed via the protocol package because
		// the auto-allow decision is currently claude-specific. Future
		// refactor will push this down into the runtime layer.
		var inner protocol.ControlRequestInner
		if parsedInner, parseErr := protocol.ParseControlRequestInner(raw); parseErr == nil {
			inner = parsedInner
			log.Printf("[session %s] control_request: subtype=%s tool=%s", s.ID, inner.Subtype, inner.ToolName)
		} else {
			log.Printf("[session %s] control_request parse failed: %v", s.ID, parseErr)
		}

		if s.PermissionMode == "auto" && !isInteractiveTool(inner.ToolName) {
			log.Printf("[session %s] auto-allowing control_request (subtype=%s tool=%s)", s.ID, inner.Subtype, inner.ToolName)
			s.mu.Unlock()
			if resp, autoErr := protocol.AutoAllowPermission(raw); autoErr == nil {
				if respBytes, mErr := json.Marshal(resp); mErr == nil {
					_ = s.process.Write(respBytes)
				}
			}
			s.broadcast(raw)
			return
		}
		log.Printf("[session %s] forwarding control_request as elicitation (subtype=%s)", s.ID, inner.Subtype)
		s.state = StateWaitingPermission
	case runtime.KindControlCancel:
		if s.state == StateWaitingPermission {
			s.state = StateProcessing
		}
	}
	s.mu.Unlock()

	s.broadcast(raw)

	if ev.Kind == runtime.KindResult {
		s.broadcastTurnStatus()
	}
}

func (s *Session) handleCLIExit(err error) {
	s.mu.Lock()
	if s.switching {
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.state = StateError
		log.Printf("[session %s] CLI process exited with error: %v", s.ID, err)
	} else {
		s.state = StateStopped
		log.Printf("[session %s] CLI process exited normally", s.ID)
	}

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	event := map[string]interface{}{
		"_gateway_event": "session_exit",
		"session_id":     s.ID,
		"error":          errMsg,
	}
	data, _ := json.Marshal(event)

	// Detach subscribers under lock, close them outside to avoid races with
	// in-flight broadcasts that already unlocked the mutex (trySend/safeClose
	// are themselves race-safe).
	subs := s.subscribers
	s.subscribers = make(map[string]*Subscriber)
	s.mu.Unlock()

	for _, sub := range subs {
		sub.trySend(json.RawMessage(data))
		sub.safeClose()
	}
}

func (s *Session) broadcast(msg json.RawMessage) {
	s.mu.Lock()
	subs := make([]*Subscriber, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		if !sub.trySend(msg) {
			log.Printf("[session %s] subscriber dropped message (closed or full)", s.ID)
		}
	}
}

func (s *Session) broadcastTurnStatus() {
	s.mu.Lock()
	pending := s.pendingTurns
	state := s.state
	s.mu.Unlock()

	status := "idle"
	switch {
	case pending > 1:
		status = "queued"
	case pending == 1:
		status = "processing"
	case state == StateWaitingPermission:
		status = "waiting_permission"
	}

	event := map[string]interface{}{
		"_gateway_event": "turn_status",
		"session_id":     s.ID,
		"pending_turns":  pending,
		"status":         status,
	}
	data, _ := json.Marshal(event)
	s.broadcast(json.RawMessage(data))
}

func (s *Session) CurrentState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) BroadcastEvent(event map[string]interface{}) {
	data, _ := json.Marshal(event)
	s.broadcast(json.RawMessage(data))
}

func (s *Session) SwitchModel(newModel string) error {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateError {
		s.mu.Unlock()
		return fmt.Errorf("session %s is %s", s.ID, s.state)
	}
	cliSessionID := s.CLISessionID
	if cliSessionID == "" {
		s.mu.Unlock()
		return fmt.Errorf("session not ready yet (no CLI session ID)")
	}
	oldProcess := s.process
	s.switching = true
	s.state = StateStarting
	s.pendingTurns = 0
	s.mu.Unlock()

	_ = oldProcess.GracefulStop(5 * time.Second)
	<-oldProcess.Done()

	newReq := s.spawnReq
	newReq.ResumeID = cliSessionID
	newReq.Config = applyModelOverride(newReq.Config, newModel)

	s.cancelKeep()
	ctx, cancel := context.WithCancel(context.Background())

	newProcess, err := s.rt.Spawn(context.Background(), newReq, runtime.Callbacks{
		OnMessage: s.handleCLIMessage,
		OnExit:    s.handleCLIExit,
	})
	if err != nil {
		cancel()
		s.mu.Lock()
		s.switching = false
		s.state = StateError
		s.mu.Unlock()
		return fmt.Errorf("spawn new CLI: %w", err)
	}

	s.mu.Lock()
	s.process = newProcess
	s.cancelKeep = cancel
	s.spawnReq = newReq
	s.switching = false
	s.lastActivity = time.Now()
	s.mu.Unlock()

	go s.keepAliveLoop(ctx, s.keepAliveInt)

	log.Printf("[session %s] switched model to %s (cli_session=%s)", s.ID, newModel, cliSessionID)
	return nil
}

// modelOverrider may be implemented by runtime.Config types that support
// switching their model at runtime (used by SwitchModel).
type modelOverrider interface {
	WithModel(model string) runtime.Config
}

func applyModelOverride(cfg runtime.Config, newModel string) runtime.Config {
	if cfg == nil {
		return cfg
	}
	if m, ok := cfg.(modelOverrider); ok {
		return m.WithModel(newModel)
	}
	log.Printf("[session] WARNING: runtime config %T does not support WithModel(); /model switch is a no-op", cfg)
	return cfg
}

func (s *Session) keepAliveLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.process.Done():
			return
		case <-ticker.C:
			raw, err := s.codec.EncodeKeepAlive()
			if err != nil {
				continue
			}
			_ = s.process.Write(raw)
		}
	}
}

func isInteractiveTool(name string) bool {
	switch name {
	case "AskUserQuestion", "EnterPlanMode", "ExitPlanMode":
		return true
	}
	return false
}
