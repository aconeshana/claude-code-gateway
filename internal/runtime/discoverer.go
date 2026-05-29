package runtime

import (
	"context"
	"time"
)

// Discoverer enumerates pre-existing sessions belonging to a specific runtime.
// Each runtime stores sessions on disk in its own layout (e.g. claude under
// ~/.claude/projects/<escaped-path>/<uuid>.jsonl). Discoverer abstracts that
// layout so the bridge can index sessions uniformly.
//
// Discoverer is read-only: it never mutates files. The bridge feeds discovered
// records into session.Manager via ImportIdleSession.
type Discoverer interface {
	// Name returns the runtime identifier. Must match the corresponding
	// Runtime.Name() so the bridge can pair them.
	Name() string

	// Scan returns sessions visible on disk under opts.WindowDays.
	// Implementations should cache mtimes between calls so repeat invocations
	// are cheap.
	Scan(ctx context.Context, opts ScanOpts) ([]DiscoveredSession, error)
}

// ScanOpts controls a Discoverer.Scan call.
type ScanOpts struct {
	// WindowDays limits the scan to sessions modified within the last N days.
	// 0 means "no window" (scan everything).
	WindowDays int
}

// DiscoveredSession is the minimal metadata a Discoverer can extract from disk
// without parsing the full session transcript.
type DiscoveredSession struct {
	// RuntimeID is the runtime-internal session identifier (for claude, the
	// jsonl basename without extension). Used as Session.CLISessionID after
	// import.
	RuntimeID string

	// WorkingDir is the absolute path the session was created in. For claude,
	// extracted from the `cwd` field in the transcript (authoritative;
	// avoids the lossy directory-name decoding).
	WorkingDir string

	// LastActivity is the file mtime — last time the runtime wrote to it.
	LastActivity time.Time

	// CustomTitle is the user-assigned name (claude /rename), empty if unset.
	// Takes display precedence over auto-generated summaries.
	CustomTitle string

	// InitialSummary is a quick placeholder summary extracted from the file
	// (typically the last user prompt, truncated). Used until the async
	// summary worker generates a polished one.
	InitialSummary string

	// SourceRef is a runtime-specific pointer the summary worker can use to
	// read more context (for claude: the absolute jsonl path).
	SourceRef string

	// IsAdminInternal flags sessions that the bridge spawned internally
	// (e.g. the summary worker's admin session reading other jsonl files).
	// These are gateway plumbing, not user conversations — discovery
	// consumers should skip them so they don't pollute /list or burn
	// summary API quota.
	IsAdminInternal bool

	// MessageCount is the number of user-authored turns (excluding meta /
	// queue events) extracted from the transcript. 0 means "unknown" —
	// runtimes that can't compute it cheaply may leave it as zero.
	MessageCount int
}
