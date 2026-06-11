package bridge

import (
	"strings"
	"testing"
)

func TestBuildBTWPrompt_IncludesEverythingNeeded(t *testing.T) {
	jsonlPath := "/Users/x/.claude/projects/-foo/abc.jsonl"
	question := "之前我们在哪个文件里改了 redactURL?"
	prompt := buildBTWPrompt(jsonlPath, question)

	// 1) Must carry the admin marker so the admin's own jsonl is correctly
	//    fingerprinted as gateway-internal (not surfaced in /list).
	if !strings.Contains(prompt, AdminSessionMarker) {
		t.Errorf("prompt missing AdminSessionMarker — admin session would leak into discovery")
	}
	// 2) Must embed the user's question.
	if !strings.Contains(prompt, question) {
		t.Errorf("prompt missing user question; got:\n%s", prompt)
	}
	// 3) Must embed the jsonl path twice (once in the jq command, once as
	//    a trailer footnote) so the model can't lose track of which file.
	if strings.Count(prompt, jsonlPath) < 2 {
		t.Errorf("expected jsonl path in prompt at least twice; got %d occurrences",
			strings.Count(prompt, jsonlPath))
	}
	// 4) Must include the anchor tag instruction so cleanBTWAnswer can
	//    extract reliably.
	if !strings.Contains(prompt, "<answer>") || !strings.Contains(prompt, "</answer>") {
		t.Errorf("prompt missing <answer> anchor tag instruction")
	}
	// 5) Must include the jq filter snippet (catches accidental refactors
	//    that drop the role+text extraction logic).
	if !strings.Contains(prompt, "select((.type ==") {
		t.Errorf("prompt missing jq filter — model would have to invent one")
	}
	// 6) Must tell the model to admit ignorance rather than hallucinate.
	if !strings.Contains(prompt, "上下文里没看到") {
		t.Errorf("prompt missing 'no info available' guardrail — model may hallucinate")
	}
}

func TestCleanBTWAnswer_ExtractsTaggedAnswer(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single tag with thinking before",
			in:   "let me check... <answer>配置文件在 config.go 第 37 行</answer>",
			want: "配置文件在 config.go 第 37 行",
		},
		{
			name: "multiline answer",
			in:   "ok\n<answer>第一行\n第二行\n第三行</answer>\n",
			want: "第一行\n第二行\n第三行",
		},
		{
			name: "answer with surrounding whitespace",
			in:   "<answer>   带空白的答案   </answer>",
			want: "带空白的答案",
		},
		{
			name: "answer with leading/trailing quotes (model loves these)",
			in:   "<answer>\"加引号的答案\"</answer>",
			want: "加引号的答案",
		},
		{
			name: "answer with backticks (model wraps in code)",
			in:   "<answer>`代码风格`</answer>",
			want: "代码风格",
		},
		{
			name: "first occurrence wins when model leaks two tags",
			in:   "<answer>真正的回答</answer> 然后又说 <answer>多余的</answer>",
			want: "真正的回答",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanBTWAnswer(c.in); got != c.want {
				t.Errorf("cleanBTWAnswer(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCleanBTWAnswer_EmptyWhenAnchorMissing(t *testing.T) {
	// When admin forgets the <answer> tag, return empty so the caller can
	// fall back to displaying the raw reply with a warning. Returning the
	// raw input here would cause the model's "let me think..." preamble to
	// land in the card body verbatim.
	cases := []string{
		"",
		"just plain text, no tags",
		"<answer>incomplete",
		"</answer>only the closing tag",
		"<ans>wrong tag name</ans>",
	}
	for _, in := range cases {
		if got := cleanBTWAnswer(in); got != "" {
			t.Errorf("cleanBTWAnswer(%q) = %q, want empty", in, got)
		}
	}
}
