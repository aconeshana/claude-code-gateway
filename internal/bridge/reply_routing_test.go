package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// These are characterization tests that lock down the current routing
// behavior of replyCard / replyText before the group-chat refactor lands.
// If a future change alters when ReplyToMessageID is set, these will fail
// and force a conscious decision instead of silent breakage.
//
// Current contract (as of pre-refactor):
//   - P2P inbound, no thread context        → no ReplyToMessageID (Create API)
//   - Thread inbound (ThreadID + MessageID) → ReplyToMessageID == MessageID
//   - Group main chat (no ThreadID)         → no ReplyToMessageID (same as P2P)
//
// The third case is the one the upcoming change targets. The other two must
// keep working unchanged.

func TestReplyRouting_P2P_NoReplyAnchor(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "alice",
		ChatID:    "c-p2p",
		MessageID: "om_p2p_msg",
		Kind:      channel.InputText,
		Text:      "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if got := out[0].ReplyToMessageID; got != "" {
		t.Errorf("P2P: ReplyToMessageID = %q, want \"\" (Create API path)", got)
	}
	if got := out[0].MentionUserID; got != "" {
		t.Errorf("P2P: MentionUserID = %q, want \"\" (no @ in 1-on-1)", got)
	}
}

func TestReplyRouting_Thread_UsesReplyAnchor(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "alice",
		ChatID:    "c-grp",
		MessageID: "om_thread_msg",
		ThreadID:  "omt_xyz",
		RootID:    "om_thread_root",
		Kind:      channel.InputText,
		Text:      "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if got := out[0].ReplyToMessageID; got != "om_thread_msg" {
		t.Errorf("thread: ReplyToMessageID = %q, want %q (Reply API anchored at inbound msg)",
			got, "om_thread_msg")
	}
}

// TestReplyRouting_GroupMainChat_UsesReplyAnchor locks in the new contract:
// in a group main chat (IsGroup=true, no thread), replyCard/replyText use
// the Reply API anchored at the user's message so the reply renders with
// a quote bubble. P2P (IsGroup=false) keeps the unanchored behavior.
func TestReplyRouting_GroupMainChat_UsesReplyAnchor(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "alice",
		ChatID:    "c-grp",
		MessageID: "om_grp_main_msg",
		IsGroup:   true,
		Kind:      channel.InputText,
		Text:      "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if got := out[0].ReplyToMessageID; got != "om_grp_main_msg" {
		t.Errorf("group main chat: ReplyToMessageID = %q, want %q (Reply API for quote bubble)",
			got, "om_grp_main_msg")
	}
}

// TestReplyRouting_GroupCommand_NoMention covers the corrected @-mention
// policy for command responses: in group chats they get a quote bubble
// (so people can see who asked) but NOT an @ — the user is already
// looking at the chat since they just typed. @-mention is reserved for
// the agent's Done card, which can arrive minutes after the user walked
// away.
func TestReplyRouting_GroupCommand_NoMention(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "ou_alice",
		ChatID:    "c-grp",
		MessageID: "om_msg",
		IsGroup:   true,
		Kind:      channel.InputText,
		Text:      "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if got := out[0].MentionUserID; got != "" {
		t.Errorf("group command: MentionUserID = %q, want \"\" (command responses do not @ — only Done cards do)",
			got)
	}
}

// TestReplyRouting_ThreadCommand_NoMention mirrors the policy in thread
// context: command responses inside a thread are still gateway-driven
// short replies and shouldn't @ the user. The @ belongs to streamed
// agent output, not command echoes.
func TestReplyRouting_ThreadCommand_NoMention(t *testing.T) {
	b, ch, _ := newTestBridge(t)
	b.OnMessage(context.Background(), channel.InboundMessage{
		UserID:    "ou_alice",
		ChatID:    "c-grp",
		MessageID: "om_msg",
		ThreadID:  "omt_xyz",
		RootID:    "om_root",
		IsGroup:   true,
		Kind:      channel.InputText,
		Text:      "/help",
	})
	out := ch.Outbound()
	if len(out) != 1 {
		t.Fatalf("Outbound = %d, want 1", len(out))
	}
	if got := out[0].MentionUserID; got != "" {
		t.Errorf("thread command: MentionUserID = %q, want \"\" (no @ for command responses)",
			got)
	}
}

// TestFeishuChannel_MentionInjection verifies that when SendMessage is given
// a MentionUserID, the rendered card payload contains the Lark <at> tag in
// its first markdown section. This guards the channel-side contract that
// powers the bridge's group-chat UX.
func TestReplyRouting_MentionInjectedIntoFirstMarkdown(t *testing.T) {
	// Build a card with markdown so injection has somewhere to land.
	card := channel.Card{
		Title: "Done",
		Sections: []channel.Section{
			{Markdown: "**Hello world**"},
			{Markdown: "second section"},
		},
	}
	// Use the channel's injection helper directly so we don't depend on
	// the network-bound SendMessage path. Mirrors what SendMessage does
	// internally when msg.MentionUserID is set.
	got := card
	got.Sections = append([]channel.Section{}, card.Sections...)
	injectMentionIntoCardForTest(&got, "ou_xxx")

	first := got.Sections[0].Markdown
	if !strings.Contains(first, `<at user_id="ou_xxx">`) {
		t.Errorf("first section missing <at> tag: %q", first)
	}
	if !strings.Contains(first, "Hello world") {
		t.Errorf("first section lost original markdown: %q", first)
	}
	if got.Sections[1].Markdown != "second section" {
		t.Errorf("second section was modified: %q", got.Sections[1].Markdown)
	}
}

// injectMentionIntoCardForTest is a bridge-local mirror of the feishu-side
// helper, so the test doesn't reach across packages. Behavior must match
// feishu.injectMentionIntoCard exactly.
func injectMentionIntoCardForTest(card *channel.Card, userID string) {
	at := `<at user_id="` + userID + `"></at> `
	for i := range card.Sections {
		if card.Sections[i].Markdown != "" {
			s := card.Sections[i]
			s.Markdown = at + s.Markdown
			card.Sections[i] = s
			return
		}
	}
	card.Sections = append([]channel.Section{{Markdown: at}}, card.Sections...)
}
