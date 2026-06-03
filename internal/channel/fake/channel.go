// Package fake provides a programmable channel.Channel implementation for
// tests. Outbound messages are recorded in-memory; inbound messages can be
// pushed by tests via Inject().
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// Channel is a test double for channel.Channel.
type Channel struct {
	mu            sync.Mutex
	outbound      []channel.OutboundMessage
	updates       []Update
	reacts        []Reaction
	removedReacts []Reaction
	handler       channel.InboundHandler
	started       bool
	nextID        int

	// sendErrFunc, when set, lets tests inject an error per outbound call.
	// Called with the OutboundMessage about to be sent; returning a non-nil
	// error skips recording and propagates that error to the caller. Common
	// use: simulate Lark's "reply anchor missing" so bridges can exercise the
	// thread fallback path.
	sendErrFunc func(channel.OutboundMessage) error
}

type Update struct {
	MessageID string
	Message   channel.OutboundMessage
}

type Reaction struct {
	MessageID string
	Emoji     string
	ID        string // populated by AddReaction; empty for add-only path
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
	if c.sendErrFunc != nil {
		if err := c.sendErrFunc(msg); err != nil {
			return "", err
		}
	}
	c.outbound = append(c.outbound, msg)
	c.nextID++
	return formatID(c.nextID), nil
}

// SetSendErrorFunc lets tests inject errors per outbound message. The function
// is called BEFORE recording, so a non-nil error suppresses the outbound
// record AND propagates the error to the caller. Pass nil to disable.
func (c *Channel) SetSendErrorFunc(f func(channel.OutboundMessage) error) {
	c.mu.Lock()
	c.sendErrFunc = f
	c.mu.Unlock()
}

func (c *Channel) UpdateMessage(ctx context.Context, messageID string, msg channel.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, Update{MessageID: messageID, Message: msg})
	return nil
}

// OpenThread implements channel.ThreadOpener. The fake fabricates a
// deterministic thread id keyed off the anchor and records the call as a
// regular outbound message tagged with the anchor's ReplyToMessageID so
// tests can assert on it.
func (c *Channel) OpenThread(ctx context.Context, anchorMsgID string, msg channel.OutboundMessage) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErrFunc != nil {
		recorded := msg
		recorded.ReplyToMessageID = anchorMsgID
		recorded.OpenThread = true
		if err := c.sendErrFunc(recorded); err != nil {
			return "", "", err
		}
	}
	c.nextID++
	msgID := formatID(c.nextID)
	threadID := "fake-thread-" + anchorMsgID
	recorded := msg
	recorded.ReplyToMessageID = anchorMsgID
	recorded.OpenThread = true
	c.outbound = append(c.outbound, recorded)
	return msgID, threadID, nil
}

func (c *Channel) Reaction(messageID, emoji string) error {
	_, err := c.AddReaction(messageID, emoji)
	return err
}

// AddReaction records the reaction and synthesizes a deterministic id so
// tests can later assert RemoveReaction was called with the right pair.
func (c *Channel) AddReaction(messageID, emoji string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := fmt.Sprintf("rxn-%d", len(c.reacts))
	c.reacts = append(c.reacts, Reaction{MessageID: messageID, Emoji: emoji, ID: id})
	return id, nil
}

// RemoveReaction records the cleanup as a Reaction entry with Emoji == ""
// so test assertions can distinguish add-then-remove pairs by ID.
func (c *Channel) RemoveReaction(messageID, reactionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removedReacts = append(c.removedReacts, Reaction{MessageID: messageID, ID: reactionID})
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

// RemovedReactions returns a copy of all RemoveReaction calls.
func (c *Channel) RemovedReactions() []Reaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Reaction, len(c.removedReacts))
	copy(out, c.removedReacts)
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
