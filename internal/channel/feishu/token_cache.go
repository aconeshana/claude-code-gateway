// Package feishu — token_cache.go provides a token cache wrapper around the
// Lark SDK's built-in localCache, adding an Invalidate() entry point so we
// can react to error 99991663 (Invalid access token).
//
// Why this exists:
//   - Lark SDK caches tenant_access_token per its `expires_in`. On macOS
//     laptop sleep, the system clock jumps forward but the cached entry's
//     TTL was computed from before sleep — the cache still thinks the
//     token is valid while the server has already invalidated it.
//   - All Lark message APIs then return HTTP 200 + body code=99991663
//     instead of HTTP 401, so the SDK's HTTP-level retry doesn't trigger.
//   - Without invalidation, the channel stays broken until the gateway
//     restarts. We invalidate proactively in `Channel.send*` retry loops.
package feishu

import (
	"context"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// tokenCache implements larkcore.Cache with an explicit Invalidate hook.
// Storage is in-memory only (matches SDK's default localCache).
type tokenCache struct {
	mu sync.RWMutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	value     string
	expiresAt time.Time // zero = never expires
}

func newTokenCache() *tokenCache {
	return &tokenCache{m: make(map[string]cacheEntry)}
}

// Set / Get satisfy larkcore.Cache.
func (c *tokenCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := cacheEntry{value: value}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl)
	}
	c.m[key] = e
	return nil
}

func (c *tokenCache) Get(_ context.Context, key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		return "", nil
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return "", nil
	}
	return e.value, nil
}

// InvalidateAll clears every cached entry. The SDK will re-fetch
// tenant_access_token / app_access_token on the next API call.
func (c *tokenCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]cacheEntry)
}

// Compile-time check.
var _ larkcore.Cache = (*tokenCache)(nil)
