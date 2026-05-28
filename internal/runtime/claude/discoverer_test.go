package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// jsonl fixtures mimicking real claude-code transcript shape (head-only;
// minimal lines needed to cover extract logic).
const (
	fixtureBasic = `{"type":"last-prompt","sessionId":"sess-1","lastPrompt":"earlier prompt"}
{"type":"permission-mode","sessionId":"sess-1","mode":"auto"}
{"type":"user","isMeta":false,"sessionId":"sess-1","cwd":"/tmp/proj-a","message":{"role":"user","content":"first user message"}}
{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}
{"type":"last-prompt","sessionId":"sess-1","lastPrompt":"latest action"}
`

	fixtureWithCustomTitle = `{"type":"last-prompt","sessionId":"sess-2","lastPrompt":"a"}
{"type":"user","isMeta":false,"sessionId":"sess-2","cwd":"/tmp/proj-b","message":{"role":"user","content":"hello"}}
{"type":"custom-title","sessionId":"sess-2","customTitle":"my project work"}
{"type":"last-prompt","sessionId":"sess-2","lastPrompt":"newest"}
`

	fixtureBlocksContent = `{"type":"last-prompt","sessionId":"sess-3","lastPrompt":"x"}
{"type":"user","isMeta":false,"sessionId":"sess-3","cwd":"/tmp/proj-c","message":{"role":"user","content":[{"type":"text","text":"block-style content"}]}}
`
)

func writeFixture(t *testing.T, dir, projDirName, sessionID, content string) string {
	t.Helper()
	pDir := filepath.Join(dir, projDirName)
	if err := os.MkdirAll(pDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pDir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScan_ExtractsBasicFields(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "-tmp-proj-a", "sess-1", fixtureBasic)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d records, want 1", len(results))
	}
	r := results[0]
	if r.RuntimeID != "sess-1" {
		t.Errorf("RuntimeID = %q, want sess-1", r.RuntimeID)
	}
	if r.WorkingDir != "/tmp/proj-a" {
		t.Errorf("WorkingDir = %q, want /tmp/proj-a", r.WorkingDir)
	}
	if !strings.Contains(r.InitialSummary, "latest action") {
		t.Errorf("InitialSummary = %q, want it to come from latest lastPrompt", r.InitialSummary)
	}
	if r.CustomTitle != "" {
		t.Errorf("CustomTitle should be empty, got %q", r.CustomTitle)
	}
}

func TestScan_PrefersCustomTitle(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "-tmp-proj-b", "sess-2", fixtureWithCustomTitle)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d records, want 1", len(results))
	}
	if results[0].CustomTitle != "my project work" {
		t.Errorf("CustomTitle = %q, want 'my project work'", results[0].CustomTitle)
	}
}

func TestScan_HandlesBlocksContent(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "-tmp-proj-c", "sess-3", fixtureBlocksContent)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d, want 1", len(results))
	}
	// lastPrompt 是 "x" 优先,如果没有就用 block 内容
	got := results[0].InitialSummary
	if got != "x" {
		t.Errorf("expected 'x' (from lastPrompt), got %q", got)
	}
}

func TestScan_WindowDaysFilter(t *testing.T) {
	dir := t.TempDir()
	recentPath := writeFixture(t, dir, "-proj-recent", "sess-r", fixtureBasic)
	oldPath := writeFixture(t, dir, "-proj-old", "sess-o", fixtureBasic)

	// Touch oldPath to 10 days ago
	old := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	_ = recentPath

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{WindowDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("WindowDays=7 should exclude 10-day-old session, got %d", len(results))
	}
	if results[0].RuntimeID != "sess-r" {
		t.Errorf("expected sess-r (recent), got %s", results[0].RuntimeID)
	}
}

func TestScan_MTimeCacheReuse(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	writeFixture(t, dir, "-proj-x", "sess-x", fixtureBasic)

	d := NewDiscoverer(dir, cachePath)
	first, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first scan: got %d, want 1", len(first))
	}

	// Second scan with fresh Discoverer (loads cache from disk)
	d2 := NewDiscoverer(dir, cachePath)
	second, err := d2.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("second scan: got %d, want 1", len(second))
	}
	if second[0].RuntimeID != first[0].RuntimeID {
		t.Errorf("cached scan should return same record")
	}
}

func TestScan_SkipsEmptyAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	// Empty file
	emptyDir := filepath.Join(dir, "-proj-empty")
	_ = os.MkdirAll(emptyDir, 0755)
	_ = os.WriteFile(filepath.Join(emptyDir, "empty.jsonl"), []byte{}, 0600)
	// Corrupt JSON
	_ = os.WriteFile(filepath.Join(emptyDir, "corrupt.jsonl"), []byte("not-json\n"), 0600)
	// Valid alongside
	writeFixture(t, dir, "-proj-empty", "sess-good", fixtureBasic)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, _ := d.Scan(context.Background(), runtime.ScanOpts{})
	gotGood := false
	for _, r := range results {
		if r.RuntimeID == "sess-good" {
			gotGood = true
		}
	}
	if !gotGood {
		t.Errorf("good session should be returned despite siblings being empty/corrupt")
	}
}

func TestScan_IgnoresForkSubdirs(t *testing.T) {
	dir := t.TempDir()
	pDir := filepath.Join(dir, "-proj-fork")
	_ = os.MkdirAll(filepath.Join(pDir, "sess-1"), 0755)
	// jsonl inside the fork subdir should NOT be picked up
	_ = os.WriteFile(filepath.Join(pDir, "sess-1", "subagent.jsonl"), []byte(fixtureBasic), 0600)
	writeFixture(t, dir, "-proj-fork", "sess-1", fixtureBasic)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, _ := d.Scan(context.Background(), runtime.ScanOpts{})
	if len(results) != 1 {
		t.Fatalf("got %d, want 1 (fork subdir should be ignored)", len(results))
	}
}

func TestScan_NameImplementsDiscoverer(t *testing.T) {
	d := NewDiscoverer(t.TempDir(), filepath.Join(t.TempDir(), "cache.json"))
	if d.Name() != "claude-code" {
		t.Errorf("Name() = %q, want claude-code", d.Name())
	}
	// Compile-time interface check
	var _ runtime.Discoverer = d
}

// TestScan_DetectsAdminInternalByCwd guards against the regression where
// admin-worker sessions polluted /list. Sessions whose cwd lives under
// AdminWorkdirPrefix must be flagged so the bridge skips them.
func TestScan_DetectsAdminInternalByCwd(t *testing.T) {
	dir := t.TempDir()
	transcript := `{"type":"user","isMeta":false,"sessionId":"s-admin","cwd":"` + AdminWorkdirPrefix + `","message":{"role":"user","content":"hi"}}` + "\n"
	writeFixture(t, dir, "-tmp-claude-code-gateway-admin", "s-admin", transcript)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsAdminInternal {
		t.Errorf("expected IsAdminInternal=true for cwd in AdminWorkdirPrefix, got %+v", results)
	}
}

// TestScan_DetectsAdminInternalByFingerprint covers the fallback path —
// sessions created before the dedicated workdir convention should still
// be identified by content fingerprints (worker prompt strings).
func TestScan_DetectsAdminInternalByFingerprint(t *testing.T) {
	dir := t.TempDir()
	transcript := `{"type":"user","isMeta":false,"sessionId":"s-old","cwd":"/Users/somebody/work","message":{"role":"user","content":"总结一个 claude-code session 的具体问题..."}}` + "\n"
	writeFixture(t, dir, "-Users-somebody-work", "s-old", transcript)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsAdminInternal {
		t.Errorf("expected IsAdminInternal=true for fingerprint match, got %+v", results)
	}
}

// TestScan_RegularSessionNotFlagged is the negative case — make sure normal
// user sessions don't get falsely classified as admin-internal.
func TestScan_RegularSessionNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "-tmp-real-proj", "s-real", fixtureBasic)

	d := NewDiscoverer(dir, filepath.Join(t.TempDir(), "cache.json"))
	results, err := d.Scan(context.Background(), runtime.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].IsAdminInternal {
		t.Errorf("regular session should NOT be admin-internal, got %+v", results)
	}
}
