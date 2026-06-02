# Claude Code Gateway

🌐 Language: **English** | [中文](./README.zh-CN.md) | [日本語](./README.ja.md)

A Go gateway that bridges remote clients (WebSocket / Feishu IM) to local [Claude Code CLI](https://github.com/anthropics/claude-code) subprocesses. Each chat session pins a dedicated CLI process and streams JSON over stdin/stdout — so a phone, browser, or chat app can drive a long-running Claude Code conversation on your laptop without losing context.

---

## Quick Start

### Option A — Let Claude Code install it for you (recommended)

If you have `claude` CLI already, paste the following one-liner into it:

```
请按照这个 QUICK_START.md 帮我部署 Claude Code Gateway，并配置为开机自启守护进程：
https://github.com/aconeshana/claude-code-gateway/blob/main/QUICK_START.md
```

Claude Code will clone, build, write the dotenv, register the launch agent / systemd unit, and verify health for you.

### Option B — Manual

Requirements: `go ≥ 1.22`, `claude` CLI, `jq` (recommended), `git`.

```bash
git clone https://github.com/aconeshana/claude-code-gateway.git
cd claude-code-gateway
go build -o gateway .

# Minimal config — same directory as the binary
cat > .env <<'EOF'
GATEWAY_DEFAULT_CWD=~          # fallback dir for main-chat plain text
ADMIN_MODEL=claude-sonnet-4-6  # for summaries / fuzzy matching
# Enable Feishu (optional)
# FEISHU_APP_ID=cli_xxx
# FEISHU_APP_SECRET=xxx
EOF

./gateway.sh start
```

Visit `http://localhost:8080/health` to verify. For Feishu, send `/help` to the bot in DM.

---

## What it does

- **Multi-session orchestration** — each chat session = one Claude Code CLI subprocess; gateway holds them as a pool with `active / idle / archived` lifecycle.
- **Cross-restart resumption** — gateway state persists to JSON; sessions resume cleanly via `claude --resume`. No conversation context lost.
- **Feishu integration** — DM-based slash commands (`/new`, `/list`, `/switch`, `/resume`, `/archive`, `/branch`, `/rename`, `/skills`, `/cron`, `/diff`, `/status`, `/config`, …), reply-in-thread for parallel session isolation, project picker UI.
- **External session discovery** — finds Claude Code sessions you started from terminal / SDK / IDE, optionally surfaces them in the IM list.
- **AI-generated summaries** — every N user messages, a tiny admin model regenerates a one-line topic for each session so `/list` stays scannable.
- **WebSocket transport** — for custom UIs that want raw stream-json events instead of going through an IM channel.

---

## Architecture

```
WebSocket / Feishu client                    ┌─────────────────────────┐
            │                                │ session.Manager         │
            ▼                                │  · per-owner index      │
┌────────────────────────────┐               │  · active / idle /      │
│  channel.Channel adapter   │ Inbound/      │    archived three-state │
│   (feishu, fake, ws)       │ Outbound      └───────────┬─────────────┘
└──────────────┬─────────────┘                           │
               │                                         │
               ▼                                         ▼
        ┌──────────────┐                       ┌─────────────────────┐
        │ bridge.Bridge│  command routing →    │ runtime.Runtime     │
        │ (commands +  │                       │  (claude-code / fake)│
        │  rendering)  │                       └──────────┬──────────┘
        └──────────────┘                                  │
                                                          ▼
                                                ┌────────────────────┐
                                                │ Claude Code CLI    │
                                                │ stream-json process │
                                                └────────────────────┘
```

Full design notes (state machine, V2 routing principles, ghost-session handling, summary worker, persistence schema) live in [`CLAUDE.md`](./CLAUDE.md) and [`docs/state-machine.md`](./docs/state-machine.md).

---

## Configuration

Set via env vars, `.env` next to the binary, or hot-edit via the `/config` command in Feishu.

| Key | Default | Purpose |
|-----|---------|---------|
| `GATEWAY_DEFAULT_CWD` | `~` | fallback working dir for plain-text inbound |
| `GATEWAY_PERMISSION_MODE` | `auto` | tool-call permission mode (`auto` / `forward`) |
| `GATEWAY_LISTEN_ADDR` | `:8080` | WebSocket listen address |
| `GATEWAY_MAX_SESSIONS` | `10` | max concurrent CLI processes |
| `SUMMARY_INTERVAL` | `5` | regenerate summary every N user messages (0 = off) |
| `ADMIN_MODEL` | `claude-haiku-4-5` | model used by the summary worker (sonnet recommended in prod) |
| `GATEWAY_SHARE_EXTERNAL_SESSIONS` | `false` | surface sessions discovered from terminal/SDK use |
| `GATEWAY_DISCOVERY_WINDOW_DAYS` | `7` | scan window for external session jsonl files |
| `GATEWAY_DISCOVERY_RESCAN_INTERVAL` | `5m` | how often to rescan disk |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | — | enables Feishu bridge when both set |
| `FEISHU_ALLOWED_USER_IDS` | — | comma-separated allow list |

---

## Common operations

```bash
./gateway.sh start          # launch as background daemon
./gateway.sh stop
./gateway.sh restart        # rebuild + restart
./gateway.sh status         # daemon state
./gateway.sh logs           # tail logs

go test -race ./...         # full test with race detector
go vet ./...
```

---

## Development

The codebase is small and self-contained. Key packages:

- `internal/session/` — manager, lifecycle, persist
- `internal/bridge/` — command routing, card rendering, summary worker
- `internal/channel/feishu/` — Lark IM adapter (cards, threads, callbacks)
- `internal/runtime/claude/` — CLI subprocess management & stream-json codec
- `internal/gateway/` — WebSocket transport

See [`CLAUDE.md`](./CLAUDE.md) for conventions and architectural decisions.

---

## License

Apache 2.0. See [`LICENSE`](./LICENSE).
