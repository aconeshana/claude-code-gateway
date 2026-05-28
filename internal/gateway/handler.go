package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/gorilla/websocket"
)

type WSHandler struct {
	conn               *websocket.Conn
	clientID           string
	sessionMgr         *session.Manager
	registry           *runtime.Registry
	defaultRuntimeKind string

	writeTimeout time.Duration

	subscriptions map[string]<-chan json.RawMessage
	subMu         sync.Mutex
	writeMu       sync.Mutex

	done chan struct{}
}

func NewWSHandler(conn *websocket.Conn, clientID string, mgr *session.Manager, registry *runtime.Registry, defaultRuntimeKind string, writeTimeout time.Duration) *WSHandler {
	return &WSHandler{
		conn:               conn,
		clientID:           clientID,
		sessionMgr:         mgr,
		registry:           registry,
		defaultRuntimeKind: defaultRuntimeKind,
		writeTimeout:       writeTimeout,
		subscriptions:      make(map[string]<-chan json.RawMessage),
		done:               make(chan struct{}),
	}
}

func (h *WSHandler) Run(ctx context.Context, cancel context.CancelFunc) {
	defer func() {
		close(h.done)
		h.cleanupSubscriptions()
		h.conn.Close()
	}()

	go func() {
		h.readLoop(ctx)
		cancel()
	}()

	<-ctx.Done()
}

func (h *WSHandler) readLoop(ctx context.Context) {
	for {
		_, data, err := h.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[ws %s] read error: %v", h.clientID, err)
			}
			return
		}

		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			h.sendError("", "", "invalid message format: "+err.Error())
			continue
		}

		h.handleAction(ctx, msg)
	}
}

func (h *WSHandler) handleAction(ctx context.Context, msg ClientMessage) {
	switch msg.Action {
	case "create_session":
		h.handleCreateSession(ctx, msg)
	case "resume_session":
		h.handleResumeSession(ctx, msg)
	case "send_message":
		h.handleSendMessage(msg)
	case "respond_permission":
		h.handleRespondPermission(msg)
	case "control":
		h.handleControl(msg)
	case "destroy_session":
		h.handleDestroySession(msg)
	case "list_sessions":
		h.handleListSessions(msg)
	case "ping":
		h.send(NewReplyEvent("pong", "", msg.RequestID, nil))
	default:
		h.sendError("", msg.RequestID, "unknown action: "+msg.Action)
	}
}

func (h *WSHandler) handleCreateSession(ctx context.Context, msg ClientMessage) {
	var payload CreateSessionPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	runtimeCfg, err := h.registry.Parse(payload.Runtime, h.defaultRuntimeKind)
	if err != nil {
		h.sendError("", msg.RequestID, "invalid runtime config: "+err.Error())
		return
	}

	sess, err := h.sessionMgr.Create(ctx, session.CreateOpts{
		WorkingDir:     payload.WorkingDir,
		PermissionMode: payload.PermissionMode,
		EnvVars:        payload.EnvVars,
		OwnerID:        payload.OwnerID,
		Label:          payload.Label,
		ChatID:         payload.ChatID,
		ChannelKind:    payload.ChannelKind,
		Origin:         session.OriginWS,
		RuntimeConfig:  runtimeCfg,
	})
	if err != nil {
		h.sendError("", msg.RequestID, "create session failed: "+err.Error())
		return
	}

	ch := sess.Subscribe(h.clientID)
	h.subMu.Lock()
	h.subscriptions[sess.ID] = ch
	h.subMu.Unlock()
	go h.forwardMessages(sess.ID, ch)

	h.send(NewReplyEvent("session_created", sess.ID, msg.RequestID, sess.Info()))
}

func (h *WSHandler) handleResumeSession(ctx context.Context, msg ClientMessage) {
	var payload ResumeSessionPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	sess, err := h.sessionMgr.Resume(ctx, session.ResumeOpts{
		CLISessionID: payload.SessionID,
		WorkingDir:   payload.WorkingDir,
		OwnerID:      payload.OwnerID,
		Label:        payload.Label,
		Summary:      payload.Summary,
		ChatID:       payload.ChatID,
		ChannelKind:  payload.ChannelKind,
	})
	if err != nil {
		h.sendError("", msg.RequestID, "resume session failed: "+err.Error())
		return
	}

	ch := sess.Subscribe(h.clientID)
	h.subMu.Lock()
	h.subscriptions[sess.ID] = ch
	h.subMu.Unlock()
	go h.forwardMessages(sess.ID, ch)

	h.send(NewReplyEvent("session_created", sess.ID, msg.RequestID, sess.Info()))
}

func (h *WSHandler) handleSendMessage(msg ClientMessage) {
	var payload SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	sess, ok := h.sessionMgr.Get(payload.SessionID)
	if !ok {
		h.sendError(payload.SessionID, msg.RequestID, "session not found")
		return
	}

	h.ensureSubscribed(sess)

	content := strings.TrimSpace(payload.Content)

	if strings.HasPrefix(content, "/model ") {
		h.handleModelSwitch(sess, msg.RequestID, content)
		return
	}

	if strings.HasPrefix(content, "!") && len(content) > 1 {
		h.handleShellCommand(sess, msg.RequestID, content)
		return
	}

	if err := sess.SendMessage(payload.Content); err != nil {
		h.sendError(payload.SessionID, msg.RequestID, "send message failed: "+err.Error())
		return
	}
}

func (h *WSHandler) handleRespondPermission(msg ClientMessage) {
	var payload PermissionResponsePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	sess, ok := h.sessionMgr.Get(payload.SessionID)
	if !ok {
		h.sendError(payload.SessionID, msg.RequestID, "session not found")
		return
	}

	if err := sess.RespondPermission(payload.RequestID, payload.ToolUseID, payload.Behavior, payload.Message, payload.UpdatedInput); err != nil {
		h.sendError(payload.SessionID, msg.RequestID, "respond permission failed: "+err.Error())
		return
	}
}

func (h *WSHandler) handleControl(msg ClientMessage) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(msg.Payload, &raw); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	var sessionID string
	if sid, ok := raw["session_id"]; ok {
		json.Unmarshal(sid, &sessionID)
	}
	if sessionID == "" {
		h.sendError("", msg.RequestID, "session_id is required")
		return
	}

	sess, ok := h.sessionMgr.Get(sessionID)
	if !ok {
		h.sendError(sessionID, msg.RequestID, "session not found")
		return
	}

	controlPayload := make(map[string]json.RawMessage)
	for k, v := range raw {
		if k != "session_id" {
			controlPayload[k] = v
		}
	}
	data, _ := json.Marshal(controlPayload)

	if err := sess.SendControl(data); err != nil {
		h.sendError(sessionID, msg.RequestID, "send control failed: "+err.Error())
		return
	}
}

func (h *WSHandler) handleDestroySession(msg ClientMessage) {
	var payload DestroySessionPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendError("", msg.RequestID, "invalid payload: "+err.Error())
		return
	}

	if err := h.sessionMgr.Destroy(payload.SessionID); err != nil {
		h.sendError(payload.SessionID, msg.RequestID, "destroy session failed: "+err.Error())
		return
	}

	h.subMu.Lock()
	delete(h.subscriptions, payload.SessionID)
	h.subMu.Unlock()

	h.send(NewReplyEvent("session_destroyed", payload.SessionID, msg.RequestID, nil))
}

func (h *WSHandler) handleListSessions(msg ClientMessage) {
	sessions := h.sessionMgr.List()
	h.send(NewReplyEvent("session_list", "", msg.RequestID, sessions))
}

func (h *WSHandler) forwardMessages(sessionID string, ch <-chan json.RawMessage) {
	for raw := range ch {
		msgType, _, _ := protocol.ParseType(raw)

		event := "message"
		switch msgType {
		case protocol.MsgTypeControlRequest:
			event = "permission_request"
		case protocol.MsgTypeControlResponse:
			event = "control_response"
		}

		if gwEvent, ok := extractGatewayEvent(raw); ok {
			switch gwEvent {
			case "turn_status":
				event = "turn_status"
			case "session_exit":
				event = "session_error"
			case "shell_result":
				event = "shell_result"
			}
		}

		h.send(NewEvent(event, sessionID, raw))
	}
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

func (h *WSHandler) ensureSubscribed(sess *session.Session) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if _, ok := h.subscriptions[sess.ID]; ok {
		return
	}
	ch := sess.Subscribe(h.clientID)
	h.subscriptions[sess.ID] = ch
	go h.forwardMessages(sess.ID, ch)
}

func (h *WSHandler) send(msg *ServerMessage) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
	if err := h.conn.WriteJSON(msg); err != nil {
		log.Printf("[ws %s] write error: %v", h.clientID, err)
	}
}

func (h *WSHandler) sendError(sessionID, requestID, errMsg string) {
	h.send(NewErrorEvent(sessionID, requestID, errMsg))
}

func (h *WSHandler) handleModelSwitch(sess *session.Session, requestID, content string) {
	modelName := strings.TrimSpace(strings.TrimPrefix(content, "/model"))
	if modelName == "" {
		h.sendError(sess.ID, requestID, "/model requires a model name, e.g. /model sonnet")
		return
	}

	state := sess.CurrentState()
	if state == session.StateProcessing || state == session.StateWaitingPermission {
		_ = sess.SendControl(json.RawMessage(`{"subtype":"interrupt"}`))
		time.Sleep(500 * time.Millisecond)
	}

	if err := sess.SwitchModel(modelName); err != nil {
		h.sendError(sess.ID, requestID, "model switch failed: "+err.Error())
		return
	}

	h.send(NewReplyEvent("model_switched", sess.ID, requestID, map[string]interface{}{
		"model": modelName,
	}))
}

func (h *WSHandler) handleShellCommand(sess *session.Session, requestID, content string) {
	cmdStr := strings.TrimSpace(content[1:])
	if cmdStr == "" {
		h.sendError(sess.ID, requestID, "! requires a command, e.g. !pwd")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = sess.WorkingDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = -1
		}
	}

	result := map[string]interface{}{
		"_gateway_event": "shell_result",
		"command":        cmdStr,
		"stdout":         stdout.String(),
		"stderr":         stderr.String(),
		"exit_code":      exitCode,
	}

	sess.BroadcastEvent(result)
}

func (h *WSHandler) cleanupSubscriptions() {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for sessionID := range h.subscriptions {
		if sess, ok := h.sessionMgr.Get(sessionID); ok {
			sess.Unsubscribe(h.clientID)
		}
	}
	h.subscriptions = make(map[string]<-chan json.RawMessage)
}
