// Package dingtalk implements channel.Channel for DingTalk.
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/card"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// Config carries the DingTalk app credentials and access policy.
type Config struct {
	AppKey         string
	AppSecret      string
	AllowedUserIDs []string
}

// Channel implements channel.Channel using DingTalk Stream mode.
type Channel struct {
	cfg      Config
	cli      *client.StreamClient
	tokenMgr *tokenManager
	handler  channel.InboundHandler

	mu          sync.RWMutex
	allowedUIDs map[string]bool
	dedup       map[string]struct{}
	// webhooks stores the latest sessionWebhook per conversationId for proactive messaging.
	webhooks map[string]string
}

// New constructs a Channel. Call Start(ctx, handler) to begin processing.
func New(cfg Config) *Channel {
	allowed := make(map[string]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}
	return &Channel{
		cfg:         cfg,
		tokenMgr:    newTokenManager(cfg.AppKey, cfg.AppSecret),
		allowedUIDs: allowed,
		dedup:       make(map[string]struct{}),
		webhooks:    make(map[string]string),
	}
}

func (c *Channel) Kind() string { return "dingtalk" }

// SetAllowedUserIDs replaces the user allowlist atomically. Empty list means
// "allow everyone".
func (c *Channel) SetAllowedUserIDs(ids []string) {
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		next[id] = true
	}
	c.mu.Lock()
	c.allowedUIDs = next
	c.mu.Unlock()
}

// Start opens the DingTalk Stream connection and dispatches events to handler.
// Blocks until ctx is cancelled or the client errors.
func (c *Channel) Start(ctx context.Context, handler channel.InboundHandler) error {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()

	c.cli = client.NewStreamClient(
		client.WithAppCredential(client.NewAppCredentialConfig(c.cfg.AppKey, c.cfg.AppSecret)),
	)
	c.cli.RegisterChatBotCallbackRouter(c.onChatBotMessage)
	c.cli.RegisterCardCallbackRouter(c.onCardCallback)

	if len(c.allowedUIDs) > 0 {
		log.Printf("[channel/dingtalk] starting (app_key=%s, allowed=%d)", shortKey(c.cfg.AppKey), len(c.allowedUIDs))
	} else {
		log.Printf("[channel/dingtalk] starting (app_key=%s, WARNING: no user allowlist)", shortKey(c.cfg.AppKey))
	}
	if err := c.cli.Start(ctx); err != nil {
		return err
	}
	// Stream SDK's Start returns immediately after connecting; block until ctx done.
	<-ctx.Done()
	return ctx.Err()
}

func (c *Channel) Shutdown() {
	if c.cli != nil {
		c.cli.Close()
	}
}

// SendMessage delivers an outbound message. Card takes precedence over Text.
// Streaming "Processing" cards are suppressed — only the final result is sent.
func (c *Channel) SendMessage(ctx context.Context, msg channel.OutboundMessage) (string, error) {
	if msg.Card != nil {
		if strings.HasPrefix(msg.Card.Title, "Processing") {
			return "dingtalk_processing", nil
		}
		return c.sendCardMessage(ctx, msg.ChatID, *msg.Card)
	}
	if msg.Text != "" {
		return c.sendMarkdownMessage(ctx, msg.ChatID, msg.Text)
	}
	return "", fmt.Errorf("OutboundMessage has neither Card nor Text")
}

// UpdateMessage always returns an error because DingTalk webhook messages
// cannot be updated in place. The bridge's streaming renderer uses this:
//   - Timer intermediate updates: error is discarded (via _ =), no extra messages
//   - finalize: error triggers fallback → sends result as a new card
func (c *Channel) UpdateMessage(ctx context.Context, messageID string, msg channel.OutboundMessage) error {
	return fmt.Errorf("dingtalk: in-place card update not supported")
}

// Reaction attempts to add an emoji to a message. DingTalk does not expose
// a reaction API for bots, so this is a no-op.
func (c *Channel) Reaction(messageID, emoji string) error {
	return nil
}

// --- DingTalk Stream handlers ---

const cardActionPrefix = "__card_action__:"

func (c *Channel) onChatBotMessage(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	if data == nil {
		return nil, nil
	}

	userID := data.SenderStaffId
	if userID == "" {
		userID = data.SenderNick
	}
	if !c.isAllowed(userID) {
		log.Printf("[channel/dingtalk] rejected message from unauthorized user %s", shortID(userID))
		return nil, nil
	}

	msgID := data.MsgId
	if c.isDuplicate(msgID) {
		return nil, nil
	}

	conversationID := data.ConversationId
	chatID := conversationID

	// Store webhook for proactive messaging.
	if data.SessionWebhook != "" {
		c.mu.Lock()
		c.webhooks[conversationID] = data.SessionWebhook
		c.mu.Unlock()
	}

	text := data.Text.Content
	text = strings.TrimSpace(text)

	// In group chats, strip @bot mention.
	if data.ConversationType == "2" {
		text = stripAtMention(text)
	}

	if text == "" {
		return nil, nil
	}

	// Detect button-click messages: buttons use dtmd://dingtalkclient/sendMessage
	// which makes the client send the action JSON as a text message with our prefix.
	if strings.HasPrefix(text, cardActionPrefix) {
		c.handleButtonAction(ctx, userID, chatID, msgID, text)
		return nil, nil
	}

	log.Printf("[channel/dingtalk] inbound msg user=%s chat=%s id=%s len=%d",
		shortID(userID), shortID(chatID), shortID(msgID), len(text))

	inbound := channel.InboundMessage{
		ChannelKind: "dingtalk",
		UserID:      userID,
		ChatID:      chatID,
		MessageID:   msgID,
		Kind:        channel.InputText,
		Text:        text,
	}

	c.dispatch(ctx, inbound)
	return nil, nil
}

// handleButtonAction parses a button-click text and dispatches it as a card action.
func (c *Channel) handleButtonAction(ctx context.Context, userID, chatID, msgID, text string) {
	payload := strings.TrimPrefix(text, cardActionPrefix)
	var action map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &action); err != nil {
		log.Printf("[channel/dingtalk] invalid button action payload: %v", err)
		return
	}

	actionName, _ := action["action"].(string)
	values := make(map[string]interface{}, len(action))
	for k, v := range action {
		values[k] = v
	}

	log.Printf("[channel/dingtalk] button action: %s user=%s", actionName, shortID(userID))

	inbound := channel.InboundMessage{
		ChannelKind: "dingtalk",
		UserID:      userID,
		ChatID:      chatID,
		MessageID:   msgID,
		Kind:        channel.InputCardAction,
		Action: &channel.CardAction{
			Name:   actionName,
			Values: values,
		},
	}
	c.dispatch(ctx, inbound)
}

func (c *Channel) onCardCallback(ctx context.Context, req *card.CardRequest) (*card.CardResponse, error) {
	if req == nil {
		return nil, nil
	}

	userID := req.UserId
	if !c.isAllowed(userID) {
		log.Printf("[channel/dingtalk] rejected card action from unauthorized user %s", shortID(userID))
		return nil, nil
	}

	params := req.CardActionData.CardPrivateData.Params
	actionName, _ := params["action"].(string)

	values := make(map[string]interface{}, len(params))
	for k, v := range params {
		values[k] = v
	}

	log.Printf("[channel/dingtalk] card action: %s user=%s outTrackId=%s", actionName, shortID(userID), shortID(req.OutTrackId))

	var (
		replyMu   sync.Mutex
		replyCard *channel.Card
	)

	inbound := channel.InboundMessage{
		ChannelKind: "dingtalk",
		UserID:      userID,
		ChatID:      req.OutTrackId,
		MessageID:   req.OutTrackId,
		Kind:        channel.InputCardAction,
		Action: &channel.CardAction{
			Name:   actionName,
			Values: values,
		},
		Reply: func(c channel.Card) {
			replyMu.Lock()
			replyCard = &c
			replyMu.Unlock()
		},
	}

	c.dispatch(ctx, inbound)

	replyMu.Lock()
	finalCard := replyCard
	replyMu.Unlock()
	if finalCard != nil {
		cardData := renderCallbackCard(*finalCard)
		paramMap := make(map[string]string)
		if body, err := json.Marshal(cardData); err == nil {
			paramMap["card"] = string(body)
		}
		return &card.CardResponse{
			CardData: &card.CardDataDto{
				CardParamMap: paramMap,
			},
		}, nil
	}
	return nil, nil
}

// --- Sending helpers ---

func (c *Channel) sendCardMessage(ctx context.Context, chatID string, crd channel.Card) (string, error) {
	outTrackID := uuid.New().String()

	// DingTalk actionCard buttons only support URL actions (no server callbacks).
	// We use dtmd://dingtalkclient/sendMessage to make button clicks send a
	// recognizable text message that onChatBotMessage intercepts and converts
	// into a card action. The card renders buttons inline next to their content.
	c.mu.RLock()
	webhook := c.webhooks[chatID]
	c.mu.RUnlock()

	cardJSON := renderCard(crd)
	if webhook != "" {
		var payload map[string]interface{}
		_ = json.Unmarshal([]byte(cardJSON), &payload)
		body, _ := json.Marshal(payload)
		resp, err := http.Post(webhook, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return outTrackID, nil
			}
		}
	}

	// Fallback: OpenAPI.
	return outTrackID, c.sendViaOpenAPI(ctx, chatID, cardJSON)
}

func cardHasButtons(c channel.Card) bool {
	for _, sec := range c.Sections {
		if len(sec.Buttons) > 0 {
			return true
		}
	}
	return false
}

func (c *Channel) sendMarkdownMessage(ctx context.Context, chatID string, text string) (string, error) {
	// Try session webhook first.
	c.mu.RLock()
	webhook := c.webhooks[chatID]
	c.mu.RUnlock()

	if webhook != "" {
		payload := map[string]interface{}{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": "message",
				"text":  convertToDingTalkMD(text),
			},
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(webhook, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return "", nil
			}
		}
	}

	// Fallback: send via OpenAPI.
	payload := map[string]interface{}{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": "message",
			"text":  convertToDingTalkMD(text),
		},
	}
	msgJSON, _ := json.Marshal(payload)
	return "", c.sendViaOpenAPI(ctx, chatID, string(msgJSON))
}

func (c *Channel) updateCardMessage(ctx context.Context, messageID string, crd channel.Card) error {
	return c.doUpdateCard(ctx, messageID, crd, true)
}

func (c *Channel) doUpdateCard(ctx context.Context, messageID string, crd channel.Card, canRetry bool) error {
	token, err := c.tokenMgr.GetToken()
	if err != nil {
		return fmt.Errorf("dingtalk update card: %w", err)
	}

	cardData := renderCallbackCard(crd)
	payload := map[string]interface{}{
		"outTrackId": messageID,
		"cardData":   cardData,
	}
	body, _ := json.Marshal(payload)

	url := "https://api.dingtalk.com/v1.0/im/interactiveCards"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk update card: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized && canRetry {
			c.tokenMgr.Invalidate()
			return c.doUpdateCard(ctx, messageID, crd, false)
		}
		return fmt.Errorf("dingtalk update card: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Channel) sendViaOpenAPI(ctx context.Context, chatID string, msgJSON string) error {
	return c.doSendViaOpenAPI(ctx, chatID, msgJSON, true)
}

func (c *Channel) doSendViaOpenAPI(ctx context.Context, chatID string, msgJSON string, canRetry bool) error {
	token, err := c.tokenMgr.GetToken()
	if err != nil {
		return fmt.Errorf("dingtalk send: %w", err)
	}

	url := "https://api.dingtalk.com/v1.0/robot/groupMessages/send"
	payload := map[string]interface{}{
		"msgParam":           msgJSON,
		"msgKey":             "sampleActionCard6",
		"openConversationId": chatID,
		"robotCode":          c.cfg.AppKey,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized && canRetry {
			c.tokenMgr.Invalidate()
			return c.doSendViaOpenAPI(ctx, chatID, msgJSON, false)
		}
		return fmt.Errorf("dingtalk send: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- Helpers ---

func (c *Channel) dispatch(ctx context.Context, m channel.InboundMessage) {
	c.mu.RLock()
	h := c.handler
	c.mu.RUnlock()
	if h == nil {
		return
	}
	h.OnMessage(ctx, m)
}

func (c *Channel) isAllowed(userID string) bool {
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
		c.dedup = make(map[string]struct{})
	}
	c.dedup[msgID] = struct{}{}
	return false
}

// stripAtMention removes @bot mentions from the message text.
func stripAtMention(text string) string {
	return strings.TrimSpace(text)
}

func shortID(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

func shortKey(s string) string {
	if len(s) < 6 {
		return s
	}
	return s[:6] + "..."
}

func mapEmoji(name string) string {
	switch name {
	case "OnIt":
		return "OK_HAND"
	case "DONE", "done":
		return "THUMBSUP"
	case "HEART", "heart":
		return "HEART"
	default:
		return "OK_HAND"
	}
}
