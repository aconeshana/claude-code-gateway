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

// TestRenderTodosSections_Empty: nil/empty todos yields no sections.
func TestRenderTodosSections_Empty(t *testing.T) {
	if s := renderTodosSections(nil, 0, ""); s != nil {
		t.Errorf("expected nil for nil todos, got %v", s)
	}
	if s := renderTodosSections([]todoItem{}, 0, ""); s != nil {
		t.Errorf("expected nil for empty todos, got %v", s)
	}
}

// TestRenderTodosSections_Icons: each status maps to the right icon inside
// the button label.
func TestRenderTodosSections_Icons(t *testing.T) {
	todos := []todoItem{
		{Content: "完成设计", Status: "completed"},
		{Content: "写代码", Status: "in_progress"},
		{Content: "写文档", Status: "pending"},
	}
	sections := renderTodosSections(todos, 0, "")
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3 (one per item)", len(sections))
	}
	labels := make([]string, 3)
	for i, s := range sections {
		if len(s.Buttons) != 1 {
			t.Errorf("section %d: want 1 button, got %d", i, len(s.Buttons))
			continue
		}
		labels[i] = s.Buttons[0].Label
	}
	if !strings.Contains(labels[0], "✅") {
		t.Errorf("completed item label %q missing ✅", labels[0])
	}
	if !strings.Contains(labels[1], "⏳") {
		t.Errorf("in_progress item label %q missing ⏳", labels[1])
	}
	if !strings.Contains(labels[2], "🔲") {
		t.Errorf("pending item label %q missing 🔲", labels[2])
	}
}

// TestRenderTodosSections_Pagination: >8 todos produces a header section and
// prev/next navigation buttons.
func TestRenderTodosSections_Pagination(t *testing.T) {
	todos := make([]todoItem, 10)
	for i := range todos {
		todos[i] = todoItem{Content: "task", Status: "pending"}
	}

	// Page 0: header + 8 items + next button = 10 sections
	sections := renderTodosSections(todos, 0, "sess-1")
	// header (1) + 8 items + nav (1) = 10
	if len(sections) != 10 {
		t.Errorf("page 0: got %d sections, want 10", len(sections))
	}
	nav := sections[len(sections)-1]
	if len(nav.Buttons) != 1 || !strings.Contains(nav.Buttons[0].Label, "下一页") {
		t.Errorf("page 0: nav should have only '下一页', got %+v", nav.Buttons)
	}
	if nav.Buttons[0].Action["page"] != "1" {
		t.Errorf("next button page = %q, want '1'", nav.Buttons[0].Action["page"])
	}

	// Page 1: header + 2 items + prev button = 4 sections
	sections = renderTodosSections(todos, 1, "sess-1")
	// header (1) + 2 items + nav (1) = 4
	if len(sections) != 4 {
		t.Errorf("page 1: got %d sections, want 4", len(sections))
	}
	nav = sections[len(sections)-1]
	if len(nav.Buttons) != 1 || !strings.Contains(nav.Buttons[0].Label, "上一页") {
		t.Errorf("page 1: nav should have only '上一页', got %+v", nav.Buttons)
	}
}

// TestRenderTodosSections_PageClamp: out-of-range page falls back to 0.
func TestRenderTodosSections_PageClamp(t *testing.T) {
	todos := []todoItem{{Content: "only one", Status: "pending"}}
	sections := renderTodosSections(todos, 99, "")
	if len(sections) != 1 {
		t.Errorf("clamped page: got %d sections, want 1", len(sections))
	}
}

// TestResultCard_TodosAreButtons: todos appear as button sections inside the
// Done card, not as a single markdown section.
func TestResultCard_TodosAreButtons(t *testing.T) {
	b, _, _ := newTestBridge(t)
	todos := []todoItem{
		{Content: "step one", Status: "completed"},
		{Content: "step two", Status: "pending"},
	}
	result := &protocol.ResultMessage{IsError: false}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "done", todos, 0, "", result, false)

	// content (1) + 2 button sections + note (1) = 4
	if len(card.Sections) != 4 {
		t.Errorf("got %d sections, want 4 (content + 2 todo buttons + note)", len(card.Sections))
	}
	// Button sections (indices 1 and 2) must have ✅ and 🔲 labels.
	if !strings.Contains(card.Sections[1].Buttons[0].Label, "✅") {
		t.Errorf("todo[0] button %q missing ✅", card.Sections[1].Buttons[0].Label)
	}
	if !strings.Contains(card.Sections[2].Buttons[0].Label, "🔲") {
		t.Errorf("todo[1] button %q missing 🔲", card.Sections[2].Buttons[0].Label)
	}
}

// TestResultCard_NoTodosSection_WhenEmpty: card with no todos still has
// exactly content + note (2 sections).
func TestResultCard_NoTodosSection_WhenEmpty(t *testing.T) {
	b, _, _ := newTestBridge(t)
	result := &protocol.ResultMessage{IsError: false}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "done", nil, 0, "", result, false)

	if len(card.Sections) != 2 {
		t.Errorf("got %d sections, want 2 (content + note)", len(card.Sections))
	}
}
