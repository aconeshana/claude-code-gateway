package claude

import (
	"strconv"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// buildArgs builds the claude-code CLI argument vector for a single spawn.
// This is the single point of contact with claude-code's flag schema; future
// upgrades to the CLI should only touch this function.
func buildArgs(cfg Config, req runtime.SpawnRequest) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
	}

	switch cfg.PermissionMode {
	case PermissionAuto, PermissionForward:
		args = append(args, "--permission-prompt-tool", "stdio")
	}

	if cfg.IncludePartials {
		args = append(args, "--include-partial-messages")
	}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	if cfg.Thinking != "" {
		args = append(args, "--thinking", cfg.Thinking)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(cfg.MaxBudgetUSD, 'f', -1, 64))
	}
	if cfg.TaskBudget > 0 {
		args = append(args, "--task-budget", strconv.FormatFloat(cfg.TaskBudget, 'f', -1, 64))
	}
	if cfg.Agent != "" {
		args = append(args, "--agent", cfg.Agent)
	}
	for _, b := range cfg.Betas {
		args = append(args, "--betas", b)
	}
	if cfg.JSONSchema != "" {
		args = append(args, "--json-schema", cfg.JSONSchema)
	}
	for _, t := range cfg.AllowedTools {
		args = append(args, "--allowedTools", t)
	}
	for _, t := range cfg.DisallowedTools {
		args = append(args, "--disallowedTools", t)
	}
	for _, t := range cfg.Tools {
		args = append(args, "--tools", t)
	}
	if cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", cfg.MCPConfig)
	}
	if cfg.FallbackModel != "" {
		args = append(args, "--fallback-model", cfg.FallbackModel)
	}
	if cfg.SessionID != "" {
		args = append(args, "--session-id", cfg.SessionID)
	}
	if cfg.ForkSession != "" {
		args = append(args, "--fork-session")
	}
	for _, d := range cfg.AddDirs {
		args = append(args, "--add-dir", d)
	}
	for _, c := range cfg.Channels {
		args = append(args, "--channels", c)
	}
	if cfg.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	if cfg.PluginDir != "" {
		args = append(args, "--plugin-dir", cfg.PluginDir)
	}
	if cfg.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}
	if cfg.PermissionModeFlag != "" {
		args = append(args, "--permission-mode", cfg.PermissionModeFlag)
	}
	if cfg.AllowDangerouslySkipPermissions {
		args = append(args, "--allow-dangerously-skip-permissions")
	}

	return args
}
