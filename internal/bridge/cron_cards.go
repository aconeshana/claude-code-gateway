package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/cron"
)

// buildCronManageCard builds the /cron management card with job list + create form.
// errMsg, when non-empty, is shown as a warning note at the top of the card so
// validation errors stay in-card instead of leaking to the main chat.
func (b *Bridge) buildCronManageCard(ownerID string, errMsg string) channel.Card {
	if b.cronStore == nil {
		return channel.Card{
			Title: "定时任务",
			Tone:  channel.ToneWarning,
			Sections: []channel.Section{{
				Markdown: "定时任务功能未启用",
			}},
		}
	}

	jobs := b.cronStore.List()
	var sections []channel.Section

	// Show validation error inline so it doesn't leak to main chat.
	if errMsg != "" {
		sections = append(sections, channel.Section{
			Note: "⚠️ " + errMsg,
		})
	}

	// Filter to owner's jobs.
	var owned []cron.Job
	for _, j := range jobs {
		if j.OwnerID == ownerID {
			owned = append(owned, j)
		}
	}

	if len(owned) == 0 {
		sections = append(sections, channel.Section{
			Markdown: "暂无定时任务",
		})
	} else {
		for _, j := range owned {
			icon := "✅"
			if !j.Enabled {
				icon = "⏸"
			}
			desc := j.Description
			if desc == "" {
				desc = truncateRunes(j.Prompt, 40)
			}

			nextStr := "—"
			if j.NextRun != nil {
				nextStr = j.NextRun.Format("01-02 15:04")
			}

			md := fmt.Sprintf("%s **%s** `%s`\n%s\n下次: %s",
				icon, shortID(j.ID), j.Expr, desc, nextStr)

			var buttons []channel.Button
			if j.Enabled {
				buttons = append(buttons, channel.Button{
					Label: "禁用",
					Style: "default",
					Action: map[string]string{
						"action": "cron_disable",
						"job_id": j.ID,
					},
				})
			} else {
				buttons = append(buttons, channel.Button{
					Label: "启用",
					Style: "primary",
					Action: map[string]string{
						"action": "cron_enable",
						"job_id": j.ID,
					},
				})
			}
			buttons = append(buttons,
				channel.Button{
					Label: "历史",
					Style: "default",
					Action: map[string]string{
						"action": "cron_history",
						"job_id": j.ID,
					},
				},
				channel.Button{
					Label: "删除",
					Style: "danger",
					Action: map[string]string{
						"action": "cron_remove",
						"job_id": j.ID,
					},
				},
			)
			sections = append(sections, channel.Section{
				Markdown: md,
				Buttons:  buttons,
			})
		}
	}

	// Divider + create form.
	sections = append(sections, channel.Section{Divider: true})
	sections = append(sections, channel.Section{
		Form: &channel.Form{
			FormID: "cron_create_form",
			Fields: []channel.FormField{
				{Name: "expr", Label: "Cron表达式", Placeholder: "*/5 * * * *"},
				{Name: "work_dir", Label: "工作目录", Placeholder: b.defaultCWD, Initial: b.defaultCWD},
				{Name: "prompt", Label: "提示词", Placeholder: "检查构建状态"},
				{Name: "description", Label: "描述(可选)", Placeholder: "每5分钟检查一次"},
			},
			Submit: channel.Button{
				Label: "创建",
				Style: "primary",
				Action: map[string]string{
					"action": "cron_create_submit",
				},
			},
		},
	})

	return channel.Card{
		Title:    "定时任务管理",
		Tone:     channel.ToneInfo,
		Sections: sections,
	}
}

// buildCronHistoryCard shows recent run records for a single job.
func (b *Bridge) buildCronHistoryCard(jobID string) channel.Card {
	records := b.cronRunLog.History(jobID)
	j, found := b.cronStore.Get(jobID)

	title := "运行历史"
	if found {
		title = fmt.Sprintf("运行历史 — %s", shortID(j.ID))
	}

	if len(records) == 0 {
		return channel.Card{
			Title: title,
			Tone:  channel.ToneNeutral,
			Sections: []channel.Section{{
				Markdown: "暂无运行记录",
			}},
		}
	}

	var sections []channel.Section
	for i := len(records) - 1; i >= 0 && i >= len(records)-10; i-- {
		r := records[i]
		icon := "✅"
		if r.Status == "error" {
			icon = "❌"
		} else if r.Status == "timeout" {
			icon = "⏰"
		}
		md := fmt.Sprintf("%s %s  耗时 %.1fs",
			icon, r.StartedAt.Format("01-02 15:04"), r.DurationS)
		if r.Error != "" {
			md += fmt.Sprintf("\n错误: %s", truncateRunes(r.Error, 80))
		}
		if r.Summary != "" {
			md += fmt.Sprintf("\n摘要: %s", truncateRunes(r.Summary, 80))
		}
		sections = append(sections, channel.Section{Markdown: md})
	}

	return channel.Card{
		Title:    title,
		Tone:     channel.ToneNeutral,
		Sections: sections,
	}
}

// buildCronResultCard creates a notification card for a finished cron job.
func (b *Bridge) buildCronResultCard(j cron.Job, r cron.ExecResult) channel.Card {
	tone := channel.ToneSuccess
	status := "✅ 完成"
	if r.Err != nil {
		tone = channel.ToneError
		status = "❌ 失败"
	}

	desc := j.Description
	if desc == "" {
		desc = truncateRunes(j.Prompt, 40)
	}

	md := fmt.Sprintf("**%s** %s\n`%s` · %s · 耗时 %.1fs",
		status, desc, j.Expr, shortID(j.ID), r.Duration.Seconds())
	if r.Err != nil {
		md += fmt.Sprintf("\n错误: %s", truncateRunes(r.Err.Error(), 100))
	}
	if r.Summary != "" {
		md += fmt.Sprintf("\n\n%s", truncateRunes(r.Summary, 300))
	}

	return channel.Card{
		Title:    "定时任务执行结果",
		Tone:     tone,
		Sections: []channel.Section{{Markdown: md}},
	}
}

// buildCronListText returns a plain-text listing of all jobs for the owner.
func buildCronListText(store cron.Store, ownerID string) string {
	jobs := store.List()
	var lines []string
	for _, j := range jobs {
		if j.OwnerID != ownerID {
			continue
		}
		icon := "✅"
		if !j.Enabled {
			icon = "⏸"
		}
		desc := j.Description
		if desc == "" {
			desc = truncateRunes(j.Prompt, 30)
		}
		nextStr := "—"
		if j.NextRun != nil {
			nextStr = j.NextRun.Format("01-02 15:04")
		}
		lines = append(lines, fmt.Sprintf("%s %s  `%s`  %s  下次: %s",
			icon, shortID(j.ID), j.Expr, desc, nextStr))
	}
	if len(lines) == 0 {
		return "暂无定时任务"
	}
	return strings.Join(lines, "\n")
}

// formatDuration renders a duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
