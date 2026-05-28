package protocol

import "encoding/json"

// --- Stdin messages (Gateway → CLI) ---

type UserMessage struct {
	Type            string      `json:"type"`
	Message         UserContent `json:"message"`
	ParentToolUseID *string     `json:"parent_tool_use_id"`
	SessionID       string      `json:"session_id"`
	UUID            string      `json:"uuid,omitempty"`
}

type UserContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func NewUserMessage(content, uuid string) *UserMessage {
	return &UserMessage{
		Type: "user",
		Message: UserContent{
			Role:    "user",
			Content: content,
		},
		ParentToolUseID: nil,
		SessionID:       "",
		UUID:            uuid,
	}
}

type ControlResponse struct {
	Type     string              `json:"type"`
	Response ControlResponseBody `json:"response"`
}

type ControlResponseBody struct {
	Subtype   string      `json:"subtype"`
	RequestID string      `json:"request_id"`
	Response  interface{} `json:"response,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type PermissionResult struct {
	Behavior     string                 `json:"behavior"`
	ToolUseID    string                 `json:"toolUseID"`
	Message      string                 `json:"message,omitempty"`
	UpdatedInput map[string]interface{} `json:"updatedInput"`
}

func NewAllowResponse(requestID, toolUseID string, updatedInput map[string]interface{}) *ControlResponse {
	ui := updatedInput
	if ui == nil {
		ui = map[string]interface{}{}
	}
	return &ControlResponse{
		Type: "control_response",
		Response: ControlResponseBody{
			Subtype:   "success",
			RequestID: requestID,
			Response: PermissionResult{
				Behavior:     "allow",
				ToolUseID:    toolUseID,
				UpdatedInput: ui,
			},
		},
	}
}

func NewDenyResponse(requestID, toolUseID, message string) *ControlResponse {
	return &ControlResponse{
		Type: "control_response",
		Response: ControlResponseBody{
			Subtype:   "success",
			RequestID: requestID,
			Response: PermissionResult{
				Behavior:  "deny",
				ToolUseID: toolUseID,
				Message:   message,
			},
		},
	}
}

type KeepAliveMessage struct {
	Type string `json:"type"`
}

func NewKeepAlive() *KeepAliveMessage {
	return &KeepAliveMessage{Type: "keep_alive"}
}

type UserMessageWithBlocks struct {
	Type    string                `json:"type"`
	Message UserContentWithBlocks `json:"message"`
	UUID    string                `json:"uuid,omitempty"`
}

type UserContentWithBlocks struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

func NewUserMessageWithBlocks(blocks []interface{}, uuid string) *UserMessageWithBlocks {
	return &UserMessageWithBlocks{
		Type: "user",
		Message: UserContentWithBlocks{
			Role:    "user",
			Content: blocks,
		},
		UUID: uuid,
	}
}

type StdinControlRequest struct {
	Type    string          `json:"type"`
	Request json.RawMessage `json:"request"`
}

func NewStdinControlRequest(payload json.RawMessage) *StdinControlRequest {
	return &StdinControlRequest{
		Type:    "control_request",
		Request: payload,
	}
}

type UpdateEnvVars struct {
	Type      string            `json:"type"`
	Variables map[string]string `json:"variables"`
}

// --- Stdout messages (CLI → Gateway) ---

type RawMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

type SystemInitMessage struct {
	Type              string   `json:"type"`
	Subtype           string   `json:"subtype"`
	Model             string   `json:"model"`
	SessionID         string   `json:"session_id"`
	ClaudeCodeVersion string   `json:"claude_code_version"`
	Cwd               string   `json:"cwd"`
	Tools             []string `json:"tools"`
	PermissionMode    string   `json:"permissionMode"`
	UUID              string   `json:"uuid"`
}

type AssistantMessage struct {
	Type            string          `json:"type"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
}

type ResultMessage struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	DurationMS   int64           `json:"duration_ms"`
	DurationAPI  int64           `json:"duration_api_ms"`
	IsError      bool            `json:"is_error"`
	NumTurns     int             `json:"num_turns"`
	Result       string          `json:"result,omitempty"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Errors       []string        `json:"errors,omitempty"`
	UUID         string          `json:"uuid"`
	SessionID    string          `json:"session_id"`
	Usage        json.RawMessage `json:"usage,omitempty"`
}

type StdoutControlRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

type ControlRequestInner struct {
	Subtype   string `json:"subtype"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// stdout message type constants
const (
	MsgTypeSystem                    = "system"
	MsgTypeAssistant                 = "assistant"
	MsgTypeResult                    = "result"
	MsgTypeControlRequest            = "control_request"
	MsgTypeControlResponse           = "control_response"
	MsgTypeControlCancelRequest      = "control_cancel_request"
	MsgTypeStreamEvent               = "stream_event"
	MsgTypeKeepAlive                 = "keep_alive"
	MsgTypeStreamlinedText           = "streamlined_text"
	MsgTypeStreamlinedToolUseSummary = "streamlined_tool_use_summary"
	MsgTypePostTurnSummary           = "post_turn_summary"
	MsgTypePromptSuggestion          = "prompt_suggestion"
	MsgTypeToolProgress              = "tool_progress"

	SubtypeInit   = "init"
	SubtypeStatus = "status"

	ControlSubtypeCanUseTool  = "can_use_tool"
	ControlSubtypeElicitation = "elicitation"
	ControlSubtypeInitialize  = "initialize"
)
