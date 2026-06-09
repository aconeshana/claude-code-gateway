package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

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

// --- Config edit card (powered by the edit_config / save_config / save_config_value actions) ---

// handleConfigCardAction dispatches edit_config / save_config / save_config_value.
// Returns true when claimed.
func (b *Bridge) handleConfigCardAction(ctx context.Context, m channel.InboundMessage) bool {
	switch m.Action.Name {
	case "edit_config":
		if key, ok := m.Action.Values["key"].(string); ok {
			field, found := FindConfigField(key)
			if !found {
				b.replyText(ctx, m, "未知配置项")
				return true
			}
			values := b.currentConfigValues()
			b.replyCard(ctx, m, buildConfigEditCard(field, values[key]))
		}
	case "save_config":
		key, _ := m.Action.Values["key"].(string)
		if key == "" {
			return true
		}
		value := ""
		if v, ok := m.Action.FormValue["config_value"]; ok {
			if s, ok := v.(string); ok {
				value = s
			}
		}
		b.persistConfigChange(ctx, m, key, value)
	case "save_config_value":
		// Used by bool/enum buttons that carry the new value inline.
		key, _ := m.Action.Values["key"].(string)
		value, _ := m.Action.Values["value"].(string)
		if key == "" {
			return true
		}
		b.persistConfigChange(ctx, m, key, value)
	default:
		return false
	}
	return true
}

func buildConfigEditCard(field ConfigField, currentValue string) channel.Card {
	desc := fmt.Sprintf("**%s** (`%s`)", field.Label, field.EnvKey)
	if currentValue != "" {
		display := currentValue
		if field.Sensitive {
			display = formatConfigValue(currentValue, field)
		}
		desc += fmt.Sprintf("\n当前值: `%s`", display)
	}
	if field.Default != "" {
		desc += fmt.Sprintf("\n默认值: `%s`", field.Default)
	}
	if field.Mutable {
		desc += "\n修改后立即生效"
	} else {
		desc += "\n修改后需重启生效"
	}

	// Bool / enum: render one button per choice, no form input needed.
	switch field.Type {
	case "bool":
		return channel.Card{
			Title:    "修改配置",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: desc, Buttons: boolButtons(field.EnvKey, currentValue)}},
		}
	case "enum":
		return channel.Card{
			Title:    "修改配置",
			Tone:     channel.ToneInfo,
			Sections: []channel.Section{{Markdown: desc, Buttons: enumButtons(field.EnvKey, field.EnumValues, currentValue)}},
		}
	}

	// Default: free-form text via a form.
	return channel.Card{
		Title: "修改配置",
		Tone:  channel.ToneInfo,
		Sections: []channel.Section{{
			Markdown: desc,
			Form: &channel.Form{
				FormID: "config_form",
				Fields: []channel.FormField{{Name: "config_value", Placeholder: "请输入新值"}},
				Submit: channel.Button{
					Label:  "保存",
					Style:  "primary",
					Action: map[string]string{"action": "save_config", "key": field.EnvKey},
				},
			},
		}},
	}
}

func boolButtons(key, current string) []channel.Button {
	current = strings.ToLower(strings.TrimSpace(current))
	makeBtn := func(label, value, style string, active bool) channel.Button {
		if active {
			label = "✓ " + label
		}
		return channel.Button{
			Label:  label,
			Style:  style,
			Action: map[string]string{"action": "save_config_value", "key": key, "value": value},
		}
	}
	return []channel.Button{
		makeBtn("开启", "true", "primary", current == "true"),
		makeBtn("关闭", "false", "default", current != "true"),
	}
}

func enumButtons(key string, values []string, current string) []channel.Button {
	btns := make([]channel.Button, 0, len(values))
	for _, v := range values {
		label := v
		style := "default"
		if v == current {
			label = "✓ " + v
			style = "primary"
		}
		btns = append(btns, channel.Button{
			Label:  label,
			Style:  style,
			Action: map[string]string{"action": "save_config_value", "key": key, "value": v},
		})
	}
	return btns
}

func looksLikePath(s string) bool {
	return strings.Contains(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".")
}

// persistConfigChange writes the new value to .env, applies it at runtime
// when the field is Mutable, and reports the outcome back to the user.
//
// When the user came from an edit card (m.MessageID set), the card is updated
// in place to show the new value and disable further submissions — this
// prevents accidental double-clicks and gives clear "saved" feedback.
func (b *Bridge) persistConfigChange(ctx context.Context, m channel.InboundMessage, key, value string) {
	if key == "GATEWAY_PERMISSION_MODE" {
		value = NormalizePermissionMode(value)
	}
	field, found := FindConfigField(key)
	if !found {
		b.replyConfigSave(ctx, m, "未知配置项", channel.ToneWarning)
		return
	}
	if b.envFilePath == "" {
		b.replyConfigSave(ctx, m, "未配置 .env,无法保存", channel.ToneWarning)
		return
	}
	if err := WriteEnvFile(b.envFilePath, map[string]string{key: value}); err != nil {
		b.replyConfigSave(ctx, m, "写入配置失败: "+err.Error(), channel.ToneWarning)
		return
	}
	hint := "已写入 .env,重启后生效"
	if field.Mutable {
		b.applyConfigChange(key, value)
		hint = "已写入 .env(已运行时生效)"
	}
	body := fmt.Sprintf("✅ **%s**\n`%s` = `%s`\n%s", field.Label, key, value, hint)
	b.replyConfigSave(ctx, m, body, channel.ToneSuccess)
}

// replyConfigSave updates the originating edit card. Uses the synchronous
// Reply hook when available (preferred — atomic with the form-submit
// response) and falls back to UpdateMessage / new card otherwise.
func (b *Bridge) replyConfigSave(ctx context.Context, m channel.InboundMessage, body string, tone channel.Tone) {
	card := channel.Card{
		Title:    "配置已保存",
		Tone:     tone,
		Sections: []channel.Section{{Markdown: body}},
	}
	if m.Reply != nil {
		m.Reply(card)
		return
	}
	if m.MessageID != "" {
		if err := b.updateCard(ctx, m.MessageID, card); err == nil {
			return
		}
	}
	b.replyCard(ctx, m, card)
}
