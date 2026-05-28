package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/sessiontest"
)

func newTestRegistry() *runtime.Registry {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})
	return r
}

func newTestServer(t *testing.T, authToken string) (*httptest.Server, *session.Manager) {
	t.Helper()
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	mgr := session.NewManager(rt, t.TempDir(), "default", 8, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	srv := NewServer(mgr, newTestRegistry(), "claude", "", authToken, 5*time.Second)
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts, mgr
}

func TestServer_HealthOK(t *testing.T) {
	ts, _ := newTestServer(t, "")

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if body["ready"] != true {
		t.Errorf("ready = %v, want true", body["ready"])
	}
}

func TestServer_HealthWarmingUp(t *testing.T) {
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	mgr := session.NewManager(rt, t.TempDir(), "default", 4, 0, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	srv := NewServer(mgr, newTestRegistry(), "claude", "", "", 5*time.Second)
	srv.SetReadyCheck(func() bool { return false })

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "warming_up" {
		t.Errorf("status = %v, want warming_up", body["status"])
	}
	if body["ready"] != false {
		t.Errorf("ready = %v, want false", body["ready"])
	}
}

func TestServer_AuthRequired(t *testing.T) {
	ts, _ := newTestServer(t, "secret-token")

	req, _ := http.NewRequest("GET", ts.URL+"/ws", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without token = %d, want 401", resp.StatusCode)
	}
}

func TestServer_AuthRejectsBadToken(t *testing.T) {
	ts, _ := newTestServer(t, "secret-token")

	req, _ := http.NewRequest("GET", ts.URL+"/ws", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status with bad token = %d, want 401", resp.StatusCode)
	}
}

// wsURL converts an http test server URL to ws://...
func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
}
