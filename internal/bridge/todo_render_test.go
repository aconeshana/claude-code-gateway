package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
)

// buildTodoWriteAssistantMsg constructs a stream-json assistant message
// containing a TodoWrite tool_use block, matching the format Claude Code emits.
func buildTodoWriteAssistantMsg(todos []map[string]string) json.RawMessage {
	content := []map[string]interface{}{
		{
			"type":  "tool_use",
			"name":  "TodoWrite",
			"input": map[string]interface{}{"todos": todos},
		},
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"type":    protocol.MsgTypeAssistant,
		"message": map[string]interface{}{"content": content},
	})
	return raw
}

func TestExtractTodoItems_ParsesTodoWrite(t *testing.T) {
	raw := buildTodoWriteAssistantMsg([]map[string]string{
		{"content": "设计接口", "status": "completed"},
		{"content": "写单测", "status": "in_progress"},
		{"content": "Code Review", "status": "pending"},
	})

	items := extractTodoItems(raw)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].Content != "设计接口" || items[0].Status != "completed" {
		t.Errorf("item[0] = %+v", items[0])
	}
	if items[1].Status != "in_progress" {
		t.Errorf("item[1].Status = %q, want in_progress", items[1].Status)
	}
	if items[2].Status != "pending" {
		t.Errorf("item[2].Status = %q, want pending", items[2].Status)
	}
}

func TestExtractTodoItems_NilForNonTodoWrite(t *testing.T) {
	// Assistant message with a Bash tool_use — should return nil
	content := []map[string]interface{}{
		{"type": "tool_use", "name": "Bash", "input": map[string]string{"command": "ls"}},
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"type":    protocol.MsgTypeAssistant,
		"message": map[string]interface{}{"content": content},
	})

	if items := extractTodoItems(raw); items != nil {
		t.Errorf("expected nil for non-TodoWrite message, got %v", items)
	}
}

func TestExtractTodoItems_NilForPlainText(t *testing.T) {
	content := []map[string]interface{}{
		{"type": "text", "text": "hello"},
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"type":    protocol.MsgTypeAssistant,
		"message": map[string]interface{}{"content": content},
	})

	if items := extractTodoItems(raw); items != nil {
		t.Errorf("expected nil for text-only message, got %v", items)
	}
}

func TestRenderTodosMarkdown_EmptySlice(t *testing.T) {
	if md := renderTodosMarkdown(nil); md != "" {
		t.Errorf("expected empty string for nil todos, got %q", md)
	}
	if md := renderTodosMarkdown([]todoItem{}); md != "" {
		t.Errorf("expected empty string for empty todos, got %q", md)
	}
}

func TestRenderTodosMarkdown_Statuses(t *testing.T) {
	todos := []todoItem{
		{Content: "完成设计", Status: "completed"},
		{Content: "写代码", Status: "in_progress"},
		{Content: "写文档", Status: "pending"},
	}
	md := renderTodosMarkdown(todos)

	if !strings.Contains(md, "✅") {
		t.Error("completed item should have ✅")
	}
	if !strings.Contains(md, "~~完成设计~~") {
		t.Error("completed item should have strikethrough")
	}
	if !strings.Contains(md, "⏳") {
		t.Error("in_progress item should have ⏳")
	}
	if !strings.Contains(md, "**写代码**") {
		t.Error("in_progress item should be bold")
	}
	if !strings.Contains(md, "🔲") {
		t.Error("pending item should have 🔲")
	}
}

func TestResultCard_TodosSectionAppears(t *testing.T) {
	b, _, _ := newTestBridge(t)
	todos := []todoItem{
		{Content: "step one", Status: "completed"},
		{Content: "step two", Status: "pending"},
	}
	result := &protocol.ResultMessage{IsError: false}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "done", todos, result, false)

	// Should have 3 sections: content, todos, HUD note
	if len(card.Sections) != 3 {
		t.Errorf("got %d sections, want 3 (content + todos + note)", len(card.Sections))
	}
	if !strings.Contains(card.Sections[1].Markdown, "✅") {
		t.Errorf("todos section missing ✅, got: %q", card.Sections[1].Markdown)
	}
}

func TestResultCard_NoTodosSection_WhenEmpty(t *testing.T) {
	b, _, _ := newTestBridge(t)
	result := &protocol.ResultMessage{IsError: false}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "done", nil, result, false)

	// Should have 2 sections: content, HUD note (no todos)
	if len(card.Sections) != 2 {
		t.Errorf("got %d sections, want 2 (content + note)", len(card.Sections))
	}
}
