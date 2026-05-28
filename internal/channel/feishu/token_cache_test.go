package feishu

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTokenCache_BasicSetGet(t *testing.T) {
	c := newTokenCache()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil || got != "v" {
		t.Errorf("Get = %q err=%v, want v", got, err)
	}
}

func TestTokenCache_TTLExpires(t *testing.T) {
	c := newTokenCache()
	ctx := context.Background()
	_ = c.Set(ctx, "k", "v", 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	got, _ := c.Get(ctx, "k")
	if got != "" {
		t.Errorf("expected empty after TTL, got %q", got)
	}
}

func TestTokenCache_InvalidateAll(t *testing.T) {
	c := newTokenCache()
	ctx := context.Background()
	_ = c.Set(ctx, "tenant_a", "tok1", time.Hour)
	_ = c.Set(ctx, "app_a", "tok2", time.Hour)
	c.InvalidateAll()
	if v, _ := c.Get(ctx, "tenant_a"); v != "" {
		t.Errorf("tenant_a should be cleared, got %q", v)
	}
	if v, _ := c.Get(ctx, "app_a"); v != "" {
		t.Errorf("app_a should be cleared, got %q", v)
	}
}

// TestWithTokenRetry_RetriesOn99991663 verifies the auto-recovery path:
// first call returns Lark's "invalid access token" code, the cache is
// invalidated, the second call sees a fresh token and succeeds.
func TestWithTokenRetry_RetriesOn99991663(t *testing.T) {
	c := &Channel{tokenCC: newTokenCache()}
	_ = c.tokenCC.Set(context.Background(), "tenant_xxx", "stale", time.Hour)

	calls := 0
	err := c.withTokenRetry("test", func() (int, string, error) {
		calls++
		if calls == 1 {
			return larkInvalidAccessToken, "Invalid access token", nil
		}
		return 0, "", nil
	})
	if err != nil {
		t.Errorf("expected success on retry, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (initial + retry), got %d", calls)
	}
	if v, _ := c.tokenCC.Get(context.Background(), "tenant_xxx"); v != "" {
		t.Errorf("cache should have been invalidated, got %q", v)
	}
}

// TestWithTokenRetry_NoRetryOnOtherErrors confirms we don't burn extra API
// calls when the failure is unrelated to token expiry (rate limit, network,
// permission denied, etc.).
func TestWithTokenRetry_NoRetryOnOtherErrors(t *testing.T) {
	c := &Channel{tokenCC: newTokenCache()}
	calls := 0
	err := c.withTokenRetry("test", func() (int, string, error) {
		calls++
		return 12345, "some other error", nil
	})
	if err == nil {
		t.Error("expected error to propagate")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on non-99991663), got %d", calls)
	}
}

// TestWithTokenRetry_TransportErrorNoRetry: actual HTTP/transport errors
// should also not retry — these are usually network blips and the SDK has
// its own HTTP retry layer.
func TestWithTokenRetry_TransportErrorNoRetry(t *testing.T) {
	c := &Channel{tokenCC: newTokenCache()}
	calls := 0
	sentinel := errors.New("network down")
	err := c.withTokenRetry("test", func() (int, string, error) {
		calls++
		return 0, "", sentinel
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

// TestWithTokenRetry_SecondAttemptStillFails: if the token is still invalid
// after a refresh (e.g. app credentials revoked), surface the error rather
// than retrying forever.
func TestWithTokenRetry_SecondAttemptStillFails(t *testing.T) {
	c := &Channel{tokenCC: newTokenCache()}
	calls := 0
	err := c.withTokenRetry("test", func() (int, string, error) {
		calls++
		return larkInvalidAccessToken, "still invalid", nil
	})
	if err == nil {
		t.Error("expected error after retry also fails")
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (initial + 1 retry, no infinite loop), got %d", calls)
	}
}
