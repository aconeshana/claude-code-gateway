package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/gorilla/websocket"
)

func dialWS(t *testing.T, ts *httptestServer, authToken string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	if authToken != "" {
		header.Set("Authorization", "Bearer "+authToken)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL), header)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// thin alias so we don't import httptest in every helper
type httptestServer = struct {
	URL string
}

func sendAction(t *testing.T, conn *websocket.Conn, action, requestID string, payload interface{}) {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		raw = data
	}
	msg := ClientMessage{Action: action, RequestID: requestID, Payload: raw}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write %s: %v", action, err)
	}
}

func waitFor(t *testing.T, conn *websocket.Conn, predicate func(*ServerMessage) bool, timeout time.Duration) *ServerMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(deadline.Sub(time.Now())))
		var ev ServerMessage
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("read event: %v", err)
		}
		if predicate(&ev) {
			return &ev
		}
	}
	t.Fatalf("no matching event in %v", timeout)
	return nil
}

func waitEvent(t *testing.T, conn *websocket.Conn, name string, timeout time.Duration) *ServerMessage {
	return waitFor(t, conn, func(e *ServerMessage) bool { return e.Event == name }, timeout)
}

func waitReply(t *testing.T, conn *websocket.Conn, requestID string, timeout time.Duration) *ServerMessage {
	return waitFor(t, conn, func(e *ServerMessage) bool { return e.RequestID == requestID }, timeout)
}

func TestHandler_Ping(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "ping", "req-1", nil)
	ev := waitReply(t, conn, "req-1", 5*time.Second)
	if ev.Event != "pong" {
		t.Errorf("event = %q, want pong", ev.Event)
	}
}

func TestHandler_UnknownAction(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "make_breakfast", "req-x", nil)
	ev := waitReply(t, conn, "req-x", 5*time.Second)
	if ev.Event != "error" {
		t.Errorf("event = %q, want error", ev.Event)
	}
	if ev.Error == "" {
		t.Error("error message is empty")
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	ev := waitEvent(t, conn, "error", 5*time.Second)
	if ev.Error == "" {
		t.Error("error message is empty")
	}
}

func TestHandler_CreateAndListSession(t *testing.T) {
	ts, mgr := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "create_session", "req-c1", CreateSessionPayload{
		WorkingDir: t.TempDir(),
	})
	ev := waitReply(t, conn, "req-c1", 5*time.Second)
	if ev.Event != "session_created" {
		t.Fatalf("event = %q, want session_created (err=%q)", ev.Event, ev.Error)
	}
	if ev.SessionID == "" {
		t.Fatal("session_id empty in session_created")
	}

	if got := mgr.List(); len(got) != 1 {
		t.Errorf("manager.List = %d, want 1", len(got))
	}

	sendAction(t, conn, "list_sessions", "req-l", nil)
	listEv := waitReply(t, conn, "req-l", 5*time.Second)
	if listEv.Event != "session_list" {
		t.Fatalf("event = %q, want session_list", listEv.Event)
	}
}

func TestHandler_CreateSessionInvalidPayload(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	// inject raw invalid payload
	msg := ClientMessage{
		Action:    "create_session",
		RequestID: "req-bad",
		Payload:   json.RawMessage(`"not-an-object"`),
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	ev := waitReply(t, conn, "req-bad", 5*time.Second)
	if ev.Event != "error" {
		t.Errorf("event = %q, want error", ev.Event)
	}
}

func TestHandler_SendMessageDeliveryAndStream(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	// Create session
	sendAction(t, conn, "create_session", "c", CreateSessionPayload{WorkingDir: t.TempDir()})
	created := waitReply(t, conn, "c", 5*time.Second)
	if created.Event != "session_created" {
		t.Fatalf("create failed: %s / %s", created.Event, created.Error)
	}
	sid := created.SessionID

	// First inbound event should be the init system message (forwarded as "message")
	waitFor(t, conn, func(e *ServerMessage) bool {
		return e.Event == "message" && e.SessionID == sid
	}, 5*time.Second)

	// Send message
	sendAction(t, conn, "send_message", "s", SendMessagePayload{
		SessionID: sid,
		Content:   "hi",
	})

	// Expect a result message to come back as event=message with type=result
	ev := waitFor(t, conn, func(e *ServerMessage) bool {
		if e.Event != "message" || e.SessionID != sid {
			return false
		}
		raw, _ := json.Marshal(e.Payload)
		t, _, _ := protocol.ParseType(raw)
		return t == protocol.MsgTypeResult
	}, 5*time.Second)
	if ev == nil {
		t.Fatal("no result message")
	}
}

func TestHandler_DestroySession(t *testing.T) {
	ts, mgr := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "create_session", "c", CreateSessionPayload{WorkingDir: t.TempDir()})
	created := waitReply(t, conn, "c", 5*time.Second)
	sid := created.SessionID

	sendAction(t, conn, "destroy_session", "d", DestroySessionPayload{SessionID: sid})
	ev := waitReply(t, conn, "d", 5*time.Second)
	if ev.Event != "session_destroyed" {
		t.Fatalf("event = %q, want session_destroyed", ev.Event)
	}
	// Manager should have removed it
	if got := mgr.List(); len(got) != 0 {
		t.Errorf("List after destroy = %d, want 0", len(got))
	}
}

func TestHandler_ResumeSession(t *testing.T) {
	ts, _ := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "resume_session", "r", ResumeSessionPayload{
		SessionID:  "prior-cli-session",
		WorkingDir: t.TempDir(),
	})
	ev := waitReply(t, conn, "r", 5*time.Second)
	if ev.Event != "session_created" {
		t.Fatalf("event = %q, want session_created (err=%q)", ev.Event, ev.Error)
	}
}

func TestHandler_CleanupOnDisconnect(t *testing.T) {
	ts, mgr := newTestServer(t, "")
	conn := dialWS(t, &httptestServer{URL: ts.URL}, "")

	sendAction(t, conn, "create_session", "c", CreateSessionPayload{WorkingDir: t.TempDir()})
	created := waitReply(t, conn, "c", 5*time.Second)
	sid := created.SessionID

	// drain init
	waitFor(t, conn, func(e *ServerMessage) bool {
		return e.Event == "message" && e.SessionID == sid
	}, 5*time.Second)

	// Close client side; server should cleanup subscription but session itself
	// stays alive (only destroy_session removes it).
	_ = conn.Close()

	// Session manager should still have the session after disconnect
	time.Sleep(100 * time.Millisecond)
	if _, ok := mgr.Get(sid); !ok {
		t.Error("session should remain after client disconnect")
	}
}
