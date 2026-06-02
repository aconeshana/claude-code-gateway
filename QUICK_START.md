# Claude Code Gateway · Quick Start (AI-driven)

This file is meant to be read **by your Claude Code agent**, not directly by you. Paste this prompt into Claude Code:

> 请按照这个 QUICK_START.md 帮我部署 Claude Code Gateway，并配置为开机自启守护进程：
> https://github.com/aconeshana/claude-code-gateway/blob/main/QUICK_START.md

The agent will then follow the steps below to install, configure, and daemonize the gateway with minimal interaction. The repo is at https://github.com/aconeshana/claude-code-gateway.

---

## 0. Greet the user briefly

In one sentence, tell them what you're about to do:

> 我会帮你拉源码、编译、写最小配置，并设置为开机自启守护进程。大约 1-2 分钟。

---

## 1. Prerequisites check

Run these in parallel and report any missing items:

```bash
command -v go && go version          # need >= 1.22
command -v claude && claude --version # claude CLI must exist
command -v git
command -v jq                         # recommended but not required
uname -s                              # detect platform: Darwin or Linux
```

**If `go` is missing**:
- macOS: `brew install go`
- Linux: ask the user to install via their package manager, or grab from https://go.dev/dl/

**If `claude` is missing**: stop and tell the user:
> 需要先安装 Claude Code CLI: https://docs.claude.com/en/docs/claude-code

**If `jq` is missing**: continue, but tell the user — without `jq` the summary worker will be 3x slower:
- macOS: `brew install jq`
- Linux: `apt install jq` / `dnf install jq`

---

## 2. Clone and build

Pick a stable install directory (don't put it in a temp dir):

```bash
INSTALL_DIR="$HOME/claude-code-gateway"

if [ -d "$INSTALL_DIR" ]; then
  cd "$INSTALL_DIR" && git pull
else
  git clone https://github.com/aconeshana/claude-code-gateway.git "$INSTALL_DIR"
  cd "$INSTALL_DIR"
fi

go build -o gateway .
```

Verify the binary works:

```bash
"$INSTALL_DIR/gateway" --help 2>&1 | head -5
```

---

## 3. Initial .env

Generate a minimal `.env` next to the binary. Ask the user (one question at a time, with sane defaults):

1. **Default working dir for plain-text inbound** — default `~`. Most users keep it.
2. **Enable Feishu?** — default no. If yes, ask for `FEISHU_APP_ID` and `FEISHU_APP_SECRET`.

Then write `$INSTALL_DIR/.env`:

```bash
cat > "$INSTALL_DIR/.env" <<EOF
GATEWAY_DEFAULT_CWD=$DEFAULT_CWD
GATEWAY_LISTEN_ADDR=:8080
ADMIN_MODEL=claude-sonnet-4-6
SUMMARY_INTERVAL=5
# Feishu (set both to enable)
# FEISHU_APP_ID=
# FEISHU_APP_SECRET=
# FEISHU_ALLOWED_USER_IDS=
EOF
```

If the user supplied Feishu credentials, uncomment and fill those lines too. **Never echo the secret value back to the user in chat output** — only confirm "written".

---

## 4. Register as boot-time daemon

Detect the platform once. Don't try both.

### 4a. macOS (Darwin) → launchd LaunchAgent

```bash
LABEL="com.aconeshana.claude-code-gateway"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$HOME/Library/Logs/claude-code-gateway"
mkdir -p "$LOG_DIR"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${INSTALL_DIR}/gateway</string>
    <string>serve</string>
  </array>
  <key>WorkingDirectory</key><string>${INSTALL_DIR}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${LOG_DIR}/out.log</string>
  <key>StandardErrorPath</key><string>${LOG_DIR}/err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
</dict>
</plist>
EOF

# Load (or reload) it
launchctl unload "$PLIST" 2>/dev/null
launchctl load -w "$PLIST"

# Wait briefly then verify
sleep 2
launchctl list | grep "$LABEL"
```

### 4b. Linux → systemd --user

```bash
UNIT="$HOME/.config/systemd/user/claude-code-gateway.service"
mkdir -p "$(dirname "$UNIT")"

cat > "$UNIT" <<EOF
[Unit]
Description=Claude Code Gateway
After=network.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/gateway serve
Restart=always
RestartSec=3
StandardOutput=append:${INSTALL_DIR}/gateway.log
StandardError=append:${INSTALL_DIR}/gateway.err

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now claude-code-gateway

# Enable lingering so the unit starts on boot even without login
loginctl enable-linger "$USER" 2>/dev/null || true

systemctl --user status claude-code-gateway --no-pager | head -10
```

---

## 5. Health check

Wait up to 10 seconds for it to come up, then probe:

```bash
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS http://localhost:8080/health 2>/dev/null; then
    echo " — gateway is up"
    break
  fi
  sleep 1
done
```

If `/health` doesn't respond within 10s, tail logs and surface the error:

```bash
# macOS
tail -50 "$HOME/Library/Logs/claude-code-gateway/err.log"
# Linux
tail -50 "${INSTALL_DIR}/gateway.err"
```

---

## 6. Report back to the user

Tell the user clearly:

> ✅ 已部署完成。
>
> - 安装目录: `~/claude-code-gateway`
> - 健康检查: `http://localhost:8080/health` (just verified)
> - 守护进程: <launchd LABEL or systemd 单元名>
> - 配置文件: `~/claude-code-gateway/.env`
> - 日志: `<path>` (tail with `tail -f <path>`)
>
> 控制命令:
> - `~/claude-code-gateway/gateway.sh restart` — 重启
> - 飞书私聊里 `/config` — 在线热改配置
> - 飞书私聊里 `/help` — 看所有命令

If Feishu was enabled, hint at next step:

> 飞书侧还需要在开发者后台「事件订阅」里把回调地址指向 `https://<你的公网域名>/feishu/event`,或用 [larksuite/oapi-sdk-go 长连接模式](https://open.feishu.cn/document/ukTMukTMukTM/uETN5UjLxUTO14SM1kTN) 让网关主动连飞书 (本网关已默认走长连接,无需公网)。

---

## Troubleshooting

If something goes wrong, use these targeted probes — don't blindly retry:

- **`go build` fails**: check `go version` ≥ 1.22; if vendored deps missing, run `go mod download`.
- **launchd loads but process exits immediately**: `tail err.log` — most common is `claude` not on PATH (LaunchAgent's PATH is minimal; we set `/opt/homebrew/bin:/usr/local/bin` but if claude lives elsewhere, add to the plist's `EnvironmentVariables.PATH`).
- **systemd unit fails to start**: `systemctl --user status` shows the error; check `which claude` returns same path in user shell.
- **Port 8080 in use**: change `GATEWAY_LISTEN_ADDR=:NNNN` in `.env`, restart daemon.

---

## Optional: enable Feishu after the fact

If the user wants to add Feishu later, they can edit `.env` and either:

- Use `/config set FEISHU_APP_ID xxx` in DM if the bridge was already partly enabled
- Or restart the daemon: `launchctl unload && load` (macOS) / `systemctl --user restart claude-code-gateway` (Linux)
