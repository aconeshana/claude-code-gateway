// Package dingtalk implements channel.Channel for DingTalk.
package dingtalk

import (
	"bytes"
	"context"
	"encoding/base64"
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

// AddReaction is a no-op for DingTalk — see Reaction.
func (c *Channel) AddReaction(messageID, emoji string) (string, error) {
	return "", nil
}

// RemoveReaction is a no-op for DingTalk — see Reaction.
func (c *Channel) RemoveReaction(messageID, reactionID string) error {
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

	isGroup := data.ConversationType == "2"

	// Handle picture messages: download image and forward as InputBlocks.
	if data.Msgtype == "picture" {
		c.handleImageMessage(ctx, data, userID, chatID, msgID, text, isGroup)
		return nil, nil
	}

	// Handle richText messages (image + text combo).
	if data.Msgtype == "richText" {
		c.handleRichTextMessage(ctx, data, userID, chatID, msgID, text, isGroup)
		return nil, nil
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
		IsGroup:     data.ConversationType == "2",
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

// --- Image handling ---

// handleImageMessage downloads the image via OpenAPI and forwards it as
// InputBlocks (same format as Feishu: base64-encoded image block).
func (c *Channel) handleImageMessage(ctx context.Context, data *chatbot.BotCallbackDataModel, userID, chatID, msgID, caption string, isGroup bool) {
	downloadCode := extractDownloadCode(data.Content)
	if downloadCode == "" {
		log.Printf("[channel/dingtalk] picture message without downloadCode, msgID=%s", shortID(msgID))
		if caption != "" {
			c.dispatch(ctx, channel.InboundMessage{
				ChannelKind: "dingtalk", UserID: userID, ChatID: chatID,
				MessageID: msgID, IsGroup: isGroup, Kind: channel.InputText, Text: caption,
			})
		}
		return
	}

	imgData, err := c.downloadRobotFile(downloadCode)
	if err != nil {
		log.Printf("[channel/dingtalk] image download failed: %v", err)
		c.dispatch(ctx, channel.InboundMessage{
			ChannelKind: "dingtalk", UserID: userID, ChatID: chatID,
			MessageID: msgID, IsGroup: isGroup, Kind: channel.InputText, Text: "[图片下载失败] " + caption,
		})
		return
	}

	mediaType := detectMediaType(imgData)
	b64 := base64.StdEncoding.EncodeToString(imgData)
	block := map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       b64,
		},
	}

	var blocks []interface{}
	blocks = append(blocks, block)
	if caption != "" {
		blocks = append([]interface{}{map[string]interface{}{"type": "text", "text": caption}}, blocks...)
	}

	log.Printf("[channel/dingtalk] image msg user=%s size=%d mediaType=%s", shortID(userID), len(imgData), mediaType)

	c.dispatch(ctx, channel.InboundMessage{
		ChannelKind: "dingtalk",
		UserID:      userID,
		ChatID:      chatID,
		MessageID:   msgID,
		IsGroup:     isGroup,
		Kind:        channel.InputBlocks,
		Blocks:      blocks,
	})
}

// handleRichTextMessage handles richText messages which may contain both
// text and images. DingTalk richText Content is typically:
//
//	{"richText":[{"text":"..."},{"downloadCode":"xxx","type":"picture"}]}
func (c *Channel) handleRichTextMessage(ctx context.Context, data *chatbot.BotCallbackDataModel, userID, chatID, msgID, caption string, isGroup bool) {
	texts, downloadCodes := parseRichTextContent(data.Content)

	// Combine any inline text with the caption from data.Text.Content
	allText := caption
	if len(texts) > 0 {
		joined := strings.Join(texts, "\n")
		if allText == "" {
			allText = joined
		} else if joined != allText {
			allText = joined
		}
	}

	// No images found — send as plain text.
	if len(downloadCodes) == 0 {
		if allText == "" {
			allText = "[富文本消息]"
		}
		c.dispatch(ctx, channel.InboundMessage{
			ChannelKind: "dingtalk", UserID: userID, ChatID: chatID,
			MessageID: msgID, IsGroup: isGroup, Kind: channel.InputText, Text: allText,
		})
		return
	}

	// Download images and build blocks.
	var blocks []interface{}
	if allText != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": allText})
	}
	for _, code := range downloadCodes {
		imgData, err := c.downloadRobotFile(code)
		if err != nil {
			log.Printf("[channel/dingtalk] richText image download failed: %v", err)
			continue
		}
		mediaType := detectMediaType(imgData)
		b64 := base64.StdEncoding.EncodeToString(imgData)
		blocks = append(blocks, map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": mediaType,
				"data":       b64,
			},
		})
	}

	if len(blocks) == 0 {
		c.dispatch(ctx, channel.InboundMessage{
			ChannelKind: "dingtalk", UserID: userID, ChatID: chatID,
			MessageID: msgID, IsGroup: isGroup, Kind: channel.InputText, Text: "[图片下载失败]",
		})
		return
	}

	log.Printf("[channel/dingtalk] richText msg user=%s texts=%d images=%d", shortID(userID), len(texts), len(downloadCodes))
	c.dispatch(ctx, channel.InboundMessage{
		ChannelKind: "dingtalk", UserID: userID, ChatID: chatID,
		MessageID: msgID, IsGroup: isGroup, Kind: channel.InputBlocks, Blocks: blocks,
	})
}

// parseRichTextContent extracts text segments and image downloadCodes from
// DingTalk richText content. The content structure is:
//
//	{"richText":[{"text":"..."},{"downloadCode":"xxx","type":"picture"}]}
//
// or sometimes the Content is already the array directly.
func parseRichTextContent(content interface{}) (texts []string, downloadCodes []string) {
	if content == nil {
		return
	}

	var items []interface{}
	switch v := content.(type) {
	case map[string]interface{}:
		if rt, ok := v["richText"].([]interface{}); ok {
			items = rt
		}
	case []interface{}:
		items = v
	case string:
		var m map[string]interface{}
		if json.Unmarshal([]byte(v), &m) == nil {
			if rt, ok := m["richText"].([]interface{}); ok {
				items = rt
			}
		}
	}

	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if t, ok := m["text"].(string); ok && t != "" {
			texts = append(texts, t)
		}
		if code, ok := m["downloadCode"].(string); ok && code != "" {
			downloadCodes = append(downloadCodes, code)
		}
	}
	return
}

// POST https://api.dingtalk.com/v1.0/robot/messageFiles/download
// This API returns a JSON response with a downloadUrl field; we then GET that URL
// to retrieve the actual file bytes.
func (c *Channel) downloadRobotFile(downloadCode string) ([]byte, error) {
	token, err := c.tokenMgr.GetToken()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	payload := map[string]string{
		"downloadCode": downloadCode,
		"robotCode":    c.cfg.AppKey,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodPost,
		"https://api.dingtalk.com/v1.0/robot/messageFiles/download",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(respBody))
	}

	// If response Content-Type indicates image, return directly as bytes.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "image/") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}
		return data, nil
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// The API returns JSON like {"downloadUrl":"https://..."}.
	var dlResp struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := json.Unmarshal(respBody, &dlResp); err != nil || dlResp.DownloadURL == "" {
		// Fallback: if the response is not JSON, treat as raw file bytes.
		if len(respBody) > 0 && respBody[0] != '{' && respBody[0] != '[' {
			return respBody, nil
		}
		return nil, fmt.Errorf("no downloadUrl in response: %s", safeString(respBody, 200))
	}

	// Fetch the actual file from the download URL.
	fileResp, err := http.Get(dlResp.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("download from url: %w", err)
	}
	defer fileResp.Body.Close()

	if fileResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download url status=%d", fileResp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(fileResp.Body, 20*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

// extractDownloadCode parses the downloadCode from the message Content field.
// DingTalk picture messages have Content like: {"downloadCode":"xxx"} or a
// string containing the downloadCode.
func extractDownloadCode(content interface{}) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case map[string]interface{}:
		if code, ok := v["downloadCode"].(string); ok {
			return code
		}
	case string:
		var m map[string]interface{}
		if json.Unmarshal([]byte(v), &m) == nil {
			if code, ok := m["downloadCode"].(string); ok {
				return code
			}
		}
	}
	return ""
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

func safeString(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return string(data[:n]) + "..."
}
