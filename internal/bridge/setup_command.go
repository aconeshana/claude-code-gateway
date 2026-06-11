package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

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
