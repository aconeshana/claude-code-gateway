package claudefiles

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Agent represents one Claude Code agent definition discovered on disk.
//
// Agents are markdown files with YAML frontmatter living under
// `~/.claude/agents/*.md` (user scope) or `<project>/.claude/agents/*.md`.
// Frontmatter fields: name, description, tools (comma-separated), model.
type Agent struct {
	Name        string
	Description string
	Tools       string // raw comma-separated list, kept as-is for display
	Model       string // optional model override (e.g. "opus", "sonnet")
	Source      Source // user / project
	Path        string // absolute file path
}

// ListAgents scans ~/.claude/agents/*.md and <workingDir>/.claude/agents/*.md
// and returns the merged result sorted by (Source, Name). Project agents
// override user agents when the names collide — same precedence Claude Code
// applies at runtime.
//
// workingDir may be empty (skips project scope). $HOME unset skips user
// scope. Both empty returns nil. Malformed files are skipped silently.
func ListAgents(workingDir string) []Agent {
	byKey := map[string]Agent{}

	// User scope first; project entries overwrite later.
	if dir := userClaudeDir(); dir != "" {
		for _, a := range scanAgentDir(filepath.Join(dir, "agents"), SourceUser) {
			byKey[a.Name] = a
		}
	}
	if dir := projectClaudeDir(workingDir); dir != "" {
		for _, a := range scanAgentDir(filepath.Join(dir, "agents"), SourceProject) {
			byKey[a.Name] = a
		}
	}

	out := make([]Agent, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			// Project entries first — they're the more relevant scope.
			return out[i].Source == SourceProject
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func scanAgentDir(dir string, source Source) []Agent {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		a, ok := parseAgentFile(path, source)
		if !ok {
			continue
		}
		out = append(out, a)
	}
	return out
}

func parseAgentFile(path string, source Source) (Agent, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Agent{}, false
	}
	fm, _ := splitFrontmatterAndBody(string(raw))
	if fm == nil {
		// Without frontmatter we can't tell what name/description to show.
		return Agent{}, false
	}
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	return Agent{
		Name:        firstNonEmpty(fm["name"], base),
		Description: fm["description"],
		Tools:       fm["tools"],
		Model:       fm["model"],
		Source:      source,
		Path:        path,
	}, true
}
