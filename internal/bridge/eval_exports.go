// Package bridge — eval_exports.go exposes a minimal surface for the
// cmd/eval_summary tool to exercise the admin worker against real jsonl
// files WITHOUT touching the gateway's persistent state. Not for production
// use; the names carry the ForEval suffix to discourage casual reuse.
package bridge

import (
	"context"

	"github.com/anthropics/claude-code-gateway/internal/session"
)

// AdminForEval is the opaque handle returned by NewAdminForEval. The eval
// tool drives it via RunSummaryPromptForEval.
type AdminForEval struct{ inner *admin }

// NewAdminForEval builds a transient admin helper backed by mgr. Callers
// must invoke Destroy when finished.
func NewAdminForEval(mgr *session.Manager, workingDir, model string) *AdminForEval {
	return &AdminForEval{inner: newAdmin(mgr, workingDir, model)}
}

func (a *AdminForEval) Destroy() { a.inner.destroy() }

// RunSummaryPromptForEval runs the exact prompt used by summaryWorker
// against sourceRef and returns the cleaned summary. No state is persisted.
func RunSummaryPromptForEval(ctx context.Context, a *AdminForEval, sourceRef string) (string, error) {
	prompt := buildSummaryPrompt(sourceRef)
	raw, err := a.inner.query(ctx, prompt)
	if err != nil {
		return raw, err
	}
	return cleanAdminSummary(raw), nil
}
