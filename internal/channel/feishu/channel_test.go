package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

func TestRenderCard_BasicMarkdown(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "Hello",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: "# Heading\nbody text"},
		},
	})
	if out == "" {
		t.Fatal("empty card")
	}
	// header template should be blue for Info
	if !contains(out, `"template":"blue"`) {
		t.Errorf("missing blue template: %s", out)
	}
	// heading should be flattened to **Heading**
	if !contains(out, "**Heading**") {
		t.Errorf("heading not converted: %s", out)
	}
}

func TestRenderCard_ButtonAction(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "Pick one",
		Sections: []channel.Section{{
			Buttons: []channel.Button{
				{Label: "OK", Style: "primary", Action: map[string]string{"action": "ok", "id": "42"}},
			},
		}},
	})
	if !contains(out, `"content":"OK"`) {
		t.Errorf("button label missing: %s", out)
	}
	if !contains(out, `"action":"ok"`) || !contains(out, `"id":"42"`) {
		t.Errorf("action payload missing: %s", out)
	}
	if !contains(out, `"type":"primary"`) {
		t.Errorf("button style missing")
	}
}

func TestRenderCard_AllTones(t *testing.T) {
	cases := []struct {
		tone channel.Tone
		want string
	}{
		{channel.ToneSuccess, "green"},
		{channel.ToneWarning, "orange"},
		{channel.ToneError, "red"},
		{channel.ToneInfo, "blue"},
		{channel.ToneNeutral, "grey"},
		{"", "grey"},
	}
	for _, c := range cases {
		out := renderCard(channel.Card{Title: "T", Tone: c.tone})
		needle := `"template":"` + c.want + `"`
		if !contains(out, needle) {
			t.Errorf("tone %q: want %s, got %s", c.tone, needle, out)
		}
	}
}

func TestRenderCard_FormAndSubmit(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "Config",
		Sections: []channel.Section{{
			Form: &channel.Form{
				FormID: "my_form",
				Fields: []channel.FormField{
					{Name: "value", Placeholder: "enter value", Initial: "foo"},
				},
				Submit: channel.Button{Label: "Save", Style: "primary", Action: map[string]string{"action": "save"}},
			},
		}},
	})
	if !contains(out, `"name":"my_form"`) {
		t.Errorf("form name missing: %s", out)
	}
	if !contains(out, `"form_action_type":"submit"`) {
		t.Errorf("submit type missing: %s", out)
	}
}

// TestRenderCard_FormSubmitNameDistinctFromForm guards against a Lark API
// constraint: the submit button's `name` must differ from the enclosing form's
// `name`. The API rejects duplicates with `ErrCode 11310; name(X) duplicate`,
// and the user-visible symptom is that the "修改" button silently fails to
// open the edit card.
//
// We encode "<action>:<key>" into the submit button name so the bridge can
// route form submissions back (Lark drops button.value during form submit).
func TestRenderCard_FormSubmitNameDistinctFromForm(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "Config",
		Sections: []channel.Section{{
			Form: &channel.Form{
				FormID: "config_form",
				Fields: []channel.FormField{{Name: "config_value", Placeholder: "value"}},
				Submit: channel.Button{Label: "Save", Action: map[string]string{"action": "save_config", "key": "MY_KEY"}},
			},
		}},
	})
	if !contains(out, `"name":"config_form"`) {
		t.Errorf("form name missing: %s", out)
	}
	// submit button name must be different from form name (Lark API constraint)
	// AND encode the routing info since Lark drops button.value on form submit
	if !contains(out, `"name":"save_config:MY_KEY"`) {
		t.Errorf("submit button name should encode <action>:<key>: %s", out)
	}
	// Sanity: form-level name appears exactly once.
	if strings.Count(out, `"name":"config_form"`) != 1 {
		t.Errorf("form name should appear exactly once, output: %s", out)
	}
}

// TestRenderCard_FormSubmitFallbackName guards against the no-action edge case:
// when Submit.Action lacks "action", we should still emit a distinct submit
// name to avoid the Lark duplicate-name rejection.
func TestRenderCard_FormSubmitFallbackName(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "X",
		Sections: []channel.Section{{
			Form: &channel.Form{
				FormID: "myform",
				Submit: channel.Button{Label: "Go"}, // no Action
			},
		}},
	})
	if !contains(out, `"name":"myform_submit"`) {
		t.Errorf("fallback submit name missing: %s", out)
	}
}

func TestRenderCard_DividerAndNote(t *testing.T) {
	out := renderCard(channel.Card{
		Title: "T",
		Sections: []channel.Section{
			{Divider: true, Markdown: "before"},
			{Note: "footer"},
		},
	})
	if !contains(out, `"tag":"hr"`) {
		t.Errorf("missing divider: %s", out)
	}
	if !contains(out, "footer") {
		t.Errorf("missing note: %s", out)
	}
}

func TestConvertToLarkMD_StripsHeaders(t *testing.T) {
	in := "# Title\nbody"
	out := convertToLarkMD(in)
	if contains(out, "# ") {
		t.Errorf("header not stripped: %s", out)
	}
	if !contains(out, "**Title**") {
		t.Errorf("header not bolded: %s", out)
	}
}

func TestConvertToLarkMD_TableRow(t *testing.T) {
	in := "|a|b|c|\n|---|---|---|\n|1|2|3|"
	out := convertToLarkMD(in)
	if !contains(out, "a  |  b  |  c") {
		t.Errorf("table not converted: %s", out)
	}
}

func TestDetectMediaType(t *testing.T) {
	// PNG header
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if got := detectMediaType(png); got != "image/png" {
		t.Errorf("PNG: %s", got)
	}
	// JPEG header
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if got := detectMediaType(jpeg); got != "image/jpeg" {
		t.Errorf("JPEG: %s", got)
	}
}

func TestParsePostContent_TextOnly(t *testing.T) {
	in := `{"zh_cn":{"title":"","content":[[{"tag":"text","text":"hello world"}]]}}`
	text, keys := parsePostContent(in)
	if text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
	if len(keys) != 0 {
		t.Errorf("expected no image keys, got %v", keys)
	}
}

// TestParsePostContent_UnwrappedShape covers the schema the Lark Go SDK
// actually delivers (locale wrapper already stripped). Captured verbatim
// from a real Feishu send at 17:40:59 on 2026-05-27. The original parser
// only handled the documented `{"zh_cn":{...}}` shape and silently
// produced empty output, dropping every image+text message the user sent.
// This is the regression the earlier unit tests failed to catch — they
// were written against the documented shape, not the wire shape.
func TestParsePostContent_UnwrappedShape(t *testing.T) {
	in := `{"title":"","content":[[{"tag":"text","text":"Provider 加不加无所谓","style":[]}],[{"tag":"img","image_key":"img_v3_02123_4a2edd44","width":720,"height":836}]]}`
	text, keys := parsePostContent(in)
	if text != "Provider 加不加无所谓" {
		t.Errorf("text = %q", text)
	}
	if len(keys) != 1 || keys[0] != "img_v3_02123_4a2edd44" {
		t.Errorf("imageKeys = %v", keys)
	}
}

func TestParsePostContent_TitlePlusBody(t *testing.T) {
	in := `{"zh_cn":{"title":"Greeting","content":[[{"tag":"text","text":"line1"}],[{"tag":"text","text":"line2"}]]}}`
	text, _ := parsePostContent(in)
	want := "Greeting\nline1\nline2"
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

// TestParsePostContent_MixedTextAndImage covers the exact case that bug
// 6befadec hit: user types a sentence and pastes an image inline. Both the
// text and the image_key must round-trip; previously the whole message was
// dropped at the channel layer because there was no "post" case at all.
func TestParsePostContent_MixedTextAndImage(t *testing.T) {
	in := `{"zh_cn":{"title":"","content":[[
		{"tag":"text","text":"please review "},
		{"tag":"img","image_key":"img_v3_abc"},
		{"tag":"text","text":" thanks"}
	]]}}`
	text, keys := parsePostContent(in)
	if text != "please review  thanks" {
		t.Errorf("text = %q", text)
	}
	if len(keys) != 1 || keys[0] != "img_v3_abc" {
		t.Errorf("imageKeys = %v, want [img_v3_abc]", keys)
	}
}

func TestParsePostContent_MultipleImages(t *testing.T) {
	in := `{"zh_cn":{"title":"","content":[[
		{"tag":"img","image_key":"k1"},
		{"tag":"img","image_key":"k2"},
		{"tag":"img","image_key":"k3"}
	]]}}`
	text, keys := parsePostContent(in)
	if text != "" {
		t.Errorf("text should be empty, got %q", text)
	}
	if len(keys) != 3 || keys[0] != "k1" || keys[1] != "k2" || keys[2] != "k3" {
		t.Errorf("imageKeys = %v", keys)
	}
}

func TestParsePostContent_AtAndAnchor(t *testing.T) {
	in := `{"zh_cn":{"title":"","content":[[
		{"tag":"at","user_name":"bot"},
		{"tag":"text","text":" check "},
		{"tag":"a","text":"this link","href":"https://example.com"}
	]]}}`
	text, _ := parsePostContent(in)
	want := "@bot check this link"
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func TestParsePostContent_EnUsFallback(t *testing.T) {
	in := `{"en_us":{"title":"hi","content":[[{"tag":"text","text":"there"}]]}}`
	text, _ := parsePostContent(in)
	if text != "hi\nthere" {
		t.Errorf("en_us fallback failed: %q", text)
	}
}

func TestParsePostContent_EmptyAndMalformed(t *testing.T) {
	cases := []string{
		``,
		`not json`,
		`{}`,
		`{"zh_cn":null}`,
		`{"zh_cn":{"title":"","content":[]}}`,
		`{"zh_cn":{"title":"","content":[[]]}}`,
		`{"zh_cn":{"title":"","content":[[{"tag":"img","image_key":""}]]}}`,
	}
	for _, c := range cases {
		text, keys := parsePostContent(c)
		if text != "" || len(keys) != 0 {
			t.Errorf("input %q -> text=%q keys=%v, want empty", c, text, keys)
		}
	}
}

func TestParsePostContent_MultiRowMixed(t *testing.T) {
	in := `{"zh_cn":{"title":"Bug report","content":[
		[{"tag":"text","text":"summary line"}],
		[{"tag":"img","image_key":"screenshot_1"}],
		[{"tag":"text","text":"footer notes"}]
	]}}`
	text, keys := parsePostContent(in)
	want := "Bug report\nsummary line\nfooter notes"
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if len(keys) != 1 || keys[0] != "screenshot_1" {
		t.Errorf("keys = %v", keys)
	}
}

func TestStripMentions(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"@_user_abc hello", " hello"},
		{"hi @_user_xyz how are you", "hi  how are you"},
		{"no mentions", "no mentions"},
	}
	for _, c := range cases {
		if got := stripMentions(c.in); got != c.want {
			t.Errorf("stripMentions(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChannel_KindAndAllowedUserCheck(t *testing.T) {
	c := New(Config{AppID: "x", AppSecret: "y", AllowedUserIDs: []string{"alice"}})
	if c.Kind() != "feishu" {
		t.Errorf("Kind = %q", c.Kind())
	}
	if !c.isAllowedUser("alice") {
		t.Error("alice should be allowed")
	}
	if c.isAllowedUser("bob") {
		t.Error("bob should not be allowed")
	}

	c2 := New(Config{AppID: "x", AppSecret: "y"})
	if !c2.isAllowedUser("anyone") {
		t.Error("empty allowlist should allow everyone")
	}
}

func TestChannel_Dedup(t *testing.T) {
	c := New(Config{AppID: "x", AppSecret: "y"})
	if c.isDuplicate("") {
		t.Error("empty msg id should not dedup")
	}
	if c.isDuplicate("m-1") {
		t.Error("first time should not be duplicate")
	}
	if !c.isDuplicate("m-1") {
		t.Error("second time should be duplicate")
	}
}

func TestChannel_DispatchWithoutHandler(t *testing.T) {
	c := New(Config{AppID: "x", AppSecret: "y"})
	c.dispatch(context.Background(), channel.InboundMessage{Text: "noop"})
	// should not panic
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
