package bridge

import (
	"context"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// TestUpdateFinalCardForSession_MentionsGroupAsker covers the Done-card-via-
// update path: when Lark patches an existing progress card in place (the
// common case after streaming sends ≥1 progress chunk), the update payload
// must still carry MentionUserID so the channel can inject the at-tag into
// the final card. Without this, group asks receive a quiet Done card even
// though the policy says Done in group → @ asker.
func TestUpdateFinalCardForSession_MentionsGroupAsker(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	sess.SetLastInbound(session.InboundLocation{
		ChatID:  "c-grp",
		MsgID:   "om_user_q",
		UserID:  "ou_alice",
		IsGroup: true,
	})

	card := channel.Card{Title: "Done", Sections: []channel.Section{{Markdown: "answer"}}}
	if err := b.updateFinalCardForSession(context.Background(), "om_progress", sess, card); err != nil {
		t.Fatalf("updateFinalCardForSession: %v", err)
	}

	ups := ch.Updates()
	if len(ups) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ups))
	}
	if got := ups[0].Message.MentionUserID; got != "ou_alice" {
		t.Errorf("group update: MentionUserID = %q, want %q", got, "ou_alice")
	}
}

// TestUpdateFinalCardForSession_P2PNoMention guards the opposite case:
// in P2P the @ would be redundant push-spam, so the update payload must
// not carry it.
func TestUpdateFinalCardForSession_P2PNoMention(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed", OwnerID: "alice",
		Origin: session.OriginFeishu, WorkingDir: "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	sess.SetLastInbound(session.InboundLocation{
		ChatID:  "c-p2p",
		MsgID:   "om_user_q",
		UserID:  "ou_alice",
		IsGroup: false,
	})

	card := channel.Card{Title: "Done", Sections: []channel.Section{{Markdown: "answer"}}}
	if err := b.updateFinalCardForSession(context.Background(), "om_progress", sess, card); err != nil {
		t.Fatalf("updateFinalCardForSession: %v", err)
	}
	ups := ch.Updates()
	if got := ups[0].Message.MentionUserID; got != "" {
		t.Errorf("P2P update: MentionUserID = %q, want \"\"", got)
	}
}

// TestReactionLifecycle_AddedOnInbound verifies that an inbound text
// message triggers AddReaction (the fake records it with a synthetic id)
// and that the session captures that id as a pending reaction. Fires in
// both P2P and group — OnIt is "I see you, working on it" feedback,
// orthogonal to the group-only quote/at policy.
func TestReactionLifecycle_AddedOnInbound(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed",
		OwnerID:      "alice",
		Origin:       session.OriginFeishu,
		WorkingDir:   "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	_ = mgr.SetFocus("alice", sess.ID)

	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "alice",
		ChatID:    "c-p2p",
		MessageID: "om_msg_1",
		// No IsGroup — defaults to P2P; reaction must fire regardless.
		Kind: channel.InputText,
		Text: "hello",
	})

	reacts := ch.Reactions()
	if len(reacts) != 1 {
		t.Fatalf("expected 1 reaction added (P2P should still get OnIt), got %d", len(reacts))
	}
	if reacts[0].MessageID != "om_msg_1" || reacts[0].Emoji != "OnIt" {
		t.Errorf("reaction mismatch: %+v", reacts[0])
	}

	pendingMsg, pendingID := sess.PendingReaction()
	if pendingMsg != "om_msg_1" || pendingID == "" {
		t.Errorf("expected pending reaction recorded on session, got msg=%q id=%q",
			pendingMsg, pendingID)
	}
}

// TestReactionLifecycle_NewInboundClearsPriorReaction covers the 1-slot
// LRU: when a second user message arrives before the previous turn
// finishes, the old reaction must be cleared before the new one lands.
func TestReactionLifecycle_NewInboundClearsPriorReaction(t *testing.T) {
	b, ch, mgr := newTestBridge(t)
	id, _ := mgr.ImportIdleSession(session.ImportOpts{
		CLISessionID: "cli-seed",
		OwnerID:      "alice",
		Origin:       session.OriginFeishu,
		WorkingDir:   "/tmp/proj",
	})
	sess, err := mgr.Reactivate(context.Background(), id)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	_ = mgr.SetFocus("alice", sess.ID)
	_ = sess

	for _, msgID := range []string{"om_msg_1", "om_msg_2"} {
		b.OnMessage(context.Background(), channel.InboundMessage{
			UserID: "alice", ChatID: "c1", MessageID: msgID,
			Kind: channel.InputText, Text: "hi",
		})
	}

	if got := len(ch.Reactions()); got != 2 {
		t.Errorf("expected 2 AddReaction calls, got %d", got)
	}
	removed := ch.RemovedReactions()
	if len(removed) != 1 {
		t.Fatalf("expected 1 RemoveReaction (prior cleared), got %d", len(removed))
	}
	if removed[0].MessageID != "om_msg_1" {
		t.Errorf("expected prior msg om_msg_1 to be cleared, got %q", removed[0].MessageID)
	}
}
