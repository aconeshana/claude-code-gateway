// Package channel defines the abstraction for an inbound/outbound messaging
// channel (Feishu, Slack, Discord, …). The session manager and bridge layer
// consume Channels without knowing the underlying transport.
package channel

import (
	"context"
	"encoding/json"
)

// Kind identifiers — implementations report one of these via Channel.Kind()
// and the gateway stores it in Session.ChannelKind. Add new constants when
// adding a new channel implementation.
const (
	KindFeishu   = "feishu"
	KindDingTalk = "dingtalk"
	// KindSlack = "slack"  — future
	// KindWeb   = "web"    — test.html / WebSocket inspector
)

// Channel is a bidirectional IM transport. Implementations are expected to
// translate platform-specific events into the inbound/outbound types defined
// in this package.
type Channel interface {
	// Kind returns a short identifier; see KindXxx constants.
	Kind() string

	// Start opens the channel's connection (e.g. WebSocket to Feishu) and
	// dispatches inbound events to handler. Blocks until ctx is cancelled or
	// the channel encounters an unrecoverable error.
	Start(ctx context.Context, handler InboundHandler) error

	// Shutdown closes the connection cleanly.
	Shutdown()

	// SendMessage delivers an outbound message and returns the platform-side
	// message id (used for later UpdateMessage / Reaction calls).
	SendMessage(ctx context.Context, msg OutboundMessage) (messageID string, err error)

	// UpdateMessage replaces an existing message in place. The semantics
	// depend on the underlying platform; for Feishu cards this rewrites the
	// card content.
	UpdateMessage(ctx context.Context, messageID string, msg OutboundMessage) error

	// Reaction adds a reaction emoji to a message. Implementations that do
	// not support reactions may treat this as a no-op.
	Reaction(messageID, emoji string) error
}

// InboundHandler receives messages delivered via Channel.Start.
type InboundHandler interface {
	OnMessage(ctx context.Context, m InboundMessage)
}

// ThreadOpener is an optional capability for channels that support threads
// (currently Feishu / Lark). Bridges detect support via type assertion:
//
//	if opener, ok := ch.(channel.ThreadOpener); ok { ... }
//
// Channels without thread support (e.g. DingTalk) simply don't implement
// this interface, so the bridge degrades gracefully — sessions stay in the
// main chat instead of being moved into a thread.
type ThreadOpener interface {
	// OpenThread sends a reply to anchorMsgID with reply_in_thread=true,
	// causing the platform to spawn a new thread anchored at that message.
	// Returns the new message id and the platform's thread id, both used by
	// the bridge to bind the session to its thread for future routing.
	OpenThread(ctx context.Context, anchorMsgID string, msg OutboundMessage) (msgID, threadID string, err error)
}

// InboundHandlerFunc is a function adapter for InboundHandler.
type InboundHandlerFunc func(ctx context.Context, m InboundMessage)

func (f InboundHandlerFunc) OnMessage(ctx context.Context, m InboundMessage) {
	f(ctx, m)
}

// --- Inbound model ---

type InputKind string

const (
	InputText       InputKind = "text"
	InputImage      InputKind = "image"
	InputBlocks     InputKind = "blocks"
	InputCardAction InputKind = "card_action"
)

// InboundMessage is the channel-agnostic view of a message received from
// the user.
type InboundMessage struct {
	ChannelKind string
	UserID      string
	ChatID      string
	MessageID   string
	Kind        InputKind

	Text   string        // populated for InputText
	Blocks []interface{} // populated for InputBlocks / InputImage
	Action *CardAction   // populated for InputCardAction

	// Lark thread context. Non-empty only on platforms that expose threads
	// (currently Feishu). ThreadID is the stable per-thread identifier
	// (omt_*); RootID is the message that started the thread (om_*);
	// ParentID is the direct message being replied to (om_*). Bridges route
	// inbound messages to a session by ThreadID when it's set.
	ThreadID string
	RootID   string
	ParentID string

	// Reply, when set by the channel, lets the handler synchronously supply a
	// card to send back in the same response cycle. Only meaningful for
	// platforms that expect a synchronous response to an inbound event
	// (e.g. Lark form-submit callbacks): if the handler later calls
	// UpdateMessage asynchronously, the client may revert to the original
	// card before the update arrives. Calling Reply guarantees the
	// returned card replaces the source card atomically.
	//
	// nil for platforms/event-kinds that don't support synchronous replies.
	Reply func(Card)

	// Raw is the underlying platform-specific event JSON. Bridges should
	// avoid depending on this; it's exposed for platform-specific edge cases.
	Raw json.RawMessage
}

// CardAction captures a button or form submission from an existing card.
type CardAction struct {
	Name      string                 // discriminator from the button payload
	Values    map[string]interface{} // raw value bag (button-specific)
	FormValue map[string]interface{} // form field values (if the action came from a form)
}

// --- Outbound model ---

// OutboundMessage is sent via Channel.SendMessage or UpdateMessage. Exactly
// one of Card or Text should be set; Card takes precedence if both are.
type OutboundMessage struct {
	ChatID string
	Card   *Card
	Text   string

	// ReplyToMessageID, when non-empty, tells the channel to use the platform's
	// Reply API (anchored at this message) instead of creating an independent
	// message. On Lark this puts the response in the same thread as the
	// anchor. Channels without a reply primitive (e.g. DingTalk) ignore this.
	ReplyToMessageID string

	// OpenThread requests that the reply opens a new thread anchored at
	// ReplyToMessageID. Only meaningful when ReplyToMessageID is set and the
	// platform supports threads.
	OpenThread bool
}

// Tone describes the visual style of a card. Implementations map this to a
// platform-appropriate color (Feishu: red/orange/green/grey/blue).
type Tone string

const (
	ToneInfo    Tone = "info"
	ToneSuccess Tone = "success"
	ToneWarning Tone = "warning"
	ToneError   Tone = "error"
	ToneNeutral Tone = "neutral"
)

// Card is a structured card (typically rendered as a Feishu interactive card
// or Slack block kit message).
type Card struct {
	Title    string
	Tone     Tone
	Sections []Section
}

// Section is one logical block of a card.
type Section struct {
	Markdown string
	Buttons  []Button
	Form     *Form
	Note     string
	Divider  bool

	// ButtonLayout controls how multiple Buttons render on supported channels:
	//   ""         → stacked / default (cluster left)
	//   "fill"     → column_set with equal-width buttons spanning the card
	//   "trailing" → Markdown on the left, single button right-aligned (the
	//                section must have exactly one button to use this layout)
	// Channels that don't support advanced layouts fall back to default.
	ButtonLayout string
}

// Button represents an interactive action in a card.
type Button struct {
	Label  string
	Style  string            // primary / default / danger
	Action map[string]string // payload sent back as InboundMessage.Action.Values
}

// Form is a multi-field input rendered inside a card section.
type Form struct {
	FormID string
	Fields []FormField
	Submit Button

	// LeadingButtons are non-submit buttons rendered alongside the form's
	// input/submit row (e.g. a [详情] button next to an [输入框][执行] pair).
	// On Lark this is implemented by wrapping the form contents in a
	// column_set so the buttons sit inline with the input. Channels that
	// don't support inline layouts fall back to stacking them above the input.
	LeadingButtons []Button

	// SecondaryButtons are non-submit buttons rendered alongside the submit
	// button (e.g. [取消] next to [保存]). On Lark they go into the same
	// column_set as Submit, all width=auto, horizontally aligned left.
	// Channels without inline support fall back to stacking them below.
	SecondaryButtons []Button
}

type FormField struct {
	Name        string
	Label       string
	Placeholder string
	Initial     string
}
