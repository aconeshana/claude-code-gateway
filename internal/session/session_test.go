package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session/sessiontest"
)

// newTestSession builds a Session driven by the fake CLI with the given env.
func newTestSession(t *testing.T, env []string) *Session {
	t.Helper()
	return newTestSessionWithPerm(t, env, "default")
}

func newTestSessionWithPerm(t *testing.T, env []string, permMode string) *Session {
	t.Helper()
	cli := sessiontest.FakeCLIPath(t)
	rt := claude.NewRuntime(cli)
	sess, err := NewSession(rt, runtime.SpawnRequest{
		WorkingDir: t.TempDir(),
		Env:        env,
		Config:     claude.Config{PermissionMode: permMode},
	}, permMode, 0)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		sess.ForceClose()
	})
	return sess
}

// waitState polls CurrentState until it equals target or timeout elapses.
func waitState(t *testing.T, sess *Session, target State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess.CurrentState() == target {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session state = %s, want %s (timeout)", sess.CurrentState(), target)
}

func TestSession_InitTransitionsToReady(t *testing.T) {
	sess := newTestSession(t, []string{"FAKE_CLI_SESSION_ID=sess-ready-1"})

	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	waitState(t, sess, StateReady, 1*time.Second)

	info := sess.Info()
	if info.CLISessionID != "sess-ready-1" {
		t.Errorf("CLISessionID = %q, want sess-ready-1", info.CLISessionID)
	}
	if info.State != "ready" {
		t.Errorf("Info.State = %q, want ready", info.State)
	}
}

func TestSession_SendMessageTransitionsState(t *testing.T) {
	sess := newTestSession(t, nil)
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	waitState(t, sess, StateReady, 1*time.Second)

	if err := sess.SendMessage("hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// After sending, immediately should be processing
	if got := sess.CurrentState(); got != StateProcessing {
		t.Errorf("state after SendMessage = %s, want processing", got)
	}

	if _, ok := collector.WaitForType(protocol.MsgTypeResult, 5*time.Second); !ok {
		t.Fatal("result not received")
	}
	waitState(t, sess, StateIdle, 1*time.Second)

	if got := sess.Info().PendingTurns; got != 0 {
		t.Errorf("pendingTurns after result = %d, want 0", got)
	}
}

func TestSession_BroadcastsToMultipleSubscribers(t *testing.T) {
	sess := newTestSession(t, nil)
	c1 := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	c2 := sessiontest.NewEventCollector(sess.Subscribe("c2"))
	defer sess.Unsubscribe("c1")
	defer sess.Unsubscribe("c2")

	if _, ok := c1.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("c1 did not receive init")
	}
	if _, ok := c2.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("c2 did not receive init")
	}
}

func TestSession_UnsubscribeClosesChannel(t *testing.T) {
	sess := newTestSession(t, nil)
	ch := sess.Subscribe("c1")
	collector := sessiontest.NewEventCollector(ch)

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	sess.Unsubscribe("c1")
	if !collector.WaitDone(1 * time.Second) {
		t.Fatal("channel did not close after Unsubscribe")
	}
}

func TestSession_TurnStatusEvents(t *testing.T) {
	sess := newTestSession(t, nil)
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if err := sess.SendMessage("hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, ok := collector.WaitForType(protocol.MsgTypeResult, 5*time.Second); !ok {
		t.Fatal("result not received")
	}

	// give the post-result broadcast a moment to flush
	time.Sleep(50 * time.Millisecond)

	raw, ok := collector.FindGatewayEvent("turn_status")
	if !ok {
		t.Fatal("turn_status event not broadcast")
	}
	var ev map[string]interface{}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("unmarshal turn_status: %v", err)
	}
	if ev["status"] == "" {
		t.Error("turn_status event missing status field")
	}
}

func TestSession_CLIExitBroadcastsSessionExit(t *testing.T) {
	sess := newTestSession(t, []string{"FAKE_CLI_EXIT_AFTER_USER=1"})
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if err := sess.SendMessage("trigger"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if !collector.WaitDone(3 * time.Second) {
		t.Fatal("subscriber channel did not close on CLI exit")
	}
	if _, ok := collector.FindGatewayEvent("session_exit"); !ok {
		t.Error("session_exit gateway event not broadcast")
	}
	waitState(t, sess, StateStopped, 1*time.Second)
}

func TestSession_CloseStopsProcess(t *testing.T) {
	sess := newTestSession(t, nil)
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	_ = sess.Close()
	if !collector.WaitDone(3 * time.Second) {
		t.Fatal("subscribers not closed after Close")
	}
}

func TestSession_SendMessageAfterStopped(t *testing.T) {
	sess := newTestSession(t, nil)
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	sess.ForceClose()
	if !collector.WaitDone(2 * time.Second) {
		t.Fatal("subscriber not closed")
	}

	err := sess.SendMessage("after-stop")
	if err == nil {
		t.Error("SendMessage after stopped returned nil, want error")
	}
}

func TestSession_PermissionForwardingForwardsControlRequest(t *testing.T) {
	sess := newTestSessionWithPerm(t, []string{"FAKE_CLI_PERMISSION_TOOL=Bash"}, "forward")
	t.Cleanup(func() { sess.ForceClose() })

	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")

	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if err := sess.SendMessage("run something"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	raw, ok := collector.WaitForType(protocol.MsgTypeControlRequest, 5*time.Second)
	if !ok {
		t.Fatal("control_request not forwarded")
	}
	inner, err := protocol.ParseControlRequestInner(raw)
	if err != nil {
		t.Fatalf("parse inner: %v", err)
	}
	if inner.ToolName != "Bash" {
		t.Errorf("inner.ToolName = %q, want Bash", inner.ToolName)
	}
	// session should be in waiting_permission state
	waitState(t, sess, StateWaitingPermission, 1*time.Second)
}

func TestSession_RespondPermissionAllow(t *testing.T) {
	sess := newTestSessionWithPerm(t, []string{"FAKE_CLI_PERMISSION_TOOL=Bash"}, "forward")
	t.Cleanup(func() { sess.ForceClose() })

	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if err := sess.SendMessage("go"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	raw, ok := collector.WaitForType(protocol.MsgTypeControlRequest, 5*time.Second)
	if !ok {
		t.Fatal("control_request not received")
	}
	var stdoutReq protocol.StdoutControlRequest
	if err := json.Unmarshal(raw, &stdoutReq); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	inner, _ := protocol.ParseControlRequestInner(raw)

	if err := sess.RespondPermission(stdoutReq.RequestID, inner.ToolUseID, "allow", "", nil); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	if _, ok := collector.WaitForType(protocol.MsgTypeResult, 5*time.Second); !ok {
		t.Fatal("result not received after permission allow")
	}
}

func TestSession_AutoAllowDoesNotEnterWaiting(t *testing.T) {
	sess := newTestSessionWithPerm(t, []string{"FAKE_CLI_PERMISSION_TOOL=Bash"}, "auto")
	t.Cleanup(func() { sess.ForceClose() })

	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}
	if err := sess.SendMessage("go"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, ok := collector.WaitForType(protocol.MsgTypeResult, 5*time.Second); !ok {
		t.Fatal("result not received under auto")
	}
}

func TestSession_SubscribeAfterMessages(t *testing.T) {
	sess := newTestSession(t, nil)

	// Send first message before subscribing
	c1 := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	if _, ok := c1.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("c1 did not get init")
	}

	// Late subscriber should not get past init, but should get future messages
	c2 := sessiontest.NewEventCollector(sess.Subscribe("c2"))
	defer sess.Unsubscribe("c2")

	if err := sess.SendMessage("hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, ok := c2.WaitForType(protocol.MsgTypeResult, 5*time.Second); !ok {
		t.Fatal("late subscriber did not get result")
	}
	sess.Unsubscribe("c1")
}

func TestSession_BroadcastEvent(t *testing.T) {
	sess := newTestSession(t, nil)
	collector := sessiontest.NewEventCollector(sess.Subscribe("c1"))
	defer sess.Unsubscribe("c1")
	if _, ok := collector.WaitForType(protocol.MsgTypeSystem, 5*time.Second); !ok {
		t.Fatal("init not received")
	}

	sess.BroadcastEvent(map[string]interface{}{
		"_gateway_event": "custom_ev",
		"hello":          "world",
	})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := collector.FindGatewayEvent("custom_ev"); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("custom gateway event not received")
}
