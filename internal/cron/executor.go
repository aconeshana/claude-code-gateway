package cron

import (
	"context"
	"time"
)

// ExecRequest bundles everything an Executor needs to run a single job.
type ExecRequest struct {
	Job     Job
	Timeout time.Duration
}

// ExecResult captures the outcome of a single job execution.
type ExecResult struct {
	Summary  string
	Err      error
	Duration time.Duration
}

// Executor is the runtime abstraction that decouples the scheduler from any
// specific AI agent implementation. The bridge package provides a concrete
// implementation that spawns claude-code sessions; future runtimes (codex,
// custom agents) implement the same interface.
type Executor interface {
	// Name returns a human-readable identifier for this executor (e.g.
	// "claude-code", "codex"). Used in logs and status output.
	Name() string

	// Execute runs the job's prompt inside a managed session. The context
	// carries the per-run deadline derived from Job.TimeoutMins.
	Execute(ctx context.Context, req ExecRequest) ExecResult
}
