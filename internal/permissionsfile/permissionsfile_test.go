package permissionsfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupProject creates a temporary HOME + project dir and returns paths.
// We override HOME so SettingsPath(SourceUser, ...) resolves under tmp,
// keeping the test hermetic — no risk of touching the real ~/.claude.
func setupProject(t *testing.T) (homeDir, projectDir string) {
	t.Helper()
	tmp := t.TempDir()
	homeDir = filepath.Join(tmp, "home")
	projectDir = filepath.Join(tmp, "project")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	t.Setenv("HOME", homeDir)
	return
}

func writeRawSettings(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSettingsPath_AllSources covers the three writable sources plus the
// empty inputs that must return "" rather than a bad path.
func TestSettingsPath_AllSources(t *testing.T) {
	home, project := setupProject(t)
	wantUser := filepath.Join(home, ".claude", "settings.json")
	wantProject := filepath.Join(project, ".claude", "settings.json")
	wantLocal := filepath.Join(project, ".claude", "settings.local.json")
	if got := SettingsPath(SourceUser, project); got != wantUser {
		t.Errorf("user path = %q, want %q", got, wantUser)
	}
	if got := SettingsPath(SourceProject, project); got != wantProject {
		t.Errorf("project path = %q, want %q", got, wantProject)
	}
	if got := SettingsPath(SourceLocal, project); got != wantLocal {
		t.Errorf("local path = %q, want %q", got, wantLocal)
	}
	if got := SettingsPath(SourceProject, ""); got != "" {
		t.Errorf("project path with empty projectDir = %q, want \"\"", got)
	}
}

func TestSettingsPath_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if got := SettingsPath(SourceUser, "/anywhere"); got != "" {
		t.Errorf("user path without $HOME = %q, want \"\"", got)
	}
}

func TestLoad_EmptyProject_NoFiles(t *testing.T) {
	_, project := setupProject(t)
	rules, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected empty rule list, got %d rules", len(rules))
	}
}

func TestLoad_MalformedJSON_ReturnsError(t *testing.T) {
	_, project := setupProject(t)
	writeRawSettings(t, SettingsPath(SourceLocal, project), "{not json")
	if _, err := Load(project); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestLoad_MergeAndSort verifies all three sources contribute rules and
// the output is sorted user→project→local then allow→deny→ask then by
// content alphabetically. Stability matters for predictable card output.
func TestLoad_MergeAndSort(t *testing.T) {
	home, project := setupProject(t)
	writeRawSettings(t, filepath.Join(home, ".claude", "settings.json"), `{
		"permissions": {
			"allow": ["WebSearch", "Bash(ls:*)"],
			"deny": ["Bash(rm -rf /)"]
		}
	}`)
	writeRawSettings(t, filepath.Join(project, ".claude", "settings.json"), `{
		"permissions": {
			"allow": ["Read(/etc/*)"],
			"ask": ["Bash(git push:*)"]
		}
	}`)
	writeRawSettings(t, filepath.Join(project, ".claude", "settings.local.json"), `{
		"permissions": {
			"allow": ["Bash(npm test)"]
		}
	}`)

	rules, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Rule{
		{SourceUser, BehaviorAllow, "Bash(ls:*)"},
		{SourceUser, BehaviorAllow, "WebSearch"},
		{SourceUser, BehaviorDeny, "Bash(rm -rf /)"},
		{SourceProject, BehaviorAllow, "Read(/etc/*)"},
		{SourceProject, BehaviorAsk, "Bash(git push:*)"},
		{SourceLocal, BehaviorAllow, "Bash(npm test)"},
	}
	if len(rules) != len(want) {
		t.Fatalf("rules count = %d, want %d; got: %+v", len(rules), len(want), rules)
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rules[%d] = %+v, want %+v", i, rules[i], w)
		}
	}
}

// TestLoad_SkipsEmptyContent_TolerantOfBadInput defends against malformed
// rule lists containing "" entries — observed in user-edited settings
// files. Empty strings would render as blank chips in the UI.
func TestLoad_SkipsEmptyContent_TolerantOfBadInput(t *testing.T) {
	_, project := setupProject(t)
	writeRawSettings(t, SettingsPath(SourceLocal, project), `{
		"permissions": {"allow": ["", "Bash(real)"]}
	}`)
	rules, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 1 || rules[0].Content != "Bash(real)" {
		t.Errorf("expected only the non-empty rule, got: %+v", rules)
	}
}

// TestAdd_CreatesMissingFile verifies Add can write to a file that
// doesn't exist yet, creating the .claude directory along the way. This
// is the "first ever permission rule via the bot" case.
func TestAdd_CreatesMissingFile(t *testing.T) {
	_, project := setupProject(t)
	rule := Rule{SourceLocal, BehaviorAllow, "Bash(make test)"}
	if err := Add(rule, project); err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(SettingsPath(SourceLocal, project))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(data), "Bash(make test)") {
		t.Errorf("file content missing rule:\n%s", data)
	}
}

// TestAdd_PreservesOtherFields confirms unrelated top-level keys
// (model, hooks, env) survive a permissions write. Regression guard
// against accidentally turning Add into a destructive rewrite.
func TestAdd_PreservesOtherFields(t *testing.T) {
	_, project := setupProject(t)
	path := SettingsPath(SourceProject, project)
	original := `{
		"model": "sonnet",
		"env": {"MY_VAR": "value"},
		"hooks": {"Notification": [{"hooks": [{"type": "command", "command": "echo hi"}]}]}
	}`
	writeRawSettings(t, path, original)

	rule := Rule{SourceProject, BehaviorAllow, "WebSearch"}
	if err := Add(rule, project); err != nil {
		t.Fatalf("Add: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse written file: %v", err)
	}
	if doc["model"] != "sonnet" {
		t.Errorf("model field lost: %v", doc["model"])
	}
	env, _ := doc["env"].(map[string]any)
	if env["MY_VAR"] != "value" {
		t.Errorf("env field lost: %+v", doc["env"])
	}
	if _, ok := doc["hooks"]; !ok {
		t.Errorf("hooks field lost: %+v", doc)
	}
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "WebSearch" {
		t.Errorf("rule not written: %+v", perms)
	}
}

// TestAdd_Duplicate_ReturnsErrAlreadyExists exercises the no-op success
// path. Callers should swallow this error.
func TestAdd_Duplicate_ReturnsErrAlreadyExists(t *testing.T) {
	_, project := setupProject(t)
	rule := Rule{SourceLocal, BehaviorAllow, "Bash(git diff:*)"}
	if err := Add(rule, project); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := Add(rule, project)
	if err != ErrAlreadyExists {
		t.Errorf("second Add = %v, want ErrAlreadyExists", err)
	}
}

// TestRemove_ExistingRule verifies the happy path and that other rules
// in the same list are not disturbed.
func TestRemove_ExistingRule(t *testing.T) {
	_, project := setupProject(t)
	writeRawSettings(t, SettingsPath(SourceLocal, project), `{
		"permissions": {"allow": ["Bash(ls:*)", "WebSearch", "Bash(pwd)"]}
	}`)
	rule := Rule{SourceLocal, BehaviorAllow, "WebSearch"}
	if err := Remove(rule, project); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	rules, _ := Load(project)
	if len(rules) != 2 {
		t.Fatalf("expected 2 remaining, got: %+v", rules)
	}
	for _, r := range rules {
		if r.Content == "WebSearch" {
			t.Errorf("WebSearch still present after Remove")
		}
	}
}

func TestRemove_MissingFile_ReturnsErrNotFound(t *testing.T) {
	_, project := setupProject(t)
	rule := Rule{SourceLocal, BehaviorAllow, "Bash(anything)"}
	if err := Remove(rule, project); err != ErrNotFound {
		t.Errorf("Remove on missing file = %v, want ErrNotFound", err)
	}
}

func TestRemove_MissingRule_ReturnsErrNotFound(t *testing.T) {
	_, project := setupProject(t)
	writeRawSettings(t, SettingsPath(SourceLocal, project), `{
		"permissions": {"allow": ["Bash(ls:*)"]}
	}`)
	rule := Rule{SourceLocal, BehaviorAllow, "WebSearch"}
	if err := Remove(rule, project); err != ErrNotFound {
		t.Errorf("Remove non-existent rule = %v, want ErrNotFound", err)
	}
}

func TestAdd_InvalidInputs(t *testing.T) {
	_, project := setupProject(t)
	cases := []Rule{
		{SourceLocal, BehaviorAllow, ""},        // empty content
		{Source("garbage"), BehaviorAllow, "x"}, // bad source
		{SourceLocal, Behavior("noop"), "x"},    // bad behavior
	}
	for _, r := range cases {
		if err := Add(r, project); err == nil {
			t.Errorf("Add(%+v) returned nil, expected validation error", r)
		}
	}
}

// TestAdd_WriteIsAtomic confirms that the temp file used during write is
// cleaned up. We don't simulate a crash mid-write (hard to trigger
// portably), but we do check that no .tmp residue is left after a
// successful write.
func TestAdd_WriteIsAtomic(t *testing.T) {
	_, project := setupProject(t)
	rule := Rule{SourceLocal, BehaviorDeny, "Bash(rm -rf /)"}
	if err := Add(rule, project); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(project, ".claude"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".settings-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
