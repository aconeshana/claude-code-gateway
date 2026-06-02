package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONStore persists jobs to a single JSON file using atomic writes
// (create-temp → write → sync → rename) to prevent corruption on crash.
type JSONStore struct {
	mu   sync.RWMutex
	path string
	jobs map[string]Job
}

// jsonFile is the on-disk representation.
type jsonFile struct {
	Jobs []Job `json:"jobs"`
}

// NewJSONStore creates a store backed by the given file path. If the file
// already exists it is loaded eagerly; a missing file is not an error.
func NewJSONStore(path string) (*JSONStore, error) {
	s := &JSONStore{
		path: path,
		jobs: make(map[string]Job),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("load cron store: %w", err)
	}
	return s, nil
}

func (s *JSONStore) List() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out
}

func (s *JSONStore) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

func (s *JSONStore) Add(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.jobs[j.ID]; dup {
		return fmt.Errorf("job %s already exists", j.ID)
	}
	s.jobs[j.ID] = j
	return s.flush()
}

func (s *JSONStore) Update(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; !ok {
		return fmt.Errorf("job %s not found", j.ID)
	}
	j.UpdatedAt = time.Now()
	s.jobs[j.ID] = j
	return s.flush()
}

func (s *JSONStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job %s not found", id)
	}
	delete(s.jobs, id)
	return s.flush()
}

func (s *JSONStore) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	j.Enabled = enabled
	j.UpdatedAt = time.Now()
	s.jobs[id] = j
	return s.flush()
}

func (s *JSONStore) MarkRun(id string, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	now := time.Now()
	j.LastRun = &now
	if runErr != nil {
		j.LastError = runErr.Error()
	} else {
		j.LastError = ""
	}
	j.UpdatedAt = now
	s.jobs[id] = j
	return s.flush()
}

// --- internal ---

func (s *JSONStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f jsonFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, j := range f.Jobs {
		s.jobs[j.ID] = j
	}
	return nil
}

func (s *JSONStore) flush() error {
	all := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		all = append(all, j)
	}
	data, err := json.MarshalIndent(jsonFile{Jobs: all}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, data, 0600)
}

// atomicWrite writes data to path via a temporary file + rename to avoid
// partial writes on crash. The temporary file is created in the same
// directory so rename(2) is guaranteed to be atomic (same filesystem).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cron-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
