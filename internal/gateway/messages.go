package gateway

import "encoding/json"

// --- Client → Gateway ---

type ClientMessage struct {
	Action    string          `json:"action"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// CreateSessionPayload is the request shape for the "create_session" action.
// Runtime-specific configuration (claude flags, etc.) is carried opaquely in
// Runtime; the gateway dispatches it to the appropriate runtime.Factory.
//
// Envelope shape for Runtime:
//
//	{"kind": "claude", "config": {...claude.Config fields...}}
//
// If Runtime is empty, the server's default runtime kind is used with an
// empty config.
type CreateSessionPayload struct {
	WorkingDir     string            `json:"working_dir"`
	PermissionMode string            `json:"permission_mode,omitempty"`
	EnvVars        map[string]string `json:"env_vars,omitempty"`
	Label          string            `json:"label,omitempty"`
	OwnerID        string            `json:"owner_id,omitempty"`
	ChatID         string            `json:"chat_id,omitempty"`
	ChannelKind    string            `json:"channel_kind,omitempty"`
	Runtime        json.RawMessage   `json:"runtime,omitempty"`
}

type ResumeSessionPayload struct {
	SessionID  string `json:"session_id"`
	WorkingDir string `json:"working_dir,omitempty"`

	OwnerID     string `json:"owner_id,omitempty"`
	Label       string `json:"label,omitempty"`
	Summary     string `json:"summary,omitempty"`
	ChatID      string `json:"chat_id,omitempty"`
	ChannelKind string `json:"channel_kind,omitempty"`
}

type SendMessagePayload struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

type PermissionResponsePayload struct {
	SessionID    string                 `json:"session_id"`
	RequestID    string                 `json:"request_id"`
	Behavior     string                 `json:"behavior"`
	ToolUseID    string                 `json:"tool_use_id"`
	Message      string                 `json:"message,omitempty"`
	UpdatedInput map[string]interface{} `json:"updated_input,omitempty"`
}

type ControlPayload struct {
	SessionID string          `json:"session_id"`
	Subtype   string          `json:"subtype"`
	Extra     json.RawMessage `json:"-"`
}

type DestroySessionPayload struct {
	SessionID string `json:"session_id"`
}

// --- Gateway → Client ---

type ServerMessage struct {
	Event     string      `json:"event"`
	SessionID string      `json:"session_id,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
	Payload   interface{} `json:"payload,omitempty"`
	Error     string      `json:"error,omitempty"`
}

func NewEvent(event, sessionID string, payload interface{}) *ServerMessage {
	return &ServerMessage{
		Event:     event,
		SessionID: sessionID,
		Payload:   payload,
	}
}

func NewErrorEvent(sessionID, requestID, errMsg string) *ServerMessage {
	return &ServerMessage{
		Event:     "error",
		SessionID: sessionID,
		RequestID: requestID,
		Error:     errMsg,
	}
}

func NewReplyEvent(event, sessionID, requestID string, payload interface{}) *ServerMessage {
	return &ServerMessage{
		Event:     event,
		SessionID: sessionID,
		RequestID: requestID,
		Payload:   payload,
	}
}
