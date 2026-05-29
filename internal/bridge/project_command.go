package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// --- Project management ---
//
// A "project" in the gateway is a working directory that owns one or more
// claude-code sessions. The set is derived from:
//   - every owned session's WorkingDir (authoritative — discovered via mgr)
//   - the user's manually-added paths (kept in-memory; if the user creates a
//     session in that path they become permanent via the session route)
//
// We deliberately don't persist manual-only projects across restarts —
// projects with sessions auto-survive, and projects without sessions are
// just "I might use this dir later" notes that aren't worth the schema bump.

// projectsForUser returns the deduped, sorted list of project paths visible
// to userID. Source of truth matches buildProjectCard's drill-in:
// ListDiscoverableByOwner (owned + shareExternal-controlled external),
// plus owned archived sessions, plus the user's manual additions.
func (b *Bridge) projectsForUser(userID string) []string {
	paths := map[string]bool{}
	for _, info := range b.mgr.ListDiscoverableByOwner(userID, b.shareExternalEnabled()) {
		if info.WorkingDir != "" {
			paths[info.WorkingDir] = true
		}
	}
	for _, info := range b.mgr.ListArchivedByOwner(userID) {
		if info.WorkingDir != "" {
			paths[info.WorkingDir] = true
		}
	}
	b.mu.Lock()
	for p := range b.manualProjects[userID] {
		paths[p] = true
	}
	b.mu.Unlock()
	out := make([]string, 0, len(paths))
	for p := range paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (b *Bridge) addManualProject(userID, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.manualProjects[userID] == nil {
		b.manualProjects[userID] = map[string]bool{}
	}
	b.manualProjects[userID][path] = true
}

func (b *Bridge) removeManualProject(userID, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.manualProjects[userID] != nil {
		delete(b.manualProjects[userID], path)
	}
}

// newSessionInDir creates a session in the given project dir, triggered
// from the /project card's [新建 session] button. Uses the same flow as
// cmdNew (snapshot focus + create + afterCreateOrActivate).
func (b *Bridge) newSessionInDir(ctx context.Context, m channel.InboundMessage, dir string) {
	if dir == "" {
		b.replyOrText(ctx, m, "目录为空,无法创建 session")
		return
	}
	priorFocus := b.snapshotFocus(m.UserID)

	sess, err := b.mgr.Create(ctx, session.CreateOpts{
		WorkingDir:  dir,
		OwnerID:     m.UserID,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
		Origin:      channelKindToOrigin(m.ChannelKind),
	})
	if err != nil {
		b.replyOrText(ctx, m, "创建 session 失败: "+err.Error())
		return
	}
	b.ensureSubscribed(ctx, sess, m)

	display := projectName(dir)
	sid := displaySessionID(sess)
	var body string
	if priorFocus == nil {
		body = fmt.Sprintf("%s · %s · 已创建", display, sid)
	} else {
		body = fmt.Sprintf("%s · %s · 已创建 · 进入话题发送消息", display, sid)
	}
	msgID, cerr := b.replyCard(ctx, m, channel.Card{
		Title:    "Session Created",
		Tone:     channel.ToneSuccess,
		Sections: []channel.Section{{Markdown: body}},
	})
	if cerr != nil {
		return
	}
	welcome := fmt.Sprintf("👋 进入话题 [`%s`] · %s\n在这里直接发消息,bot 会路由到该 session。", sid, display)
	b.afterCreateOrActivate(ctx, sess, m.UserID, msgID, welcome, priorFocus, false)
}

// --- /project command ---

func (b *Bridge) cmdProject(ctx context.Context, m channel.InboundMessage) {
	if m.ThreadID != "" {
		b.replyText(ctx, m, "请回主聊天 /project (在话题里管理项目没有意义)")
		return
	}
	card := b.buildProjectsCard(m.UserID)
	b.replyCard(ctx, m, card)
}

func (b *Bridge) buildProjectsCard(userID string) channel.Card {
	projects := b.projectsForUser(userID)

	// Mark which project the focused session lives in.
	var focusedDir string
	if sess, ok := b.mgr.FocusedSession(userID); ok {
		focusedDir = sess.Info().WorkingDir
	}

	sections := make([]channel.Section, 0, len(projects)+2)
	if len(projects) == 0 {
		sections = append(sections, channel.Section{
			Markdown: "_暂无项目_。点 ➕ 添加项目 选择一个工作目录开始。",
		})
	} else {
		for _, p := range projects {
			name := projectName(p)
			marker := ""
			if p == focusedDir {
				marker = " ★"
			}
			n := b.countSessionsInProject(userID, p)
			body := fmt.Sprintf("**%s**%s · `%s` · %d sessions", name, marker, p, n)
			sections = append(sections, channel.Section{
				Markdown: body,
				Buttons: []channel.Button{
					{Label: "进入", Style: "primary",
						Action: map[string]string{"action": "show_project", "working_dir": p, "return_to": "projects"}},
					{Label: "新建 session", Style: "default",
						Action: map[string]string{"action": "new_session_in", "working_dir": p}},
				},
			})
		}
	}
	sections = append(sections, channel.Section{Divider: true})
	sections = append(sections, channel.Section{
		Buttons: []channel.Button{{
			Label: "➕ 添加项目", Style: "primary",
			Action: map[string]string{
				"action":  "pick_dir",
				"path":    homeDir(),
				"purpose": "add_project",
			},
		}},
	})
	return channel.Card{
		Title:    "Projects",
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

func (b *Bridge) countSessionsInProject(userID, dir string) int {
	// Use the same source-of-truth as buildProjectCard so the count
	// matches what the drill-in actually shows (otherwise external/shared
	// sessions get displayed inside but counted as 0 outside).
	visible := b.filterAliveSessions(b.mgr.ListDiscoverableByOwner(userID, b.shareExternalEnabled()))
	n := 0
	for _, info := range visible {
		if info.WorkingDir == dir &&
			(info.Status == string(session.StatusActive) || info.Status == string(session.StatusIdle)) {
			n++
		}
	}
	return n
}

// --- pick_dir card action: directory picker ---

const (
	maxDirEntries = 25
)

func (b *Bridge) handlePickDir(ctx context.Context, m channel.InboundMessage) {
	path, _ := m.Action.Values["path"].(string)
	purpose, _ := m.Action.Values["purpose"].(string)
	sortBy, _ := m.Action.Values["sort"].(string)
	if path == "" {
		path = homeDir()
	}
	if sortBy == "" {
		sortBy = "name" // default: alphabetical (用户偏好,跟 macOS Finder 一致)
	}
	path = expandHome(path)

	card := b.buildPickDirCard(path, purpose, sortBy)
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// handlePickDirConfirm finalizes the picker — applies the picked path
// according to purpose.
func (b *Bridge) handlePickDirConfirm(ctx context.Context, m channel.InboundMessage) {
	path, _ := m.Action.Values["path"].(string)
	purpose, _ := m.Action.Values["purpose"].(string)
	path = expandHome(path)
	if path == "" {
		b.replyOrText(ctx, m, "路径为空")
		return
	}

	switch purpose {
	case "add_project":
		b.addManualProject(m.UserID, path)
		if m.Reply != nil {
			m.Reply(b.buildProjectsCard(m.UserID))
		} else {
			b.replyCard(ctx, m, b.buildProjectsCard(m.UserID))
		}
	case "setup_cwd":
		if b.envFilePath == "" {
			b.replyOrText(ctx, m, "未配置 .env 文件路径,无法保存")
			return
		}
		if err := WriteEnvFile(b.envFilePath, map[string]string{"GATEWAY_DEFAULT_CWD": path}); err != nil {
			b.replyOrText(ctx, m, "写入失败: "+err.Error())
			return
		}
		b.applyConfigChange("GATEWAY_DEFAULT_CWD", path)
		b.replyOrText(ctx, m, fmt.Sprintf("✅ 默认工作目录设为 `%s` (已生效)", path))
	default:
		b.replyOrText(ctx, m, "未知 purpose: "+purpose)
	}
}

func (b *Bridge) buildPickDirCard(path, purpose, sortBy string) channel.Card {
	entries, err := listSubdirs(path, maxDirEntries, sortBy)

	headline := fmt.Sprintf("📁 **%s**", path)
	if err != nil {
		headline += fmt.Sprintf("\n_读取失败: %v_", err)
	}
	sections := []channel.Section{{Markdown: headline}}

	// One subdir per section, button-only (label carries the name) — one
	// row per directory, no extra markdown line, no time badge.
	if len(entries) > 0 {
		for _, e := range entries {
			sub := filepath.Join(path, e.Name)
			sections = append(sections, channel.Section{
				Buttons: []channel.Button{{
					Label: "📂 " + e.Name, Style: "default",
					Action: map[string]string{
						"action":  "pick_dir",
						"path":    sub,
						"purpose": purpose,
						"sort":    sortBy,
					},
				}},
			})
		}
	} else if err == nil {
		sections = append(sections, channel.Section{Markdown: "_(无子目录)_"})
	}

	// Navigation row: parent + home + sort-toggle + confirm
	other := "name"
	otherLabel := "按名称"
	if sortBy == "name" {
		other = "mtime"
		otherLabel = "按时间"
	}
	navBtns := []channel.Button{}
	// "Return to the caller" — closes the picker loop. Routed by purpose so
	// add_project goes back to the project list, future setup_cwd could go
	// back to /config, etc.
	if back := pickDirReturnButton(purpose); back != nil {
		navBtns = append(navBtns, *back)
	}
	parent := filepath.Dir(path)
	if parent != path {
		navBtns = append(navBtns, channel.Button{
			Label: "← 上级", Style: "default",
			Action: map[string]string{
				"action":  "pick_dir",
				"path":    parent,
				"purpose": purpose,
				"sort":    sortBy,
			},
		})
	}
	if path != homeDir() {
		navBtns = append(navBtns, channel.Button{
			Label: "🏠 家目录", Style: "default",
			Action: map[string]string{
				"action":  "pick_dir",
				"path":    homeDir(),
				"purpose": purpose,
				"sort":    sortBy,
			},
		})
	}
	navBtns = append(navBtns, channel.Button{
		Label: otherLabel, Style: "default",
		Action: map[string]string{
			"action":  "pick_dir",
			"path":    path,
			"purpose": purpose,
			"sort":    other,
		},
	})
	navBtns = append(navBtns, channel.Button{
		Label: "✓ 选这里", Style: "primary",
		Action: map[string]string{
			"action":  "pick_dir_confirm",
			"path":    path,
			"purpose": purpose,
		},
	})
	sections = append(sections, channel.Section{Divider: true, Buttons: navBtns})

	return channel.Card{
		Title:    "选择目录",
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// dirEntry is one subdirectory returned by listSubdirs.
type dirEntry struct {
	Name  string
	MTime time.Time
}

// listSubdirs returns subdirectories under path with their mtimes. Hides
// dotfiles, caps at limit entries (after sorting). sortBy is "mtime" (DESC)
// or "name" (ASC).
func listSubdirs(path string, limit int, sortBy string) ([]dirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var out []dirEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, dirEntry{Name: name, MTime: info.ModTime()})
	}
	switch sortBy {
	case "name":
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	default: // mtime
		sort.Slice(out, func(i, j int) bool { return out[i].MTime.After(out[j].MTime) })
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/"
}

// pickDirReturnButton returns the "back" button shown in the picker's nav row,
// routed by purpose so each entry-point closes its own loop:
//   - add_project → back to the /project list
//   - setup_cwd / others (none yet) → no return (the picker is the whole
//     flow; users dismiss it by walking away)
// Returns nil when no return action makes sense.
func pickDirReturnButton(purpose string) *channel.Button {
	switch purpose {
	case "add_project":
		return &channel.Button{
			Label: "← 返回项目", Style: "default",
			Action: map[string]string{"action": "show_projects"},
		}
	}
	return nil
}
