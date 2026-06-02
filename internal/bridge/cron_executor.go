package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/cron"
	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

const originCron = "cron"

// bridgeExecutor implements cron.Executor by spawning a claude-code session
// for each job run.
type bridgeExecutor struct {
	mgr *session.Manager
	cwd string
}

func newBridgeExecutor(mgr *session.Manager, defaultCWD string) *bridgeExecutor {
	return &bridgeExecutor{mgr: mgr, cwd: defaultCWD}
}

func (e *bridgeExecutor) Name() string { return "claude-code" }

func (e *bridgeExecutor) Execute(ctx context.Context, req cron.ExecRequest) cron.ExecResult {
	t0 := time.Now()
	workDir := req.Job.WorkDir
	if workDir == "" {
		workDir = e.cwd
	}

	sess, err := e.mgr.Create(ctx, session.CreateOpts{
		WorkingDir: workDir,
		OwnerID:    req.Job.OwnerID,
		ChatID:     req.Job.ChatID,
		Origin:     originCron,
	})
	if err != nil {
		return cron.ExecResult{Err: fmt.Errorf("create session: %w", err), Duration: time.Since(t0)}
	}

	subID := fmt.Sprintf("cron-%s", req.Job.ID[:8])
	ch := sess.Subscribe(subID)
	defer sess.Unsubscribe(subID)

	if err := sess.SendMessage(req.Job.Prompt); err != nil {
		_ = e.mgr.Destroy(sess.ID)
		return cron.ExecResult{Err: fmt.Errorf("send message: %w", err), Duration: time.Since(t0)}
	}

	var lastAssistant string
	for {
		select {
		case <-ctx.Done():
			_ = e.mgr.Destroy(sess.ID)
			return cron.ExecResult{
				Summary:  truncateRunes(lastAssistant, 200),
				Err:      ctx.Err(),
				Duration: time.Since(t0),
			}
		case raw, ok := <-ch:
			if !ok {
				_ = e.mgr.Destroy(sess.ID)
				return cron.ExecResult{
					Summary:  truncateRunes(lastAssistant, 200),
					Err:      fmt.Errorf("session closed unexpectedly"),
					Duration: time.Since(t0),
				}
			}
			if _, isGW := extractGatewayEvent(raw); isGW {
				continue
			}
			msgType, _, err := protocol.ParseType(raw)
			if err != nil {
				continue
			}
			switch msgType {
			case protocol.MsgTypeAssistant:
				if t := extractAssistantText(raw); t != "" {
					lastAssistant = t
				}
			case protocol.MsgTypeResult:
				if req.Job.SessionMode == cron.ModeNewPerRun {
					_ = e.mgr.Destroy(sess.ID)
				}
				return cron.ExecResult{
					Summary:  truncateRunes(lastAssistant, 200),
					Duration: time.Since(t0),
				}
			}
		}
	}
}

func truncateRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// cronResultNotifier returns a ResultCallback that posts cron job results
// to the user's IM channel.
func (b *Bridge) cronResultNotifier() cron.ResultCallback {
	return func(j cron.Job, r cron.ExecResult) {
		if j.Silent {
			return
		}
		card := b.buildCronResultCard(j, r)
		ctx := context.Background()
		if j.ChatID != "" {
			_, _ = b.sendCard(ctx, j.ChatID, card)
		} else {
			log.Printf("[cron] no chatID for job %s result notification", shortID(j.ID))
		}
	}
}
