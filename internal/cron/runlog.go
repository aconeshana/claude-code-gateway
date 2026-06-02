package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// RunRecord captures the outcome of a single job execution.
type RunRecord struct {
	JobID     string    `json:"job_id"`
	StartedAt time.Time `json:"started_at"`
	DurationS float64   `json:"duration_s"`
	Status    string    `json:"status"` // "ok", "error", "timeout"
	Error     string    `json:"error,omitempty"`
	Summary   string    `json:"summary,omitempty"`
}

// RunLog maintains a per-job ring buffer of recent execution records with
// periodic persistence to disk.
type RunLog struct {
	mu      sync.Mutex
	records map[string][]RunRecord
	cap     int
	path    string
}

type runLogFile struct {
	Records map[string][]RunRecord `json:"records"`
}

// NewRunLog creates a RunLog that persists to path. cap controls how many
// records to retain per job. Loads existing history from disk if present.
func NewRunLog(path string, cap int) *RunLog {
	if cap <= 0 {
		cap = 20
	}
	rl := &RunLog{
		records: make(map[string][]RunRecord),
		cap:     cap,
		path:    path,
	}
	_ = rl.load()
	return rl
}

// Append adds a record and flushes to disk. Thread-safe.
func (rl *RunLog) Append(r RunRecord) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	buf := rl.records[r.JobID]
	buf = append(buf, r)
	if len(buf) > rl.cap {
		buf = buf[len(buf)-rl.cap:]
	}
	rl.records[r.JobID] = buf
	_ = rl.flush()
}

// History returns the most recent records for a job, newest last.
func (rl *RunLog) History(jobID string) []RunRecord {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	src := rl.records[jobID]
	out := make([]RunRecord, len(src))
	copy(out, src)
	return out
}

// AllHistory returns records for all jobs (for /cron history with no ID).
func (rl *RunLog) AllHistory(limit int) []RunRecord {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	var all []RunRecord
	for _, recs := range rl.records {
		all = append(all, recs...)
	}
	// Sort newest first.
	for i := 0; i < len(all); i++ {
		for k := i + 1; k < len(all); k++ {
			if all[k].StartedAt.After(all[i].StartedAt) {
				all[i], all[k] = all[k], all[i]
			}
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// Purge removes all records for a deleted job.
func (rl *RunLog) Purge(jobID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.records, jobID)
	_ = rl.flush()
}

func (rl *RunLog) load() error {
	data, err := os.ReadFile(rl.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f runLogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", rl.path, err)
	}
	if f.Records != nil {
		rl.records = f.Records
	}
	return nil
}

func (rl *RunLog) flush() error {
	data, err := json.MarshalIndent(runLogFile{Records: rl.records}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(rl.path, data, 0600)
}
