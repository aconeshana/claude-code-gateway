package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/cron"
)

// cmdCron handles the /cron slash command.
func (b *Bridge) cmdCron(ctx context.Context, m channel.InboundMessage, args string) {
	if b.cronStore == nil {
		b.replyText(ctx, m, "定时任务功能未启用")
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		card := b.buildCronManageCard(m.UserID, "")
		b.replyCard(ctx, m, card)
		return
	}

	parts := strings.SplitN(args, " ", 2)
	sub := strings.ToLower(parts[0])
	subArgs := ""
	if len(parts) > 1 {
		subArgs = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "list":
		text := buildCronListText(b.cronStore, m.UserID)
		b.replyText(ctx, m, text)

	case "add":
		b.cronAdd(ctx, m, subArgs)

	case "remove", "rm", "delete":
		b.cronRemove(ctx, m, subArgs)

	case "enable":
		b.cronSetEnabled(ctx, m, subArgs, true)

	case "disable":
		b.cronSetEnabled(ctx, m, subArgs, false)

	case "history":
		b.cronHistory(ctx, m, subArgs)

	default:
		b.replyText(ctx, m, "未知子命令: "+sub+"\n用法: /cron [list|add|remove|enable|disable|history]")
	}
}

// cronAdd handles /cron add "<expr>" <prompt>
func (b *Bridge) cronAdd(ctx context.Context, m channel.InboundMessage, args string) {
	if args == "" {
		b.replyText(ctx, m, "用法: /cron add \"<cron表达式>\" <提示词>\n例: /cron add \"*/5 * * * *\" 检查构建状态")
		return
	}

	expr, prompt, err := parseCronAddArgs(args)
	if err != nil {
		b.replyText(ctx, m, "参数解析错误: "+err.Error())
		return
	}

	if _, err := cron.ParseSchedule(expr); err != nil {
		b.replyText(ctx, m, "cron 表达式无效: "+err.Error())
		return
	}

	j := cron.NewJob("", m.UserID, m.ChatID, expr, prompt, b.defaultCWD, "")
	if err := b.cronStore.Add(j); err != nil {
		b.replyText(ctx, m, "创建失败: "+err.Error())
		return
	}
	if b.cronScheduler != nil {
		b.cronScheduler.Reload()
	}
	b.replyText(ctx, m, fmt.Sprintf("✅ 已创建定时任务 %s\n表达式: `%s`\n提示词: %s", shortID(j.ID), expr, prompt))
}

// cronRemove handles /cron remove <id-prefix>
func (b *Bridge) cronRemove(ctx context.Context, m channel.InboundMessage, idPrefix string) {
	if idPrefix == "" {
		b.replyText(ctx, m, "用法: /cron remove <任务ID前缀>")
		return
	}
	j, ok := findJobByPrefix(b.cronStore, idPrefix)
	if !ok {
		b.replyText(ctx, m, "未找到匹配的任务: "+idPrefix)
		return
	}
	if err := b.cronStore.Remove(j.ID); err != nil {
		b.replyText(ctx, m, "删除失败: "+err.Error())
		return
	}
	if b.cronRunLog != nil {
		b.cronRunLog.Purge(j.ID)
	}
	if b.cronScheduler != nil {
		b.cronScheduler.Reload()
	}
	b.replyText(ctx, m, "✅ 已删除任务 "+shortID(j.ID))
}

// cronSetEnabled handles /cron enable|disable <id-prefix>
func (b *Bridge) cronSetEnabled(ctx context.Context, m channel.InboundMessage, idPrefix string, enabled bool) {
	if idPrefix == "" {
		verb := "enable"
		if !enabled {
			verb = "disable"
		}
		b.replyText(ctx, m, "用法: /cron "+verb+" <任务ID前缀>")
		return
	}
	j, ok := findJobByPrefix(b.cronStore, idPrefix)
	if !ok {
		b.replyText(ctx, m, "未找到匹配的任务: "+idPrefix)
		return
	}
	if err := b.cronStore.SetEnabled(j.ID, enabled); err != nil {
		b.replyText(ctx, m, "操作失败: "+err.Error())
		return
	}
	if b.cronScheduler != nil {
		b.cronScheduler.Reload()
	}
	action := "启用"
	if !enabled {
		action = "禁用"
	}
	b.replyText(ctx, m, fmt.Sprintf("✅ 已%s任务 %s", action, shortID(j.ID)))
}

// cronHistory handles /cron history [id-prefix]
func (b *Bridge) cronHistory(ctx context.Context, m channel.InboundMessage, idPrefix string) {
	if b.cronRunLog == nil {
		b.replyText(ctx, m, "运行历史未启用")
		return
	}
	if idPrefix == "" {
		records := b.cronRunLog.AllHistory(15)
		if len(records) == 0 {
			b.replyText(ctx, m, "暂无运行记录")
			return
		}
		var lines []string
		for _, r := range records {
			icon := "✅"
			if r.Status == "error" {
				icon = "❌"
			} else if r.Status == "timeout" {
				icon = "⏰"
			}
			lines = append(lines, fmt.Sprintf("%s %s  %s  %.1fs",
				icon, shortID(r.JobID), r.StartedAt.Format("01-02 15:04"), r.DurationS))
		}
		b.replyText(ctx, m, strings.Join(lines, "\n"))
		return
	}

	j, ok := findJobByPrefix(b.cronStore, idPrefix)
	if !ok {
		b.replyText(ctx, m, "未找到匹配的任务: "+idPrefix)
		return
	}
	card := b.buildCronHistoryCard(j.ID)
	b.replyCard(ctx, m, card)
}

// handleCronCardAction dispatches card button/form actions for the cron module.
func (b *Bridge) handleCronCardAction(ctx context.Context, m channel.InboundMessage) bool {
	if m.Action == nil || b.cronStore == nil {
		return false
	}

	switch m.Action.Name {
	case "cron_enable":
		id, _ := m.Action.Values["job_id"].(string)
		if id == "" {
			return false
		}
		if err := b.cronStore.SetEnabled(id, true); err != nil {
			b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "启用失败: "+err.Error()))
			return true
		}
		if b.cronScheduler != nil {
			b.cronScheduler.Reload()
		}
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, ""))
		return true

	case "cron_disable":
		id, _ := m.Action.Values["job_id"].(string)
		if id == "" {
			return false
		}
		if err := b.cronStore.SetEnabled(id, false); err != nil {
			b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "禁用失败: "+err.Error()))
			return true
		}
		if b.cronScheduler != nil {
			b.cronScheduler.Reload()
		}
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, ""))
		return true

	case "cron_remove":
		id, _ := m.Action.Values["job_id"].(string)
		if id == "" {
			return false
		}
		if err := b.cronStore.Remove(id); err != nil {
			b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "删除失败: "+err.Error()))
			return true
		}
		if b.cronRunLog != nil {
			b.cronRunLog.Purge(id)
		}
		if b.cronScheduler != nil {
			b.cronScheduler.Reload()
		}
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, ""))
		return true

	case "cron_history":
		id, _ := m.Action.Values["job_id"].(string)
		if id == "" {
			return false
		}
		card := b.buildCronHistoryCard(id)
		b.replyCard(ctx, m, card)
		return true

	case "cron_create_submit":
		b.handleCronCreateForm(ctx, m)
		return true

	default:
		return false
	}
}

// handleCronCreateForm processes the create-job form submission.
// All validation errors are shown inline in the card (via m.Reply) — they
// never leak to the main chat as a separate message.
func (b *Bridge) handleCronCreateForm(ctx context.Context, m channel.InboundMessage) {
	fv := m.Action.FormValue
	if fv == nil {
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "表单数据为空"))
		return
	}
	expr, _ := fv["expr"].(string)
	workDir, _ := fv["work_dir"].(string)
	prompt, _ := fv["prompt"].(string)
	desc, _ := fv["description"].(string)

	expr = strings.TrimSpace(expr)
	prompt = strings.TrimSpace(prompt)
	workDir = strings.TrimSpace(workDir)

	if expr == "" || prompt == "" {
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "Cron表达式和提示词不能为空"))
		return
	}

	if _, err := cron.ParseSchedule(expr); err != nil {
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "cron 表达式无效: "+err.Error()))
		return
	}

	if workDir == "" {
		workDir = b.defaultCWD
	}

	j := cron.NewJob("", m.UserID, m.ChatID, expr, prompt, workDir, desc)
	if err := b.cronStore.Add(j); err != nil {
		b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, "创建失败: "+err.Error()))
		return
	}
	if b.cronScheduler != nil {
		b.cronScheduler.Reload()
	}

	log.Printf("[cron] job created: id=%s expr=%s owner=%s", shortID(j.ID), expr, shortID(m.UserID))

	b.cronReplyCard(ctx, m, b.buildCronManageCard(m.UserID, ""))
}

// cronReplyCard edits the original card in place when m.Reply is available
// (synchronous Lark callback), otherwise sends a new card. This keeps all
// cron card interactions in a single message — no secondary chat noise.
func (b *Bridge) cronReplyCard(ctx context.Context, m channel.InboundMessage, card channel.Card) {
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	b.replyCard(ctx, m, card)
}

// --- helpers ---

// findJobByPrefix finds a job whose ID starts with the given prefix.
func findJobByPrefix(store cron.Store, prefix string) (cron.Job, bool) {
	prefix = strings.ToLower(prefix)
	for _, j := range store.List() {
		if strings.HasPrefix(strings.ToLower(j.ID), prefix) {
			return j, true
		}
	}
	return cron.Job{}, false
}

// parseCronAddArgs splits "/cron add" arguments into expression + prompt.
// Supports both quoted and unquoted expression formats:
//
//	"*/5 * * * *" check build   → expr="*/5 * * * *", prompt="check build"
//	*/5 * * * * check build     → expr="*/5 * * * *", prompt="check build"
func parseCronAddArgs(args string) (expr, prompt string, err error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", "", fmt.Errorf("empty arguments")
	}

	// Quoted: "expr" prompt
	if args[0] == '"' {
		end := strings.Index(args[1:], "\"")
		if end < 0 {
			return "", "", fmt.Errorf("未闭合的引号")
		}
		expr = args[1 : end+1]
		prompt = strings.TrimSpace(args[end+2:])
		if prompt == "" {
			return "", "", fmt.Errorf("缺少提示词")
		}
		return expr, prompt, nil
	}

	// Unquoted: first 5 space-separated tokens are the cron fields.
	tokens := strings.Fields(args)
	if len(tokens) < 6 {
		return "", "", fmt.Errorf("需要至少6部分: 5个cron字段 + 提示词")
	}
	expr = strings.Join(tokens[:5], " ")
	prompt = strings.Join(tokens[5:], " ")
	return expr, prompt, nil
}
