package dingtalk

import (
	"regexp"
	"strings"
)

var (
	htmlTagRegex = regexp.MustCompile(`<[^>]+>`)
)

// convertToDingTalkMD adapts standard markdown to DingTalk's markdown subset.
// DingTalk actionCard markdown follows CommonMark rules: a single \n between
// non-empty lines is treated as a soft break (rendered as a space). To force
// visible line breaks we append two trailing spaces before every \n that
// separates two non-empty lines ("hard line break" in CommonMark terms).
func convertToDingTalkMD(md string) string {
	md = htmlTagRegex.ReplaceAllString(md, "")

	lines := strings.Split(md, "\n")
	var result []string
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		// If this line is non-empty and the NEXT line is also non-empty,
		// append two trailing spaces to force a hard break — unless this
		// line already ends with two spaces or is a blank separator.
		if trimmed != "" && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
			if !strings.HasSuffix(trimmed, "  ") {
				trimmed += "  "
			}
		}
		result = append(result, trimmed)
	}

	// Collapse multiple consecutive blank lines into one.
	var cleaned []string
	prevEmpty := false
	for _, line := range result {
		isEmpty := strings.TrimSpace(line) == ""
		if isEmpty && prevEmpty {
			continue
		}
		cleaned = append(cleaned, line)
		prevEmpty = isEmpty
	}

	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}
