// Package permissionsfile reads and writes the `permissions` block of
// claude-code's settings.json files (allow / deny / ask rules).
//
// claude-code stores tool permission rules across three source files,
// with later sources overriding earlier ones at decision time:
//
//	1. ~/.claude/settings.json              (user)
//	2. <project>/.claude/settings.json      (project — git-tracked)
//	3. <project>/.claude/settings.local.json (local — gitignored)
//
// This package is the single point of read/write contact for those files.
// All writes are atomic (temp file + rename) and preserve any other fields
// that already live in settings.json (model, hooks, env, ...).
//
// Two intentional limitations:
//   - We do not touch policySettings / flagSettings / command-level rules
//     (claude-code treats those as read-only — see permissions.ts L1334-39).
//   - JSON key order in settings.json is not preserved across writes; the
//     Go encoder reorders alphabetically. Acceptable trade-off vs. pulling
//     in a key-order-preserving JSON library.
package permissionsfile

import (
	"os"
	"path/filepath"
)

// Source identifies which settings.json the rule lives in. The value
// strings match claudefiles.Source so renderers can share source-tag
// helpers across packages.
type Source string

const (
	SourceUser    Source = "user"
	SourceProject Source = "project"
	SourceLocal   Source = "local"
)

// Behavior is the rule category — the three top-level arrays inside
// `permissions` in settings.json.
type Behavior string

const (
	BehaviorAllow Behavior = "allow"
	BehaviorDeny  Behavior = "deny"
	BehaviorAsk   Behavior = "ask"
)

// Rule is a single allow/deny/ask entry. Content is the raw rule string
// in claude-code's permission-rule grammar — e.g. "Bash(git push:*)",
// "WebFetch(domain:example.com)", or the bare tool name "WebSearch" to
// allow/deny the entire tool.
//
// The grammar itself is opaque to this package; we read and write strings
// verbatim. claude-code's permissions.ts is the authority on what a valid
// rule string is.
type Rule struct {
	Source   Source
	Behavior Behavior
	Content  string
}

// AllSources is the canonical user/project/local ordering. Renderers
// should display rules in this order so the precedence (later overrides
// earlier) reads top-to-bottom on screen.
var AllSources = []Source{SourceUser, SourceProject, SourceLocal}

// AllBehaviors is the canonical allow/deny/ask ordering. allow first
// because it's the most common, deny second so dangerous rules stand out,
// ask last (rarest).
var AllBehaviors = []Behavior{BehaviorAllow, BehaviorDeny, BehaviorAsk}

// IsValidSource reports whether s is one of the three writable sources.
func IsValidSource(s Source) bool {
	switch s {
	case SourceUser, SourceProject, SourceLocal:
		return true
	}
	return false
}

// IsValidBehavior reports whether b is one of the three rule categories.
func IsValidBehavior(b Behavior) bool {
	switch b {
	case BehaviorAllow, BehaviorDeny, BehaviorAsk:
		return true
	}
	return false
}

// SettingsPath returns the absolute path of the settings.json file that
// owns rules for the given source. Returns "" when the necessary inputs
// aren't available (e.g. $HOME unset for SourceUser, projectDir empty for
// project/local). Callers should treat "" as "this source doesn't apply
// to the current context" rather than an error.
func SettingsPath(src Source, projectDir string) string {
	switch src {
	case SourceUser:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, ".claude", "settings.json")
	case SourceProject:
		if projectDir == "" {
			return ""
		}
		return filepath.Join(projectDir, ".claude", "settings.json")
	case SourceLocal:
		if projectDir == "" {
			return ""
		}
		return filepath.Join(projectDir, ".claude", "settings.local.json")
	}
	return ""
}
