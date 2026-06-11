package claudefiles

import (
	"path/filepath"
	"testing"
)

func TestListHooks_FlattensNestedSchema(t *testing.T) {
	withFakeHome(t, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{
            "hooks": {
                "PostToolUse": [
                    {
                        "matcher": "Edit|Write",
                        "hooks": [
                            {"type": "command", "command": "/path/to/format.sh", "timeout": 5},
                            {"type": "http", "url": "http://logger"}
                        ]
                    }
                ],
                "Notification": [
                    {"hooks": [{"type": "http", "url": "http://notifier"}]}
                ]
            }
        }`)

		project := t.TempDir()
		writeFile(t, filepath.Join(project, ".claude", "settings.json"), `{
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "Bash",
                        "hooks": [{"type": "command", "command": "echo pre", "async": true}]
                    }
                ]
            }
        }`)

		got := ListHooks(project)
		// 2 (PostToolUse from user) + 1 (Notification from user) + 1 (PreToolUse from project) = 4.
		if len(got) != 4 {
			t.Fatalf("expected 4 flattened hooks, got %d: %+v", len(got), got)
		}

		// Project entries sort before user entries.
		if got[0].Source != SourceProject || got[0].Event != "PreToolUse" {
			t.Errorf("project hook should be first, got %+v", got[0])
		}
		if !got[0].Async {
			t.Errorf("async flag lost: %+v", got[0])
		}
	})
}

func TestListHooks_EmptyMatcherTreatedAsMatchAll(t *testing.T) {
	withFakeHome(t, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{
            "hooks": {
                "Stop": [
                    {"hooks": [{"type": "command", "command": "x"}]}
                ]
            }
        }`)
		got := ListHooks("")
		if len(got) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(got))
		}
		if got[0].Matcher != "" {
			t.Errorf("expected empty matcher, got %q", got[0].Matcher)
		}
	})
}

// settings.local.json (gitignored) should produce SourceLocal, not
// SourceProject — the renderer relies on this to flag potentially
// secret-bearing local-only entries with a different visual.
func TestListHooks_LocalSettingsFileGetsSourceLocal(t *testing.T) {
	withFakeHome(t, func(home string) {
		_ = home
		project := t.TempDir()
		writeFile(t, filepath.Join(project, ".claude", "settings.json"), `{
            "hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "shared"}]}]}
        }`)
		writeFile(t, filepath.Join(project, ".claude", "settings.local.json"), `{
            "hooks": {"PostToolUse": [{"hooks": [{"type": "command", "command": "local-only"}]}]}
        }`)
		got := ListHooks(project)
		if len(got) != 2 {
			t.Fatalf("expected 2 hooks, got %d: %+v", len(got), got)
		}
		// Local entries sort first.
		if got[0].Source != SourceLocal || got[0].Event != "PostToolUse" {
			t.Errorf("expected local PostToolUse first, got %+v", got[0])
		}
		if got[1].Source != SourceProject || got[1].Event != "PreToolUse" {
			t.Errorf("expected project PreToolUse second, got %+v", got[1])
		}
	})
}
