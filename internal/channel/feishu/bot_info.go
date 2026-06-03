package feishu

// bot_info.go — fetch and cache this bot's own open_id.
//
// Why: in group chats Lark delivers im.message.receive_v1 to every bot
// subscribed to the event, regardless of which bot was @-mentioned. To
// avoid responding to messages directed at other bots in the same group,
// we filter mentions against our own bot's open_id. The id is fetched
// once on startup via /open-apis/bot/v3/info and cached on disk under
// the gateway state directory so subsequent starts skip the API call.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fetchBotOpenID retrieves the open_id of the bot identified by appID /
// appSecret via the public /open-apis/bot/v3/info endpoint. Returns the
// open_id (always prefixed with "ou_") on success.
//
// Implementation: two HTTP calls — first exchange app credentials for a
// tenant_access_token, then GET the bot info. No SDK client needed; the
// endpoints are simple and well-defined.
func fetchBotOpenID(ctx context.Context, appID, appSecret string) (string, error) {
	token, err := fetchTenantAccessToken(ctx, appID, appSecret)
	if err != nil {
		return "", fmt.Errorf("tenant_access_token: %w", err)
	}
	openID, err := fetchBotInfoOpenID(ctx, token)
	if err != nil {
		return "", fmt.Errorf("bot/v3/info: %w", err)
	}
	if !strings.HasPrefix(openID, "ou_") {
		return "", fmt.Errorf("unexpected open_id format: %q", openID)
	}
	return openID, nil
}

func fetchTenantAccessToken(ctx context.Context, appID, appSecret string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	return out.TenantAccessToken, nil
}

func fetchBotInfoOpenID(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://open.feishu.cn/open-apis/bot/v3/info", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if out.Code != 0 || out.Bot.OpenID == "" {
		return "", fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Bot.OpenID, nil
}

// loadBotOpenIDFromCache reads a cached open_id from path. Returns ""
// when the file is missing, empty, malformed, or doesn't have the
// expected ou_ prefix — caller falls back to fetching from the API.
// Validating the prefix keeps a corrupted cache file (manual edit,
// truncated write) from silently disabling the @-mention filter for
// the lifetime of the process.
func loadBotOpenIDFromCache(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !strings.HasPrefix(id, "ou_") {
		return ""
	}
	return id
}

// saveBotOpenIDToCache persists an open_id to path. Best-effort: cache
// write failures are logged by the caller but never block startup.
// MkdirAll ensures the gateway works on a fresh install where the
// state directory doesn't exist yet — without it the cache would
// silently fail to land and we'd hit the API on every restart.
func saveBotOpenIDToCache(path, openID string) error {
	if path == "" || openID == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(openID+"\n"), 0o600)
}

// httpClient is a process-wide HTTP client for one-shot calls (token
// exchange, bot info). Short timeout because both endpoints are fast and
// we don't want startup to hang on a flaky network.
var httpClient = &http.Client{Timeout: 10 * time.Second}
