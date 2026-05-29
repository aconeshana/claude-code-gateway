package bridge

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ConfigField describes one user-configurable setting. Used by /config to
// render the edit UI and validate keys.
type ConfigField struct {
	EnvKey    string
	Label     string
	Required  bool
	Sensitive bool
	Mutable   bool // can be changed at runtime without restart
	Default   string

	// Type controls the edit UI. Empty defaults to "string" (text input).
	// "bool" renders enable/disable buttons; "enum" renders one button per
	// EnumValues entry. The save_config_value card action persists the
	// chosen value without going through a form.
	Type       string
	EnumValues []string
}

// ConfigFields enumerates the settings exposed via /config.
var ConfigFields = []ConfigField{
	{EnvKey: "GATEWAY_DEFAULT_CWD", Label: "默认工作目录(主聊天 plain text 兜底)", Default: "~", Mutable: true},
	{EnvKey: "SUMMARY_INTERVAL", Label: "摘要更新轮数(每 N 条用户消息后重生成,0=关闭)", Mutable: true, Default: "5"},
	{EnvKey: "ADMIN_MODEL", Label: "管理员 AI 模型", Mutable: true, Default: "claude-haiku-4-5"},
	{EnvKey: "GATEWAY_SHARE_EXTERNAL_SESSIONS", Label: "共享外部 session(terminal/SDK 等创建的)", Default: "false", Type: "bool", Mutable: true},
	{EnvKey: "GATEWAY_DISCOVERY_WINDOW_DAYS", Label: "磁盘扫描时间窗口(天,0=全量)", Default: "7", Mutable: true},
	{EnvKey: "GATEWAY_DISCOVERY_RESCAN_INTERVAL", Label: "重新扫描间隔(如 5m)", Default: "5m", Mutable: true},
	{EnvKey: "FEISHU_ALLOWED_USER_IDS", Label: "允许的用户 ID(逗号分隔)", Mutable: true},
	{EnvKey: "FEISHU_APP_ID", Label: "飞书 App ID"},
	{EnvKey: "FEISHU_APP_SECRET", Label: "飞书 App Secret", Sensitive: true},
	{EnvKey: "CLAUDE_CLI_PATH", Label: "Claude CLI 路径", Default: "claude", Mutable: true},
	{EnvKey: "GATEWAY_PERMISSION_MODE", Label: "权限模式", Mutable: true, Default: "auto", Type: "enum", EnumValues: []string{"auto", "forward"}},
	{EnvKey: "GATEWAY_LISTEN_ADDR", Label: "监听地址", Default: ":8080"},
	{EnvKey: "GATEWAY_AUTH_TOKEN", Label: "认证 Token", Sensitive: true},
}

// FindConfigField looks up a field by env key.
func FindConfigField(envKey string) (ConfigField, bool) {
	for _, f := range ConfigFields {
		if f.EnvKey == envKey {
			return f, true
		}
	}
	return ConfigField{}, false
}

// NormalizePermissionMode lowercases + trims user input. Wire values are
// the canonical PermissionXxx constants in runtime/claude; anything else
// passes through and the runtime layer rejects it.
func NormalizePermissionMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

// WriteEnvFile writes/updates KEY=VALUE pairs in a dotenv file, preserving
// comments and existing key order.
func WriteEnvFile(path string, updates map[string]string) error {
	existing, err := readLines(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	seen := make(map[string]bool)
	var result []string

	for _, line := range existing {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			result = append(result, line)
			continue
		}
		eqIdx := strings.IndexByte(trimmed, '=')
		if eqIdx < 0 {
			result = append(result, line)
			continue
		}
		key := strings.TrimSpace(trimmed[:eqIdx])
		if newVal, ok := updates[key]; ok {
			seen[key] = true
			result = append(result, fmt.Sprintf("%s=%s", key, newVal))
		} else {
			result = append(result, line)
		}
	}

	for key, val := range updates {
		if !seen[key] {
			result = append(result, fmt.Sprintf("%s=%s", key, val))
		}
	}

	content := strings.Join(result, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0600)
}

// ParseEnvValues reads a dotenv file and returns a key→value map. Missing
// file is treated as empty (returns nil, no error).
func ParseEnvValues(path string) (map[string]string, error) {
	lines, err := readLines(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx >= 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, `"'`)
			values[key] = val
		}
	}
	return values, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return strings.Replace(path, "~", home, 1)
		}
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return path
}
