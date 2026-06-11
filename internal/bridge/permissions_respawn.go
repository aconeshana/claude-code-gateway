package bridge

import (
	"fmt"
	"log"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/permissionsfile"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// respawnDecision categorizes whether a settings.json mutation should
// immediately recycle an active session's CLI process. The "smart
// judgment" policy was confirmed with the user; see CLAUDE.md and the
// /permissions design notes for context.
type respawnDecision int

const (
	// respawnDecisionLater means the change will land naturally on the
	// next reactivate or turn — the session is idle (lifecycle) or in a
	// fragile runtime state (starting / stopped / error). Don't poke it.
	respawnDecisionLater respawnDecision = iota
	// respawnDecisionNow means the session is active and quiescent
	// (lifecycle active, runtime ready/idle, no pending turns). Safe to
	// respawn immediately so the next user message picks up the change.
	respawnDecisionNow
	// respawnDecisionInFlight means a turn is running (or paused for
	// permission). We refuse to interrupt; the change will apply on the
	// turn AFTER this one (which the user can /terminate or /stop to
	// hasten if they want).
	respawnDecisionInFlight
)

func (d respawnDecision) String() string {
	switch d {
	case respawnDecisionNow:
		return "now"
	case respawnDecisionInFlight:
		return "in_flight"
	}
	return "later"
}

// decideRespawn implements the policy from the /permissions UX brief.
// It is intentionally conservative: only StateReady and StateIdle
// trigger immediate respawn. StateWaitingPermission counts as in-flight
// because the turn is paused waiting on a tool approval — respawning
// would drop that approval pipeline.
func decideRespawn(info session.SessionInfo) respawnDecision {
	if info.Status != string(session.StatusActive) {
		return respawnDecisionLater
	}
	if info.PendingTurns > 0 {
		return respawnDecisionInFlight
	}
	switch info.State {
	case "ready", "idle":
		return respawnDecisionNow
	case "waiting_permission":
		return respawnDecisionInFlight
	}
	return respawnDecisionLater
}

// applySettingsRespawn checks the current state of every active session
// owned by ownerID whose working_dir matches the rule's effective scope,
// and respawns those that decideRespawn says are safe to recycle.
//
// Scope match:
//   - SourceUser: applies to ALL sessions of this owner (settings.json is
//     global)
//   - SourceProject / SourceLocal: applies only to sessions whose
//     working_dir == projectDir
//
// Returns a structured outcome the caller renders into a status section
// of the /permissions card so the user knows what happened.
type respawnReport struct {
	Now      []string // displayId list of sessions respawned now
	InFlight []string // displayId list of sessions skipped (in-flight)
	Later    []string // displayId list of sessions skipped (idle/etc.)
}

func (b *Bridge) applySettingsRespawn(ownerID string, src permissionsfile.Source, projectDir, reason string) respawnReport {
	var report respawnReport
	// ListActiveByOwner returns active + idle lifecycle states; decideRespawn
	// filters from there. Snapshot data (SessionInfo) is sufficient for the
	// scope check and policy decision; we only resolve to *Session via
	// mgr.Get when we actually need to call RestartForSettings.
	for _, info := range b.mgr.ListActiveByOwner(ownerID) {
		// Scope check: project/local rules only affect their own working
		// directory. SourceUser is global so this filter is a no-op.
		if src != permissionsfile.SourceUser && info.WorkingDir != projectDir {
			continue
		}
		decision := decideRespawn(info)
		display := displayIDFromInfo(info)
		switch decision {
		case respawnDecisionNow:
			sess, ok := b.mgr.Get(info.ID)
			if !ok {
				// Race: archived between list and get. Treat as "later" —
				// the next reactivate will pick up the change.
				report.Later = append(report.Later, display)
				continue
			}
			if err := sess.RestartForSettings(reason); err != nil {
				log.Printf("[bridge/permissions] respawn %s failed: %v",
					info.ID, err)
				report.Later = append(report.Later, display)
				continue
			}
			report.Now = append(report.Now, display)
		case respawnDecisionInFlight:
			report.InFlight = append(report.InFlight, display)
		case respawnDecisionLater:
			report.Later = append(report.Later, display)
		}
	}
	return report
}

// summarizeRespawn renders a report as a markdown bullet list suitable
// for embedding in a Card section. Returns "" when the report is empty
// (no matched sessions) so the caller can elide the section entirely.
func summarizeRespawn(r respawnReport) string {
	if len(r.Now)+len(r.InFlight)+len(r.Later) == 0 {
		return ""
	}
	var lines []string
	if len(r.Now) > 0 {
		lines = append(lines, fmt.Sprintf("- ✓ 已立即重启,下次发消息生效: %s",
			joinDisplay(r.Now)))
	}
	if len(r.InFlight) > 0 {
		lines = append(lines, fmt.Sprintf("- ⏳ 当前 turn 进行中,跳过(下次手动 /stop 或 turn 结束后可重启): %s",
			joinDisplay(r.InFlight)))
	}
	if len(r.Later) > 0 {
		lines = append(lines, fmt.Sprintf("- 💤 idle/启动中,下次激活时自然生效: %s",
			joinDisplay(r.Later)))
	}
	return concatLines(lines)
}

func joinDisplay(ids []string) string {
	if len(ids) == 0 {
		return "—"
	}
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += " · "
		}
		out += "`" + id + "`"
	}
	return out
}

func concatLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// settingsRespawnNote is a short single-line summary used inline (e.g.
// at the bottom of an [总是允许] confirmation card) when the bigger
// section above would be overkill.
func settingsRespawnNote(r respawnReport) string {
	switch {
	case len(r.Now) > 0:
		return fmt.Sprintf("已重启 %d 个 session 让规则立即生效", len(r.Now))
	case len(r.InFlight) > 0:
		return fmt.Sprintf("%d 个 session 正在进行 turn,规则下次生效", len(r.InFlight))
	case len(r.Later) > 0:
		return fmt.Sprintf("%d 个 session 处于 idle,下次激活时自然生效", len(r.Later))
	}
	return "未匹配到受影响的 session — 规则会作用于后续新建的 session"
}

// confirmCard reuses the existing channel.Section/Card vocabulary; defined
// here to avoid forcing every caller to construct the same wrapper.
func (b *Bridge) confirmCard(title, body string, tone channel.Tone) channel.Card {
	return channel.Card{
		Title:    title,
		Tone:     tone,
		Sections: []channel.Section{{Markdown: body}},
	}
}
