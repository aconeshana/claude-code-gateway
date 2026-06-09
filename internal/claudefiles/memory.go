package claudefiles

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MemoryFile represents one Claude Code memory file discovered on disk.
// Memory files are CLAUDE.md / CLAUDE.local.md / rules/*.md —
// the same set the CLI loads at session start.
type MemoryFile struct {
	// Label is the short display name shown in the UI.
	Label string
	// Type classifies the memory scope.
	Type MemoryType
	// Path is the absolute file path.
	Path string
	// Content is the raw file text (may be truncated for very large files).
	Content string
}

// MemoryType mirrors the four tiers from the official CLI's claudemd.ts.
type MemoryType string

const (
	MemoryTypeUser    MemoryType = "user"    // ~/.claude/CLAUDE.md + ~/.claude/rules/*.md
	MemoryTypeProject MemoryType = "project" // <wd>/CLAUDE.md, <wd>/.claude/CLAUDE.md
	MemoryTypeRules   MemoryType = "rules"   // <wd>/.claude/rules/*.md
	MemoryTypeLocal   MemoryType = "local"   // <wd>/CLAUDE.local.md
)

const memoryMaxBytes = 32 * 1024 // 32 KiB — truncate very large files for display

// ListMemoryFiles discovers all memory files for the given working directory,
// in the same order the CLI loads them (matching claudemd.ts::getMemoryFiles):
//  1. User memory:       ~/.claude/CLAUDE.md
//  2. User rules:        ~/.claude/rules/*.md  (alphabetical)
//  3. Project memory:    <wd>/CLAUDE.md, <wd>/.claude/CLAUDE.md
//  4. Project rules:     <wd>/.claude/rules/*.md  (alphabetical)
//  5. Local memory:      <wd>/CLAUDE.local.md
//
// Files that do not exist are silently skipped. workingDir may be empty
// (skips project/rules/local scopes).
func ListMemoryFiles(workingDir string) []MemoryFile {
	var files []MemoryFile

	if home := homeDir(); home != "" {
		claudeDir := filepath.Join(home, ".claude")

		// 1. User memory — ~/.claude/CLAUDE.md
		files = append(files, tryMemoryFile(
			filepath.Join(claudeDir, "CLAUDE.md"),
			"User memory",
			MemoryTypeUser,
		)...)

		// 2. User rules — ~/.claude/rules/*.md
		files = append(files, scanRulesDir(filepath.Join(claudeDir, "rules"), MemoryTypeUser)...)
	}

	if workingDir != "" {
		// 3. Project memory — <wd>/CLAUDE.md
		files = append(files, tryMemoryFile(
			filepath.Join(workingDir, "CLAUDE.md"),
			"Project memory",
			MemoryTypeProject,
		)...)

		// 3b. Project memory — <wd>/.claude/CLAUDE.md
		files = append(files, tryMemoryFile(
			filepath.Join(workingDir, ".claude", "CLAUDE.md"),
			"Project memory (.claude/)",
			MemoryTypeProject,
		)...)

		// 4. Project rules — <wd>/.claude/rules/*.md
		files = append(files, scanRulesDir(filepath.Join(workingDir, ".claude", "rules"), MemoryTypeRules)...)

		// 5. Local memory — <wd>/CLAUDE.local.md
		files = append(files, tryMemoryFile(
			filepath.Join(workingDir, "CLAUDE.local.md"),
			"Local memory",
			MemoryTypeLocal,
		)...)
	}

	return files
}

// scanRulesDir reads all *.md files from dir alphabetically.
func scanRulesDir(dir string, t MemoryType) []MemoryFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []MemoryFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		files = append(files, tryMemoryFile(p, e.Name(), t)...)
	}
	return files
}

// tryMemoryFile reads path and returns a one-element slice when the file
// exists and is readable, or nil otherwise.
func tryMemoryFile(path, label string, t MemoryType) []MemoryFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	if len(data) > memoryMaxBytes {
		cut := memoryMaxBytes
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut--
		}
		content = string(data[:cut]) + "\n…（文件过大，已截断）"
	}
	return []MemoryFile{{Label: label, Type: t, Path: path, Content: content}}
}
