package cron

import (
	"context"
	"log"
	"sync"
	"time"
)

// ResultCallback is invoked after each job execution. Implementations
// typically push the result to the user's IM channel.
type ResultCallback func(j Job, r ExecResult)

// SchedulerStats exposes runtime metrics for the /status endpoint.
type SchedulerStats struct {
	Active    int       `json:"active"`
	Disabled  int       `json:"disabled"`
	Running   int       `json:"running"`
	LastFired time.Time `json:"last_fired,omitempty"`
}

// Scheduler drives job execution according to their cron expressions.
// A single goroutine owns the timer map; external callers signal changes
// via Reload(). Execution is delegated to the pluggable Executor.
type Scheduler struct {
	store    Store
	executor Executor
	runLog   *RunLog
	onResult ResultCallback

	mu      sync.Mutex
	timers  map[string]*time.Timer
	running map[string]bool // jobs currently executing
	cancel  context.CancelFunc
	wake    chan struct{}
	stats   SchedulerStats
}

// NewScheduler creates a scheduler. Call Start to begin processing.
func NewScheduler(store Store, exec Executor, rl *RunLog, cb ResultCallback) *Scheduler {
	return &Scheduler{
		store:    store,
		executor: exec,
		runLog:   rl,
		onResult: cb,
		timers:   make(map[string]*time.Timer),
		running:  make(map[string]bool),
		wake:     make(chan struct{}, 1),
	}
}

// Start launches the scheduling loop. Blocks until ctx is canceled.
func (sc *Scheduler) Start(ctx context.Context) {
	ctx, sc.cancel = context.WithCancel(ctx)
	sc.replan()
	for {
		select {
		case <-ctx.Done():
			sc.stopAllTimers()
			return
		case <-sc.wake:
			sc.replan()
		}
	}
}

// Reload signals the scheduler to re-read the store and recalculate all
// timers. Non-blocking; coalesces if a reload is already pending.
func (sc *Scheduler) Reload() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// Stats returns a snapshot of scheduler metrics.
func (sc *Scheduler) Stats() SchedulerStats {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.stats
}

// replan stops all existing timers and creates new ones based on the
// current store contents.
func (sc *Scheduler) replan() {
	sc.stopAllTimers()
	now := time.Now()
	jobs := sc.store.List()

	sc.mu.Lock()
	active, disabled := 0, 0
	sc.mu.Unlock()

	for _, j := range jobs {
		if !j.Enabled {
			disabled++
			continue
		}
		active++
		sched, err := ParseSchedule(j.Expr)
		if err != nil {
			log.Printf("[cron] invalid expr for job %s (%s): %v", shortID(j.ID), j.Expr, err)
			continue
		}
		next := sched.NextAfter(now)
		if next.IsZero() {
			continue
		}

		// Update the NextRun field on the stored job (best-effort).
		nj := j
		nj.NextRun = &next
		_ = sc.store.Update(nj)

		dur := time.Until(next)
		if dur < 0 {
			dur = 0
		}
		jobCopy := j
		timer := time.AfterFunc(dur, func() {
			sc.fire(jobCopy)
		})
		sc.mu.Lock()
		sc.timers[j.ID] = timer
		sc.mu.Unlock()
	}

	sc.mu.Lock()
	sc.stats.Active = active
	sc.stats.Disabled = disabled
	sc.mu.Unlock()
}

// fire executes a single job, records the result, and reschedules.
func (sc *Scheduler) fire(j Job) {
	sc.mu.Lock()
	if sc.running[j.ID] {
		sc.mu.Unlock()
		log.Printf("[cron] job %s still running, skipping overlap", shortID(j.ID))
		return
	}
	sc.running[j.ID] = true
	sc.stats.Running++
	sc.stats.LastFired = time.Now()
	sc.mu.Unlock()

	timeout := time.Duration(j.TimeoutMins) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	log.Printf("[cron] firing job %s (%s)", shortID(j.ID), j.Description)
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	result := sc.executor.Execute(ctx, ExecRequest{Job: j, Timeout: timeout})
	cancel()
	result.Duration = time.Since(start)

	// Record the run.
	status := "ok"
	errMsg := ""
	if result.Err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		} else {
			status = "error"
		}
		errMsg = result.Err.Error()
	}
	_ = sc.store.MarkRun(j.ID, result.Err)

	if sc.runLog != nil {
		sc.runLog.Append(RunRecord{
			JobID:     j.ID,
			StartedAt: start,
			DurationS: result.Duration.Seconds(),
			Status:    status,
			Error:     errMsg,
			Summary:   result.Summary,
		})
	}

	sc.mu.Lock()
	delete(sc.running, j.ID)
	sc.stats.Running--
	sc.mu.Unlock()

	if sc.onResult != nil {
		sc.onResult(j, result)
	}

	log.Printf("[cron] job %s finished in %v (status=%s)", shortID(j.ID), result.Duration, status)

	// Schedule next run.
	sched, err := ParseSchedule(j.Expr)
	if err != nil {
		return
	}
	next := sched.NextAfter(time.Now())
	if next.IsZero() {
		return
	}

	nj := j
	nj.NextRun = &next
	_ = sc.store.Update(nj)

	dur := time.Until(next)
	if dur < 0 {
		dur = 0
	}
	timer := time.AfterFunc(dur, func() {
		sc.fire(j)
	})
	sc.mu.Lock()
	sc.timers[j.ID] = timer
	sc.mu.Unlock()
}

func (sc *Scheduler) stopAllTimers() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for id, t := range sc.timers {
		t.Stop()
		delete(sc.timers, id)
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
