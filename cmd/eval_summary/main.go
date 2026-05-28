// Command eval_summary runs the summary worker against N real jsonl files
// and prints the generated summary alongside the file path for human grading.
// Does NOT write to gateway_state.json — pure evaluation, no side effects.
//
// Usage:
//
//	go run ./cmd/eval_summary -n 10                 # pick 10 most recent
//	go run ./cmd/eval_summary -n 10 -window 7       # within last 7 days
//
// Output: one block per session with path / mtime / first user prompt /
// generated summary. Use the "first user prompt" to judge relevance.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/bridge"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

func main() {
	n := flag.Int("n", 10, "how many sessions to evaluate")
	window := flag.Int("window", 7, "lookback window in days")
	cliPath := flag.String("cli", "claude", "claude CLI binary")
	model := flag.String("model", "claude-haiku-4-5", "admin model")
	minSize := flag.Int64("min-size", 50*1024, "skip jsonl smaller than N bytes (filter out trivial sessions)")
	maxSize := flag.Int64("max-size", 5*1024*1024, "skip jsonl larger than N bytes (admin times out on huge files)")
	skip := flag.String("skip", "summary,batch,_meta", "comma-separated substrings — skip sessions whose first-user prompt contains any (filters meta worker sessions)")
	flag.Parse()

	// Discover real jsonl files
	disc := claudeRT.NewDiscoverer("", "")
	records, err := disc.Scan(context.Background(), runtime.ScanOpts{WindowDays: *window})
	if err != nil {
		log.Fatalf("scan: %v", err)
	}
	if len(records) == 0 {
		log.Fatal("no records to evaluate")
	}
	// Most recent first
	sort.Slice(records, func(i, j int) bool {
		return records[i].LastActivity.After(records[j].LastActivity)
	})

	// Apply size + path filters; we'll keep a larger pool and only stop
	// once we have N non-meta results.
	skipParts := strings.Split(*skip, ",")
	var pool []runtime.DiscoveredSession
	for _, r := range records {
		st, err := os.Stat(r.SourceRef)
		if err != nil {
			continue
		}
		if st.Size() < *minSize || st.Size() > *maxSize {
			continue
		}
		isMeta := false
		for _, s := range skipParts {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if strings.Contains(strings.ToLower(r.InitialSummary), strings.ToLower(s)) {
				isMeta = true
				break
			}
		}
		if isMeta {
			continue
		}
		pool = append(pool, r)
	}
	if len(pool) == 0 {
		log.Fatal("no records after filtering")
	}

	// Standalone manager + admin (no persister, no save side effects)
	rt := claudeRT.NewRuntime(*cliPath)
	mgr := session.NewManager(rt, mustHome(), "auto", 16, 0, 0)
	defer mgr.Shutdown(context.Background())

	ad := bridge.NewAdminForEval(mgr, mustHome(), *model)
	defer ad.Destroy()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Walk the pool until we collect N non-meta summaries. _skip_meta_
	// results don't count toward N — they indicate the prompt correctly
	// flagged a worker/eval session and we should try the next candidate.
	type evalResult struct {
		rec     runtime.DiscoveredSession
		summary string
		took    time.Duration
		err     error
	}
	results := make([]evalResult, 0, *n)
	collected := 0
	for i, r := range pool {
		if collected >= *n {
			break
		}
		fmt.Printf("\n[%d/%d (collected %d/%d)] %s\n", i+1, len(pool), collected, *n, r.RuntimeID[:8])
		fmt.Printf("        file:     %s\n", r.SourceRef)
		fmt.Printf("        cwd:      %s\n", r.WorkingDir)
		fmt.Printf("        mtime:    %s\n", r.LastActivity.Format("2006-01-02 15:04"))
		fmt.Printf("        lastPrompt placeholder: %q\n", truncate(r.InitialSummary, 80))

		t0 := time.Now()
		summary, err := bridge.RunSummaryPromptForEval(ctx, ad, r.SourceRef)
		took := time.Since(t0).Round(100 * time.Millisecond)
		if err != nil {
			fmt.Printf("        \033[31mERROR\033[0m (%s): %v\n", took, err)
			results = append(results, evalResult{rec: r, took: took, err: err})
			collected++
			continue
		}
		if summary == "_skip_meta_" {
			fmt.Printf("        \033[33mSKIP_META\033[0m (%s) — trying next candidate\n", took)
			continue
		}
		fmt.Printf("        \033[32mSUMMARY\033[0m (%s): %s\n", took, summary)
		results = append(results, evalResult{rec: r, summary: summary, took: took})
		collected++
	}

	// Final tally
	fmt.Println("\n=== summary table (non-meta only) ===")
	for i, r := range results {
		fmt.Printf("%2d. [%s] %-30s | %s\n",
			i+1, r.rec.RuntimeID[:8], shortProject(r.rec.WorkingDir), r.summary)
	}
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return h
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func shortProject(dir string) string {
	if len(dir) > 40 {
		return "..." + dir[len(dir)-37:]
	}
	return dir
}
