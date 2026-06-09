package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/shellexec"
)

func (b *Bridge) handleShell(ctx context.Context, m channel.InboundMessage, cmdStr string) {
	wd := b.defaultCWD
	if focused, ok := b.mgr.FocusedSession(m.UserID); ok {
		if focused.WorkingDir != "" {
			wd = focused.WorkingDir
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = b.ch.Reaction(m.MessageID, "OnIt")

	cmd := shellexec.Command(execCtx, cmdStr)
	cmd.Dir = wd

	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, limit: 64 * 1024}
	cmd.Stderr = io.Discard

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if execCtx.Err() == context.DeadlineExceeded {
			exitCode = -1
		}
	}
	var text strings.Builder
	text.WriteString(fmt.Sprintf("$ %s\n", cmdStr))
	if stdout.Len() > 0 {
		text.WriteString(stdout.String())
	}
	if exitCode != 0 {
		text.WriteString(fmt.Sprintf("\n(exit code: %d)", exitCode))
	}
	body := fmt.Sprintf("```\n%s\n```", strings.TrimSpace(text.String()))
	b.replyCard(ctx, m, channel.Card{
		Title:    "Shell",
		Tone:     channel.ToneNeutral,
		Sections: []channel.Section{{Markdown: body}},
	})
}
