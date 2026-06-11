package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

const (
	maxDiffLinesPerFile = 50
	maxDiffTotalLen     = 12000
	diffTimeout         = 10 * time.Second
	diffFilesPerPage    = 8
)

type fileDiff struct {
	Name    string
	Added   int
	Deleted int
	Lines   []diffLine
}

type diffLine struct {
	Type    byte
	Content string
}

func (b *Bridge) cmdDiff(ctx context.Context, m channel.InboundMessage) {
	wd := b.defaultCWD
	if m.ThreadID != "" {
		if sess, ok := b.mgr.GetByThreadID(m.ThreadID); ok && sess.WorkingDir != "" {
			wd = sess.WorkingDir
		}
	} else if focused, ok := b.mgr.FocusedSession(m.UserID); ok && focused.WorkingDir != "" {
		wd = focused.WorkingDir
	} else if sess := b.mgr.ResolveResumable(m.UserID); sess != nil && sess.WorkingDir != "" {
		wd = sess.WorkingDir
	}

	diffCtx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	stat := strings.TrimSpace(runGit(diffCtx, wd, "diff", "HEAD", "--shortstat"))
	rawDiff := runGit(diffCtx, wd, "diff", "HEAD")
	untracked := runGit(diffCtx, wd, "ls-files", "--others", "--exclude-standard")

	fileDiffs := parseDiffByFile(rawDiff)
	if untracked != "" {
		for _, f := range strings.Split(strings.TrimSpace(untracked), "\n") {
			f = strings.TrimSpace(f)
			if f != "" {
				fileDiffs = append(fileDiffs, fileDiff{Name: f, Lines: []diffLine{{Type: '+', Content: "(new untracked file)"}}})
			}
		}
	}

	if len(fileDiffs) == 0 && stat == "" {
		b.replyCard(ctx, m, channel.Card{
			Title:    "Diff",
			Tone:     channel.ToneNeutral,
			Sections: []channel.Section{{Markdown: "No changes"}},
		})
		return
	}

	b.replyCard(ctx, m, buildDiffListCard(wd, stat, fileDiffs, 0))
}

// buildDiffListCard renders a paginated file-list card for /diff.
// Each file is one button row; clicking drills into the single-file diff view.
func buildDiffListCard(wd, stat string, fileDiffs []fileDiff, page int) channel.Card {
	total := len(fileDiffs)
	maxPage := (total - 1) / diffFilesPerPage
	if page < 0 {
		page = 0
	}
	if page > maxPage {
		page = maxPage
	}
	start := page * diffFilesPerPage
	end := start + diffFilesPerPage
	if end > total {
		end = total
	}

	title := fmt.Sprintf("Diff: %d files", total)
	if stat != "" {
		title = "Diff: " + stat
	}

	var sections []channel.Section

	if total > diffFilesPerPage {
		sections = append(sections, channel.Section{
			Markdown: fmt.Sprintf("**文件** %d–%d / %d", start+1, end, total),
		})
	}

	for _, f := range fileDiffs[start:end] {
		label := "📄 " + shortenFilePath(f.Name)
		if f.Added > 0 || f.Deleted > 0 {
			label += " "
			if f.Deleted > 0 {
				label += fmt.Sprintf("-%d", f.Deleted)
			}
			if f.Added > 0 {
				label += fmt.Sprintf("+%d", f.Added)
			}
		} else if len(f.Lines) > 0 && f.Lines[0].Content == "(new untracked file)" {
			label += " [new]"
		}
		sections = append(sections, channel.Section{
			Buttons: []channel.Button{{
				Label: label,
				Style: "default",
				Action: map[string]string{
					"action":      "diff_file_detail",
					"working_dir": wd,
					"file":        f.Name,
					"list_page":   strconv.Itoa(page),
				},
			}},
		})
	}

	if total > diffFilesPerPage {
		var navBtns []channel.Button
		if page > 0 {
			navBtns = append(navBtns, channel.Button{
				Label: "◀ 上一页", Style: "default",
				Action: map[string]string{
					"action":      "diff_file_list",
					"working_dir": wd,
					"page":        strconv.Itoa(page - 1),
				},
			})
		}
		if end < total {
			navBtns = append(navBtns, channel.Button{
				Label: "下一页 ▶", Style: "default",
				Action: map[string]string{
					"action":      "diff_file_list",
					"working_dir": wd,
					"page":        strconv.Itoa(page + 1),
				},
			})
		}
		if len(navBtns) > 0 {
			sections = append(sections, channel.Section{Buttons: navBtns})
		}
	}

	return channel.Card{
		Title:    title,
		Tone:     channel.ToneSuccess,
		Sections: sections,
	}
}

// handleDiffFileList re-runs git diff and re-renders the paginated file list.
// Called by the prev/next navigation buttons in buildDiffListCard.
func (b *Bridge) handleDiffFileList(ctx context.Context, m channel.InboundMessage) {
	if m.Action == nil {
		return
	}
	wd, _ := m.Action.Values["working_dir"].(string)
	pageStr, _ := m.Action.Values["page"].(string)
	page, _ := strconv.Atoi(pageStr)
	if wd == "" {
		return
	}

	diffCtx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	stat := strings.TrimSpace(runGit(diffCtx, wd, "diff", "HEAD", "--shortstat"))
	rawDiff := runGit(diffCtx, wd, "diff", "HEAD")
	untracked := runGit(diffCtx, wd, "ls-files", "--others", "--exclude-standard")

	fileDiffs := parseDiffByFile(rawDiff)
	if untracked != "" {
		for _, f := range strings.Split(strings.TrimSpace(untracked), "\n") {
			f = strings.TrimSpace(f)
			if f != "" {
				fileDiffs = append(fileDiffs, fileDiff{Name: f, Lines: []diffLine{{Type: '+', Content: "(new untracked file)"}}})
			}
		}
	}

	card := buildDiffListCard(wd, stat, fileDiffs, page)
	if m.Reply != nil {
		m.Reply(card)
	} else {
		b.replyCard(ctx, m, card)
	}
}

// handleDiffFileDetail fetches the diff for one file and renders it.
// Called when the user clicks a file button in buildDiffListCard.
func (b *Bridge) handleDiffFileDetail(ctx context.Context, m channel.InboundMessage) {
	if m.Action == nil {
		return
	}
	wd, _ := m.Action.Values["working_dir"].(string)
	file, _ := m.Action.Values["file"].(string)
	listPage, _ := strconv.Atoi(func() string {
		if v, ok := m.Action.Values["list_page"].(string); ok {
			return v
		}
		return "0"
	}())
	if wd == "" || file == "" {
		return
	}

	diffCtx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	rawDiff := runGit(diffCtx, wd, "diff", "HEAD", "--", file)
	// Also check if it's an untracked file.
	isUntracked := false
	if strings.TrimSpace(rawDiff) == "" {
		untracked := runGit(diffCtx, wd, "ls-files", "--others", "--exclude-standard", "--", file)
		if strings.TrimSpace(untracked) != "" {
			isUntracked = true
		}
	}

	var sections []channel.Section
	if isUntracked {
		sections = append(sections, channel.Section{Markdown: "_(new untracked file — no diff available)_"})
	} else {
		parsed := parseDiffByFile(rawDiff)
		if len(parsed) > 0 {
			rendered := renderDiffLines(parsed[0].Lines)
			if rendered != "" {
				sections = append(sections, channel.Section{Markdown: rendered})
			}
		}
		if len(sections) == 0 {
			sections = append(sections, channel.Section{Markdown: "_(no diff)_"})
		}
	}

	// Back button returns to the file list at the same page.
	sections = append(sections, channel.Section{
		Divider: true,
		Buttons: []channel.Button{{
			Label: "← 文件列表", Style: "default",
			Action: map[string]string{
				"action":      "diff_file_list",
				"working_dir": wd,
				"page":        strconv.Itoa(listPage),
			},
		}},
	})

	card := channel.Card{
		Title:    "Diff: " + shortenFilePath(file),
		Tone:     channel.ToneSuccess,
		Sections: sections,
	}
	if m.Reply != nil {
		m.Reply(card)
	} else {
		b.replyCard(ctx, m, card)
	}
}

func runGit(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &out, limit: 64 * 1024}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return ""
	}
	return out.String()
}

func parseDiffByFile(diff string) []fileDiff {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	var files []fileDiff
	rawLines := strings.Split(diff, "\n")
	var current *fileDiff
	totalLen := 0
	lineCount := 0

	flush := func() {
		if current == nil {
			return
		}
		files = append(files, *current)
		current = nil
	}

	for _, line := range rawLines {
		if strings.HasPrefix(line, "diff --git") {
			flush()
			if totalLen > maxDiffTotalLen {
				files = append(files, fileDiff{Name: "...", Lines: []diffLine{{Type: ' ', Content: "(truncated)"}}})
				return files
			}
			parts := strings.Fields(line)
			name := ""
			if len(parts) >= 4 {
				name = strings.TrimPrefix(parts[3], "b/")
			}
			current = &fileDiff{Name: name}
			lineCount = 0
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		if lineCount >= maxDiffLinesPerFile {
			if lineCount == maxDiffLinesPerFile {
				current.Lines = append(current.Lines, diffLine{Type: ' ', Content: "... (truncated)"})
				lineCount++
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			current.Lines = append(current.Lines, diffLine{Type: '@', Content: line})
		case strings.HasPrefix(line, "+"):
			current.Lines = append(current.Lines, diffLine{Type: '+', Content: line[1:]})
			current.Added++
			totalLen += len(line)
		case strings.HasPrefix(line, "-"):
			current.Lines = append(current.Lines, diffLine{Type: '-', Content: line[1:]})
			current.Deleted++
			totalLen += len(line)
		case line == "":
			current.Lines = append(current.Lines, diffLine{Type: ' ', Content: ""})
		default:
			if len(line) > 0 && line[0] == ' ' {
				current.Lines = append(current.Lines, diffLine{Type: ' ', Content: line[1:]})
			} else {
				current.Lines = append(current.Lines, diffLine{Type: ' ', Content: line})
			}
		}
		lineCount++
	}
	flush()
	return files
}

func renderDiffLines(lines []diffLine) string {
	var parts []string
	var buf []string
	curType := byte(0)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		switch curType {
		case '-':
			for _, line := range buf {
				parts = append(parts, "<font color='red'>~~"+line+"~~</font>")
			}
		case '+':
			parts = append(parts, "<font color='green'>"+strings.Join(buf, "\n")+"</font>")
		default:
			parts = append(parts, strings.Join(buf, "\n"))
		}
		buf = buf[:0]
	}
	for _, dl := range lines {
		t := dl.Type
		if t == '@' {
			t = ' '
		}
		if t != curType {
			flush()
			curType = t
		}
		content := dl.Content
		if dl.Type == '@' {
			content = "<font color='grey'>" + escapeLarkMD(content) + "</font>"
		} else {
			content = escapeLarkMD(content)
		}
		buf = append(buf, content)
	}
	flush()
	return strings.Join(parts, "\n")
}

func escapeLarkMD(s string) string {
	return strings.ReplaceAll(s, "~", "\\~")
}

func shortenFilePath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// handleDiffCardAction dispatches the diff_file_list / diff_file_detail
// pagination buttons. Returns true when claimed.
func (b *Bridge) handleDiffCardAction(ctx context.Context, m channel.InboundMessage) bool {
	switch m.Action.Name {
	case "diff_file_list":
		b.handleDiffFileList(ctx, m)
	case "diff_file_detail":
		b.handleDiffFileDetail(ctx, m)
	default:
		return false
	}
	return true
}
