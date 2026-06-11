package bridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// /list and /project both render the same merged Projects card via
// buildProjectsCard. The card includes:
//   - one section per project (with focus marker + session counts)
//   - per-project [进入][新建会话] buttons
//   - footer [➕ 添加项目][归档对话 (N)] entries
//
// Card-action drill-in flows (show_project / back_to_list / show_projects)
// all route back to this same card so the user stays inside one Reply chain.

func (b *Bridge) cmdList(ctx context.Context, m channel.InboundMessage) {
	b.cmdProject(ctx, m)
}

// replyWithProjectsCard re-renders the merged projects card via Reply (edits
// the original card in place) — used by "← 返回" buttons in drill-in views.
func (b *Bridge) replyWithProjectsCard(ctx context.Context, m channel.InboundMessage) {
	card := b.buildProjectsCard(m.UserID)
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// buildArchivedSections renders archived sessions with resume/delete buttons.
// Used by both the global archive view and per-project drill-down.
func buildArchivedSections(archived []session.SessionInfo) []channel.Section {
	return buildArchivedSectionsWithDir(archived, "")
}

// buildSessionListSections renders the per-session card for one project
// (drill-down view) or for an arbitrary filtered slice. Two-line layout:
//
//	id: <short> · <status> · [tag] · <relative time>
//	[<custom-title>] <summary>
//
// sortSessionsForList orders sessions for the user-facing list view:
//   - active sessions first (running, immediately switchable)
//   - then idle (resumable but cold)
//   - everything else (external/archived) last
//   - within each tier: most recently active first
//
// Returns a new slice; the input is not mutated.
func sortSessionsForList(sessions []session.SessionInfo) []session.SessionInfo {
	out := make([]session.SessionInfo, len(sessions))
	copy(out, sessions)
	rank := func(s session.SessionInfo) int {
		switch s.Status {
		case string(session.StatusActive):
			return 0
		case string(session.StatusIdle):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		ti := parseRFC3339(out[i].LastActivity)
		tj := parseRFC3339(out[j].LastActivity)
		return ti.After(tj)
	})
	return out
}

// Buttons depend on status; tags indicate provenance.
func buildSessionListSections(sessions []session.SessionInfo, focusedID string) []channel.Section {
	sorted := sortSessionsForList(sessions)
	sections := make([]channel.Section, 0, len(sorted))
	for _, info := range sorted {
		isActive := info.Status == string(session.StatusActive)

		header := renderSessionHeader(info, focusedID)
		body := renderSessionTitle(info)
		md := header + "\n" + body

		btns := []channel.Button{}
		switch {
		case info.ID == focusedID:
			// Already focused — no primary action.
		case isActive:
			btns = append(btns, channel.Button{
				Label: "切换", Style: "primary",
				Action: map[string]string{"action": "switch_session", "session_id": sessionPayloadID(info)},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": sessionPayloadID(info)},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": sessionPayloadID(info)},
		})
		btns = append(btns, channel.Button{
			Label: "刷新摘要", Style: "default",
			Action: map[string]string{"action": "refresh_summary", "session_id": sessionPayloadID(info)},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns, ButtonLayout: "fill"})
	}
	return sections
}

// buildArchivedSectionsWithDir is like buildArchivedSections but includes
// working_dir and return_to in button payloads so the handler can navigate
// back to the correct project view after resume/delete.
func buildArchivedSectionsWithDir(archived []session.SessionInfo, dir string, returnTo ...string) []channel.Section {
	rt := ""
	if len(returnTo) > 0 {
		rt = returnTo[0]
	}
	sections := make([]channel.Section, 0, len(archived))
	for _, info := range archived {
		header := renderSessionHeader(info, "")
		body := renderSessionTitle(info)
		md := header + "\n" + body
		resumeAction := map[string]string{"action": "resume_archived", "session_id": sessionPayloadID(info), "working_dir": dir}
		removeAction := map[string]string{"action": "remove_archived", "session_id": sessionPayloadID(info), "working_dir": dir}
		if rt != "" {
			resumeAction["return_to"] = rt
			removeAction["return_to"] = rt
		}
		sections = append(sections, channel.Section{
			Markdown:     md,
			ButtonLayout: "fill",
			Buttons: []channel.Button{
				{Label: "恢复", Style: "primary", Action: resumeAction},
				{Label: "删除", Style: "danger", Action: removeAction},
			},
		})
	}
	return sections
}

// buildSessionListSectionsWithDir is like buildSessionListSections but
// includes working_dir in button payloads for project-scoped navigation.
func buildSessionListSectionsWithDir(sessions []session.SessionInfo, focusedID string, dir string) []channel.Section {
	sorted := sortSessionsForList(sessions)
	sections := make([]channel.Section, 0, len(sorted))
	for _, info := range sorted {
		isActive := info.Status == string(session.StatusActive)

		header := renderSessionHeader(info, focusedID)
		body := renderSessionTitle(info)
		md := header + "\n" + body

		btns := []channel.Button{}
		switch {
		case info.ID == focusedID:
			// Already focused — no primary action.
		case isActive:
			btns = append(btns, channel.Button{
				Label: "切换", Style: "primary",
				Action: map[string]string{"action": "switch_session", "session_id": sessionPayloadID(info), "working_dir": dir},
			})
		default:
			btns = append(btns, channel.Button{
				Label: "恢复", Style: "primary",
				Action: map[string]string{"action": "resume_session", "session_id": sessionPayloadID(info), "working_dir": dir},
			})
		}
		btns = append(btns, channel.Button{
			Label: "归档", Style: "danger",
			Action: map[string]string{"action": "archive_session", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		btns = append(btns, channel.Button{
			Label: "刷新摘要", Style: "default",
			Action: map[string]string{"action": "refresh_summary", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		btns = append(btns, channel.Button{
			Label: "重命名", Style: "default",
			Action: map[string]string{"action": "rename_session", "session_id": sessionPayloadID(info), "working_dir": dir},
		})
		sections = append(sections, channel.Section{Markdown: md, Buttons: btns, ButtonLayout: "fill"})
	}
	return sections
}

// focusedID may be empty (e.g. archive view); when set, the focused session
// gets a ★ marker.
func renderSessionHeader(info session.SessionInfo, focusedID string) string {
	idStr := shortID(info.CLISessionID)
	if idStr == "" {
		idStr = "gw:" + shortID(info.ID)
	}
	parts := []string{fmt.Sprintf("`%s`", idStr)}

	status := info.Status
	if status == "" {
		status = info.State
	}
	if info.ID == focusedID {
		status += " ★"
	}
	parts = append(parts, status)

	switch info.Origin {
	case session.OriginFeishu:
		parts = append(parts, "`[💬feishu]`")
	case session.OriginDingTalk:
		parts = append(parts, "`[💬dingtalk]`")
	}

	if t := parseRFC3339(info.LastActivity); !t.IsZero() {
		parts = append(parts, humanAgo(time.Since(t)))
	}

	if info.MessageCount > 0 {
		parts = append(parts, fmt.Sprintf("%d 条", info.MessageCount))
	}

	return strings.Join(parts, " · ")
}

// renderSessionTitle renders the second line: `[custom] summary` or just
// `summary` (or short id when both are empty).
func renderSessionTitle(info session.SessionInfo) string {
	switch {
	case info.CustomTitle != "" && info.Summary != "":
		return fmt.Sprintf("**[%s]** %s", info.CustomTitle, info.Summary)
	case info.CustomTitle != "":
		return fmt.Sprintf("**[%s]**", info.CustomTitle)
	case info.Summary != "":
		return info.Summary
	default:
		// Worker classified this session as too short / meta-like and
		// didn't generate a summary. Fall back to the latest user message
		// so /list still gives the user enough signal to pick the right
		// session — multiple short parallel sessions are otherwise
		// indistinguishable.
		if info.LatestUserMessage != "" {
			return "_(短对话)_ " + info.LatestUserMessage
		}
		return "_(短对话,无摘要)_"
	}
}

// buildResumeWelcome constructs the first message sent into a thread when a
// session is resumed. Includes the session summary so the user knows what
// was being worked on without opening the full history.
func buildResumeWelcome(info session.SessionInfo, display string) string {
	sid := shortID(info.CLISessionID)
	if sid == "" {
		sid = shortID(info.ID)
	}
	footer := fmt.Sprintf("👋 话题 [`%s`] · %s 已恢复\n\n在当前对话框继续沟通", sid, display)
	summary := renderSessionTitle(info)
	if summary != "" && summary != "_(短对话,无摘要)_" {
		return "📝 " + summary + "\n\n" + footer
	}
	return footer
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
