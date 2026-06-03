package feishu

// TestGroupThreadBypassesMentionFilter is a regression test for the bug where
// messages sent inside a Lark thread (话题) in a group chat were silently
// dropped because the @mention filter ran before checking ThreadID.
//
// Repro: user sends text in a group thread without @-ing the bot → the message
// must still be dispatched (thread context implies intent to talk to the bot).
//
// Broken by: feat(bridge): group UX overhaul, mention filter (e6a0034).
// Fixed by: skip mention requirement when inbound.ThreadID != "".

import (
	"context"
	"sync"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// captureHandler collects every InboundMessage dispatched to it.
type captureHandler struct {
	mu   sync.Mutex
	msgs []channel.InboundMessage
}

func (h *captureHandler) OnMessage(_ context.Context, m channel.InboundMessage) {
	h.mu.Lock()
	h.msgs = append(h.msgs, m)
	h.mu.Unlock()
}

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

func (h *captureHandler) last() (channel.InboundMessage, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.msgs) == 0 {
		return channel.InboundMessage{}, false
	}
	return h.msgs[len(h.msgs)-1], true
}

// makeGroupTextEvent constructs a minimal P2MessageReceiveV1 for a group text
// message. threadID may be empty (main chat) or non-empty (inside a thread).
// mentions is the list of @-mentions in the message.
func makeGroupTextEvent(msgID, userID, chatID, text, threadID string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	content := `{"text":"` + text + `"}`
	chatType := "group"
	msgType := "text"
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &userID},
			},
			Message: &larkim.EventMessage{
				MessageId:   &msgID,
				ChatId:      &chatID,
				ChatType:    &chatType,
				MessageType: &msgType,
				Content:     &content,
				ThreadId:    nilIfEmpty(threadID),
				Mentions:    mentions,
			},
		},
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func newTestChannel(botOpenID string) *Channel {
	c := New(Config{AppID: "test-app", AppSecret: "test-secret"})
	c.botOpenID = botOpenID
	return c
}

func TestGroupThread_NoMention_Dropped(t *testing.T) {
	// Group threads support multiple participants, so they follow the same
	// @mention rule as the main group chat (rule 2). P2P threads are unaffected.
	c := newTestChannel("ou_bot")
	h := &captureHandler{}
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()

	ev := makeGroupTextEvent("msg-1", "ou_user", "oc_chat", "zerotier 好了没", "omt_thread1", nil)
	if err := c.onMessageReceive(context.Background(), ev); err != nil {
		t.Fatalf("onMessageReceive: %v", err)
	}
	if h.count() != 0 {
		t.Fatalf("expected 0 dispatches (group thread without @mention must be dropped), got %d", h.count())
	}
}

func TestGroupMain_NoMention_Dropped(t *testing.T) {
	// Main-chat message without @mention must still be dropped.
	c := newTestChannel("ou_bot")
	h := &captureHandler{}
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()

	ev := makeGroupTextEvent("msg-2", "ou_user", "oc_chat", "hello", "", nil)
	if err := c.onMessageReceive(context.Background(), ev); err != nil {
		t.Fatalf("onMessageReceive: %v", err)
	}
	if h.count() != 0 {
		t.Fatalf("expected 0 dispatches, got %d (main-chat without @mention should be dropped)", h.count())
	}
}

func TestGroupMain_WithMention_Dispatched(t *testing.T) {
	// Main-chat message @-mentioning the bot must be dispatched (existing behavior).
	c := newTestChannel("ou_bot")
	h := &captureHandler{}
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()

	ev := makeGroupTextEvent("msg-3", "ou_user", "oc_chat", "hello", "", []*larkim.MentionEvent{mentionFor("ou_bot")})
	if err := c.onMessageReceive(context.Background(), ev); err != nil {
		t.Fatalf("onMessageReceive: %v", err)
	}
	if h.count() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", h.count())
	}
}

func TestGroupThread_WithMention_MentionStripped(t *testing.T) {
	// When the user @-mentions the bot inside a thread, the mention tag should
	// be stripped from the final text (same as main-chat behavior).
	c := newTestChannel("ou_bot")
	h := &captureHandler{}
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()

	// Content includes a @_user_1 placeholder that stripMentions removes.
	msgID := "msg-4"
	userID := "ou_user"
	chatID := "oc_chat"
	chatType := "group"
	msgType := "text"
	threadID := "omt_thread2"
	content := `{"text":"@_user_1 help me"}`
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &userID},
			},
			Message: &larkim.EventMessage{
				MessageId:   &msgID,
				ChatId:      &chatID,
				ChatType:    &chatType,
				MessageType: &msgType,
				Content:     &content,
				ThreadId:    &threadID,
				Mentions:    []*larkim.MentionEvent{mentionFor("ou_bot")},
			},
		},
	}
	if err := c.onMessageReceive(context.Background(), ev); err != nil {
		t.Fatalf("onMessageReceive: %v", err)
	}
	if h.count() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", h.count())
	}
	msg, _ := h.last()
	if msg.Text == "" {
		t.Error("text should not be empty after stripping @mention")
	}
}

func TestGroupThread_OnlyMention_Dropped(t *testing.T) {
	// If the entire thread message is just a @mention (nothing left after strip),
	// it should be dropped even in a thread.
	c := newTestChannel("ou_bot")
	h := &captureHandler{}
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()

	msgID := "msg-5"
	userID := "ou_user"
	chatID := "oc_chat"
	chatType := "group"
	msgType := "text"
	threadID := "omt_thread3"
	content := `{"text":"@_user_1"}`
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &userID},
			},
			Message: &larkim.EventMessage{
				MessageId:   &msgID,
				ChatId:      &chatID,
				ChatType:    &chatType,
				MessageType: &msgType,
				Content:     &content,
				ThreadId:    &threadID,
				Mentions:    []*larkim.MentionEvent{mentionFor("ou_bot")},
			},
		},
	}
	if err := c.onMessageReceive(context.Background(), ev); err != nil {
		t.Fatalf("onMessageReceive: %v", err)
	}
	if h.count() != 0 {
		t.Fatalf("expected 0 dispatches (text empty after strip), got %d", h.count())
	}
}
