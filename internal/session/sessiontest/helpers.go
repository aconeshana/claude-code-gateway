// Package sessiontest provides shared helpers for session-related tests:
// builds a fake claude-code CLI binary (once per test run) and exposes
// utilities for asserting on subscriber events.
package sessiontest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

// FakeCLIPath returns the absolute path to a freshly built fake CLI binary.
// The binary is compiled once per test process and reused across tests.
//
// The fake CLI's behavior is controlled by environment variables — see
// internal/session/sessiontest/fakecli/main.go for the list.
func FakeCLIPath(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakePath, fakeErr = buildFakeCLI()
	})
	if fakeErr != nil {
		t.Fatalf("build fake CLI: %v", fakeErr)
	}
	return fakePath
}

func buildFakeCLI() (string, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	srcDir := filepath.Join(filepath.Dir(thisFile), "fakecli")

	tmp, err := os.MkdirTemp("", "fakecli-build-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(tmp, "fakecli")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &buildErr{output: string(out), err: err}
	}
	return bin, nil
}

type buildErr struct {
	output string
	err    error
}

func (e *buildErr) Error() string {
	return e.err.Error() + ": " + e.output
}

// EventCollector consumes a subscriber channel and exposes a thread-safe view
// of received events for assertions.
type EventCollector struct {
	mu     sync.Mutex
	events []json.RawMessage
	done   chan struct{}
}

// NewEventCollector starts a goroutine that drains ch into a buffer.
// Closes its done channel when ch is closed.
func NewEventCollector(ch <-chan json.RawMessage) *EventCollector {
	c := &EventCollector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for raw := range ch {
			c.mu.Lock()
			c.events = append(c.events, raw)
			c.mu.Unlock()
		}
	}()
	return c
}

// Events returns a copy of all events received so far.
func (c *EventCollector) Events() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]json.RawMessage, len(c.events))
	copy(out, c.events)
	return out
}

// Count returns the number of events received so far.
func (c *EventCollector) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// WaitForCount blocks until at least n events have been received or timeout
// elapses. Returns true if the count was reached.
func (c *EventCollector) WaitForCount(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Count() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.Count() >= n
}

// WaitForType blocks until at least one event with the given top-level "type"
// has been received or timeout elapses. Returns the raw event and true on success.
func (c *EventCollector) WaitForType(msgType string, timeout time.Duration) (json.RawMessage, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, raw := range c.Events() {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &header); err == nil && header.Type == msgType {
				return raw, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, false
}

// WaitDone blocks until the subscriber channel was closed or timeout elapses.
// Returns true if the channel was closed.
func (c *EventCollector) WaitDone(timeout time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// FindGatewayEvent scans collected events for one with the given _gateway_event
// value. Returns the raw event and true if found.
func (c *EventCollector) FindGatewayEvent(name string) (json.RawMessage, bool) {
	for _, raw := range c.Events() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		ev, ok := m["_gateway_event"]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(ev, &s); err != nil {
			continue
		}
		if s == name {
			return raw, true
		}
	}
	return nil, false
}
