package claude_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session/sessiontest"
)

type runtimeFixture struct {
	t        *testing.T
	rt       *claude.Runtime
	proc     runtime.Process
	messages chan json.RawMessage
	exitErr  chan error
	mu       sync.Mutex
	closed   bool
}

func newRuntimeFixture(t *testing.T, env []string) *runtimeFixture {
	t.Helper()
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	f := &runtimeFixture{
		t:        t,
		rt:       rt,
		messages: make(chan json.RawMessage, 32),
		exitErr:  make(chan error, 1),
	}
	proc, err := rt.Spawn(context.Background(), runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Env:        env,
		Config:     claude.Config{},
	}, runtime.Callbacks{
		OnMessage: func(raw json.RawMessage) {
			f.mu.Lock()
			if f.closed {
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
			f.messages <- raw
		},
		OnExit: func(err error) {
			f.exitErr <- err
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.proc = proc
	t.Cleanup(func() {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		_ = proc.Kill()
		<-proc.Done()
	})
	return f
}

func (f *runtimeFixture) waitMessage(timeout time.Duration) (json.RawMessage, bool) {
	select {
	case raw := <-f.messages:
		return raw, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (f *runtimeFixture) waitMessageType(msgType string, timeout time.Duration) (json.RawMessage, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, ok := f.waitMessage(time.Until(deadline))
		if !ok {
			return nil, false
		}
		t, _, _ := protocol.ParseType(raw)
		if t == msgType {
			return raw, true
		}
	}
	return nil, false
}

func TestRuntime_Name(t *testing.T) {
	rt := claude.NewRuntime("/usr/bin/claude")
	if got := rt.Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want claude-code", got)
	}
}

func TestRuntime_SpawnEmitsInit(t *testing.T) {
	f := newRuntimeFixture(t, []string{"FAKE_CLI_SESSION_ID=rt-init-1"})

	raw, ok := f.waitMessageType(protocol.MsgTypeSystem, 5*time.Second)
	if !ok {
		t.Fatal("did not receive init message in time")
	}
	var init protocol.SystemInitMessage
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatalf("unmarshal init: %v", err)
	}
	if init.SessionID != "rt-init-1" {
		t.Errorf("session id = %q, want rt-init-1", init.SessionID)
	}
	if init.Subtype != protocol.SubtypeInit {
		t.Errorf("subtype = %q, want %q", init.Subtype, protocol.SubtypeInit)
	}

	// RuntimeID should be populated after init
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if f.proc.RuntimeID() == "rt-init-1" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.proc.RuntimeID(); got != "rt-init-1" {
		t.Errorf("RuntimeID = %q, want rt-init-1", got)
	}
}

func TestRuntime_UserRoundTrip(t *testing.T) {
	f := newRuntimeFixture(t, nil)

	if _, ok := f.waitMessageType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	user, _ := json.Marshal(protocol.NewUserMessage("hello", "u-1"))
	if err := f.proc.Write(user); err != nil {
		t.Fatalf("write user: %v", err)
	}

	raw, ok := f.waitMessageType(protocol.MsgTypeResult, 5*time.Second)
	if !ok {
		t.Fatal("result not received")
	}
	var result protocol.ResultMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.IsError {
		t.Errorf("result is_error = true, want false")
	}
	if result.NumTurns != 1 {
		t.Errorf("num_turns = %d, want 1", result.NumTurns)
	}
}

func TestRuntime_GracefulStop(t *testing.T) {
	f := newRuntimeFixture(t, nil)
	if _, ok := f.waitMessageType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	_ = f.proc.GracefulStop(2 * time.Second)

	select {
	case <-f.proc.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("process did not exit after GracefulStop")
	}

	select {
	case <-f.exitErr:
	case <-time.After(1 * time.Second):
		t.Fatal("onExit was not called")
	}
}

func TestRuntime_Kill(t *testing.T) {
	f := newRuntimeFixture(t, nil)
	if _, ok := f.waitMessageType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	if err := f.proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-f.proc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after Kill")
	}
}

func TestRuntime_WriteAfterDone(t *testing.T) {
	f := newRuntimeFixture(t, nil)
	if _, ok := f.waitMessageType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	_ = f.proc.Kill()
	<-f.proc.Done()

	user, _ := json.Marshal(protocol.NewUserMessage("after-done", "u-x"))
	err := f.proc.Write(user)
	if err == nil {
		t.Fatal("Write after Done returned nil error, want error")
	}
}

func TestRuntime_FailedStartCallsOnExit(t *testing.T) {
	f := newRuntimeFixture(t, []string{"FAKE_CLI_FAIL_START=1"})

	select {
	case err := <-f.exitErr:
		if err == nil {
			t.Error("onExit err = nil, want non-nil for failed start")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onExit was not called for failed start")
	}
}

func TestRuntime_RejectsWrongConfigType(t *testing.T) {
	rt := claude.NewRuntime("/usr/bin/claude")
	_, err := rt.Spawn(context.Background(), runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Config:     wrongCfg{},
	}, runtime.Callbacks{})
	if err == nil {
		t.Fatal("Spawn with wrong config type returned nil error")
	}
}

type wrongCfg struct{}

func (wrongCfg) RuntimeName() string { return "other" }

func TestRuntime_ArgsBuildsBetas(t *testing.T) {
	// Indirect coverage of args.go: spawning with a Betas slice and checking
	// the process actually starts (fake CLI ignores all args).
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	proc, err := rt.Spawn(context.Background(), runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Config: claude.Config{
			Model:           "test",
			MaxTurns:        5,
			IncludePartials: true,
			Effort:          "high",
			Thinking:        "deep",
			Betas:           []string{"beta1", "beta2"},
			AllowedTools:    []string{"Bash"},
			AddDirs:         []string{"/tmp"},
			MCPConfig:       "mcp.json",
			PluginDir:       "/plugins",
		},
	}, runtime.Callbacks{})
	if err != nil {
		t.Fatalf("Spawn with rich config: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill(); <-proc.Done() })
}

func TestRuntime_CodecRoundTrip(t *testing.T) {
	codec := claude.Codec{}
	raw, err := codec.EncodeUserText("hello", "u-1")
	if err != nil {
		t.Fatalf("encode user text: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("encoded bytes empty")
	}

	ka, err := codec.EncodeKeepAlive()
	if err != nil {
		t.Fatalf("encode keep alive: %v", err)
	}
	if len(ka) == 0 {
		t.Fatal("keep alive bytes empty")
	}

	resp, err := codec.EncodeControlResponse("req-1", "tu-1", "allow", "", nil)
	if err != nil {
		t.Fatalf("encode permission: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("permission bytes empty")
	}

	if _, err := codec.EncodeControlResponse("r", "t", "bogus", "", nil); err == nil {
		t.Error("EncodeControlResponse with bogus behavior should error")
	}
}

func TestRuntime_CodecParseInit(t *testing.T) {
	codec := claude.Codec{}
	initJSON := `{"type":"system","subtype":"init","session_id":"sid","model":"m","cwd":"/tmp","tools":[],"permissionMode":"default","uuid":"u"}`
	ev, err := codec.ParseEvent(json.RawMessage(initJSON))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Kind != runtime.KindInit {
		t.Errorf("Kind = %d, want KindInit", ev.Kind)
	}
	if ev.RuntimeID != "sid" {
		t.Errorf("RuntimeID = %q, want sid", ev.RuntimeID)
	}
}
