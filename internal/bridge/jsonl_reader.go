package bridge

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// recapTurn is a single user or assistant exchange extracted from a JSONL
// transcript for use as LLM context.
type recapTurn struct {
	Role string // "user" or "assistant"
	Text string
}

// jsonlLine is the minimal shape we need to parse from a session JSONL.
// Fields match the Claude Code transcript format exactly.
type jsonlLine struct {
	Type       string `json:"type"`
	IsMeta     bool   `json:"isMeta"`
	IsSidechain bool  `json:"isSidechain"`
	Message    struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// extractText pulls plain text out of a content field, which may be either a
// plain string or an array of content blocks (matching Claude's API format).
// Tool-use and tool-result blocks are skipped — only text blocks are kept,
// matching what normalizeMessagesForAPI passes to the model in the official
// CLI. Empty strings are returned for purely tool-only messages.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first (older transcript entries store content as a plain string)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	// Array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, " ")
}

// readAllTurns reads the full JSONL transcript and returns every user/assistant
// turn as a slice, applying the same filter rules as readRecentTurns but
// without the sliding window. Used by /export which needs the complete history.
func readAllTurns(path string) ([]recapTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var turns []recapTurn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		if line.IsMeta || line.IsSidechain {
			continue
		}
		text := extractText(line.Message.Content)
		if text == "" {
			continue
		}
		role := line.Message.Role
		if role == "" {
			role = line.Type
		}
		turns = append(turns, recapTurn{Role: role, Text: text})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return turns, nil
}

// readRecentTurns streams the JSONL transcript at path and returns the last n
// user/assistant turns as a formatted context string suitable for embedding
// directly in an LLM prompt.
//
// Filter rules mirror the official CLI's normalizeMessagesForAPI:
//   - Only type=="user" or type=="assistant" lines are kept
//   - isMeta==true lines are skipped (tool scaffolding, not real conversation)
//   - isSidechain==true lines are skipped (forked session branches)
//   - Lines whose text content is empty after extraction are skipped
//
// The returned string has one line per turn: "[user] text" or "[assistant] text".
// Returns "" when the file cannot be read or yields no usable turns.
func readRecentTurns(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Collect all qualifying turns; we keep the last n at the end.
	// Memory is bounded: each turn is at most a few KB, and n ≤ 30.
	var turns []recapTurn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MiB per line — handles large tool outputs
	for scanner.Scan() {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		if line.IsMeta || line.IsSidechain {
			continue
		}
		text := extractText(line.Message.Content)
		if text == "" {
			continue
		}
		role := line.Message.Role
		if role == "" {
			role = line.Type
		}
		turns = append(turns, recapTurn{Role: role, Text: text})
	}

	if len(turns) == 0 {
		return ""
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}

	var sb strings.Builder
	for _, t := range turns {
		sb.WriteString("[")
		sb.WriteString(t.Role)
		sb.WriteString("] ")
		sb.WriteString(t.Text)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
