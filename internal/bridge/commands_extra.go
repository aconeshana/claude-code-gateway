package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/shellexec"
	"github.com/google/uuid"
)

// modelInfo enumerates the models exposed via /model. Name is the alias
// passed to `claude --model` so the CLI resolves it through env
// (ANTHROPIC_DEFAULT_<TIER>_MODEL) — never hard-code specific versions
// like "claude-opus-4-6" here, that bypasses the user's env and gets
// stale the moment Anthropic ships a new minor.
type modelInfo struct {
	Name  string
	Alias string
	Desc  string
}

var availableModels = []modelInfo{
	{"sonnet", "sonnet", "Best coding model, fast (resolves via $ANTHROPIC_DEFAULT_SONNET_MODEL)"},
	{"opus", "opus", "Deepest reasoning (resolves via $ANTHROPIC_DEFAULT_OPUS_MODEL)"},
	{"haiku", "haiku", "Fastest, lightweight (resolves via $ANTHROPIC_DEFAULT_HAIKU_MODEL)"},
}

// --- /model ---

func (b *Bridge) cmdModel(ctx context.Context, m channel.InboundMessage, args string) {
	modelName := strings.TrimSpace(args)
	if modelName == "" {
		b.showModelMenu(ctx, m)
		return
	}
	sess, err := b.ensureCurrentSession(ctx, m, true)
	if err != nil {
		// Error after a button click: replace the menu in place with the
		// failure card so the user doesn't end up with a stale menu plus
		// a separate error message.
		b.replyModelResult(ctx, m, channel.Card{
			Title:    "Switch Model · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: err.Error()}},
		})
		return
	}
	if err := sess.SwitchModel(modelName); err != nil {
		b.replyModelResult(ctx, m, channel.Card{
			Title:    "Switch Model · 失败",
			Tone:     channel.ToneWarning,
			Sections: []channel.Section{{Markdown: "切换模型失败: " + err.Error()}},
		})
		return
	}
	b.replyModelResult(ctx, m, channel.Card{
		Title: "Switch Model · ✓ " + modelName,
		Tone:  channel.ToneSuccess,
		Sections: []channel.Section{{
			Markdown: fmt.Sprintf("已切换 session **%s** 的模型为 `%s`",
				displaySessionID(sess), modelName),
		}},
	})
}

// replyModelResult posts a /model result card. When the inbound came
// from a card-action click (m.Reply non-nil), the original menu is
// replaced in place to keep the chat dense. Falls back to a fresh
// outbound for typed `/model <name>` invocations.
func (b *Bridge) replyModelResult(ctx context.Context, m channel.InboundMessage, card channel.Card) {
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// showModelMenu renders the Switch Model card. The card header surfaces
// the target session so users don't have to guess what the buttons will
// act on — and so "no session at all" turns into a clear instruction
// instead of a pointless menu followed by an error on click.
func (b *Bridge) showModelMenu(ctx context.Context, m channel.InboundMessage) {
	target := b.modelMenuTarget(m)

	var subtitle string
	switch {
	case target == nil:
		// No session anywhere — menu is meaningless because there's
		// nothing to switch. Skip the buttons entirely.
		b.replyText(ctx, m, "暂无可切换的 session。先 /new 创建一个,然后再 /model。")
		return
	case target.Info().Status == string(session.StatusActive):
		subtitle = fmt.Sprintf("将作用于 session **%s** (active)", displaySessionID(target))
	default:
		subtitle = fmt.Sprintf("将作用于 session **%s** (idle,点击按钮会自动恢复)",
			displaySessionID(target))
	}

	var btns []channel.Button
	for _, mi := range availableModels {
		btns = append(btns, channel.Button{
			Label:  mi.Name,
			Style:  "default",
			Action: map[string]string{"action": "switch_model", "model": mi.Name, "session_id": target.ID},
		})
	}
	b.replyCard(ctx, m, channel.Card{
		Title: "Switch Model",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{
			{Markdown: subtitle},
			{Buttons: btns},
		},
	})
}

// modelMenuTarget picks the session a /model button will act on. Mirrors
// the lookup chain used by ensureCurrentSession so the menu subtitle
// stays truthful: thread-bound > main-chat focus > any resumable idle
// session.
func (b *Bridge) modelMenuTarget(m channel.InboundMessage) *session.Session {
	if sess, ok := b.currentSession(m); ok {
		return sess
	}
	return b.mgr.ResolveResumable(m.UserID)
}

// --- /diff ---

const (
	maxDiffLinesPerFile = 50
	maxDiffTotalLen     = 12000
	diffTimeout         = 10 * time.Second
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
	if focused, ok := b.mgr.FocusedSession(m.UserID); ok {
		if focused.WorkingDir != "" {
			wd = focused.WorkingDir
		}
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

	title := fmt.Sprintf("Diff: %d files", len(fileDiffs))
	if stat != "" {
		title = "Diff: " + stat
	}

	var sections []channel.Section
	for i, f := range fileDiffs {
		header := "**" + shortenFilePath(f.Name) + "**"
		if f.Added > 0 || f.Deleted > 0 {
			header += " · "
			if f.Deleted > 0 {
				header += fmt.Sprintf("<font color='red'>-%d</font> ", f.Deleted)
			}
			if f.Added > 0 {
				header += fmt.Sprintf("<font color='green'>+%d</font>", f.Added)
			}
		}
		sec := channel.Section{Markdown: header}
		if len(f.Lines) > 0 {
			sec.Markdown += "\n" + renderDiffLines(f.Lines)
		}
		sections = append(sections, sec)
		if i < len(fileDiffs)-1 {
			sections = append(sections, channel.Section{Divider: true})
		}
	}

	b.replyCard(ctx, m, channel.Card{
		Title:    title,
		Tone:     channel.ToneSuccess,
		Sections: sections,
	})
}

func runGit(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &out, limit: 64 * 1024}
	cmd.Stderr = io.Discard
	_ = cmd.Run()
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

// --- /config ---

func (b *Bridge) cmdConfig(ctx context.Context, m channel.InboundMessage, args string) {
	args = strings.TrimSpace(args)
	switch {
	case args == "" || args == "show":
		values := b.currentConfigValues()
		b.replyCard(ctx, m, buildConfigCard(values, b.envFilePath))
	case strings.HasPrefix(args, "set "):
		parts := strings.SplitN(strings.TrimPrefix(args, "set "), " ", 2)
		if len(parts) < 2 {
			b.replyText(ctx, m, "用法: /config set <KEY> <VALUE>")
			return
		}
		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		value := strings.TrimSpace(parts[1])
		if key == "GATEWAY_PERMISSION_MODE" {
			value = NormalizePermissionMode(value)
		}
		field, ok := FindConfigField(key)
		if !ok {
			b.replyText(ctx, m, fmt.Sprintf("未知配置项: %s\n使用 /config 查看可用配置", key))
			return
		}
		if b.envFilePath == "" {
			b.replyText(ctx, m, "未配置 .env 文件路径,无法保存")
			return
		}
		if err := WriteEnvFile(b.envFilePath, map[string]string{key: value}); err != nil {
			b.replyText(ctx, m, "写入配置失败: "+err.Error())
			return
		}
		if field.Mutable {
			b.applyConfigChange(key, value)
			b.replyText(ctx, m, fmt.Sprintf("✅ %s 已更新为 `%s`(已生效)", field.Label, value))
		} else {
			b.replyText(ctx, m, fmt.Sprintf("✅ %s 已写入 .env,重启后生效", field.Label))
		}
	default:
		b.replyText(ctx, m, "用法:\n/config — 查看当前配置\n/config set <KEY> <VALUE> — 修改配置")
	}
}

func (b *Bridge) currentConfigValues() map[string]string {
	values := make(map[string]string)
	if b.envFilePath != "" {
		if v, err := ParseEnvValues(b.envFilePath); err == nil {
			for k, val := range v {
				values[k] = val
			}
		}
	}
	if b.summaryInterval > 0 || values["SUMMARY_INTERVAL"] == "" {
		values["SUMMARY_INTERVAL"] = strconv.Itoa(b.summaryInterval)
	}
	if b.admin != nil {
		values["ADMIN_MODEL"] = b.admin.model
	}
	return values
}

// applyConfigChange applies a Mutable change to the live bridge without
// requiring a restart.
func (b *Bridge) applyConfigChange(key, value string) {
	switch key {
	case "SUMMARY_INTERVAL":
		if n, err := strconv.Atoi(value); err == nil {
			b.mu.Lock()
			b.summaryInterval = n
			b.mu.Unlock()
		}
	case "ADMIN_MODEL":
		if b.admin != nil {
			b.admin.setModel(value)
			b.admin.destroy()
		}
	case "GATEWAY_DEFAULT_CWD":
		dir := expandHome(value)
		b.mu.Lock()
		b.defaultCWD = dir
		b.mu.Unlock()
		b.mgr.AddAllowedBaseDir(dir)
		b.mgr.SetDefaultWorkingDir(dir)
		if b.admin != nil {
			b.admin.setWorkingDir(dir)
		}
	case "GATEWAY_PERMISSION_MODE":
		b.mgr.SetDefaultPermissionMode(NormalizePermissionMode(value))
	case "GATEWAY_SHARE_EXTERNAL_SESSIONS":
		b.mu.Lock()
		b.shareExternal = parseConfigBool(value)
		b.mu.Unlock()
	case "GATEWAY_DISCOVERY_WINDOW_DAYS":
		if n, err := strconv.Atoi(value); err == nil {
			b.mu.Lock()
			b.discoveryWindowDays = n
			b.mu.Unlock()
		}
	case "GATEWAY_DISCOVERY_RESCAN_INTERVAL":
		if d, err := time.ParseDuration(value); err == nil {
			b.mu.Lock()
			b.rescanInterval = d
			b.mu.Unlock()
		}
	case "FEISHU_ALLOWED_USER_IDS":
		if b.applyAllowedUsers != nil {
			var ids []string
			for _, id := range strings.Split(value, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					ids = append(ids, id)
				}
			}
			b.applyAllowedUsers(ids)
		}
	case "CLAUDE_CLI_PATH":
		if b.applyCLIPath != nil && value != "" {
			b.applyCLIPath(value)
		}
	}
}

// parseConfigBool mirrors the env-var bool parsing in config.go.
func parseConfigBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

func buildConfigCard(values map[string]string, envPath string) channel.Card {
	var required, optional []ConfigField
	for _, field := range ConfigFields {
		if field.Required {
			required = append(required, field)
		} else {
			optional = append(optional, field)
		}
	}

	var sections []channel.Section
	// Only render the "必填配置" heading when there's something to put under
	// it — otherwise the empty heading looks broken to the user.
	if len(required) > 0 {
		sections = append(sections, channel.Section{Markdown: "**必填配置:**"})
		for _, field := range required {
			sections = append(sections, configFieldSection(field, values[field.EnvKey]))
		}
		sections = append(sections, channel.Section{Divider: true, Markdown: "**可选配置:**"})
	}
	for _, field := range optional {
		sections = append(sections, configFieldSection(field, values[field.EnvKey]))
	}
	sections = append(sections, channel.Section{
		Divider: true,
		Note:    fmt.Sprintf("配置文件: %s | 也可用 /config set KEY VALUE", envPath),
	})
	return channel.Card{Title: "Gateway Config", Tone: channel.ToneInfo, Sections: sections}
}

func configFieldSection(field ConfigField, val string) channel.Section {
	display := formatConfigValue(val, field)
	if display == "(未设置)" && field.Default != "" {
		display = field.Default + " (默认)"
	}
	tag := ""
	if !field.Required {
		if field.Mutable {
			tag = " · <font color='green'>运行时可改</font>"
		} else {
			tag = " · <font color='grey'>需重启</font>"
		}
	}
	header := fmt.Sprintf("**%s**%s\n`%s` = `%s`", field.Label, tag, field.EnvKey, display)
	if field.Required {
		status := "✅"
		if val == "" {
			status = "⚠️"
		}
		header = fmt.Sprintf("%s **%s**%s\n`%s` = `%s`", status, field.Label, tag, field.EnvKey, display)
	}
	style := "default"
	if field.Required {
		style = "primary"
	}
	return channel.Section{
		Markdown: header,
		Buttons: []channel.Button{{
			Label:  "修改",
			Style:  style,
			Action: map[string]string{"action": "edit_config", "key": field.EnvKey},
		}},
	}
}

func formatConfigValue(val string, field ConfigField) string {
	if val == "" {
		return "(未设置)"
	}
	if field.Sensitive {
		if len(val) <= 4 {
			return "****"
		}
		end := 4 + 12
		if end > len(val) {
			end = len(val)
		}
		return val[:4] + strings.Repeat("*", end-4)
	}
	return val
}

// --- !shell ---

func (b *Bridge) handleShell(ctx context.Context, m channel.InboundMessage, cmdStr string) {
	wd := b.defaultCWD
	if focused, ok := b.mgr.FocusedSession(m.UserID); ok {
		if focused.WorkingDir != "" {
			wd = focused.WorkingDir
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

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

// --- /setup ---

func (b *Bridge) handleSetup(ctx context.Context, m channel.InboundMessage, content string) {
	if b.admin == nil || b.envFilePath == "" {
		b.replyCard(ctx, m, channel.Card{
			Title: "Welcome",
			Tone:  channel.ToneWarning,
			Sections: []channel.Section{{
				Markdown: "Gateway 缺少必要组件(admin / env 文件),无法接受配置。请检查启动参数 / 联系管理员。",
			}},
		})
		return
	}
	configs, err := b.parseConfigFromNL(ctx, content)
	if err != nil || len(configs) == 0 {
		b.replyCard(ctx, m, channel.Card{
			Title: "Welcome",
			Tone:  channel.ToneWarning,
			Sections: []channel.Section{{
				Markdown: "请配置工作目录,例如:\n「工作目录 /Users/me/projects 项目根目录也是这个」",
			}},
		})
		return
	}
	if err := WriteEnvFile(b.envFilePath, configs); err != nil {
		b.replyText(ctx, m, "写入配置失败: "+err.Error())
		return
	}
	var lines []string
	needRestart := false
	for key, val := range configs {
		field, _ := FindConfigField(key)
		if field.Mutable {
			b.applyConfigChange(key, val)
		} else {
			needRestart = true
		}
		lines = append(lines, fmt.Sprintf("- **%s** = `%s`", field.Label, val))
	}
	msg := "**配置已保存:**\n" + strings.Join(lines, "\n")
	if needRestart {
		msg += "\n\n⚠️ 部分配置需重启后生效。"
	} else {
		msg += "\n\n✅ 已热生效,无需重启。"
	}
	msg += "\n\n现在可以直接发消息开始对话了。"
	b.replyCard(ctx, m, channel.Card{
		Title:    "Config Saved",
		Tone:     channel.ToneSuccess,
		Sections: []channel.Section{{Markdown: msg}},
	})
}

// --- /rename ---

// cmdRename sets a custom display title on the focused session and forwards
// the same command to the CLI so its internal session name stays in sync.
func (b *Bridge) cmdRename(ctx context.Context, m channel.InboundMessage, args string) {
	title := strings.TrimSpace(args)
	if title == "" {
		b.replyText(ctx, m, "用法: /rename <新名字>")
		return
	}

	// /rename only mutates session metadata (CustomTitle); the CLI doesn't
	// need to be live. Pass mustBeActive=false so a focused-idle (or
	// fallback resumable-idle) session is reused as-is without paying the
	// 10–30s reactivate cost just to update a display name.
	sess, err := b.ensureCurrentSession(ctx, m, false)
	if err != nil {
		b.replyText(ctx, m, err.Error())
		return
	}

	if err := b.mgr.SetCustomTitle(sess.ID, title); err != nil {
		b.replyText(ctx, m, "重命名失败: "+err.Error())
		return
	}
	b.saveStateIfPossible()
	b.replyText(ctx, m, fmt.Sprintf("已重命名为 **%s** (session %s)",
		title, displaySessionID(sess)))
}

// limitedWriter caps how many bytes are written to its buffer.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}

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
