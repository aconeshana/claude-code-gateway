package feishu

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestIsMentionedInGroup_OurOpenID(t *testing.T) {
	ours := "ou_alpha"
	other := "ou_beta"

	c := &Channel{botOpenID: ours}

	cases := []struct {
		name     string
		mentions []*larkim.MentionEvent
		want     bool
	}{
		{
			name:     "empty mentions → no",
			mentions: nil,
			want:     false,
		},
		{
			name:     "single mention of other bot → no",
			mentions: []*larkim.MentionEvent{mentionFor(other)},
			want:     false,
		},
		{
			name:     "single mention of us → yes",
			mentions: []*larkim.MentionEvent{mentionFor(ours)},
			want:     true,
		},
		{
			name:     "us alongside others → yes",
			mentions: []*larkim.MentionEvent{mentionFor(other), mentionFor(ours)},
			want:     true,
		},
		{
			name: "mention with nil id is skipped",
			mentions: []*larkim.MentionEvent{
				{Id: nil},
				mentionFor(ours),
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.isMentionedInGroup(tc.mentions); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsMentionedInGroup_UnknownOpenIDFallback exercises the degraded
// path: when Start() failed to resolve our open_id, fall back to accepting
// any mention so the bot stays operational.
func TestIsMentionedInGroup_UnknownOpenIDFallback(t *testing.T) {
	c := &Channel{botOpenID: ""}
	if !c.isMentionedInGroup([]*larkim.MentionEvent{mentionFor("ou_random")}) {
		t.Error("with unknown open_id and ≥1 mention, should accept (degraded fallback)")
	}
	if c.isMentionedInGroup(nil) {
		t.Error("with unknown open_id and no mentions, should still reject")
	}
}

func mentionFor(openID string) *larkim.MentionEvent {
	id := openID
	return &larkim.MentionEvent{
		Id: &larkim.UserId{OpenId: &id},
	}
}
