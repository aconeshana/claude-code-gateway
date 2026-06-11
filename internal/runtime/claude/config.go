// Package claude implements the runtime.Runtime interface for Anthropic's
// claude-code CLI (https://github.com/anthropics/claude-code).
package claude

import "github.com/anthropics/claude-code-gateway/internal/runtime"

// Permission-mode wire values for claude-code. Stored in
// Config.PermissionMode, persisted in gateway_state.json, written in .env.
// One canonical spelling — no legacy aliases.
const (
	PermissionAuto    = "auto"
	PermissionForward = "forward"
	PermissionDefault = "default"
)

// Config carries the claude-code-specific arguments for a single spawn.
// The session manager passes it via runtime.SpawnRequest.Config.
type Config struct {
	// PermissionMode controls how the runtime handles tool permission requests.
	// One of PermissionDefault / PermissionAuto / PermissionForward.
	PermissionMode string

	Model           string
	MaxTurns        int
	IncludePartials bool

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
	ForkSession                     string
	AddDirs                         []string
	Channels                        []string
	IncludeHookEvents               bool
	PluginDir                       string
	NoSessionPersistence            bool
	PermissionModeFlag              string
	AllowDangerouslySkipPermissions bool
}

// RuntimeName satisfies runtime.Config.
func (Config) RuntimeName() string { return "claude-code" }

// WithModel returns a copy of cfg with Model set to model. Used by the
// session manager when handling /model switches.
func (c Config) WithModel(model string) runtime.Config {
	c.Model = model
	return c
}

// WithEffort returns a copy of cfg with Effort set to level. Used by the
// session manager when handling /effort switches. Empty string clears the
// flag (claude-code resolves an absent --effort against settings.json
// effortLevel, which itself falls back to a model-default level).
func (c Config) WithEffort(level string) runtime.Config {
	c.Effort = level
	return c
}

// WithAddDir returns a copy of cfg with dir appended to AddDirs (no-op when
// already present). Used by the session manager when handling /add-dir.
func (c Config) WithAddDir(dir string) runtime.Config {
	for _, d := range c.AddDirs {
		if d == dir {
			return c
		}
	}
	newDirs := make([]string, len(c.AddDirs)+1)
	copy(newDirs, c.AddDirs)
	newDirs[len(c.AddDirs)] = dir
	c.AddDirs = newDirs
	return c
}

// WithPermissionMode returns a copy of cfg with PermissionMode set to mode.
// Used by the session manager when handling /permissions switches.
// Caller is responsible for passing one of the canonical PermissionAuto /
// PermissionForward / PermissionDefault constants — bridge layer normalizes
// user-typed input via bridge.NormalizePermissionMode before reaching here.
func (c Config) WithPermissionMode(mode string) runtime.Config {
	c.PermissionMode = mode
	return c
}
