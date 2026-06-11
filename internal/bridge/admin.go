package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

// admin holds the lifecycle of a shared "administrative" CLI session that the
// bridge uses for AI-assisted side tasks: per-conversation summaries and
// fuzzy-matching of /switch arguments.
type admin struct {
	mu           sync.Mutex
	mgr          *session.Manager
	workingDir   string
	model        string
	queryTimeout time.Duration

	sessionID string
}

const (
	adminQueryTimeout = 10 * time.Minute
	maxSummaryRunes   = 60
)

// newAdmin constructs an admin helper. mgr must outlive the admin.
// workingDir passed in is ignored — admin always runs from
// claudeRT.AdminWorkdirPrefix so its jsonl files land in a dedicated
// projects subdir and discovery can skip them by path prefix.
func newAdmin(mgr *session.Manager, workingDir, model string) *admin {
	dir := claudeRT.AdminWorkdirPrefix
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("[bridge/admin] mkdir %s failed: %v (falling back to %s)", dir, err, workingDir)
	} else {
		mgr.AddAllowedBaseDir(dir)
		workingDir = dir
	}
	return &admin{
		mgr:          mgr,
		workingDir:   workingDir,
		model:        model,
		queryTimeout: adminQueryTimeout,
	}
}

func (a *admin) setWorkingDir(dir string) {
	a.mu.Lock()
	a.workingDir = dir
	a.mu.Unlock()
}

func (a *admin) setModel(model string) {
	a.mu.Lock()
	a.model = model
	a.mu.Unlock()
}

// destroy tears down the underlying admin session, if any.
func (a *admin) destroy() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.destroyLocked()
}

// destroyLocked is the unlocked body of destroy. Use when the caller cannot
// acquire a.mu — e.g. query() trying to break a wedged previous query that
// still holds the lock. Manager.Destroy is concurrent-safe on its own.
func (a *admin) destroyLocked() {
	if a.sessionID != "" {
		_ = a.mgr.Destroy(a.sessionID)
		a.sessionID = ""
	}
}

// query sends prompt to the admin session and returns the final result text.
// Re-spawns the session if needed.
//
// Concurrency: serialized via a.mu so only one admin.query runs at a time
// (admin shares a single CLI subprocess). If a previous query is still
// holding the lock when a new one arrives — which only happens when the
// previous query is wedged inside SendMessage / channel read despite its
// ctx having expired — we proactively destroy the admin session. Closing
// its subscriber channels unblocks the stuck goroutine so it releases the
// lock. We then spawn a fresh admin session for the new query.
func (a *admin) query(ctx context.Context, prompt string) (string, error) {
	queryID := fmt.Sprintf("q%d", time.Now().UnixNano()%1_000_000)
	t0 := time.Now()

	if !a.mu.TryLock() {
		log.Printf("[bridge/admin %s] previous query still holds lock — destroying admin to unblock", queryID)
		// Cannot a.destroy() here — it would wait on a.mu which the wedged
		// query owns. mgr.Destroy is concurrent-safe; closing the session
		// will close subscriber channels, unblocking the previous query's
		// `case raw, ok := <-ch` so it returns and releases the lock.
		a.destroyLocked()
		a.mu.Lock()
		log.Printf("[bridge/admin %s] acquired lock after destroy (waited %v)", queryID, time.Since(t0))
	}
	defer a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, a.queryTimeout)
	defer cancel()

	sess, err := a.getOrCreateSession(ctx)
	if err != nil {
		log.Printf("[bridge/admin %s] getOrCreateSession failed: %v", queryID, err)
		return "", err
	}

	subID := fmt.Sprintf("admin-%s", queryID)
	ch := sess.Subscribe(subID)
	defer sess.Unsubscribe(subID)

	sendStart := time.Now()
	sendErr := make(chan error, 1)
	go func() { sendErr <- sess.SendMessage(prompt) }()
	const writeTimeout = 2 * time.Minute
	select {
	case err := <-sendErr:
		if err != nil {
			log.Printf("[bridge/admin %s] SendMessage failed after %v: %v", queryID, time.Since(sendStart), err)
			return "", fmt.Errorf("send to admin: %w", err)
		}
	case <-time.After(writeTimeout):
		// Most likely the admin CLI stdin pipe is wedged (subprocess hung
		// but pipe not closed). The goroutine doing the syscall is leaked
		// but the query gives up here; the next query will see TryLock
		// fail and force-destroy the admin to release everything.
		log.Printf("[bridge/admin %s] SendMessage stuck > %v — abandoning (CLI stdin likely wedged)", queryID, writeTimeout)
		return "", fmt.Errorf("admin SendMessage timed out after %v", writeTimeout)
	case <-ctx.Done():
		log.Printf("[bridge/admin %s] ctx done during SendMessage: %v", queryID, ctx.Err())
		return "", ctx.Err()
	}
	log.Printf("[bridge/admin %s] SendMessage ok in %v, waiting for result", queryID, time.Since(sendStart))

	// We accumulate per-assistant-message text instead of concatenating.
	// Claude code's stream-json emits one assistant message per turn (one
	// per tool round-trip), and we only care about the FINAL response — the
	// intermediate "let me think...let me check..." chatter would otherwise
	// pollute summaries.
	var lastAssistant string
	rawCount, assistantCount := 0, 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("[bridge/admin %s] ctx done after %v (raws=%d, assistants=%d, lastLen=%d): %v",
				queryID, time.Since(t0), rawCount, assistantCount, len(lastAssistant), ctx.Err())
			return lastAssistant, ctx.Err()
		case raw, ok := <-ch:
			if !ok {
				log.Printf("[bridge/admin %s] subscriber ch closed after %v (raws=%d)", queryID, time.Since(t0), rawCount)
				return lastAssistant, fmt.Errorf("admin session closed")
			}
			rawCount++
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
					assistantCount++
				}
			case protocol.MsgTypeResult:
				log.Printf("[bridge/admin %s] result received in %v (raws=%d, assistants=%d, finalLen=%d)",
					queryID, time.Since(t0), rawCount, assistantCount, len(lastAssistant))
				return strings.TrimSpace(lastAssistant), nil
			}
		}
	}
}

func (a *admin) getOrCreateSession(ctx context.Context) (*session.Session, error) {
	if a.sessionID != "" {
		if sess, ok := a.mgr.Get(a.sessionID); ok {
			st := sess.CurrentState()
			if st != session.StateStopped && st != session.StateError {
				return sess, nil
			}
		}
		_ = a.mgr.Destroy(a.sessionID)
		a.sessionID = ""
	}
	sess, err := a.mgr.Create(ctx, session.CreateOpts{
		WorkingDir: a.workingDir,
		Model:      a.model,
		Origin:     session.OriginAdmin,
	})
	if err != nil {
		return nil, fmt.Errorf("create admin session: %w", err)
	}
	a.sessionID = sess.ID
	log.Printf("[bridge/admin] session created: %s", shortID(sess.ID))
	return sess, nil
}

// updateSummary asks the admin to summarize sessionID and writes the result
// back via mgr.SetSummary. The conversation context is read from the on-disk
// JSONL and embedded directly in the prompt — admin acts as a pure text LLM
// with no tools needed. Both the background worker and this path use
// buildRecapPrompt for consistent output.
func (b *Bridge) updateSummary(sessionID, userID string, messages []string) {
	if b.admin == nil {
		b.mgr.ClearSummaryPending(sessionID)
		return
	}
	sess, ok := b.mgr.Get(sessionID)
	if !ok {
		return
	}
	info := sess.Info()
	jsonlPath := persist.SessionJSONLPath(info.WorkingDir, info.CLISessionID)
	if jsonlPath == "" {
		b.mgr.ClearSummaryPending(sessionID)
		return
	}
	prompt := buildRecapPrompt(jsonlPath)
	if prompt == "" {
		b.mgr.ClearSummaryPending(sessionID)
		return
	}
	reply, err := b.admin.query(context.Background(), prompt)
	if err != nil {
		log.Printf("[bridge/admin] update summary failed for %s: %v", shortID(sessionID), err)
		b.mgr.ClearSummaryPending(sessionID)
		return
	}
	summary := cleanSummaryOutput(reply)
	if summary == "" || summary == "_skip_meta_" {
		log.Printf("[bridge/admin] update summary skipped for %s (empty or _skip_meta_)", shortID(sessionID))
		b.mgr.ClearSummaryPending(sessionID)
		return
	}
	if r := []rune(summary); len(r) > 150 {
		summary = string(r[:150]) + "…"
	}
	_ = b.mgr.SetSummary(sessionID, summary)
	b.saveStateIfPossible()
	log.Printf("[bridge/admin] session %s summary: %s", shortID(sessionID), summary)
}

// matchSessionByQuery uses the admin to pick the best session ID prefix that
// matches the user's natural-language query.
func (b *Bridge) matchSessionByQuery(ctx context.Context, userID, query string) (string, error) {
	if b.admin == nil {
		return "", fmt.Errorf("admin disabled")
	}
	active := b.mgr.ListActiveByOwner(userID)
	archived := b.mgr.ListArchivedByOwner(userID)
	if len(active) == 0 && len(archived) == 0 {
		return "", fmt.Errorf("no sessions")
	}

	var lines []string
	var ids []string
	for _, s := range active {
		label := s.Label
		if label == "" {
			label = s.WorkingDir
		}
		lines = append(lines, fmt.Sprintf("%s %s (dir: %s)", displayIDFromInfo(s), label, s.WorkingDir))
		ids = append(ids, s.ID)
	}
	for _, s := range archived {
		label := s.Label
		if label == "" {
			label = s.WorkingDir
		}
		lines = append(lines, fmt.Sprintf("%s %s (dir: %s, archived)", displayIDFromInfo(s), label, s.WorkingDir))
	}

	prompt := fmt.Sprintf(
		"以下是用户的会话列表:\n%s\n\n用户想要切换到的会话描述:\"%s\"\n\n请返回最匹配的会话ID前缀(前8个字符),只输出ID本身,不要解释。如果没有合理匹配,输出 none",
		strings.Join(lines, "\n"), query,
	)
	result, err := b.admin.query(ctx, prompt)
	if err != nil {
		return "", err
	}
	result = strings.TrimSpace(strings.Trim(result, "`\"'"))
	if result == "" || strings.EqualFold(result, "none") {
		return "", fmt.Errorf("no match")
	}
	for _, id := range ids {
		if strings.HasPrefix(strings.ToLower(id), strings.ToLower(result)) {
			return id, nil
		}
	}
	return "", fmt.Errorf("AI returned unrecognized ID: %s", result)
}

// parseConfigFromNL uses the admin to extract KEY=VALUE pairs from a free-form
// user message during initial setup.
func (b *Bridge) parseConfigFromNL(ctx context.Context, userMessage string) (map[string]string, error) {
	if b.admin == nil {
		return nil, fmt.Errorf("admin disabled")
	}
	prompt := fmt.Sprintf(
		"你是配置助手。用户想要设置以下配置项:\n%s\n\n用户输入:\"%s\"\n\n"+
			"从用户输入中提取要设置的配置。每行输出一个 KEY=VALUE,只输出提取到的配置,不要解释。"+
			"如果用户输入中没有任何可识别的配置意图,只输出 NONE",
		buildConfigFieldsPrompt(), userMessage,
	)
	reply, err := b.admin.query(ctx, prompt)
	if err != nil {
		return nil, err
	}
	reply = strings.TrimSpace(reply)
	if strings.EqualFold(reply, "NONE") || reply == "" {
		return nil, nil
	}
	result := make(map[string]string)
	for _, line := range strings.Split(reply, "\n") {
		line = strings.Trim(strings.TrimSpace(line), "`")
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			if _, ok := FindConfigField(key); ok {
				result[key] = val
			}
		}
	}
	return result, nil
}

func buildConfigFieldsPrompt() string {
	var lines []string
	for _, f := range ConfigFields {
		desc := f.EnvKey + " — " + f.Label
		if f.Default != "" {
			desc += "(默认: " + f.Default + ")"
		}
		lines = append(lines, desc)
	}
	return strings.Join(lines, "\n")
}

// saveStateIfPossible writes the current state via JSONStore, if configured.
func (b *Bridge) saveStateIfPossible() {
	if b.persister != nil {
		if err := b.persister.Save(b.mgr); err != nil {
			log.Printf("[bridge] save state failed: %v", err)
		}
	}
}

// unused; keep json import alive for future expansion
var _ = json.Marshal
