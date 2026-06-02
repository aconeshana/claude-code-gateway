package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/bridge"
	dingtalkCh "github.com/anthropics/claude-code-gateway/internal/channel/dingtalk"
	feishuCh "github.com/anthropics/claude-code-gateway/internal/channel/feishu"
	"github.com/anthropics/claude-code-gateway/internal/cron"
	"github.com/anthropics/claude-code-gateway/internal/gateway"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/anthropics/claude-code-gateway/internal/session/persist"
)

const stateFileName = "gateway_state.json"

func cmdServe() {
	// One-time migration: move runtime files from binary dir into ~/.ccg/
	// before loading config so the migrated .env is picked up immediately.
	migrateRuntimeFiles()

	configPath := flag.String("config", "", "path to config file (JSON)")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("Claude Code Gateway starting")
	log.Printf("  CLI path: %s", cfg.CLIPath)
	log.Printf("  Listen: %s", cfg.ListenAddr)
	log.Printf("  Max sessions: %d", cfg.MaxSessions)
	log.Printf("  Permission mode: %s", cfg.PermissionMode)
	log.Printf("  Session idle timeout: %s", cfg.SessionIdleTimeout)

	rt := claude.NewRuntime(cfg.CLIPath)
	mgr := session.NewManager(
		rt,
		cfg.DefaultWorkingDir,
		cfg.PermissionMode,
		cfg.MaxSessions,
		cfg.KeepAliveInterval,
		cfg.SessionIdleTimeout,
	)

	registry := runtime.NewRegistry()
	registry.Register(claude.Factory{})

	srv := gateway.NewServer(
		mgr,
		registry,
		"claude",
		cfg.ListenAddr,
		cfg.AuthToken,
		cfg.WSWriteTimeout,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		newBridge       *bridge.Bridge
		channelShutdown func()
	)

	// Cron store + run history — shared by all channel paths.
	cronStore, cronRunLog := initCronState()

	if cfg.Feishu.AppID != "" && cfg.DingTalk.AppKey != "" {
		log.Fatalf("cannot enable both Feishu and DingTalk channels; configure only one")
	}

	if cfg.Feishu.AppID != "" {
		statePath := defaultStatePath()
		store := persist.NewJSONStore(statePath)
		mgr.SetSummaryStore(store)

		feiCh := feishuCh.New(feishuCh.Config{
			AppID:          cfg.Feishu.AppID,
			AppSecret:      cfg.Feishu.AppSecret,
			AllowedUserIDs: cfg.Feishu.AllowedUserIDs,
		})
		discoverer := claude.NewDiscoverer("", "")
		newBridge = bridge.New(bridge.Options{
			Manager:             mgr,
			Channel:             feiCh,
			DefaultCWD:          cfg.DefaultWorkingDir,
			EnvFilePath:         cfg.EnvFilePath,
			AdminModel:          cfg.AdminModel,
			SummaryInterval:     cfg.SummaryInterval,
			Persister:           store,
			Discoverer:          discoverer,
			ShareExternal:       cfg.ShareExternalSessions,
			DiscoveryWindowDays: cfg.DiscoveryWindowDays,
			RescanInterval:      cfg.DiscoveryRescanInterval,
			ApplyAllowedUsers:   feiCh.SetAllowedUserIDs,
			ApplyCLIPath:        rt.SetCLIPath,
			CronStore:           cronStore,
			CronRunLog:          cronRunLog,
		})
		newBridge.Start(ctx)
		channelShutdown = feiCh.Shutdown
		go func() {
			if err := feiCh.Start(ctx, newBridge); err != nil {
				log.Printf("[channel/feishu] error: %v", err)
			}
		}()
		log.Printf("  Feishu bridge: enabled (app_id=%s, state=%s)", cfg.Feishu.AppID, statePath)
	} else if cfg.DingTalk.AppKey != "" {
		statePath := defaultStatePath()
		store := persist.NewJSONStore(statePath)
		mgr.SetSummaryStore(store)

		dtCh := dingtalkCh.New(dingtalkCh.Config{
			AppKey:         cfg.DingTalk.AppKey,
			AppSecret:      cfg.DingTalk.AppSecret,
			AllowedUserIDs: cfg.DingTalk.AllowedUserIDs,
		})
		discoverer := claude.NewDiscoverer("", "")
		newBridge = bridge.New(bridge.Options{
			Manager:             mgr,
			Channel:             dtCh,
			DefaultCWD:          cfg.DefaultWorkingDir,
			EnvFilePath:         cfg.EnvFilePath,
			AdminModel:          cfg.AdminModel,
			SummaryInterval:     cfg.SummaryInterval,
			Persister:           store,
			Discoverer:          discoverer,
			ShareExternal:       cfg.ShareExternalSessions,
			DiscoveryWindowDays: cfg.DiscoveryWindowDays,
			RescanInterval:      cfg.DiscoveryRescanInterval,
			ApplyAllowedUsers:   dtCh.SetAllowedUserIDs,
			ApplyCLIPath:        rt.SetCLIPath,
			CronStore:           cronStore,
			CronRunLog:          cronRunLog,
		})
		newBridge.Start(ctx)
		channelShutdown = dtCh.Shutdown
		go func() {
			if err := dtCh.Start(ctx, newBridge); err != nil {
				log.Printf("[channel/dingtalk] error: %v", err)
			}
		}()
		log.Printf("  DingTalk bridge: enabled (app_key=%s, state=%s)", cfg.DingTalk.AppKey, statePath)
	}

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if newBridge != nil {
		newBridge.Shutdown()
	}
	if channelShutdown != nil {
		channelShutdown()
	}

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	mgr.Shutdown(shutdownCtx)
	log.Printf("goodbye")
}

// defaultStatePath returns the canonical path for gateway_state.json.
// On first run after upgrading, it migrates the file from the old binary-dir
// location (and the even older feishu_state.json name) to ~/.ccg/.
func defaultStatePath() string {
	if home, err := os.UserHomeDir(); err == nil {
		target := filepath.Join(home, ".ccg", stateFileName)
		if _, err := os.Stat(target); err == nil {
			return target
		}
		// Migrate from binary dir if present.
		if src := findLegacyStatePath(); src != "" {
			_ = os.MkdirAll(filepath.Dir(target), 0700)
			log.Printf("[main] migrating state file %s → %s", src, target)
			if err := os.Rename(src, target); err != nil {
				log.Printf("[main] migrate state failed: %v", err)
			} else {
				return target
			}
		}
		return target
	}
	// Fallback when $HOME is unavailable.
	return filepath.Join(binaryDir(), stateFileName)
}

// findLegacyStatePath returns the first existing state file in the binary
// directory, checking the current name then the old feishu_ name.
func findLegacyStatePath() string {
	dir := binaryDir()
	for _, name := range []string{stateFileName, "feishu_state.json"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// binaryDir returns the directory containing the running executable.
func binaryDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// migrateRuntimeFiles moves .env from the binary directory to ~/.ccg/ on the
// first run after upgrading. gateway_state.json is handled by defaultStatePath.
func migrateRuntimeFiles() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	ccg := filepath.Join(home, ".ccg")
	_ = os.MkdirAll(ccg, 0700)

	newEnv := filepath.Join(ccg, ".env")
	if _, err := os.Stat(newEnv); os.IsNotExist(err) {
		oldEnv := filepath.Join(binaryDir(), ".env")
		if _, err := os.Stat(oldEnv); err == nil {
			if err := os.Rename(oldEnv, newEnv); err == nil {
				log.Printf("[main] migrated .env %s → %s", oldEnv, newEnv)
			}
		}
	}
}

// initCronState creates cron store and run log instances backed by ~/.ccg/.
// Returns nil for both if filesystem initialisation fails.
func initCronState() (cron.Store, *cron.RunLog) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[cron] cannot determine home dir: %v — cron disabled", err)
		return nil, nil
	}
	ccg := filepath.Join(home, ".ccg")
	_ = os.MkdirAll(ccg, 0700)

	store, err := cron.NewJSONStore(filepath.Join(ccg, "cron_jobs.json"))
	if err != nil {
		log.Printf("[cron] failed to init store: %v — cron disabled", err)
		return nil, nil
	}
	rl := cron.NewRunLog(filepath.Join(ccg, "cron_history.json"), 20)
	log.Printf("[cron] store and history loaded from %s", ccg)
	return store, rl
}
