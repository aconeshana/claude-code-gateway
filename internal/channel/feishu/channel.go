// Package feishu implements channel.Channel for Lark/Feishu.
package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// Config carries the Feishu app credentials and access policy.
type Config struct {
	AppID          string
	AppSecret      string
	AllowedUserIDs []string

	// BotInfoCachePath is the on-disk location where the bot's own open_id
	// is cached after the first successful /bot/v3/info call. Empty path
	// skips caching (Start() then fetches every restart). The file holds a
	// single line (the open_id, prefixed with ou_).
	BotInfoCachePath string
}

// Channel implements channel.Channel using the Lark long-poll WebSocket API.
// outbound via Lark REST; token cache is wrapped so we can invalidate on
// 99991663 (Invalid access token) — see token_cache.go.
type Channel struct {
	client   *lark.Client
	wsClient *larkws.Client
	cfg      Config
	tokenCC  *tokenCache

	mu          sync.RWMutex
	handler     channel.InboundHandler
	allowedUIDs map[string]bool
	dedup       map[string]struct{}

	// botOpenID is this bot's own open_id, used to filter group-message
	// mentions to "@ me only". Resolved at Start() via fetchBotOpenID,
	// then cached on disk for subsequent restarts. Empty when the fetch
	// failed — group mention filter then degrades to "any mention"
	// (current legacy behavior) so the bot stays usable.
	botOpenID string
}

// New constructs a Channel. Call Start(ctx, handler) to begin processing.
func New(cfg Config) *Channel {
	allowed := make(map[string]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}
	tc := newTokenCache()
	return &Channel{
		client:      lark.NewClient(cfg.AppID, cfg.AppSecret, lark.WithTokenCache(tc)),
		cfg:         cfg,
		tokenCC:     tc,
		allowedUIDs: allowed,
		dedup:       make(map[string]struct{}),
	}
}

func (c *Channel) Kind() string { return channel.KindFeishu }

// SetAllowedUserIDs replaces the user allowlist atomically. Empty list means
// "allow everyone". Used by /config to update the allowlist at runtime
// without a restart.
func (c *Channel) SetAllowedUserIDs(ids []string) {
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		next[id] = true
	}
	c.mu.Lock()
	c.allowedUIDs = next
	c.mu.Unlock()
}

// Start opens the WebSocket connection and dispatches events to handler.
// Returns when ctx is cancelled or the underlying client errors.
func (c *Channel) Start(ctx context.Context, handler channel.InboundHandler) error {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()

	c.resolveBotOpenID(ctx)

	disp := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(c.onMessageReceive).
		OnP2CardActionTrigger(c.onCardAction)

	c.wsClient = larkws.NewClient(c.cfg.AppID, c.cfg.AppSecret,
		larkws.WithEventHandler(disp),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	if len(c.allowedUIDs) > 0 {
		log.Printf("[channel/feishu] starting (app_id=%s, allowed=%d, bot_open_id=%s)",
			c.cfg.AppID, len(c.allowedUIDs), shortID(c.botOpenID))
	} else {
		log.Printf("[channel/feishu] starting (app_id=%s, bot_open_id=%s, WARNING: no user allowlist)",
			c.cfg.AppID, shortID(c.botOpenID))
	}
	return c.wsClient.Start(ctx)
}

// resolveBotOpenID populates c.botOpenID by reading the on-disk cache or,
// when missing/empty, fetching from the Lark API and persisting the result.
// All failures are non-fatal — leaving botOpenID empty degrades the group
// mention check to the legacy "any mention" behavior, which is incorrect
// but not broken.
func (c *Channel) resolveBotOpenID(ctx context.Context) {
	if cached := loadBotOpenIDFromCache(c.cfg.BotInfoCachePath); cached != "" {
		c.botOpenID = cached
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	openID, err := fetchBotOpenID(fetchCtx, c.cfg.AppID, c.cfg.AppSecret)
	if err != nil {
		log.Printf("[channel/feishu] WARN: failed to resolve bot open_id: %v "+
			"(group @ filter will accept any mention as fallback)", err)
		return
	}
	c.botOpenID = openID
	if err := saveBotOpenIDToCache(c.cfg.BotInfoCachePath, openID); err != nil {
		log.Printf("[channel/feishu] WARN: cache bot open_id to %s failed: %v",
			c.cfg.BotInfoCachePath, err)
	}
}

func (c *Channel) Shutdown() {
	// larkws.Client.Start exits on ctx cancellation; nothing to do here.
}

// SendMessage delivers an outbound message. Card takes precedence over Text.
// When msg.ReplyToMessageID is non-empty, the Reply API is used so the
// response lands in the same thread (or opens a new one if OpenThread=true).
// When msg.MentionUserID is non-empty, the user is @-mentioned (Lark syntax)
// inside the card's first markdown section or prepended to text content.
func (c *Channel) SendMessage(ctx context.Context, msg channel.OutboundMessage) (string, error) {
	textBody := msg.Text
	if msg.MentionUserID != "" && msg.Text != "" {
		textBody = larkAtTag(msg.MentionUserID) + " " + msg.Text
	}

	var (
		msgType string
		content string
	)
	switch {
	case msg.Card != nil:
		msgType = "interactive"
		card := *msg.Card
		if msg.MentionUserID != "" {
			injectMentionIntoCard(&card, msg.MentionUserID)
		}
		content = renderCard(card)
	case msg.Text != "":
		msgType = "text"
		raw, _ := json.Marshal(map[string]string{"text": textBody})
		content = string(raw)
	default:
		return "", fmt.Errorf("OutboundMessage has neither Card nor Text")
	}

	if msg.ReplyToMessageID != "" {
		msgID, _, err := c.sendReply(ctx, msg.ReplyToMessageID, msgType, content, msg.OpenThread)
		return msgID, err
	}
	if msgType == "interactive" {
		return c.sendCard(ctx, string(msg.ChatID), content)
	}
	return "", c.sendText(ctx, string(msg.ChatID), textBody)
}

// larkAtTag returns the Lark @-mention tag for a user id. The id can be an
// open_id (ou_*), user_id, or union_id — Lark's API resolves any of them.
func larkAtTag(userID string) string {
	return fmt.Sprintf(`<at user_id="%s"></at>`, userID)
}

// injectMentionIntoCard prepends a Lark @-mention tag to the card's first
// markdown section. If no section has markdown, a new one is inserted at the
// front.
//
// IMPORTANT: this mutates *card. The caller passes a local copy of the
// underlying Card struct (via `card := *msg.Card`), but `card.Sections`
// still shares the same backing slice as the caller's. We re-slice here
// into a fresh array so writes through `card.Sections[i] = …` don't bleed
// into the caller's Card — without this, the renderer's failure-fallback
// path (update fails → send) would inject the at-tag twice.
func injectMentionIntoCard(card *channel.Card, userID string) {
	card.Sections = append([]channel.Section(nil), card.Sections...)
	at := larkAtTag(userID) + " "
	for i := range card.Sections {
		if card.Sections[i].Markdown != "" {
			// Shallow-copy: clone the section before mutating to avoid
			// touching any shared underlying data.
			s := card.Sections[i]
			s.Markdown = at + s.Markdown
			card.Sections[i] = s
			return
		}
	}
	// No markdown section yet — prepend one.
	card.Sections = append([]channel.Section{{Markdown: at}}, card.Sections...)
}

func (c *Channel) UpdateMessage(ctx context.Context, messageID string, msg channel.OutboundMessage) error {
	if msg.Card == nil {
		return fmt.Errorf("UpdateMessage requires Card")
	}
	card := *msg.Card
	if msg.MentionUserID != "" {
		injectMentionIntoCard(&card, msg.MentionUserID)
	}
	return c.updateCard(ctx, messageID, renderCard(card))
}

func (c *Channel) Reaction(messageID, emoji string) error {
	if messageID == "" {
		return nil
	}
	_, err := c.addReaction(messageID, emoji)
	return err
}

// AddReaction adds the emoji reaction and returns the platform's
// reaction id. Callers that want to clean up later (e.g. clear "OnIt"
// when the agent finishes) must hold onto the id and pass it to
// RemoveReaction.
func (c *Channel) AddReaction(messageID, emoji string) (string, error) {
	if messageID == "" {
		return "", nil
	}
	return c.addReaction(messageID, emoji)
}

// RemoveReaction deletes a reaction by its platform id. Silently returns
// nil when either arg is empty so callers can call this unconditionally.
func (c *Channel) RemoveReaction(messageID, reactionID string) error {
	if messageID == "" || reactionID == "" {
		return nil
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	return c.withTokenRetry("removeReaction", func() (int, string, error) {
		resp, err := c.client.Im.MessageReaction.Delete(context.Background(), req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		return 0, "", nil
	})
}

// --- Lark SDK helpers ---

// larkInvalidAccessToken is the body-level error code Lark returns when the
// cached tenant_access_token has been invalidated server-side (HTTP 200,
// not 401). Triggered most commonly after a laptop sleep/wake where the
// process clock skewed relative to the issuance time.
const larkInvalidAccessToken = 99991663

// withTokenRetry runs the given call; if it fails with 99991663 the token
// cache is invalidated and the call is retried once. Returns the second
// error verbatim — if it still fails, the operator needs to look.
func (c *Channel) withTokenRetry(label string, call func() (int, string, error)) error {
	code, msg, err := call()
	if err == nil && code == 0 {
		return nil
	}
	if code == larkInvalidAccessToken {
		log.Printf("[channel/feishu] %s: token invalid (code=%d) — invalidating cache and retrying", label, code)
		c.tokenCC.InvalidateAll()
		code, msg, err = call()
		if err == nil && code == 0 {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("feishu API: %w", err)
	}
	return fmt.Errorf("feishu API: code=%d msg=%s", code, msg)
}

func (c *Channel) sendCard(ctx context.Context, chatID, cardJSON string) (string, error) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("interactive").
			ReceiveId(chatID).
			Content(cardJSON).
			Build()).
		Build()

	var msgID string
	err := c.withTokenRetry("sendCard", func() (int, string, error) {
		resp, err := c.client.Im.Message.Create(ctx, req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
		return 0, "", nil
	})
	return msgID, err
}

func (c *Channel) sendText(ctx context.Context, chatID, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("text").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()
	return c.withTokenRetry("sendText", func() (int, string, error) {
		resp, err := c.client.Im.Message.Create(ctx, req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		return 0, "", nil
	})
}

// sendReply posts a reply to anchorMsgID using the Lark Reply API. When
// openThread is true the reply opens a new thread anchored at that message
// (only effective for the first reply to a given anchor). Returns the new
// message id, the thread id (only meaningful when openThread is true OR the
// anchor is already in a thread), and any error. The returned error is
// wrapped with replyErrNotFound when the anchor was deleted / no longer
// accessible (HTTP body code 230020 — message not exist) so callers can
// fall back to a plain Create.
func (c *Channel) sendReply(ctx context.Context, anchorMsgID, msgType, content string, openThread bool) (msgID, threadID string, err error) {
	body := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(msgType).
		Content(content)
	if openThread {
		body = body.ReplyInThread(true)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(anchorMsgID).
		Body(body.Build()).
		Build()

	err = c.withTokenRetry("sendReply", func() (int, string, error) {
		resp, err := c.client.Im.Message.Reply(ctx, req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		if resp.Data != nil {
			if resp.Data.MessageId != nil {
				msgID = *resp.Data.MessageId
			}
			if resp.Data.ThreadId != nil {
				threadID = *resp.Data.ThreadId
			}
		}
		return 0, "", nil
	})
	if err != nil && isReplyAnchorMissing(err) {
		return msgID, threadID, errReplyAnchorMissing
	}
	return msgID, threadID, err
}

// OpenThread implements channel.ThreadOpener. It uses the Lark Reply API
// with reply_in_thread=true so the platform creates a new thread anchored at
// anchorMsgID, returning the new message id and the thread id for the bridge
// to bind to the session.
func (c *Channel) OpenThread(ctx context.Context, anchorMsgID string, msg channel.OutboundMessage) (string, string, error) {
	var (
		msgType string
		content string
	)
	switch {
	case msg.Card != nil:
		msgType = "interactive"
		content = renderCard(*msg.Card)
	case msg.Text != "":
		msgType = "text"
		raw, _ := json.Marshal(map[string]string{"text": msg.Text})
		content = string(raw)
	default:
		return "", "", fmt.Errorf("OpenThread: OutboundMessage has neither Card nor Text")
	}
	return c.sendReply(ctx, anchorMsgID, msgType, content, true)
}

// errReplyAnchorMissing is returned by sendReply when the anchor message no
// longer exists (the user deleted the thread root or the message). Bridge
// catches this to clear the session's thread binding and fall back to
// Create-to-main-chat with a user notification.
var errReplyAnchorMissing = fmt.Errorf("feishu: reply anchor missing")

// ErrReplyAnchorMissing is the exported sentinel for the same condition.
// Bridges should errors.Is(err, feishu.ErrReplyAnchorMissing) to detect a
// stale thread binding and recover (clear ThreadID, resend via main chat).
var ErrReplyAnchorMissing = errReplyAnchorMissing

// isReplyAnchorMissing matches the Lark body-level codes that mean
// "anchor message gone": 230020 (message not exist), 230002 (message
// recalled), 230001 (no permission to operate). Treats them all as
// recoverable by clearing the thread binding.
func isReplyAnchorMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"code=230020", "code=230002", "code=230001"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

func (c *Channel) updateCard(ctx context.Context, messageID, cardJSON string) error {
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build()
	return c.withTokenRetry("updateCard", func() (int, string, error) {
		resp, err := c.client.Im.Message.Patch(ctx, req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		return 0, "", nil
	})
}

func (c *Channel) addReaction(msgID, emojiType string) (string, error) {
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(msgID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build()

	var reactionID string
	err := c.withTokenRetry("addReaction", func() (int, string, error) {
		resp, err := c.client.Im.MessageReaction.Create(context.Background(), req)
		if err != nil {
			return 0, "", err
		}
		if !resp.Success() {
			return resp.Code, resp.Msg, nil
		}
		if resp.Data != nil && resp.Data.ReactionId != nil {
			reactionID = *resp.Data.ReactionId
		}
		return 0, "", nil
	})
	return reactionID, err
}

func (c *Channel) downloadMessageImage(msgID, imageKey string) ([]byte, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(msgID).
		FileKey(imageKey).
		Type("image").
		Build()

	resp, err := c.client.Im.MessageResource.Get(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("download image: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.File == nil {
		return nil, fmt.Errorf("download image: empty response")
	}
	buf := new(strings.Builder)
	_, err = copyAndLimit(buf, resp.File, 20*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	return []byte(buf.String()), nil
}

func copyAndLimit(dst *strings.Builder, src interface{ Read([]byte) (int, error) }, limit int64) (int64, error) {
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if total+int64(n) > limit {
				return total, fmt.Errorf("exceeds %d bytes", limit)
			}
			dst.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return total, nil
			}
			return total, err
		}
	}
}

// --- Inbound event handlers ---

func (c *Channel) onMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	msg := event.Event.Message
	sender := event.Event.Sender

	if sender == nil || sender.SenderId == nil || sender.SenderId.OpenId == nil {
		return nil
	}
	userID := *sender.SenderId.OpenId

	if !c.isAllowedUser(userID) {
		log.Printf("[channel/feishu] rejected message from unauthorized user %s", shortID(userID))
		return nil
	}

	msgID := ""
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}
	if c.isDuplicate(msgID) {
		return nil
	}

	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}

	chatType := ""
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}

	inbound := channel.InboundMessage{
		ChannelKind: channel.KindFeishu,
		UserID:      userID,
		ChatID:      chatID,
		MessageID:   msgID,
		IsGroup:     chatType == "group",
		ThreadID:    derefStr(msg.ThreadId),
		RootID:      derefStr(msg.RootId),
		ParentID:    derefStr(msg.ParentId),
	}

	msgType := ""
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	contentLen := 0
	if msg.Content != nil {
		contentLen = len(*msg.Content)
	}
	log.Printf("[channel/feishu] inbound msg type=%s user=%s chat=%s id=%s len=%d",
		msgType, shortID(userID), shortID(chatID), shortID(msgID), contentLen)

	switch msgType {
	case "text":
		text := extractTextContent(*msg.Content)
		if text == "" {
			return nil
		}
		if chatType == "group" && inbound.ThreadID == "" {
			// Thread messages bypass the @mention requirement: the user is
			// already inside a bot-session thread, so every message in that
			// thread is implicitly directed at the bot.
			if !c.isMentionedInGroup(msg.Mentions) {
				return nil
			}
			text = strings.TrimSpace(stripMentions(text))
			if text == "" {
				return nil
			}
		} else if chatType == "group" && inbound.ThreadID != "" {
			// Strip @bot mentions even in thread messages so the bot doesn't
			// see its own mention tag as part of the user's input.
			text = strings.TrimSpace(stripMentions(text))
			if text == "" {
				return nil
			}
		}
		inbound.Kind = channel.InputText
		inbound.Text = text

	case "image":
		var ic struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &ic); err != nil || ic.ImageKey == "" {
			log.Printf("[channel/feishu] failed to parse image content")
			return nil
		}
		block, err := c.imageKeyToBlock(msgID, ic.ImageKey)
		if err != nil {
			log.Printf("[channel/feishu] image conversion failed: %v", err)
			return nil
		}
		inbound.Kind = channel.InputBlocks
		inbound.Blocks = []interface{}{block}

	case "post":
		// Feishu's rich-text "post" type: title + nested rows of tags. We
		// flatten it into text + image blocks. Schema:
		//   {"zh_cn":{"title":"...","content":[[{"tag":"text","text":"..."},
		//                                       {"tag":"img","image_key":"..."}]]}}
		// Mention tags are flattened to their plain text (or dropped if
		// they target the bot itself, mirroring the text branch).
		text, imageKeys := parsePostContent(*msg.Content)
		if chatType == "group" && inbound.ThreadID == "" && !c.isMentionedInGroup(msg.Mentions) {
			return nil
		}
		var blocks []interface{}
		for _, key := range imageKeys {
			block, err := c.imageKeyToBlock(msgID, key)
			if err != nil {
				log.Printf("[channel/feishu] post image conversion failed: %v", err)
				continue
			}
			blocks = append(blocks, block)
		}
		if chatType == "group" {
			text = strings.TrimSpace(stripMentions(text))
		}
		if len(blocks) == 0 && text == "" {
			return nil
		}
		if len(blocks) == 0 {
			inbound.Kind = channel.InputText
			inbound.Text = text
		} else {
			inbound.Kind = channel.InputBlocks
			inbound.Blocks = blocks
			inbound.Text = text
		}

	default:
		log.Printf("[channel/feishu] unsupported message type: %s", msgType)
		// Tell the user explicitly so a silent drop doesn't masquerade as
		// the bot ignoring them. Cheap to send and avoids the "why no
		// reply" debugging round trips.
		_, _ = c.SendMessage(ctx, channel.OutboundMessage{
			ChatID: chatID,
			Text:   "暂不支持该消息类型: " + msgType + "(目前支持 text / image / post)",
		})
		return nil
	}

	c.dispatch(ctx, inbound)
	return nil
}

func (c *Channel) onCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return nil, nil
	}
	action := event.Event.Action
	operator := event.Event.Operator
	if operator == nil {
		return nil, nil
	}
	userID := operator.OpenID
	if !c.isAllowedUser(userID) {
		log.Printf("[channel/feishu] rejected card action from unauthorized user %s", shortID(userID))
		return nil, nil
	}

	chatID := ""
	cardMsgID := ""
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
		cardMsgID = event.Event.Context.OpenMessageID
	}
	if chatID == "" {
		return nil, nil
	}

	actionName, _ := action.Value["action"].(string)
	values := make(map[string]interface{}, len(action.Value))
	for k, v := range action.Value {
		values[k] = v
	}

	// Form submit: Lark sends only form_value + button.name (Value is nil).
	// We encode "<action>:<key>" into the submit button name; decode it here
	// so the bridge can route the submission like a normal action.
	if actionName == "" && len(action.FormValue) > 0 && action.Name != "" {
		if idx := strings.IndexByte(action.Name, ':'); idx > 0 {
			actionName = action.Name[:idx]
			values["action"] = actionName
			values["key"] = action.Name[idx+1:]
		}
	}

	log.Printf("[channel/feishu] card action: %s values=%v user=%s", actionName, values, shortID(userID))

	var (
		replyMu   sync.Mutex
		replyCard *channel.Card
	)

	inbound := channel.InboundMessage{
		ChannelKind: channel.KindFeishu,
		UserID:      userID,
		ChatID:      chatID,
		MessageID:   cardMsgID,
		Kind:        channel.InputCardAction,
		Action: &channel.CardAction{
			Name:      actionName,
			Values:    values,
			FormValue: action.FormValue,
		},
		Reply: func(card channel.Card) {
			replyMu.Lock()
			c := card
			replyCard = &c
			replyMu.Unlock()
		},
	}
	c.dispatch(ctx, inbound)

	replyMu.Lock()
	finalCard := replyCard
	replyMu.Unlock()
	if finalCard != nil {
		cardJSON := renderCard(*finalCard)
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(cardJSON), &raw); err == nil {
			return &callback.CardActionTriggerResponse{
				Card: &callback.Card{Type: "raw", Data: raw},
			}, nil
		}
	}
	return nil, nil
}

func (c *Channel) dispatch(ctx context.Context, m channel.InboundMessage) {
	c.mu.RLock()
	h := c.handler
	c.mu.RUnlock()
	if h == nil {
		return
	}
	h.OnMessage(ctx, m)
}

func (c *Channel) isAllowedUser(userID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.allowedUIDs) == 0 {
		return true
	}
	return c.allowedUIDs[userID]
}

func (c *Channel) isDuplicate(msgID string) bool {
	if msgID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.dedup[msgID]; ok {
		return true
	}
	if len(c.dedup) > 4096 {
		// crude cap to bound memory; older entries lost
		c.dedup = make(map[string]struct{})
	}
	c.dedup[msgID] = struct{}{}
	return false
}

func (c *Channel) imageKeyToBlock(msgID, imageKey string) (map[string]interface{}, error) {
	data, err := c.downloadMessageImage(msgID, imageKey)
	if err != nil {
		return nil, err
	}
	mediaType := detectMediaType(data)
	b64 := base64.StdEncoding.EncodeToString(data)
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       b64,
		},
	}, nil
}

func detectMediaType(data []byte) string {
	ct := http.DetectContentType(data)
	switch {
	case strings.HasPrefix(ct, "image/jpeg"):
		return "image/jpeg"
	case strings.HasPrefix(ct, "image/png"):
		return "image/png"
	case strings.HasPrefix(ct, "image/gif"):
		return "image/gif"
	case strings.HasPrefix(ct, "image/webp"):
		return "image/webp"
	default:
		return "image/png"
	}
}

func extractTextContent(contentJSON string) string {
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &c); err != nil {
		return ""
	}
	return strings.TrimSpace(c.Text)
}

// parsePostContent flattens a Feishu rich-text "post" message into plain text
// + a list of image_keys. Two on-wire shapes are observed:
//
//  1. Top-level locale wrap (older Open API docs):
//     {"zh_cn":{"title":"...","content":[[...]]}}
//  2. Already-unwrapped (the SDK strips the locale layer for us):
//     {"title":"...","content":[[{"tag":"text","text":"...","style":[]},
//     {"tag":"img","image_key":"..."}]]}
//
// We try shape 2 first (what the Lark Go SDK actually delivers in practice)
// and fall back to shape 1. Tag schema within a row:
//
//	{"tag":"text"|"a", "text":"..."}
//	{"tag":"at",       "user_name":"..."}
//	{"tag":"img",      "image_key":"..."}
//
// Title (if any) becomes the first line; each row in `content` becomes one
// line; tags within a row are concatenated without separator. Returns empty
// strings/slice when parsing fails or no section is present — callers should
// treat that as "drop message".
//
// Pure function (no I/O) so we can unit-test parse behavior without mocking
// the Lark image-download client.
func parsePostContent(contentJSON string) (text string, imageKeys []string) {
	type postSection struct {
		Title   string             `json:"title"`
		Content [][]map[string]any `json:"content"`
	}
	// Shape 2: try the unwrapped form first — that's what the Lark SDK
	// hands us in production. Shape 1 is the legacy/documented form.
	var flat postSection
	section := &flat
	if err := json.Unmarshal([]byte(contentJSON), &flat); err != nil || len(flat.Content) == 0 {
		var wrapped struct {
			ZHCN *postSection `json:"zh_cn"`
			EN   *postSection `json:"en_us"`
		}
		if err := json.Unmarshal([]byte(contentJSON), &wrapped); err != nil {
			return "", nil
		}
		section = wrapped.ZHCN
		if section == nil {
			section = wrapped.EN
		}
		if section == nil {
			return "", nil
		}
	}
	var textParts []string
	if section.Title != "" {
		textParts = append(textParts, section.Title)
	}
	for _, row := range section.Content {
		var rowParts []string
		for _, tag := range row {
			switch tag["tag"] {
			case "text", "a":
				if t, _ := tag["text"].(string); t != "" {
					rowParts = append(rowParts, t)
				}
			case "at":
				if t, _ := tag["user_name"].(string); t != "" {
					rowParts = append(rowParts, "@"+t)
				}
			case "img":
				if key, _ := tag["image_key"].(string); key != "" {
					imageKeys = append(imageKeys, key)
				}
			}
		}
		if joined := strings.TrimSpace(strings.Join(rowParts, "")); joined != "" {
			textParts = append(textParts, joined)
		}
	}
	return strings.TrimSpace(strings.Join(textParts, "\n")), imageKeys
}

// isMentionedInGroup decides whether to act on a group-chat message based
// on its mentions. When we know our own bot open_id, only act when at
// least one mention targets us — this prevents responding to messages
// directed at other bots that happen to share the group. When the open
// id is unknown (Start-time fetch failed), fall back to the legacy
// "any mention" behavior so the bot stays usable.
func (c *Channel) isMentionedInGroup(mentions []*larkim.MentionEvent) bool {
	if len(mentions) == 0 {
		return false
	}
	if c.botOpenID == "" {
		return true // degraded fallback — see Start() warning log
	}
	for _, m := range mentions {
		if m == nil || m.Id == nil || m.Id.OpenId == nil {
			continue
		}
		if *m.Id.OpenId == c.botOpenID {
			return true
		}
	}
	return false
}

func stripMentions(text string) string {
	for {
		start := strings.Index(text, "@_user_")
		if start == -1 {
			break
		}
		end := start + len("@_user_")
		for end < len(text) && text[end] != ' ' && text[end] != '\n' {
			end++
		}
		text = text[:start] + text[end:]
	}
	return text
}

func shortID(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
