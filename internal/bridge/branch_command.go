package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/google/uuid"
)

func (b *Bridge) cmdBranch(ctx context.Context, m channel.InboundMessage, args string) {
	if m.ThreadID != "" {
		b.replyText(ctx, m, "请回主聊天 /branch(在话题里 fork 容易混淆)")
		return
	}
	name := strings.TrimSpace(args)

	// Forking requires the source CLI to be alive so the new branch
	// inherits the exact transcript state — pass mustBeActive=true.
	focused, err := b.ensureCurrentSession(ctx, m, true)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}

	info := focused.Info()
	if info.CLISessionID == "" {
		b.replyText(ctx, m, "当前 session 没有 CLI session ID，无法 branch")
		return
	}

	// Capture prior focus = the session we're branching FROM. We always
	// keep the parent as main-chat focus and put the fork into a thread.
	priorFocus := focused

	// Pre-assign a UUID so the CLI uses it as its session ID (--session-id flag).
	// This lets us stamp CLISessionID immediately rather than waiting for KindInit.
	forkCLISessionID := uuid.New().String()

	branchSess, err := b.mgr.Create(ctx, session.CreateOpts{
		OwnerID:     m.UserID,
		WorkingDir:  info.WorkingDir,
		ResumeID:    info.CLISessionID,
		ForkSession: "1",
		SessionID:   forkCLISessionID,
		Origin:      channelKindToOrigin(m.ChannelKind),
		Label:       name,
		ChatID:      m.ChatID,
		ChannelKind: m.ChannelKind,
	})
	if err != nil {
		b.replyText(ctx, m, "创建 branch 失败: "+err.Error())
		return
	}

	// Stamp the known CLI session ID immediately so /list shows it right away.
	_ = b.mgr.SetCLISessionID(branchSess.ID, forkCLISessionID)
	b.ensureSubscribed(ctx, branchSess, m)

	branchSID := shortID(forkCLISessionID)
	parentSID := displayIDFromInfo(info)
	display := name
	if display == "" {
		display = projectName(info.WorkingDir)
	}
	body := fmt.Sprintf("%s · %s · 已分支自 %s · 进入话题发送消息", display, branchSID, parentSID)
	msgID, cardErr := b.replyCard(ctx, m, channel.Card{
		Title:    "🌱 " + display,
		Tone:     channel.ToneSuccess,
		Sections: []channel.Section{{Markdown: body}},
	})
	if cardErr != nil {
		log.Printf("[bridge] cmdBranch: response card send failed: %v", cardErr)
		return
	}
	welcome := fmt.Sprintf("🌱 话题 [`%s`] · %s 已创建（分支自 `%s`）\n\n在当前对话框继续沟通", branchSID, display, parentSID)
	// /branch always opens a thread (forceThread=true). priorFocus must be
	// non-nil here (we've already returned above when ok=false).
	b.afterCreateOrActivate(ctx, branchSess, m.UserID, msgID, welcome, priorFocus, true)
}
