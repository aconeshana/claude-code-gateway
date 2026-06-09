package claudefiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Hook represents one configured Claude Code hook.
//
// Source files (in order):
//  1. `~/.claude/settings.json` (user)
//  2. `<workingDir>/.claude/settings.json` (project)
//  3. `<workingDir>/.claude/settings.local.json` (project, gitignored)
//
// Schema flattening: settings.json nests `hooks.<EventName>[].hooks[]` —
// we expose each leaf as a single Hook entry tagged with the parent event
// and matcher so the renderer can flatten the tree into a flat list.
type Hook struct {
	Event   string // "PreToolUse" / "PostToolUse" / "Stop" / "Notification" / "UserPromptSubmit" / ...
	Matcher string // tool/event pattern; empty means "match all"
	Type    string // "command" / "http"
	Command string // shell command for type=command
	URL     string // remote URL for type=http
	Timeout int    // seconds; 0 = framework default
	Async   bool
	Source  Source
	Path    string
}

// rawSettingsFile is a partial view of settings.json — only the hooks block
// matters for our purposes.
type rawSettingsFile struct {
	Hooks map[string][]rawHookGroup `json:"hooks"`
}

type rawHookGroup struct {
	Matcher string         `json:"matcher,omitempty"`
	Hooks   []rawHookEntry `json:"hooks"`
}

type rawHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

// ListHooks scans user and project settings files and returns a flat list
// sorted by (Source, Event, Matcher). Missing files are skipped.
//
// settings.local.json is gitignored by convention and uses SourceLocal so
// the renderer can flag it visually — its hooks tend to carry per-machine
// secrets the user wouldn't want broadcast.
func ListHooks(workingDir string) []Hook {
	var out []Hook

	if dir := userClaudeDir(); dir != "" {
		out = append(out, scanSettingsFile(filepath.Join(dir, "settings.json"), SourceUser)...)
	}
	if dir := projectClaudeDir(workingDir); dir != "" {
		out = append(out, scanSettingsFile(filepath.Join(dir, "settings.json"), SourceProject)...)
		out = append(out, scanSettingsFile(filepath.Join(dir, "settings.local.json"), SourceLocal)...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return sourceRank(out[i].Source) < sourceRank(out[j].Source)
		}
		if out[i].Event != out[j].Event {
			return out[i].Event < out[j].Event
		}
		return out[i].Matcher < out[j].Matcher
	})
	return out
}

// sourceRank orders sources for list display: local first (most relevant +
// most sensitive), then project, then user. Matches the precedence Claude
// Code itself applies at runtime — local overrides project overrides user.
func sourceRank(s Source) int {
	switch s {
	case SourceLocal:
		return 0
	case SourceProject:
		return 1
	default:
		return 2
	}
}

func scanSettingsFile(path string, source Source) []Hook {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f rawSettingsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	var out []Hook
	for event, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out = append(out, Hook{
					Event:   event,
					Matcher: g.Matcher,
					Type:    h.Type,
					Command: h.Command,
					URL:     h.URL,
					Timeout: h.Timeout,
					Async:   h.Async,
					Source:  source,
					Path:    path,
				})
			}
		}
	}
	return out
}
