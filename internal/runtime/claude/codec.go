package claude

import (
	"encoding/json"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// Codec implements runtime.Codec on top of the existing protocol package.
// All methods are safe for concurrent use.
type Codec struct{}

func (Codec) EncodeUserText(text, uuid string) ([]byte, error) {
	return json.Marshal(protocol.NewUserMessage(text, uuid))
}

func (Codec) EncodeUserBlocks(blocks []interface{}, uuid string) ([]byte, error) {
	return json.Marshal(protocol.NewUserMessageWithBlocks(blocks, uuid))
}

func (Codec) EncodeControlResponse(requestID, toolUseID, behavior, message string, updatedInput map[string]interface{}) ([]byte, error) {
	switch behavior {
	case "allow":
		return json.Marshal(protocol.NewAllowResponse(requestID, toolUseID, updatedInput))
	case "allowForSession":
		ui := updatedInput
		if ui == nil {
			ui = map[string]interface{}{}
		}
		resp := protocol.NewAllowResponse(requestID, toolUseID, ui)
		resp.Response.Response = protocol.PermissionResult{
			Behavior:     "allowForSession",
			ToolUseID:    toolUseID,
			UpdatedInput: ui,
		}
		return json.Marshal(resp)
	case "deny":
		return json.Marshal(protocol.NewDenyResponse(requestID, toolUseID, message))
	default:
		return nil, ErrInvalidBehavior(behavior)
	}
}

func (Codec) EncodeControl(payload json.RawMessage) ([]byte, error) {
	return json.Marshal(protocol.NewStdinControlRequest(payload))
}

func (Codec) EncodeKeepAlive() ([]byte, error) {
	return json.Marshal(protocol.NewKeepAlive())
}

func (Codec) ParseEvent(raw json.RawMessage) (runtime.Event, error) {
	msgType, subtype, err := protocol.ParseType(raw)
	if err != nil {
		return runtime.Event{Raw: raw, Kind: runtime.KindUnknown}, err
	}
	ev := runtime.Event{
		Subtype: subtype,
		Raw:     raw,
	}
	switch msgType {
	case protocol.MsgTypeSystem:
		if subtype == protocol.SubtypeInit {
			ev.Kind = runtime.KindInit
			var init protocol.SystemInitMessage
			if json.Unmarshal(raw, &init) == nil {
				ev.RuntimeID = init.SessionID
			}
		}
	case protocol.MsgTypeAssistant:
		ev.Kind = runtime.KindAssistant
	case protocol.MsgTypeResult:
		ev.Kind = runtime.KindResult
	case protocol.MsgTypeControlRequest:
		ev.Kind = runtime.KindControlRequest
	case protocol.MsgTypeControlResponse:
		ev.Kind = runtime.KindControlResponse
	case protocol.MsgTypeControlCancelRequest:
		ev.Kind = runtime.KindControlCancel
	case protocol.MsgTypeKeepAlive:
		ev.Kind = runtime.KindKeepAlive
	case protocol.MsgTypeToolProgress:
		ev.Kind = runtime.KindToolProgress
	default:
		ev.Kind = runtime.KindUnknown
	}
	return ev, nil
}

// ErrInvalidBehavior is returned when EncodeControlResponse receives an
// unrecognized behavior string.
type ErrInvalidBehavior string

func (e ErrInvalidBehavior) Error() string {
	return "invalid behavior: " + string(e)
}
