// Package fake provides a programmable channel.Channel implementation for
// tests. Outbound messages are recorded in-memory; inbound messages can be
// pushed by tests via Inject().
package fake

import (
	"context"
	"sync"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// Channel is a test double for channel.Channel.
type Channel struct {
	mu       sync.Mutex
	outbound []channel.OutboundMessage
	updates  []Update
	reacts   []Reaction
	handler  channel.InboundHandler
	started  bool
	nextID   int
}

type Update struct {
	MessageID string
	Message   channel.OutboundMessage
}

type Reaction struct {
	MessageID string
	Emoji     string
}

func New() *Channel { return &Channel{} }

func (c *Channel) Kind() string { return "fake" }

func (c *Channel) Start(ctx context.Context, handler channel.InboundHandler) error {
	c.mu.Lock()
	c.handler = handler
	c.started = true
	c.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (c *Channel) Shutdown() {
	c.mu.Lock()
	c.started = false
	c.mu.Unlock()
}

func (c *Channel) SendMessage(ctx context.Context, msg channel.OutboundMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outbound = append(c.outbound, msg)
	c.nextID++
	return formatID(c.nextID), nil
}

func (c *Channel) UpdateMessage(ctx context.Context, messageID string, msg channel.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, Update{MessageID: messageID, Message: msg})
	return nil
}

func (c *Channel) Reaction(messageID, emoji string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reacts = append(c.reacts, Reaction{MessageID: messageID, Emoji: emoji})
	return nil
}

// Inject pushes an inbound message to the registered handler. Returns false
// if Start has not been called.
func (c *Channel) Inject(ctx context.Context, m channel.InboundMessage) bool {
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h == nil {
		return false
	}
	h.OnMessage(ctx, m)
	return true
}

// Outbound returns a copy of all sent messages.
func (c *Channel) Outbound() []channel.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channel.OutboundMessage, len(c.outbound))
	copy(out, c.outbound)
	return out
}

// Updates returns a copy of all UpdateMessage calls.
func (c *Channel) Updates() []Update {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Update, len(c.updates))
	copy(out, c.updates)
	return out
}

// Reactions returns a copy of all Reaction calls.
func (c *Channel) Reactions() []Reaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Reaction, len(c.reacts))
	copy(out, c.reacts)
	return out
}

func formatID(n int) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "fake-msg-0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{hex[n%16]}, digits...)
		n /= 16
	}
	return "fake-msg-" + string(digits)
}
