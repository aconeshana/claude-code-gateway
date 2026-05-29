package session

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"

	"sync"
	"time"
)

type CreateOpts struct {
	WorkingDir      string
	PermissionMode  string
	Model           string
	MaxTurns        int
	IncludePartials bool
	EnvVars         map[string]string

	// User metadata (optional; populated by IM bridges).
	OwnerID     string
	Label       string
	ChatID      string
	ChannelKind string
	Origin      string // "feishu" / "ws" / "external"; default "" treated as ws

	// RuntimeConfig, when non-nil, supersedes the claude-specific fields
	// below. Callers (typically gateway/handler.go via runtime.Factory) set
	// this when they have already parsed an opaque runtime payload.
	RuntimeConfig runtime.Config

	Thinking                        string
	Effort                          string
	MaxBudgetUSD                    float64
	TaskBudget                      float64
	Agent                           string
	Betas                           []string
	JSONSchema                      string
	AllowedTools                    []string
	DisallowedTools                 []string
	Tools                           []string
	MCPConfig                       string
	FallbackModel                   string
	SessionID                       string
	// ResumeID, when set, passes --resume <id> to the CLI at spawn time.
	// Used with ForkSession to create a branch of an existing session.
	ResumeID    string
	ForkSession string
	AddDirs                         []string
	Channels                        []string
	IncludeHookEvents               bool
	PluginDir                       string
	NoSessionPersistence            bool
	PermissionModeFlag              string
	AllowDangerouslySkipPermissions bool
}

type ResumeOpts struct {
	CLISessionID string
	WorkingDir   string
	OwnerID      string
	Label        string
	Summary      string
	ChatID       string
	ChannelKind  string
	Origin       string
}

// Filter selects sessions for List queries. Zero-value Filter returns all
// sessions across all owners (matches the old List() behavior).
type Filter struct {
	OwnerID  string   // if non-empty, only sessions for this owner
	Statuses []Status // if non-empty, only sessions with one of these statuses
	Origins  []string // if non-empty, only sessions whose Origin matches one of these
}

func (f Filter) matches(s *Session) bool {
	if f.OwnerID != "" && s.OwnerID != f.OwnerID {
		return false
	}
	if len(f.Statuses) > 0 {
		match := false
		for _, st := range f.Statuses {
			if s.Status == st {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(f.Origins) > 0 {
		match := false
		for _, o := range f.Origins {
			if s.Origin == o {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// ExternalAugmentation is the gateway-augmentation metadata stored alongside
// an external (unowned) session — typically AI-generated summary + the
// PromptVersion that produced it. JSON tags are here so persist.JSONStore
// can serialize the same type without a separate wire shape.
type ExternalAugmentation struct {
	Summary       string `json:"summary,omitempty"`
	CustomTitle   string `json:"custom_title,omitempty"`
	PromptVersion int    `json:"prompt_version,omitempty"`
}

// SummaryStore is the persistence contract the manager uses to read/write
// augmentation data for unowned sessions. Defined here so callers (worker,
// status, cleanup) don't reach across to internal/session/persist directly —
// they go through Manager.SetExternalSummary / ExternalSummary / etc.
//
// Implemented by persist.JSONStore.
type SummaryStore interface {
	ExternalAugmentationFor(cliSessionID string) (ExternalAugmentation, bool)
	RecordExternalSummary(cliSessionID string, aug ExternalAugmentation)
	CountFreshExternalSummaries(minVersion int) int
	CountSkippedExternalSummaries(minVersion int) int
	PurgeExternalSummaries(pred func(cliID string, aug ExternalAugmentation) bool) int
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	idx      *UserIndex

	rt                runtime.Runtime
	defaultWorkingDir string
	defaultPermMode   string
	maxSessions       int
	keepAliveInterval time.Duration
	idleTimeout       time.Duration
	allowedBaseDirs   []string
	cancelCleanup     context.CancelFunc

	// summaryStore, when set via SetSummaryStore, lets ExternalSummary*
	// methods on the manager delegate to the persist layer without leaking
	// it across the manager boundary.
	summaryStore SummaryStore
}

// NewManager constructs a Manager that uses rt to spawn runtime instances.
func NewManager(rt runtime.Runtime, defaultWorkingDir, defaultPermMode string, maxSessions int, keepAliveInterval, idleTimeout time.Duration) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	absDefault, _ := filepath.Abs(defaultWorkingDir)

	m := &Manager{
		sessions:          make(map[string]*Session),
		idx:               newUserIndex(),
		rt:                rt,
		defaultWorkingDir: defaultWorkingDir,
		defaultPermMode:   defaultPermMode,
		maxSessions:       maxSessions,
		keepAliveInterval: keepAliveInterval,
		idleTimeout:       idleTimeout,
		allowedBaseDirs:   []string{absDefault},
		cancelCleanup:     cancel,
	}
	go m.cleanupLoop(ctx)
	return m
}

func (m *Manager) AddAllowedBaseDir(dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.allowedBaseDirs {
		if existing == abs {
			return
		}
	}
	m.allowedBaseDirs = append(m.allowedBaseDirs, abs)
}

// SetDefaultPermissionMode updates the permission mode used for newly created
// sessions. Existing sessions are unaffected.
func (m *Manager) SetDefaultPermissionMode(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultPermMode = mode
}

// SetDefaultWorkingDir updates the working directory used for newly created
// sessions when CreateOpts.WorkingDir is empty. Existing sessions keep their
// own WorkingDir. Used by the bridge's /config hot-reload of
// GATEWAY_DEFAULT_CWD so newly typed messages immediately route to the
// configured project without a process restart.
func (m *Manager) SetDefaultWorkingDir(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultWorkingDir = abs
}

// SetSummaryStore wires up the persistence backend for external-session
// augmentation. Optional — without it, ExternalSummary* methods are no-ops.
func (m *Manager) SetSummaryStore(s SummaryStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaryStore = s
}

// ExternalSummary returns the persisted augmentation for an unowned external
// session, if any.
func (m *Manager) ExternalSummary(cliSessionID string) (ExternalAugmentation, bool) {
	m.mu.RLock()
	store := m.summaryStore
	m.mu.RUnlock()
	if store == nil {
		return ExternalAugmentation{}, false
	}
	return store.ExternalAugmentationFor(cliSessionID)
}

// SetExternalSummary persists an augmentation for an unowned external
// session. Workers call this after generating a fresh AI summary so
// discovery can skip the work on the next pass.
func (m *Manager) SetExternalSummary(cliSessionID string, aug ExternalAugmentation) {
	m.mu.RLock()
	store := m.summaryStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	store.RecordExternalSummary(cliSessionID, aug)
}

// CountFreshExternalSummaries returns how many persisted external
// augmentations are at PromptVersion >= minVersion. Used by /status.
func (m *Manager) CountFreshExternalSummaries(minVersion int) int {
	m.mu.RLock()
	store := m.summaryStore
	m.mu.RUnlock()
	if store == nil {
		return 0
	}
	return store.CountFreshExternalSummaries(minVersion)
}

// CountSkippedExternalSummaries returns how many persisted external
// augmentations are at PromptVersion >= minVersion but have an empty Summary
// (i.e. processed but the LLM returned _skip_meta_). Used by /status.
func (m *Manager) CountSkippedExternalSummaries(minVersion int) int {
	m.mu.RLock()
	store := m.summaryStore
	m.mu.RUnlock()
	if store == nil {
		return 0
	}
	return store.CountSkippedExternalSummaries(minVersion)
}

// PurgeExternalSummaries clears persisted augmentations matching pred.
// Used by startup cleanup helpers. Returns the number removed.
func (m *Manager) PurgeExternalSummaries(pred func(cliID string, aug ExternalAugmentation) bool) int {
	m.mu.RLock()
	store := m.summaryStore
	m.mu.RUnlock()
	if store == nil {
		return 0
	}
	return store.PurgeExternalSummaries(pred)
}

// GetByCLISessionID returns the session whose runtime-internal ID matches.
// Used by Discovery to dedup against already-imported sessions.
func (m *Manager) GetByCLISessionID(cliID string) (*Session, bool) {
	if cliID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s != nil && s.CLISessionID == cliID {
			return s, true
		}
	}
	return nil, false
}

func (m *Manager) validateWorkingDir(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	return absDir, nil
}

// countActiveLocked returns the number of user-visible sessions currently
// holding a runtime process. Admin (Origin=admin) sessions don't count —
// they're gateway plumbing and shouldn't squeeze out user-initiated Create
// calls. Caller must hold m.mu.
func (m *Manager) countActiveLocked() int {
	n := 0
	for _, s := range m.sessions {
		if s != nil && s.Status == StatusActive && s.Origin != OriginAdmin {
			n++
		}
	}
	return n
}

func (m *Manager) Create(ctx context.Context, opts CreateOpts) (*Session, error) {
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = m.defaultWorkingDir
	}
	absDir, err := m.validateWorkingDir(workingDir)
	if err != nil {
		return nil, err
	}

	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = m.defaultPermMode
	}

	envSlice := envSliceFromMap(opts.EnvVars)

	// Reserve a slot with a placeholder to prevent TOCTOU race
	m.mu.Lock()
	if m.countActiveLocked() >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("max sessions (%d) reached", m.maxSessions)
	}
	placeholderID := "placeholder-" + fmt.Sprint(time.Now().UnixNano())
	m.sessions[placeholderID] = nil
	m.mu.Unlock()

	cfg := opts.RuntimeConfig
	if cfg == nil {
		cfg = claudeConfigFromOpts(opts, permMode)
	}
	req := runtime.SpawnRequest{
		WorkingDir: absDir,
		Env:        envSlice,
		Config:     cfg,
		ResumeID:   opts.ResumeID,
	}
	sess, spawnErr := NewSession(m.rt, req, permMode, m.keepAliveInterval)

	m.mu.Lock()
	delete(m.sessions, placeholderID)
	if spawnErr != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("spawn runtime: %w", spawnErr)
	}
	sess.OwnerID = opts.OwnerID
	sess.Label = opts.Label
	sess.ChatID = opts.ChatID
	sess.ChannelKind = opts.ChannelKind
	sess.Origin = opts.Origin
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	if opts.OwnerID != "" {
		m.idx.addSession(opts.OwnerID, sess.ID)
		m.idx.setFocus(opts.OwnerID, sess.ID)
	}

	log.Printf("[manager] created session %s (owner=%s, dir=%s, perm=%s, runtime=%s)", sess.ID, opts.OwnerID, absDir, permMode, m.rt.Name())
	return sess, nil
}

func (m *Manager) Resume(ctx context.Context, opts ResumeOpts) (*Session, error) {
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = m.defaultWorkingDir
	}
	absDir, err := m.validateWorkingDir(workingDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.countActiveLocked() >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("max sessions (%d) reached", m.maxSessions)
	}
	placeholderID := "placeholder-" + fmt.Sprint(time.Now().UnixNano())
	m.sessions[placeholderID] = nil
	m.mu.Unlock()

	req := runtime.SpawnRequest{
		WorkingDir: absDir,
		Config:     claude.Config{PermissionMode: m.defaultPermMode},
		ResumeID:   opts.CLISessionID,
	}
	sess, spawnErr := NewSession(m.rt, req, m.defaultPermMode, m.keepAliveInterval)

	m.mu.Lock()
	delete(m.sessions, placeholderID)
	if spawnErr != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("spawn runtime for resume: %w", spawnErr)
	}
	sess.OwnerID = opts.OwnerID
	sess.Label = opts.Label
	sess.Summary = opts.Summary
	sess.ChatID = opts.ChatID
	sess.ChannelKind = opts.ChannelKind
	sess.Origin = opts.Origin
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	if opts.OwnerID != "" {
		m.idx.addSession(opts.OwnerID, sess.ID)
		m.idx.setFocus(opts.OwnerID, sess.ID)
		m.idx.setResumeHint(opts.OwnerID, opts.CLISessionID)
	}

	log.Printf("[manager] resumed session %s (owner=%s, runtime_session=%s)", sess.ID, opts.OwnerID, opts.CLISessionID)
	return sess, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	if ok && sess == nil {
		return nil, false
	}
	return sess, ok
}

func (m *Manager) Destroy(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok || sess == nil {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	delete(m.sessions, id)
	owner := sess.OwnerID
	m.mu.Unlock()

	if owner != "" {
		m.idx.removeSession(owner, id)
	}

	log.Printf("[manager] destroying session %s", id)
	return sess.Close()
}

// Terminate stops the underlying CLI process but keeps the session record.
// The bridge's read loop will see the channel close and transition the
// session to idle (so a follow-up message will reactivate it via --resume).
//
// Use this for /terminate-style operations where the user wants to free up
// resources without losing the conversation. Use Destroy when you want to
// remove the session entirely.
func (m *Manager) Terminate(id string) error {
	sess, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if sess.CurrentState() == StateStopped {
		return nil
	}
	log.Printf("[manager] terminating session %s (CLI process)", id)
	return sess.Close()
}

func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	infos := make([]SessionInfo, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if sess != nil {
			infos = append(infos, sess.Info())
		}
	}
	return infos
}

func (m *Manager) Shutdown(ctx context.Context) {
	m.cancelCleanup()

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	log.Printf("[manager] shutting down %d sessions", len(sessions))
	var wg sync.WaitGroup
	for _, sess := range sessions {
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			if err := s.Close(); err != nil {
				log.Printf("[manager] error closing session %s: %v", s.ID, err)
			}
		}(sess)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("[manager] all sessions closed")
	case <-ctx.Done():
		log.Printf("[manager] shutdown timeout, force killing remaining sessions")
		for _, sess := range sessions {
			sess.ForceClose()
		}
	}
}

func (m *Manager) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanupIdleSessions()
		}
	}
}

func (m *Manager) cleanupIdleSessions() {
	// Sessions should only be destroyed by explicit user action.
	// The feishu layer handles dormant conversion when CLI processes exit.
}

func envSliceFromMap(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// claudeConfigFromOpts builds a claude.Config from the manager's CreateOpts.
// This is the single point where the manager knows about claude-specific
// fields; future Phase 5 will replace CreateOpts with an opaque RuntimeConfig.
func claudeConfigFromOpts(opts CreateOpts, permMode string) claude.Config {
	return claude.Config{
		PermissionMode:                  permMode,
		Model:                           opts.Model,
		MaxTurns:                        opts.MaxTurns,
		IncludePartials:                 opts.IncludePartials,
		Thinking:                        opts.Thinking,
		Effort:                          opts.Effort,
		MaxBudgetUSD:                    opts.MaxBudgetUSD,
		TaskBudget:                      opts.TaskBudget,
		Agent:                           opts.Agent,
		Betas:                           opts.Betas,
		JSONSchema:                      opts.JSONSchema,
		AllowedTools:                    opts.AllowedTools,
		DisallowedTools:                 opts.DisallowedTools,
		Tools:                           opts.Tools,
		MCPConfig:                       opts.MCPConfig,
		FallbackModel:                   opts.FallbackModel,
		SessionID:                       opts.SessionID,
		ForkSession:                     opts.ForkSession,
		AddDirs:                         opts.AddDirs,
		Channels:                        opts.Channels,
		IncludeHookEvents:               opts.IncludeHookEvents,
		PluginDir:                       opts.PluginDir,
		NoSessionPersistence:            opts.NoSessionPersistence,
		PermissionModeFlag:              opts.PermissionModeFlag,
		AllowDangerouslySkipPermissions: opts.AllowDangerouslySkipPermissions,
	}
}
