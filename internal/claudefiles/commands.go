package claudefiles

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Command represents one user-defined Claude Code slash command.
//
// Files live under `~/.claude/commands/*.md` (user) or
// `<project>/.claude/commands/*.md`. Subdirectories produce namespaced
// commands: `~/.claude/commands/foo/bar.md` → `/foo:bar`.
//
// Frontmatter is optional. Description falls back to the first heading or
// non-empty body line.
type Command struct {
	// Name without the leading slash. Includes namespace when applicable
	// (e.g. "ccpanes:browse-sessions"). Render as "/<Name>" for display.
	Name        string
	Description string
	Source      Source
	Path        string
}

// ListCommands scans both user and project command directories and returns
// the merged set sorted by (Source, Name). Project entries take precedence
// when names collide. Empty workingDir skips project scope; missing $HOME
// skips user scope.
func ListCommands(workingDir string) []Command {
	byName := map[string]Command{}

	if dir := userClaudeDir(); dir != "" {
		for _, c := range scanCommandTree(filepath.Join(dir, "commands"), "", SourceUser) {
			byName[c.Name] = c
		}
	}
	if dir := projectClaudeDir(workingDir); dir != "" {
		for _, c := range scanCommandTree(filepath.Join(dir, "commands"), "", SourceProject) {
			byName[c.Name] = c
		}
	}

	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == SourceProject
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// scanCommandTree walks the commands directory recursively. namespace is
// the colon-prefixed path segment for nested entries (empty at the root).
func scanCommandTree(dir, namespace string, source Source) []Command {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Command
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			ns := e.Name()
			if namespace != "" {
				ns = namespace + ":" + ns
			}
			out = append(out, scanCommandTree(full, ns, source)...)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".md")
		name := base
		if namespace != "" {
			name = namespace + ":" + base
		}
		c, ok := parseCommandFile(full, name, source)
		if !ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

func parseCommandFile(path, name string, source Source) (Command, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Command{}, false
	}
	fm, body := splitFrontmatterAndBody(string(raw))

	desc := ""
	if fm != nil {
		desc = fm["description"]
	}
	if desc == "" {
		desc = firstHeadingOrLine(body)
	}

	return Command{
		Name:        name,
		Description: desc,
		Source:      source,
		Path:        path,
	}, true
}

// firstHeadingOrLine returns the text of the first markdown heading
// (`# xxx`), or the first non-empty body line, whichever appears earliest.
func firstHeadingOrLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		return trimmed
	}
	return ""
}
