# Claude Code Gateway

🌐 Language: [English](./README.md) | **中文** | [日本語](./README.ja.md)

Go 写的网关，把远程客户端（WebSocket / 飞书 IM）桥接到本地 [Claude Code CLI](https://github.com/anthropics/claude-code) 子进程。每个会话对应一个独立 CLI 进程，通过 stream-json 双向 stdin/stdout 通信 —— 这样手机、浏览器、IM 都能驱动跑在你笔记本上的长会话，不丢上下文。

---

## 快速开始

### 方式 A —— 让 Claude Code 帮你装（推荐）

打开本地的 `claude` CLI（或 IDE 插件），把下面这段贴过去：

```
请按照这个 QUICK_START.md 帮我部署 Claude Code Gateway，并配置为开机自启守护进程：
https://github.com/aconeshana/claude-code-gateway/blob/main/QUICK_START.md
```

Claude Code 会自动完成 clone、编译、写 .env、注册 launchd / systemd 守护、健康检查，全程无需你手动操作。

### 方式 B —— 手动

依赖：`go ≥ 1.22`、`claude` CLI、`git`。

```bash
git clone https://github.com/aconeshana/claude-code-gateway.git
cd claude-code-gateway
go build -o gateway .

# 最小配置 —— 跟 gateway 二进制同目录
cat > .env <<'EOF'
GATEWAY_DEFAULT_CWD=~          # 主聊天 plain text 兜底目录
ADMIN_MODEL=claude-haiku-4-5   # 摘要 / 语义匹配用的模型
# 启用飞书(可选)
# FEISHU_APP_ID=cli_xxx
# FEISHU_APP_SECRET=xxx
EOF

./gateway.sh start
```

启动后访问 `http://localhost:8080/health` 验证。启用飞书后，在私聊里发 `/help` 看命令列表。

---

## 能做什么

- **多会话编排** —— 每条聊天会话对应一个 Claude Code CLI 子进程；网关维护进程池 + active/idle/archived 三态生命周期
- **跨重启恢复** —— 网关状态持久化到 JSON，session 通过 `claude --resume` 无缝接续，上下文不丢
- **飞书集成** —— 私聊斜杠命令（`/new` `/list` `/switch` `/resume` `/archive` `/branch` `/rename` `/skills` `/cron` `/diff` `/status` `/config` 等）、自动开话题做并行 session 物理隔离、项目选择器卡片
- **外部会话发现** —— 自动扫描终端 / SDK / IDE 直接调用 Claude CLI 创建的会话，按配置选择是否在 IM 里展示
- **AI 自动生成摘要** —— 每 N 条用户消息后调小模型重生成一句话主题，`/list` 一眼能扫
- **WebSocket 协议** —— 给自定义 UI 用的纯 stream-json 通道，不走 IM

---

## 架构

```
WebSocket / 飞书客户端                       ┌─────────────────────────┐
            │                                │ session.Manager         │
            ▼                                │  · per-owner 索引       │
┌────────────────────────────┐               │  · active / idle /      │
│  channel.Channel 适配器     │ Inbound/      │    archived 三态        │
│   (feishu, fake, ws)        │ Outbound      └───────────┬─────────────┘
└──────────────┬─────────────┘                            │
               │                                          │
               ▼                                          ▼
        ┌──────────────┐                        ┌─────────────────────┐
        │ bridge.Bridge│  命令路由 →           │ runtime.Runtime     │
        │ (命令 +       │                       │  (claude-code / fake)│
        │  渲染)       │                       └──────────┬──────────┘
        └──────────────┘                                  │
                                                          ▼
                                                ┌────────────────────┐
                                                │ Claude Code CLI    │
                                                │ stream-json 子进程  │
                                                └────────────────────┘
```

完整设计（状态机、V2 路由优先级、ghost 处理、摘要 worker、持久化 schema）见 [`CLAUDE.md`](./CLAUDE.md) 和 [`docs/state-machine.md`](./docs/state-machine.md)。

---

## 配置项

通过环境变量、`.env` 文件（跟二进制同目录），或飞书 `/config` 命令热改。

| 键 | 默认 | 说明 |
|---|---|---|
| `GATEWAY_DEFAULT_CWD` | `~` | 主聊天 plain text 兜底工作目录 |
| `GATEWAY_PERMISSION_MODE` | `auto` | 工具调用权限（`auto` / `forward`） |
| `GATEWAY_LISTEN_ADDR` | `:8080` | WebSocket 监听地址 |
| `GATEWAY_MAX_SESSIONS` | `10` | 最大并发 CLI 进程数 |
| `SUMMARY_INTERVAL` | `5` | 每 N 条用户消息触发一次摘要重生成（0 = 关） |
| `ADMIN_MODEL` | `claude-haiku-4-5` | 摘要 worker 和 `/recap` 使用的模型 |
| `GATEWAY_SHARE_EXTERNAL_SESSIONS` | `false` | 是否在 IM 里展示终端/SDK 创建的外部会话 |
| `GATEWAY_DISCOVERY_WINDOW_DAYS` | `7` | 外部会话扫描时间窗口（0 = 全量） |
| `GATEWAY_DISCOVERY_RESCAN_INTERVAL` | `5m` | 重新扫描间隔 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | — | 两者都设了就启用飞书 |
| `FEISHU_ALLOWED_USER_IDS` | — | 飞书用户白名单（逗号分隔） |

---

## 常用操作

```bash
./gateway.sh start          # 后台启动
./gateway.sh stop
./gateway.sh restart        # rebuild + 重启
./gateway.sh status         # 守护状态
./gateway.sh logs           # tail 日志

go test -race ./...         # 全测试 + race
go vet ./...
```

---

## 开发

代码量很小且自洽。主要包：

- `internal/session/` —— 会话管理器、生命周期、持久化
- `internal/bridge/` —— 命令路由、卡片渲染、摘要 worker
- `internal/channel/feishu/` —— 飞书 IM 适配器（卡片、话题、回调）
- `internal/runtime/claude/` —— CLI 子进程管理 + stream-json 编解码
- `internal/gateway/` —— WebSocket 传输层

详细约定见 [`CLAUDE.md`](./CLAUDE.md)。

---

## License

Apache 2.0，见 [`LICENSE`](./LICENSE)。
