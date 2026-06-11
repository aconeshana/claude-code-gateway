// Package claudefiles reads ~/.claude and project-level Claude Code
// configuration files (agents, slash commands, MCP servers, hooks).
//
// Exposed as a thin scanning library — no opinions on how to render results.
// All scan functions tolerate missing files, malformed entries, and absent
// $HOME by skipping silently rather than erroring.
package claudefiles

import (
	"os"
	"path/filepath"
	"strings"
)

// Source identifies whether a config entry came from the user-global scope
// (~/.claude/...), the active project's checked-in config
// (<project>/.claude/settings.json, agents/, commands/), or a local-only
// override (<project>/.claude/settings.local.json). The local file is
// gitignored by convention and typically carries more sensitive data
// (per-machine API keys, dev-only hooks) — renderers should flag it
// visually so users don't mistakenly post its contents to a chat.
type Source string

const (
	SourceUser    Source = "user"
	SourceProject Source = "project"
	SourceLocal   Source = "local"
)

// homeDir returns $HOME or "" when unset. Callers must guard against the
// empty case — every scan function below silently skips the user scope when
// $HOME is missing rather than failing the whole call.
func homeDir() string {
	return os.Getenv("HOME")
}

// userClaudeDir returns ~/.claude (or "" when $HOME unset).
func userClaudeDir() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// projectClaudeDir returns <workingDir>/.claude (or "" when workingDir empty).
func projectClaudeDir(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	return filepath.Join(workingDir, ".claude")
}

// parseFrontmatter parses a YAML frontmatter block (minus the leading/trailing
// `---` fences). Supports:
//   - inline scalars: `key: value`
//   - block scalar indicators (>, |, >-, |-) for multi-line values
//   - YAML list syntax (`key:` followed by indented `- item` lines) — the
//     list is collapsed into a comma-joined string so downstream renderers
//     can treat list-form and inline-form identically. Anthropic's
//     agent/skill docs show both forms for `tools:`, so we need to accept
//     either without losing the value.
//
// Returns an empty map for empty input. This is a pragmatic subset of YAML —
// it doesn't handle nesting or flow-style mappings. agents/commands
// frontmatter is always flat key:value(:|list), so we don't need a full
// YAML library.
func parseFrontmatter(block string) map[string]string {
	m := map[string]string{}
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Block scalar indicators: read indented continuation lines.
		if val == ">" || val == "|" || val == ">-" || val == "|-" {
			folded := val == ">" || val == ">-"
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				cont := lines[j]
				if strings.TrimSpace(cont) == "" {
					if !folded {
						parts = append(parts, "")
					}
					continue
				}
				if cont[0] != ' ' && cont[0] != '\t' {
					i = j - 1
					break
				}
				parts = append(parts, strings.TrimSpace(cont))
				i = j
			}
			if folded {
				val = strings.Join(parts, " ")
			} else {
				val = strings.Join(parts, "\n")
			}
		} else if val == "" {
			// Empty inline value → check for YAML list continuation:
			//   key:
			//     - item1
			//     - item2
			// Stops at the first non-indented or non-`-` line.
			var items []string
			for j := i + 1; j < len(lines); j++ {
				cont := lines[j]
				trimmed := strings.TrimSpace(cont)
				if trimmed == "" {
					// Blank line ends the list (matches yaml.Unmarshal behavior
					// for our flat-frontmatter case).
					i = j
					break
				}
				if cont[0] != ' ' && cont[0] != '\t' {
					i = j - 1
					break
				}
				if !strings.HasPrefix(trimmed, "-") {
					// Indented but not a list item — bail out, leave value empty.
					i = j - 1
					break
				}
				item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
				// Strip surrounding quotes from list items so `- "foo"` and
				// `- foo` produce the same output.
				if (strings.HasPrefix(item, "\"") && strings.HasSuffix(item, "\"")) ||
					(strings.HasPrefix(item, "'") && strings.HasSuffix(item, "'")) {
					item = item[1 : len(item)-1]
				}
				if item != "" {
					items = append(items, item)
				}
				i = j
			}
			val = strings.Join(items, ", ")
		}

		val = strings.TrimSpace(val)
		if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
			(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
			val = val[1 : len(val)-1]
		}
		m[key] = val
	}
	return m
}

// splitFrontmatterAndBody pulls the YAML frontmatter map (between leading
// `---` fences) and the remaining markdown body out of a file's raw content.
// Returns (nil, content) when no frontmatter is present.
func splitFrontmatterAndBody(raw string) (map[string]string, string) {
	content := strings.TrimSpace(raw)
	if !strings.HasPrefix(content, "---") {
		return nil, content
	}
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, content
	}
	fm := parseFrontmatter(rest[:end])
	body := strings.TrimSpace(rest[end+4:])
	return fm, body
}

// firstNonEmpty returns the first non-empty trimmed string from cands.
func firstNonEmpty(cands ...string) string {
	for _, c := range cands {
		if s := strings.TrimSpace(c); s != "" {
			return s
		}
	}
	return ""
}
