package bridge

import (
	"context"
	"testing"
)

// TestHeartbeat_FlushStopsTicker is the regression test for the
// goroutine-leak bug fixed at the same time the heartbeat shipped:
// flush() — the catch-all shutdown path (ctx cancellation, subscriber
// channel close, session reactivate) — must call stopHeartbeat, not just
// finalize. Otherwise the goroutine + Ticker leak per turn and accumulate
// on long-running gateway processes.
//
// The test starts a heartbeat, calls flush, then checks that
// heartbeatStop has been cleared — the same state finalize would leave
// it in. We can't easily prove the goroutine actually exited without
// adding wait-on-done plumbing, but the heartbeatStop=nil postcondition
// is a strong proxy and matches the contract documented on stopHeartbeat.
func TestHeartbeat_FlushStopsTicker(t *testing.T) {
	b, _, _ := newTestBridge(t)
	s := &streamState{}

	// Need messageID set for startHeartbeat to be meaningful (it's a
	// no-op precondition is that the card exists). We don't actually
	// send a card; just simulate the post-appendText state.
	s.mu.Lock()
	s.messageID = "om_test"
	s.startHeartbeat(context.Background(), b, "c1")
	s.mu.Unlock()

	if s.heartbeatStop == nil {
		t.Fatal("startHeartbeat should have initialized heartbeatStop")
	}

	s.flush()

	if s.heartbeatStop != nil {
		t.Errorf("flush should have cleared heartbeatStop; goroutine + ticker would leak")
	}
	if !s.finalized {
		t.Errorf("flush should set finalized=true")
	}
}

// TestHeartbeat_StartIsIdempotent guards the comment claim ("safe to
// call repeatedly — subsequent calls are no-ops while one is already
// running"). A buggy implementation that started a second goroutine
// would leak the first one forever (no one ever sends to the orphaned
// stop channel).
func TestHeartbeat_StartIsIdempotent(t *testing.T) {
	b, _, _ := newTestBridge(t)
	s := &streamState{}

	s.mu.Lock()
	s.messageID = "om_test"
	s.startHeartbeat(context.Background(), b, "c1")
	first := s.heartbeatStop
	s.startHeartbeat(context.Background(), b, "c1")
	second := s.heartbeatStop
	s.mu.Unlock()

	if first != second {
		t.Errorf("second startHeartbeat should be a no-op (same channel pointer), got new")
	}

	s.flush()
}

// TestHeartbeat_StopWithoutStartIsSafe — stopHeartbeat must be no-op
// when no heartbeat ever started. finalize calls stopHeartbeat
// unconditionally on every turn end, including turns that never
// produced enough text to warrant a card.
func TestHeartbeat_StopWithoutStartIsSafe(t *testing.T) {
	s := &streamState{}
	s.stopHeartbeat() // must not panic
	if s.heartbeatStop != nil {
		t.Errorf("stop on never-started heartbeat should leave field nil")
	}
}

// TestHeartbeat_RestartAfterFlush — after flush clears the heartbeat,
// the next turn should be able to start a fresh one. Otherwise a single
// stop would permanently disable heartbeats for the session.
func TestHeartbeat_RestartAfterFlush(t *testing.T) {
	b, _, _ := newTestBridge(t)
	s := &streamState{}

	s.mu.Lock()
	s.messageID = "om_test"
	s.startHeartbeat(context.Background(), b, "c1")
	s.mu.Unlock()

	s.flush()

	// Simulate next turn: clear finalized so startHeartbeat's path is
	// consistent with appendText (which only runs when !finalized).
	s.mu.Lock()
	s.finalized = false
	s.messageID = "om_test_2"
	s.startHeartbeat(context.Background(), b, "c1")
	restarted := s.heartbeatStop
	s.mu.Unlock()

	if restarted == nil {
		t.Errorf("after flush, next startHeartbeat should re-arm heartbeatStop")
	}

	s.flush()
}