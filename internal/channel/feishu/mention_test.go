package feishu

import (
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

func TestInjectMentionIntoCard_FirstMarkdownSection(t *testing.T) {
	card := channel.Card{
		Title: "Done",
		Sections: []channel.Section{
			{Markdown: "**Hello world**"},
			{Markdown: "second section"},
		},
	}
	injectMentionIntoCard(&card, "ou_alice")

	first := card.Sections[0].Markdown
	if !strings.Contains(first, `<at user_id="ou_alice"></at>`) {
		t.Errorf("first section missing <at> tag: %q", first)
	}
	if !strings.Contains(first, "Hello world") {
		t.Errorf("first section lost original markdown: %q", first)
	}
	if card.Sections[1].Markdown != "second section" {
		t.Errorf("second section was modified: %q", card.Sections[1].Markdown)
	}
}

func TestInjectMentionIntoCard_SkipsNonMarkdownSections(t *testing.T) {
	card := channel.Card{
		Sections: []channel.Section{
			{Divider: true},
			{Note: "footer"},
			{Markdown: "real content"},
		},
	}
	injectMentionIntoCard(&card, "ou_bob")

	if !strings.HasPrefix(card.Sections[2].Markdown, `<at user_id="ou_bob"></at>`) {
		t.Errorf("expected at-tag prepended to the markdown section, got %q",
			card.Sections[2].Markdown)
	}
	if card.Sections[0].Markdown != "" {
		t.Errorf("divider section gained markdown: %q", card.Sections[0].Markdown)
	}
}

func TestInjectMentionIntoCard_NoMarkdownSection_PrependsOne(t *testing.T) {
	card := channel.Card{
		Sections: []channel.Section{
			{Divider: true},
		},
	}
	injectMentionIntoCard(&card, "ou_carol")

	if len(card.Sections) != 2 {
		t.Fatalf("expected a new section inserted, got %d sections", len(card.Sections))
	}
	if got := card.Sections[0].Markdown; !strings.Contains(got, `<at user_id="ou_carol">`) {
		t.Errorf("first section should hold the at-tag, got %q", got)
	}
}

func TestLarkAtTag_Format(t *testing.T) {
	got := larkAtTag("ou_xyz")
	want := `<at user_id="ou_xyz"></at>`
	if got != want {
		t.Errorf("larkAtTag = %q, want %q", got, want)
	}
}

// TestInjectMentionIntoCard_DoubleInjectionGuard exercises the renderer's
// failure-fallback path: when SendMessage's update attempt fails and the
// same Card is then handed to a fresh send (or retry), the at-tag must
// not be injected twice. Without the defensive slice clone in
// injectMentionIntoCard, both calls would mutate the shared backing
// array and the user would see "<at>… <at>…" doubled up.
func TestInjectMentionIntoCard_DoubleInjectionGuard(t *testing.T) {
	original := channel.Card{
		Title: "Done",
		Sections: []channel.Section{
			{Markdown: "real content"},
			{Markdown: "second"},
		},
	}

	// Simulate the renderer pattern: build the card once, then call
	// SendMessage's injection twice (mirroring update→send fallback).
	a := original
	injectMentionIntoCard(&a, "ou_alice")
	b := original
	injectMentionIntoCard(&b, "ou_alice")

	if strings.Count(a.Sections[0].Markdown, "<at user_id=") != 1 {
		t.Errorf("first call should produce exactly one <at>, got %q", a.Sections[0].Markdown)
	}
	if strings.Count(b.Sections[0].Markdown, "<at user_id=") != 1 {
		t.Errorf("second call against the original card should also produce one <at>, got %q",
			b.Sections[0].Markdown)
	}
	// Most important: the source struct that BOTH calls were derived
	// from must remain untouched — proving the slice was deep-copied.
	if strings.Contains(original.Sections[0].Markdown, "<at") {
		t.Errorf("original Card was mutated: %q (deep-copy guard missing)",
			original.Sections[0].Markdown)
	}
}
