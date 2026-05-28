package session

import "sync"

// UserIndex tracks per-owner views over the manager's sessions: which session
// is currently focused, which CLI session to prefer on auto-resume, and the
// (unordered) set of session IDs that belong to the owner.
//
// All methods are safe for concurrent use.
type UserIndex struct {
	mu      sync.RWMutex
	perUser map[string]*UserView
}

// UserView is a per-owner read snapshot. Returned slices/maps are owned by
// the caller and safe to mutate.
type UserView struct {
	OwnerID      string
	FocusedID    string // session.ID currently in focus, or ""
	ResumeHintID string // CLISessionID of last focused session, used for cross-restart auto-resume
	SessionIDs   []string
}

func newUserIndex() *UserIndex {
	return &UserIndex{perUser: make(map[string]*UserView)}
}

// addSession associates a session.ID with the given owner. Idempotent.
func (idx *UserIndex) addSession(ownerID, sessionID string) {
	if ownerID == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	v := idx.getOrCreate(ownerID)
	for _, id := range v.SessionIDs {
		if id == sessionID {
			return
		}
	}
	v.SessionIDs = append(v.SessionIDs, sessionID)
}

// removeSession drops a session.ID from the owner. If the removed session was
// focused, focus is cleared.
func (idx *UserIndex) removeSession(ownerID, sessionID string) {
	if ownerID == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	v, ok := idx.perUser[ownerID]
	if !ok {
		return
	}
	out := v.SessionIDs[:0]
	for _, id := range v.SessionIDs {
		if id != sessionID {
			out = append(out, id)
		}
	}
	v.SessionIDs = out
	if v.FocusedID == sessionID {
		v.FocusedID = ""
	}
}

// setFocus sets the focused session for the owner. Empty sessionID clears focus.
func (idx *UserIndex) setFocus(ownerID, sessionID string) {
	if ownerID == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	v := idx.getOrCreate(ownerID)
	v.FocusedID = sessionID
}

// setResumeHint records the runtime ID that should be preferred on cross-restart
// auto-resume.
func (idx *UserIndex) setResumeHint(ownerID, cliSessionID string) {
	if ownerID == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	v := idx.getOrCreate(ownerID)
	v.ResumeHintID = cliSessionID
}

// view returns a copy of the per-owner view, or zero-value if no sessions
// have been added.
func (idx *UserIndex) view(ownerID string) UserView {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	v, ok := idx.perUser[ownerID]
	if !ok {
		return UserView{OwnerID: ownerID}
	}
	ids := make([]string, len(v.SessionIDs))
	copy(ids, v.SessionIDs)
	return UserView{
		OwnerID:      v.OwnerID,
		FocusedID:    v.FocusedID,
		ResumeHintID: v.ResumeHintID,
		SessionIDs:   ids,
	}
}

func (idx *UserIndex) getOrCreate(ownerID string) *UserView {
	v, ok := idx.perUser[ownerID]
	if !ok {
		v = &UserView{OwnerID: ownerID}
		idx.perUser[ownerID] = v
	}
	return v
}

// allOwners returns all owner IDs that have ever had a session.
// Returned slice is a copy.
func (idx *UserIndex) allOwners() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]string, 0, len(idx.perUser))
	for k := range idx.perUser {
		out = append(out, k)
	}
	return out
}
