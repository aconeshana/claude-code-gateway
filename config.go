package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/anthropics/claude-code-gateway/internal/bridge"
	dingtalkCh "github.com/anthropics/claude-code-gateway/internal/channel/dingtalk"
	feishuCh "github.com/anthropics/claude-code-gateway/internal/channel/feishu"
)

type Config struct {
	ListenAddr          string            `json:"listen_addr"`
	CLIPath             string            `json:"cli_path"`
	DefaultWorkingDir   string            `json:"default_working_dir"`
	ProjectRoot         string            `json:"project_root"`
	PermissionMode      string            `json:"permission_mode"`
	MaxSessions         int               `json:"max_sessions"`
	SessionIdleTimeout  time.Duration     `json:"session_idle_timeout"`
	PermissionTimeout   time.Duration     `json:"permission_timeout"`
	KeepAliveInterval   time.Duration     `json:"keep_alive_interval"`
	ProcessStartTimeout time.Duration     `json:"process_start_timeout"`
	WSWriteTimeout      time.Duration     `json:"ws_write_timeout"`
	WSPongTimeout       time.Duration     `json:"ws_pong_timeout"`
	WSPingInterval      time.Duration     `json:"ws_ping_interval"`
	AuthToken           string            `json:"-"`
	CLIEnv              map[string]string `json:"cli_env"`
	Feishu              feishuCh.Config   `json:"feishu"`
	DingTalk            dingtalkCh.Config `json:"dingtalk"`
	SummaryInterval     int               `json:"summary_interval"`
	AdminModel          string            `json:"admin_model"`
	EnvFilePath         string            `json:"-"`

	// Discovery
	ShareExternalSessions   bool          `json:"share_external_sessions"`
	DiscoveryWindowDays     int           `json:"discovery_window_days"`
	DiscoveryRescanInterval time.Duration `json:"discovery_rescan_interval"`
}

func DefaultConfig() *Config {
	return &Config{
		ListenAddr:              ":8080",
		CLIPath:                 "claude",
		DefaultWorkingDir:       ".",
		PermissionMode:          "auto",
		MaxSessions:             10,
		SessionIdleTimeout:      30 * time.Minute,
		PermissionTimeout:       120 * time.Second,
		KeepAliveInterval:       30 * time.Second,
		ProcessStartTimeout:     30 * time.Second,
		WSWriteTimeout:          10 * time.Second,
		WSPongTimeout:           60 * time.Second,
		WSPingInterval:          30 * time.Second,
		DiscoveryWindowDays:     7,
		DiscoveryRescanInterval: 5 * time.Minute,
	}
}

func LoadConfig(configPath string) (*Config, error) {
	cfg := DefaultConfig()

	envPath := findEnvFile()
	cfg.EnvFilePath = envPath
	if envPath != "" {
		_ = godotenv.Load(envPath)
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	if v := os.Getenv("GATEWAY_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("CLAUDE_CLI_PATH"); v != "" {
		cfg.CLIPath = v
	}
	if v := os.Getenv("GATEWAY_DEFAULT_CWD"); v != "" {
		cfg.DefaultWorkingDir = expandHomePath(v)
	}
	if v := os.Getenv("GATEWAY_PROJECT_ROOT"); v != "" {
		cfg.ProjectRoot = expandHomePath(v)
	}
	if v := os.Getenv("GATEWAY_PERMISSION_MODE"); v != "" {
		cfg.PermissionMode = v
	}
	cfg.PermissionMode = bridge.NormalizePermissionMode(cfg.PermissionMode)
	if v := os.Getenv("GATEWAY_MAX_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxSessions = n
		}
	}
	if v := os.Getenv("GATEWAY_SESSION_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SessionIdleTimeout = d
		}
	}
	if v := os.Getenv("GATEWAY_PERMISSION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.PermissionTimeout = d
		}
	}
	if v := os.Getenv("GATEWAY_AUTH_TOKEN"); v != "" {
		cfg.AuthToken = v
	}
	if v := os.Getenv("FEISHU_APP_ID"); v != "" {
		cfg.Feishu.AppID = v
	}
	if v := os.Getenv("FEISHU_APP_SECRET"); v != "" {
		cfg.Feishu.AppSecret = v
	}

	if v := os.Getenv("SUMMARY_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SummaryInterval = n
		}
	}
	if cfg.SummaryInterval <= 0 && os.Getenv("SUMMARY_INTERVAL") == "" {
		cfg.SummaryInterval = 5
	}

	if v := os.Getenv("ADMIN_MODEL"); v != "" {
		cfg.AdminModel = v
	}
	if cfg.AdminModel == "" {
		cfg.AdminModel = "claude-haiku-4-5"
	}

	if v := os.Getenv("FEISHU_ALLOWED_USER_IDS"); v != "" {
		var ids []string
		for _, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
		cfg.Feishu.AllowedUserIDs = ids
	}

	if v := os.Getenv("DINGTALK_APP_KEY"); v != "" {
		cfg.DingTalk.AppKey = v
	}
	if v := os.Getenv("DINGTALK_APP_SECRET"); v != "" {
		cfg.DingTalk.AppSecret = v
	}
	if v := os.Getenv("DINGTALK_ALLOWED_USER_IDS"); v != "" {
		var ids []string
		for _, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
		cfg.DingTalk.AllowedUserIDs = ids
	}

	if v := os.Getenv("GATEWAY_SHARE_EXTERNAL_SESSIONS"); v != "" {
		cfg.ShareExternalSessions = parseBool(v)
	}
	if v := os.Getenv("GATEWAY_DISCOVERY_WINDOW_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.DiscoveryWindowDays = n
		}
	}
	if v := os.Getenv("GATEWAY_DISCOVERY_RESCAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.DiscoveryRescanInterval = d
		}
	}

	return cfg, nil
}

// parseBool accepts true/yes/on/1 (case-insensitive) as true; anything else
// is false. Used for env-var booleans where we want lenient parsing.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

func findEnvFile() string {
	// 1. ~/.ccg/.env — written by 'gateway register'
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".ccg", ".env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// 2. <binary_dir>/.env — legacy / manual placement
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), ".env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// 3. <cwd>/.env
	if _, err := os.Stat(".env"); err == nil {
		abs, err := filepath.Abs(".env")
		if err == nil {
			return abs
		}
		return ".env"
	}

	return ""
}

func expandHomePath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return path
}
