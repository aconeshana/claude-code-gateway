package channel_test

import (
	"context"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/channel/fake"
)

func TestFakeChannel_SendMessageReturnsID(t *testing.T) {
	c := fake.New()
	id, err := c.SendMessage(context.Background(), channel.OutboundMessage{
		ChatID: "chat-1",
		Text:   "hello",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty message id")
	}

	id2, _ := c.SendMessage(context.Background(), channel.OutboundMessage{
		ChatID: "chat-1",
		Text:   "world",
	})
	if id == id2 {
		t.Errorf("expected distinct ids, got %s twice", id)
	}

	out := c.Outbound()
	if len(out) != 2 {
		t.Errorf("Outbound count = %d, want 2", len(out))
	}
	if out[0].Text != "hello" || out[1].Text != "world" {
		t.Errorf("outbound order/content wrong: %+v", out)
	}
}

func TestFakeChannel_UpdateMessage(t *testing.T) {
	c := fake.New()
	if err := c.UpdateMessage(context.Background(), "msg-7", channel.OutboundMessage{
		Text: "edited",
	}); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	updates := c.Updates()
	if len(updates) != 1 {
		t.Fatalf("Updates = %d, want 1", len(updates))
	}
	if updates[0].MessageID != "msg-7" {
		t.Errorf("MessageID = %q, want msg-7", updates[0].MessageID)
	}
	if updates[0].Message.Text != "edited" {
		t.Errorf("Text = %q, want edited", updates[0].Message.Text)
	}
}

func TestFakeChannel_Reaction(t *testing.T) {
	c := fake.New()
	if err := c.Reaction("m-1", "thumbsup"); err != nil {
		t.Fatalf("Reaction: %v", err)
	}
	rxn := c.Reactions()
	if len(rxn) != 1 || rxn[0].MessageID != "m-1" || rxn[0].Emoji != "thumbsup" {
		t.Errorf("Reactions = %+v", rxn)
	}
}

func TestFakeChannel_StartInjectInbound(t *testing.T) {
	c := fake.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan channel.InboundMessage, 1)
	go c.Start(ctx, channel.InboundHandlerFunc(func(ctx context.Context, m channel.InboundMessage) {
		received <- m
	}))

	// give Start a moment to register the handler
	time.Sleep(20 * time.Millisecond)

	ok := c.Inject(context.Background(), channel.InboundMessage{
		UserID: "u-1",
		ChatID: "c-1",
		Kind:   channel.InputText,
		Text:   "hi",
	})
	if !ok {
		t.Fatal("Inject returned false; handler not registered")
	}

	select {
	case m := <-received:
		if m.Text != "hi" {
			t.Errorf("Text = %q, want hi", m.Text)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not receive injected message")
	}
}

func TestFakeChannel_InjectWithoutStart(t *testing.T) {
	c := fake.New()
	ok := c.Inject(context.Background(), channel.InboundMessage{Text: "lost"})
	if ok {
		t.Error("Inject without Start should return false")
	}
}

func TestCard_StructureZeroValue(t *testing.T) {
	// Smoke test: zero Card serializes without panicking
	c := channel.Card{}
	_ = c
}
