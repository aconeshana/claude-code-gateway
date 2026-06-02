package cron

// Store is the persistence interface for cron jobs. Implementations must
// be safe for concurrent use from the scheduler goroutine and command
// handlers.
type Store interface {
	List() []Job
	Get(id string) (Job, bool)
	Add(j Job) error
	Update(j Job) error
	Remove(id string) error
	SetEnabled(id string, enabled bool) error
	MarkRun(id string, runErr error) error
}
