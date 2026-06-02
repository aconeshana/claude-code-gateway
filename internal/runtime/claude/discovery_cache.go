package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// cacheSchemaVersion is bumped any time we add/rename a field on
// DiscoveredSession that affects classification. On load, entries with an
// older version are silently dropped so the next Scan re-parses with the
// current logic (e.g. v2 added IsAdminInternal — without invalidation, old
// cached entries report IsAdminInternal=false and pollute /list).
//
// v1: initial schema (RuntimeID, WorkingDir, LastActivity, CustomTitle,
//
//	InitialSummary, SourceRef)
//
// v2: added IsAdminInternal
// v3: expanded adminPromptFingerprints (legacy admin worker prompts)
const cacheSchemaVersion = 3

type cachePayload struct {
	Version int                   `json:"version"`
	Entries map[string]cacheEntry `json:"entries"`
}

// MTimeCache memoizes parsed DiscoveredSession records keyed by jsonl path.
// A cache hit skips file I/O entirely when the mtime hasn't changed since the
// last parse.
//
// The cache is persisted to disk between runs (typically
// ~/.ccg/.gateway-discovery-cache.json) so gateway restarts don't pay the
// full scan cost. Wrapped with cacheSchemaVersion so adding fields to
// DiscoveredSession safely invalidates stale entries.
type MTimeCache struct {
	path string

	mu      sync.Mutex
	entries map[string]cacheEntry
	dirty   bool
}

type cacheEntry struct {
	MTimeUnixNano int64                     `json:"mtime"`
	Record        runtime.DiscoveredSession `json:"record"`
}

func NewMTimeCache(path string) *MTimeCache {
	return &MTimeCache{
		path:    path,
		entries: make(map[string]cacheEntry),
	}
}

// Load reads the on-disk cache (if present). Missing, corrupt, or stale-
// schema files are silently treated as empty — Scan will repopulate.
func (c *MTimeCache) Load() error {
	if c.path == "" {
		return nil
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload cachePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		// corrupt cache: discard
		return nil
	}
	if payload.Version != cacheSchemaVersion {
		// schema migrated — drop everything so Scan re-parses with current
		// extractors (notably, adds IsAdminInternal to legacy entries).
		return nil
	}
	c.mu.Lock()
	c.entries = payload.Entries
	if c.entries == nil {
		c.entries = make(map[string]cacheEntry)
	}
	c.dirty = false
	c.mu.Unlock()
	return nil
}

// Save writes the cache to disk. No-op if no changes were made since the last
// Load/Save.
func (c *MTimeCache) Save() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]cacheEntry, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.dirty = false
	c.mu.Unlock()

	payload := cachePayload{Version: cacheSchemaVersion, Entries: snapshot}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}
	return os.WriteFile(c.path, data, 0600)
}

// Get returns a cached record if the file's mtime matches the cached one.
func (c *MTimeCache) Get(path string, mtime time.Time) (runtime.DiscoveredSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[path]
	if !ok {
		return runtime.DiscoveredSession{}, false
	}
	if e.MTimeUnixNano != mtime.UnixNano() {
		return runtime.DiscoveredSession{}, false
	}
	return e.Record, true
}

// Put stores a parsed record for `path` with its observed mtime.
func (c *MTimeCache) Put(path string, mtime time.Time, rec runtime.DiscoveredSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[path] = cacheEntry{
		MTimeUnixNano: mtime.UnixNano(),
		Record:        rec,
	}
	c.dirty = true
}
