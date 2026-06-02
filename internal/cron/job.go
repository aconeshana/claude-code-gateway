package cron

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// SessionMode controls how the scheduler manages the underlying AI session
// across successive runs of the same job.
type SessionMode string

const (
	// ModeNewPerRun tears down the session after each execution. Each run
	// starts with a clean context — no conversation history leaks between
	// invocations. Suitable for stateless monitoring / one-shot tasks.
	ModeNewPerRun SessionMode = "new_per_run"

	// ModeReuse keeps the session alive between runs. The AI retains prior
	// conversation context, which is useful for iterative workflows (e.g.
	// a deployment pipeline that remembers the last build status).
	ModeReuse SessionMode = "reuse"
)

// Job represents a single scheduled task. All mutable fields (Enabled,
// LastRun, LastError, NextRun) are owned by the Store — callers must go
// through Store methods to mutate them.
type Job struct {
	ID          string      `json:"id"`
	Project     string      `json:"project"`
	OwnerID     string      `json:"owner_id"`
	ChatID      string      `json:"chat_id"`
	Expr        string      `json:"expr"`
	Prompt      string      `json:"prompt"`
	WorkDir     string      `json:"work_dir"`
	Description string      `json:"description"`
	Enabled     bool        `json:"enabled"`
	Silent      bool        `json:"silent"`
	SessionMode SessionMode `json:"session_mode"`
	TimeoutMins int         `json:"timeout_mins"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	LastRun     *time.Time  `json:"last_run,omitempty"`
	LastError   string      `json:"last_error,omitempty"`
	NextRun     *time.Time  `json:"next_run,omitempty"`
}

const defaultTimeoutMins = 30

// NewJob creates a Job with sensible defaults and a random ID.
func NewJob(project, ownerID, chatID, expr, prompt, workDir, desc string) Job {
	now := time.Now()
	return Job{
		ID:          randomID(),
		Project:     project,
		OwnerID:     ownerID,
		ChatID:      chatID,
		Expr:        expr,
		Prompt:      prompt,
		WorkDir:     workDir,
		Description: desc,
		Enabled:     true,
		SessionMode: ModeNewPerRun,
		TimeoutMins: defaultTimeoutMins,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
