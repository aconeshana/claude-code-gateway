// Package persist provides persistent storage for session.Manager state.
// The JSONStore implementation writes a JSON file and supports legacy
// (dormant_sessions / archived bool) field migration.
package persist

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// PersistentState is the on-disk schema written by JSONStore.Save.
type PersistentState struct {
	Users map[string]*PersistentUser `json:"users"`

	// ExternalSummaries persists AI-generated summaries for sessions that
	// have no owner (Origin="external"). Keyed by CLISessionID. Without this,
	// every gateway restart would re-run the summary worker for the full
	// disk-discovered set.
	ExternalSummaries map[string]ExternalAugmentation `json:"external_summaries,omitempty"`
}

// ExternalAugmentation is re-exported from session so callers can use either
// package's type interchangeably. persist owns the on-disk format; session
// owns the API contract.
type ExternalAugmentation = session.ExternalAugmentation

type PersistentUser struct {
	ActiveLabel  string              `json:"active_label"`
	FocusedCLIID string              `json:"focused_cli_id,omitempty"`
	Sessions     []PersistentSession `json:"sessions"`

	// legacy field for backward-compatible reading
	DormantSessions []PersistentSession `json:"dormant_sessions,omitempty"`
}

type PersistentSession struct {
	CLISessionID      string `json:"cli_session_id"`
	Label             string `json:"label"`
	Summary           string `json:"summary,omitempty"`
	CustomTitle       string `json:"custom_title,omitempty"`
	LatestUserMessage string `json:"latest_user_message,omitempty"`
	Origin            string `json:"origin,omitempty"`
	ChannelKind       string `json:"channel_kind,omitempty"`
	ThreadID          string   `json:"thread_id,omitempty"`
	RootMessageID     string   `json:"root_message_id,omitempty"`
	ExtraAddDirs      []string `json:"extra_add_dirs,omitempty"`
	WorkingDir        string   `json:"working_dir"`
	ChatID            string `json:"chat_id"`
	Status            string `json:"status,omitempty"`
	JSONLPath         string `json:"jsonl_path,omitempty"`
	Archived          bool   `json:"archived,omitempty"` // legacy
}

// JSONStore reads and writes session state to a JSON file. It is safe for
// concurrent use; callers should ensure Save/Load do not race against each
// other (a single goroutine driving the lifecycle is fine).
type JSONStore struct {
	path string

	mu                 sync.Mutex
	removedArchivedIDs map[string]bool
	externalAugs       map[string]ExternalAugmentation
}

// NewJSONStore creates a JSONStore that persists to path.
func NewJSONStore(path string) *JSONStore {
	return &JSONStore{
		path:               path,
		removedArchivedIDs: make(map[string]bool),
		externalAugs:       make(map[string]ExternalAugmentation),
	}
}

// ExternalAugmentationFor returns the persisted summary/customTitle for an
// external (unowned) session, if any. Returns ok=false when no augmentation
// was loaded for this CLI session id.
func (s *JSONStore) ExternalAugmentationFor(cliSessionID string) (ExternalAugmentation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.externalAugs[cliSessionID]
	return a, ok
}

// RecordExternalSummary stores an augmentation entry for an unowned external
// session. Worker calls this after generating a fresh summary so future
// discovery scans can skip the work (gated by PromptVersion).
//
// Call Save after recording to persist to disk.
func (s *JSONStore) RecordExternalSummary(cliSessionID string, aug ExternalAugmentation) {
	if cliSessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.externalAugs[cliSessionID] = aug
}

// CountFreshExternalSummaries returns the number of persisted external
// augmentations whose PromptVersion >= minVersion. Used by /status to show
// "how many sessions already have a current-version summary" — disk is the
// source of truth, not the in-memory worker stats counter (which resets
// across gateway restarts).
func (s *JSONStore) CountFreshExternalSummaries(minVersion int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.externalAugs {
		if a.PromptVersion >= minVersion && a.Summary != "" {
			n++
		}
	}
	return n
}

// CountSkippedExternalSummaries returns the number of persisted external
// augmentations whose PromptVersion >= minVersion but Summary is empty
// (i.e. worker processed them but the LLM returned _skip_meta_).
func (s *JSONStore) CountSkippedExternalSummaries(minVersion int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.externalAugs {
		if a.PromptVersion >= minVersion && a.Summary == "" {
			n++
		}
	}
	return n
}

// CountFreshExternalSummariesIn restricts CountFreshExternalSummaries to
// records whose CLI session id is in cliIDs. Used by /status so percentages
// stay bounded by the live external inventory.
func (s *JSONStore) CountFreshExternalSummariesIn(minVersion int, cliIDs map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, a := range s.externalAugs {
		if !cliIDs[id] {
			continue
		}
		if a.PromptVersion >= minVersion && a.Summary != "" {
			n++
		}
	}
	return n
}

// CountSkippedExternalSummariesIn is the scoped form of CountSkippedExternalSummaries.
func (s *JSONStore) CountSkippedExternalSummariesIn(minVersion int, cliIDs map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, a := range s.externalAugs {
		if !cliIDs[id] {
			continue
		}
		if a.PromptVersion >= minVersion && a.Summary == "" {
			n++
		}
	}
	return n
}

// ClearExternalSummary drops the cached augmentation for one external session.
// Used by startup cleanup when we identify a session as gateway-internal noise
// (admin-worker / eval scratch). Caller should Save afterwards.
func (s *JSONStore) ClearExternalSummary(cliSessionID string) {
	if cliSessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.externalAugs, cliSessionID)
}

// PurgeExternalSummaries removes every augmentation matching pred. Returns
// the number of entries removed. Caller should Save to persist. Useful for
// startup cleanup of legacy data (e.g. admin-internal placeholders left
// over from before the detector was added).
func (s *JSONStore) PurgeExternalSummaries(pred func(cliID string, aug ExternalAugmentation) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for k, v := range s.externalAugs {
		if pred(k, v) {
			delete(s.externalAugs, k)
			removed++
		}
	}
	return removed
}

// TrackArchivedRemoval records that the given CLI session id was explicitly
// removed from the archived list. Future Save() calls will not resurrect it
// from a stale file.
func (s *JSONStore) TrackArchivedRemoval(cliSessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removedArchivedIDs[cliSessionID] = true
}

// Path returns the file path being managed.
func (s *JSONStore) Path() string { return s.path }

// Save writes mgr's current state to the JSON file. It merges with the
// existing file (so archived sessions for owners not in memory survive)
// and excludes any CLI session ids tracked via TrackArchivedRemoval.
func (s *JSONStore) Save(mgr *session.Manager) error {
	s.mu.Lock()
	removed := s.removedArchivedIDs
	s.removedArchivedIDs = make(map[string]bool)
	s.mu.Unlock()

	existing := s.readFile()
	current := buildCurrentState(mgr)
	// Inject the worker-managed external augmentations (carrying summary +
	// prompt version). buildCurrentState doesn't touch this map because the
	// PromptVersion lives only in the store's tracked augmentations.
	s.mu.Lock()
	for k, v := range s.externalAugs {
		current.ExternalSummaries[k] = v
	}
	s.mu.Unlock()
	merged := mergeStates(existing, current, removed)

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		s.restoreRemovedIDs(removed)
		return fmt.Errorf("marshal: %w", err)
	}
	if err := atomicWriteFile(s.path, data, 0600); err != nil {
		s.restoreRemovedIDs(removed)
		return fmt.Errorf("write %s: %w", s.path, err)
	}

	totalActive, totalArchived := 0, 0
	for _, u := range merged.Users {
		for _, ps := range u.Sessions {
			if ps.Status == string(session.StatusArchived) {
				totalArchived++
			} else {
				totalActive++
			}
		}
	}
	log.Printf("[persist] saved state: %d users, %d active, %d archived → %s",
		len(merged.Users), totalActive, totalArchived, s.path)
	return nil
}

// Load reads the JSON file and imports each session into mgr. Missing file
// is not an error. Legacy fields (dormant_sessions, archived bool) are
// migrated transparently.
func (s *JSONStore) Load(mgr *session.Manager) error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.path, err)
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}

	// Stash external augmentations so the bridge's discovery can pick them up
	// before the worker re-runs.
	s.mu.Lock()
	s.externalAugs = make(map[string]ExternalAugmentation, len(state.ExternalSummaries))
	for k, v := range state.ExternalSummaries {
		s.externalAugs[k] = v
	}
	s.mu.Unlock()

	for userID, pu := range state.Users {
		if pu.FocusedCLIID != "" {
			mgr.SetResumeHint(userID, pu.FocusedCLIID)
		}

		// legacy dormant_sessions → archived
		for _, ps := range pu.DormantSessions {
			_, _ = mgr.ImportArchivedSession(session.ImportOpts{
				CLISessionID:      ps.CLISessionID,
				OwnerID:           userID,
				Label:             ps.Label,
				Summary:           ps.Summary,
				CustomTitle:       ps.CustomTitle,
				LatestUserMessage: ps.LatestUserMessage,
				Origin:            defaultOrigin(ps.Origin),
				WorkingDir:        ps.WorkingDir,
				ChatID:            ps.ChatID,
				ChannelKind:       defaultChannelKind(ps.ChannelKind),
				ThreadID:          ps.ThreadID,
				RootMessageID:     ps.RootMessageID,
			})
		}

		for _, ps := range pu.Sessions {
			status := ps.Status
			if status == "" {
				if ps.Archived {
					status = string(session.StatusArchived)
				} else {
					status = string(session.StatusActive)
				}
			}
			opts := session.ImportOpts{
				CLISessionID:      ps.CLISessionID,
				OwnerID:           userID,
				Label:             ps.Label,
				Summary:           ps.Summary,
				CustomTitle:       ps.CustomTitle,
				LatestUserMessage: ps.LatestUserMessage,
				Origin:            defaultOrigin(ps.Origin),
				WorkingDir:        ps.WorkingDir,
				ChatID:            ps.ChatID,
				ChannelKind:       defaultChannelKind(ps.ChannelKind),
				ThreadID:          ps.ThreadID,
				RootMessageID:     ps.RootMessageID,
				ExtraAddDirs:      ps.ExtraAddDirs,
			}
			if status == string(session.StatusArchived) {
				_, _ = mgr.ImportArchivedSession(opts)
			} else {
				_, _ = mgr.ImportIdleSession(opts)
			}
		}
	}

	total := 0
	for _, pu := range state.Users {
		total += len(pu.Sessions) + len(pu.DormantSessions)
	}
	if total > 0 {
		log.Printf("[persist] restored %d sessions from %s", total, s.path)
	}
	return nil
}

func (s *JSONStore) restoreRemovedIDs(ids map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range ids {
		s.removedArchivedIDs[id] = true
	}
}

// defaultOrigin returns "feishu" for legacy records that pre-date the Origin
// field — every persisted session before Step 2 was created via the Feishu
// channel.
func defaultOrigin(o string) string {
	if o == "" {
		return "feishu"
	}
	return o
}

// defaultChannelKind returns "feishu" for legacy records that pre-date the
// ChannelKind field — every persisted session before H4-bis was from Feishu.
func defaultChannelKind(k string) string {
	if k == "" {
		return channel.KindFeishu
	}
	return k
}

func (s *JSONStore) readFile() *PersistentState {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return &PersistentState{Users: make(map[string]*PersistentUser)}
	}
	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[persist] WARNING: state file %s has invalid JSON, treating as empty: %v", s.path, err)
		return &PersistentState{Users: make(map[string]*PersistentUser)}
	}
	if state.Users == nil {
		state.Users = make(map[string]*PersistentUser)
	}
	// migrate legacy on read
	for _, pu := range state.Users {
		for _, d := range pu.DormantSessions {
			d.Status = string(session.StatusArchived)
			pu.Sessions = append(pu.Sessions, d)
		}
		pu.DormantSessions = nil
		for i := range pu.Sessions {
			if pu.Sessions[i].Status == "" {
				if pu.Sessions[i].Archived {
					pu.Sessions[i].Status = string(session.StatusArchived)
				} else {
					pu.Sessions[i].Status = string(session.StatusActive)
				}
			}
		}
	}
	return &state
}

func buildCurrentState(mgr *session.Manager) *PersistentState {
	state := &PersistentState{
		Users:             make(map[string]*PersistentUser),
		ExternalSummaries: make(map[string]ExternalAugmentation),
	}

	for _, ownerID := range mgr.AllOwners() {
		if ownerID == "" {
			continue
		}
		infos := mgr.ListBy(session.Filter{OwnerID: ownerID})
		if len(infos) == 0 {
			continue
		}

		pu := &PersistentUser{
			FocusedCLIID: mgr.ResumeHint(ownerID),
		}
		if focused, ok := mgr.FocusedSession(ownerID); ok {
			pu.ActiveLabel = focused.Label
			if focused.CLISessionID != "" {
				pu.FocusedCLIID = focused.CLISessionID
			}
		}

		for _, info := range infos {
			if info.CLISessionID == "" {
				continue
			}
			pu.Sessions = append(pu.Sessions, PersistentSession{
				CLISessionID:      info.CLISessionID,
				Label:             info.Label,
				Summary:           info.Summary,
				CustomTitle:       info.CustomTitle,
				LatestUserMessage: info.LatestUserMessage,
				Origin:            info.Origin,
				ChannelKind:       info.ChannelKind,
				ThreadID:          info.ThreadID,
				RootMessageID:     info.RootMessageID,
				ExtraAddDirs:      info.ExtraAddDirs,
				WorkingDir:        info.WorkingDir,
				ChatID:            info.ChatID,
				Status:            info.Status,
				JSONLPath:         SessionJSONLPath(info.WorkingDir, info.CLISessionID),
			})
		}

		if len(pu.Sessions) > 0 {
			state.Users[ownerID] = pu
		}
	}

	// External (unowned) sessions live in state.ExternalSummaries, but the
	// worker manages those via JSONStore.RecordExternalSummary so we can
	// track PromptVersion alongside the summary. Save() injects them into
	// the projected state before writing to disk.

	return state
}

// MergeStates is the exported version of mergeStates for callers that want
// to reuse the merge logic without the JSONStore wrapper.
func MergeStates(existing, current *PersistentState, removedIDs map[string]bool) *PersistentState {
	return mergeStates(existing, current, removedIDs)
}

// BuildCurrentState is the exported view of the in-memory → persistent
// projection.
func BuildCurrentState(mgr *session.Manager) *PersistentState {
	return buildCurrentState(mgr)
}

// mergeStates combines existing (read from file) and current (built from
// manager) states. Active/idle sessions come from memory; archived sessions
// are unioned with file-side records (deduped by CLISessionID, with file
// values preserved when memory doesn't have them). removedIDs excludes any
// CLI session ids that were explicitly deleted.
func mergeStates(existing, current *PersistentState, removedIDs map[string]bool) *PersistentState {
	result := &PersistentState{
		Users:             make(map[string]*PersistentUser),
		ExternalSummaries: make(map[string]ExternalAugmentation),
	}

	// External summaries: store.externalAugs (delivered via current) is the
	// single source of truth. We do NOT merge with existing — that would
	// resurrect entries cleared by PurgeExternalSummaries / ClearExternalSummary.
	// Save() always loads-then-writes the full store map into current, so
	// nothing is lost by ignoring existing here.
	for k, v := range current.ExternalSummaries {
		result.ExternalSummaries[k] = v
	}

	allUserIDs := make(map[string]bool)
	for id := range existing.Users {
		allUserIDs[id] = true
	}
	for id := range current.Users {
		allUserIDs[id] = true
	}

	for userID := range allUserIDs {
		existUser := existing.Users[userID]
		currUser := current.Users[userID]

		pu := &PersistentUser{}

		activeCLIIDs := make(map[string]bool)
		if currUser != nil {
			pu.ActiveLabel = currUser.ActiveLabel
			pu.FocusedCLIID = currUser.FocusedCLIID
			for _, s := range currUser.Sessions {
				if s.Status != string(session.StatusArchived) {
					pu.Sessions = append(pu.Sessions, s)
					if s.CLISessionID != "" {
						activeCLIIDs[s.CLISessionID] = true
					}
				}
			}
		}
		if pu.FocusedCLIID == "" && existUser != nil {
			pu.FocusedCLIID = existUser.FocusedCLIID
		}

		// archived: merge file + memory, dedupe by CLISessionID
		archivedMap := make(map[string]PersistentSession)
		if existUser != nil {
			for _, s := range existUser.Sessions {
				if s.Status == string(session.StatusArchived) && s.CLISessionID != "" {
					archivedMap[s.CLISessionID] = s
				}
			}
		}
		if currUser != nil {
			for _, s := range currUser.Sessions {
				if s.Status == string(session.StatusArchived) && s.CLISessionID != "" {
					archivedMap[s.CLISessionID] = s
				}
			}
		}

		for id := range removedIDs {
			delete(archivedMap, id)
		}
		for id := range activeCLIIDs {
			delete(archivedMap, id)
		}

		for _, s := range archivedMap {
			if s.JSONLPath == "" {
				s.JSONLPath = SessionJSONLPath(s.WorkingDir, s.CLISessionID)
			}
			pu.Sessions = append(pu.Sessions, s)
		}

		if len(pu.Sessions) > 0 {
			result.Users[userID] = pu
		}
	}

	return result
}

// SessionJSONLPath is the on-disk transcript path for a claude-code session.
// Thin proxy to runtime/claude.SessionJSONLPath — the canonical
// implementation lives there since the encoding is claude-specific.
//
// New code should import internal/runtime/claude directly. This re-export
// exists so persist's own buildCurrentState / mergeStates code can compute
// the field without an import cycle through bridge.
func SessionJSONLPath(workingDir, cliSessionID string) string {
	return claudeRT.SessionJSONLPath(workingDir, cliSessionID)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, path)
}
