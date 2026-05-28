package feishu

import (
	"regexp"
	"strings"
)

var (
	headerRegex    = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)
	codeBlockRegex = regexp.MustCompile("(?s)```[a-zA-Z]*\n(.*?)```")
	tableRowRegex  = regexp.MustCompile(`(?m)^\|(.+)\|$`)
	tableSepRegex  = regexp.MustCompile(`(?m)^\|[-| :]+\|$`)
)

// convertToLarkMD adapts CommonMark-ish markdown to Lark's lark_md flavor:
// strips headers, collapses code fences, etc.
func convertToLarkMD(md string) string {
	md = headerRegex.ReplaceAllString(md, "**$2**")

	md = codeBlockRegex.ReplaceAllStringFunc(md, func(block string) string {
		matches := codeBlockRegex.FindStringSubmatch(block)
		if len(matches) < 2 {
			return block
		}
		code := strings.TrimSpace(matches[1])
		lines := strings.Split(code, "\n")
		for i, line := range lines {
			lines[i] = "  " + line
		}
		return "\n" + strings.Join(lines, "\n") + "\n"
	})

	md = tableSepRegex.ReplaceAllString(md, "")

	md = tableRowRegex.ReplaceAllStringFunc(md, func(row string) string {
		cells := strings.Split(strings.Trim(row, "|"), "|")
		var parts []string
		for _, c := range cells {
			c = strings.TrimSpace(c)
			if c != "" {
				parts = append(parts, c)
			}
		}
		return strings.Join(parts, "  |  ")
	})

	lines := strings.Split(md, "\n")
	var cleaned []string
	prevEmpty := false
	for _, line := range lines {
		isEmpty := strings.TrimSpace(line) == ""
		if isEmpty && prevEmpty {
			continue
		}
		cleaned = append(cleaned, line)
		prevEmpty = isEmpty
	}
	md = strings.Join(cleaned, "\n")

	return strings.TrimSpace(md)
}
