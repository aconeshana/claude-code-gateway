package protocol

import (
	"encoding/json"
	"fmt"
)

func AutoAllowPermission(raw json.RawMessage) (*ControlResponse, error) {
	var req StdoutControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	var inner ControlRequestInner
	if err := json.Unmarshal(req.Request, &inner); err != nil {
		return nil, err
	}

	return NewAllowResponse(req.RequestID, inner.ToolUseID, nil), nil
}

func ParseControlRequestInner(raw json.RawMessage) (ControlRequestInner, error) {
	var req StdoutControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return ControlRequestInner{}, err
	}
	var inner ControlRequestInner
	if err := json.Unmarshal(req.Request, &inner); err != nil {
		return ControlRequestInner{}, err
	}
	return inner, nil
}

type ElicitationQuestion struct {
	Question    string              `json:"question"`
	Header      string              `json:"header"`
	Options     []ElicitationOption `json:"options"`
	MultiSelect bool                `json:"multiSelect"`
}

type ElicitationOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type ElicitationRequest struct {
	RequestID string
	ToolUseID string
	Input     map[string]interface{}
	Questions []ElicitationQuestion
}

func ParseElicitation(raw json.RawMessage) (*ElicitationRequest, error) {
	var req StdoutControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	var inner struct {
		Subtype   string                 `json:"subtype"`
		ToolName  string                 `json:"tool_name"`
		ToolUseID string                 `json:"tool_use_id"`
		Input     map[string]interface{} `json:"input"`
	}
	if err := json.Unmarshal(req.Request, &inner); err != nil {
		return nil, err
	}

	if inner.ToolName != "AskUserQuestion" {
		return nil, fmt.Errorf("not an AskUserQuestion request: tool=%s", inner.ToolName)
	}

	var questions []ElicitationQuestion
	if qs, ok := inner.Input["questions"]; ok {
		qsJSON, _ := json.Marshal(qs)
		_ = json.Unmarshal(qsJSON, &questions)
	}

	return &ElicitationRequest{
		RequestID: req.RequestID,
		ToolUseID: inner.ToolUseID,
		Input:     inner.Input,
		Questions: questions,
	}, nil
}
