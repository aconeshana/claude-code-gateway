// Package dingtalk implements channel.Channel for DingTalk.
package dingtalk

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenManager handles DingTalk access token retrieval and caching.
// Tokens are cached in memory and automatically refreshed before expiry.
type tokenManager struct {
	appKey    string
	appSecret string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newTokenManager(appKey, appSecret string) *tokenManager {
	return &tokenManager{
		appKey:    appKey,
		appSecret: appSecret,
	}
}

// GetToken returns a valid access token, refreshing if needed.
func (t *tokenManager) GetToken() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Return cached token if still valid (with 5 min buffer).
	if t.token != "" && time.Now().Add(5*time.Minute).Before(t.expiresAt) {
		return t.token, nil
	}

	token, expiresIn, err := t.fetchToken()
	if err != nil {
		return "", err
	}
	t.token = token
	t.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return t.token, nil
}

// Invalidate clears the cached token, forcing a refresh on next GetToken call.
func (t *tokenManager) Invalidate() {
	t.mu.Lock()
	t.token = ""
	t.expiresAt = time.Time{}
	t.mu.Unlock()
}

func (t *tokenManager) fetchToken() (string, int, error) {
	url := fmt.Sprintf("https://oapi.dingtalk.com/gettoken?appkey=%s&appsecret=%s", t.appKey, t.appSecret)
	resp, err := http.Get(url)
	if err != nil {
		return "", 0, fmt.Errorf("dingtalk gettoken: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("dingtalk gettoken read body: %w", err)
	}

	var result struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", 0, fmt.Errorf("dingtalk gettoken parse: %w", err)
	}
	if result.ErrCode != 0 {
		return "", 0, fmt.Errorf("dingtalk gettoken: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}
	return result.AccessToken, result.ExpiresIn, nil
}
