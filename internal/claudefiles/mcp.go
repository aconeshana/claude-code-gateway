package claudefiles

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// maxClaudeJSONSize caps how big a ~/.claude.json file we'll read into
// memory. Claude Code stores per-project conversation history in this same
// file under `projects.<dir>.history`, and active users routinely have
// 50-200 MB files. We only care about `mcpServers` + `projects.<wd>.mcpServers`;
// trying to Unmarshal the entire thing every /mcp invocation would spike
// memory by 4-8x file size and add seconds of pause.
//
// 10 MB is well above any realistic mcpServers payload (~200 bytes/server)
// and well below the conversation-history bloat zone.
//
// Exposed as `var` (not `const`) so tests can lower the threshold without
// having to write multi-MB fixtures.
var maxClaudeJSONSize int64 = 10 * 1024 * 1024

// MCPServer represents one MCP (Model Context Protocol) server entry.
//
// Sources scanned:
//  1. `~/.claude.json` top-level `mcpServers` (user scope)
//  2. `<workingDir>/.mcp.json` top-level `mcpServers` (project scope)
//  3. `~/.claude.json` `projects.<workingDir>.mcpServers` (project scope —
//     Claude Code stores per-project MCP overrides here when configured via
//     `claude mcp add` with project-scope flag)
type MCPServer struct {
	Name    string
	Type    string            // "stdio" / "http" / "sse" — Claude Code's transport tag
	Command string            // executable for stdio servers
	Args    []string          // CLI args
	URL     string            // remote URL for http/sse transports
	Env     map[string]string // environment variables passed to the server
	Source  Source
	Path    string // file the entry came from
}

// rawMCPServer mirrors the JSON shape used in both ~/.claude.json and .mcp.json.
type rawMCPServer struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// claudeJSONFile is a partial view of ~/.claude.json — only the fields we
// need to enumerate MCP servers.
type claudeJSONFile struct {
	MCPServers map[string]rawMCPServer            `json:"mcpServers"`
	Projects   map[string]projectClaudeJSONEntry  `json:"projects"`
}

type projectClaudeJSONEntry struct {
	MCPServers map[string]rawMCPServer `json:"mcpServers"`
}

// projectMCPFile is the .mcp.json shape (a single object with `mcpServers`).
type projectMCPFile struct {
	MCPServers map[string]rawMCPServer `json:"mcpServers"`
}

// ListMCPServers returns the merged user + project MCP server set sorted by
// (Source, Name). Project entries override user entries on name collision.
func ListMCPServers(workingDir string) []MCPServer {
	byName := map[string]MCPServer{}

	// 1. ~/.claude.json
	homeFile := claudeJSONPath()
	if homeFile != "" {
		userTop, projectsByDir := readClaudeJSON(homeFile)
		for name, raw := range userTop {
			byName[name] = toMCPServer(name, raw, SourceUser, homeFile)
		}
		// Project-scoped MCP entries written via `claude mcp add --scope project`
		// land in the same ~/.claude.json under `projects.<wd>.mcpServers`.
		if workingDir != "" {
			if entry, ok := projectsByDir[workingDir]; ok {
				for name, raw := range entry.MCPServers {
					byName[name] = toMCPServer(name, raw, SourceProject, homeFile)
				}
			}
		}
	}

	// 2. <workingDir>/.mcp.json — the canonical project-checked-in form.
	if workingDir != "" {
		projFile := filepath.Join(workingDir, ".mcp.json")
		if servers := readMCPJSON(projFile); servers != nil {
			for name, raw := range servers {
				byName[name] = toMCPServer(name, raw, SourceProject, projFile)
			}
		}
	}

	out := make([]MCPServer, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == SourceProject
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// claudeJSONPath returns ~/.claude.json or "" when $HOME is unset.
func claudeJSONPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// readClaudeJSON parses ~/.claude.json and returns (top-level mcpServers,
// projects map). Both default to empty maps on read/parse error so callers
// can range over them safely.
//
// Bounded by maxClaudeJSONSize — when ~/.claude.json grows past that
// threshold (typical for users with long conversation histories), we skip
// the entire user-scope read rather than spike memory. Project-scoped MCP
// entries from <wd>/.mcp.json still work.
func readClaudeJSON(path string) (map[string]rawMCPServer, map[string]projectClaudeJSONEntry) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if info.Size() > maxClaudeJSONSize {
		log.Printf("[claudefiles] skipping %s: size %d bytes exceeds %d", path, info.Size(), maxClaudeJSONSize)
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var f claudeJSONFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil
	}
	return f.MCPServers, f.Projects
}

// readMCPJSON parses a project-level .mcp.json. Returns nil on read/parse
// failure (caller treats nil as "no entries").
func readMCPJSON(path string) map[string]rawMCPServer {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f projectMCPFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return f.MCPServers
}

func toMCPServer(name string, raw rawMCPServer, source Source, path string) MCPServer {
	return MCPServer{
		Name:    name,
		Type:    raw.Type,
		Command: raw.Command,
		Args:    raw.Args,
		URL:     raw.URL,
		Env:     raw.Env,
		Source:  source,
		Path:    path,
	}
}
