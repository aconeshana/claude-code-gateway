package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSummarizeToolInvocation_Bash verifies the shell-command extraction
// produces the canonical Bash(<cmd>:*) pattern for the simple case, and
// that the markdown body actually shows the command (otherwise users
// can't judge what they're approving).
func TestSummarizeToolInvocation_Bash(t *testing.T) {
	md, pat := summarizeToolInvocation("Bash", map[string]interface{}{
		"command":     "curl ipinfo.io",
		"description": "fetch IP info",
	})
	if !strings.Contains(md, "curl ipinfo.io") {
		t.Errorf("Bash markdown should show command; got: %s", md)
	}
	if !strings.Contains(md, "fetch IP info") {
		t.Errorf("Bash markdown should show description when present; got: %s", md)
	}
	if pat != "Bash(curl:*)" {
		t.Errorf("Bash pattern = %q, want Bash(curl:*)", pat)
	}
}

// TestSummarizeToolInvocation_Bash_QuotedAbsolutePath documents that the
// inferRulePattern is intentionally simple — quoted absolute paths
// produce a slightly broken pattern that the user is expected to edit
// in step 3 of the wizard. This test pins the current behavior so a
// future "smarter parser" change is a deliberate one, not accidental.
func TestSummarizeToolInvocation_Bash_QuotedAbsolutePath(t *testing.T) {
	_, pat := summarizeToolInvocation("Bash", map[string]interface{}{
		"command": `"/Applications/IntelliJ IDEA.app/Contents/.../mvn" --version`,
	})
	// First whitespace-delimited token includes the opening quote — this
	// is wrong as a literal Bash rule and the user will need to fix it.
	// Better than silently making up a "correct" rule that mishandles
	// arguments.
	if !strings.Contains(pat, "Bash(") {
		t.Errorf("expected Bash(...) prefix, got %q", pat)
	}
}

func TestSummarizeToolInvocation_FileTools(t *testing.T) {
	cases := []struct {
		tool     string
		input    map[string]interface{}
		mustHave string // markdown substring
		wantPat  string
	}{
		{"Read", map[string]interface{}{"file_path": "/etc/hosts"}, "/etc/hosts", "Read(/etc/*)"},
		{"Read", map[string]interface{}{"file_path": "/etc/hosts", "limit": 100.0}, "limit=100", "Read(/etc/*)"},
		{"Write", map[string]interface{}{"file_path": "/tmp/x.txt", "content": "hello"}, "hello", "Write(/tmp/*)"},
		{"Edit", map[string]interface{}{
			"file_path":  "/srv/app.go",
			"old_string": "OLD CONTENT",
			"new_string": "NEW CONTENT",
		}, "NEW CONTENT", "Edit(/srv/*)"},
	}
	for _, c := range cases {
		md, pat := summarizeToolInvocation(c.tool, c.input)
		if !strings.Contains(md, c.mustHave) {
			t.Errorf("%s markdown missing %q; got: %s", c.tool, c.mustHave, md)
		}
		if pat != c.wantPat {
			t.Errorf("%s pattern = %q, want %q", c.tool, pat, c.wantPat)
		}
	}
}

// TestSummarizeToolInvocation_FileToolEmptyPath_NoPattern verifies that
// missing/empty file_path turns off the [总是允许] button entirely
// (returning "" for the pattern). Otherwise we'd offer "Read(*)" which
// is too broad and would erode forward-mode's value.
func TestSummarizeToolInvocation_FileToolEmptyPath_NoPattern(t *testing.T) {
	for _, tool := range []string{"Read", "Write", "Edit"} {
		_, pat := summarizeToolInvocation(tool, map[string]interface{}{})
		if pat != "" {
			t.Errorf("%s with empty file_path should suppress pattern; got %q", tool, pat)
		}
	}
}

func TestSummarizeToolInvocation_WebFetch(t *testing.T) {
	md, pat := summarizeToolInvocation("WebFetch", map[string]interface{}{
		"url":    "https://api.example.com/v1/foo?bar=1",
		"prompt": "extract the title",
	})
	if !strings.Contains(md, "https://api.example.com/v1/foo") {
		t.Errorf("WebFetch markdown should show URL; got: %s", md)
	}
	if !strings.Contains(md, "extract the title") {
		t.Errorf("WebFetch markdown should show prompt; got: %s", md)
	}
	if pat != "WebFetch(domain:api.example.com)" {
		t.Errorf("WebFetch pattern = %q, want WebFetch(domain:api.example.com)", pat)
	}
}

func TestSummarizeToolInvocation_WebFetch_BadURL_NoPattern(t *testing.T) {
	_, pat := summarizeToolInvocation("WebFetch", map[string]interface{}{
		"url": "not-a-url",
	})
	if pat != "" {
		t.Errorf("WebFetch with unparseable URL should yield empty pattern; got %q", pat)
	}
}

func TestSummarizeToolInvocation_BlanketTools(t *testing.T) {
	cases := map[string]struct {
		tool, wantPat string
		input         map[string]interface{}
	}{
		"grep":   {"Grep", "Grep", map[string]interface{}{"pattern": "TODO"}},
		"glob":   {"Glob", "Glob", map[string]interface{}{"pattern": "**/*.go"}},
		"search": {"WebSearch", "WebSearch", map[string]interface{}{"query": "claude code"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, pat := summarizeToolInvocation(c.tool, c.input)
			if pat != c.wantPat {
				t.Errorf("%s pattern = %q, want %q", c.tool, pat, c.wantPat)
			}
		})
	}
}

// TestSummarizeToolInvocation_UnknownTool_NoPattern keeps the rule
// inference conservative — for tools we don't know about we still
// render the input (so user can judge), but suppress the [总是允许]
// button (so we don't silently allow future calls of an unmodeled
// tool with a wrong rule).
func TestSummarizeToolInvocation_UnknownTool_NoPattern(t *testing.T) {
	md, pat := summarizeToolInvocation("MysteryTool", map[string]interface{}{
		"some_field": "some_value",
	})
	if !strings.Contains(md, "some_field") {
		t.Errorf("unknown-tool markdown should dump raw input; got: %s", md)
	}
	if pat != "" {
		t.Errorf("unknown tool should suppress pattern; got %q", pat)
	}
}

func TestFirstShellWord(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"   ":              "",
		"ls":               "ls",
		"ls -la":           "ls",
		"  ls -la":         "ls",
		"git\tcommit":      "git",
		"command\nwithnl":  "command",
		`"path with sp" x`: `"path`, // pinned: see TestSummarizeToolInvocation_Bash_QuotedAbsolutePath
	}
	for in, want := range cases {
		if got := firstShellWord(in); got != want {
			t.Errorf("firstShellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInferFileRulePattern(t *testing.T) {
	cases := map[string]struct {
		tool, path, want string
	}{
		"absolute_with_dir":   {"Read", "/etc/hosts", "Read(/etc/*)"},
		"relative_in_cwd":     {"Edit", "main.go", "Edit(main.go)"},
		"deep_nested":         {"Write", "/srv/app/internal/x.go", "Write(/srv/app/internal/*)"},
		"empty":               {"Read", "", ""},
		"whitespace":          {"Read", "  ", ""},
		"just_a_dir_no_slash": {"Read", "foo", "Read(foo)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := inferFileRulePattern(c.tool, c.path)
			if got != c.want {
				t.Errorf("inferFileRulePattern(%q, %q) = %q, want %q",
					c.tool, c.path, got, c.want)
			}
		})
	}
}

func TestExtractDomain(t *testing.T) {
	cases := map[string]string{
		"https://example.com/path":              "example.com",
		"http://Example.COM:8080/foo":           "example.com:8080", // port preserved, host lowercased
		"https://api.x.io/v1?a=b":               "api.x.io",
		"":                                      "",
		"not-a-url":                             "",
		"ftp://files.example.com":               "files.example.com",
		"https://用户中文.example.com/path":         "用户中文.example.com",
	}
	for in, want := range cases {
		if got := extractDomain(in); got != want {
			t.Errorf("extractDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateForCardBytes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		maxB    int
		expect  string // either exact match or "" for "must contain truncation marker"
		marker  bool
	}{
		{"under_budget", "hello", 100, "hello", false},
		{"exact_budget", "hello", 5, "hello", false},
		{"over_budget_ascii", "hello world this is long", 5, "", true},
		// CJK char is 3 bytes in UTF-8, must not split mid-rune
		{"over_budget_cjk", "你好世界", 7, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := truncateForCardBytes(c.in, c.maxB)
			if c.marker {
				if !strings.Contains(out, "已截断") {
					t.Errorf("expected truncation marker, got: %s", out)
				}
				// Must remain valid UTF-8
				if !utf8.ValidString(out) {
					t.Errorf("output is not valid UTF-8: %q", out)
				}
			} else {
				if out != c.expect {
					t.Errorf("got %q, want %q", out, c.expect)
				}
			}
		})
	}
}
