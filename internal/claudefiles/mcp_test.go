package claudefiles

import (
	"path/filepath"
	"testing"
)

func TestListMCPServers_UserAndProjectAndOverride(t *testing.T) {
	withFakeHome(t, func(home string) {
		project := t.TempDir()
		writeFile(t, filepath.Join(home, ".claude.json"), `{
            "mcpServers": {
                "shared": {"type": "stdio", "command": "user-bin"},
                "user-only": {"type": "stdio", "command": "u"}
            },
            "projects": {
                "`+project+`": {
                    "mcpServers": {
                        "from-projects-block": {"type": "stdio", "command": "p1"}
                    }
                }
            }
        }`)
		writeFile(t, filepath.Join(project, ".mcp.json"), `{
            "mcpServers": {
                "shared": {"type": "stdio", "command": "project-bin"},
                "from-mcp-json": {"type": "http", "url": "http://x"}
            }
        }`)

		got := ListMCPServers(project)
		// Expected: user-only (user), shared (project override), from-projects-block (project), from-mcp-json (project).
		if len(got) != 4 {
			t.Fatalf("expected 4 servers, got %d: %+v", len(got), got)
		}
		var byName = make(map[string]MCPServer, len(got))
		for _, s := range got {
			byName[s.Name] = s
		}
		if byName["shared"].Command != "project-bin" || byName["shared"].Source != SourceProject {
			t.Errorf("shared override failed: %+v", byName["shared"])
		}
		if byName["from-mcp-json"].Type != "http" || byName["from-mcp-json"].URL != "http://x" {
			t.Errorf("http server fields wrong: %+v", byName["from-mcp-json"])
		}
		if byName["user-only"].Source != SourceUser {
			t.Errorf("user-only should stay user-scoped")
		}
	})
}

func TestListMCPServers_NoFiles(t *testing.T) {
	withFakeHome(t, func(home string) {
		_ = home
		if got := ListMCPServers(""); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})
}

// Ensures the size guard kicks in: a ~/.claude.json over the threshold is
// skipped entirely (user-scope returns empty) while project-scope .mcp.json
// still flows through. Without this guard, users with multi-hundred-MB
// conversation history files would OOM /mcp invocations.
func TestListMCPServers_SkipsOversizeUserJSON(t *testing.T) {
	withFakeHome(t, func(home string) {
		// Lower the threshold so we don't have to write multi-MB fixtures.
		old := maxClaudeJSONSize
		maxClaudeJSONSize = 64
		t.Cleanup(func() { maxClaudeJSONSize = old })

		project := t.TempDir()
		// A valid JSON > 64 bytes that lists a user MCP server. Should be
		// skipped because it's over the threshold.
		writeFile(t, filepath.Join(home, ".claude.json"), `{
            "mcpServers": {
                "user-scoped-but-oversize": {"type": "stdio", "command": "x"}
            }
        }`)
		// Project file stays small and must still be read.
		writeFile(t, filepath.Join(project, ".mcp.json"), `{
            "mcpServers": {"proj": {"type": "stdio", "command": "p"}}
        }`)

		got := ListMCPServers(project)
		if len(got) != 1 {
			t.Fatalf("expected only project entry (user skipped due to size), got %d: %+v", len(got), got)
		}
		if got[0].Name != "proj" {
			t.Errorf("expected project entry, got %+v", got[0])
		}
	})
}
