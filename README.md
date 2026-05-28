# Claude Code Gateway

Go 网关,把外部客户端(WebSocket 或飞书 IM)桥接到 [Claude Code CLI](https://github.com/anthropics/claude-code) 子进程。每个会话对应一个独立 CLI 进程,通过 stream-json stdin/stdout 双向通信。

完整架构、模块说明、命令、配置见 [CLAUDE.md](./CLAUDE.md)。

---

## 运行依赖

| 工具 | 用途 | 必需 | 安装 |
|------|------|------|------|
| Go ≥ 1.22 | 编译 / 运行 gateway | 必需 | `brew install go` |
| `claude` CLI | 实际跑会话的子进程 | 必需 | [安装文档](https://docs.claude.com/en/docs/claude-code) |
| `jq` | 摘要 worker 解析 jsonl(用预制 jq pipeline 喂给 admin 模型,避免 6-8 个 tool 轮次探索格式) | **推荐** | macOS `brew install jq` / Debian/Ubuntu `apt install jq` |
| `git` | `/diff` 命令 + 工作目录变更检测 | 可选 | 系统自带 |

**`jq` 缺失会怎样?** 摘要功能依然能用,但 admin 模型每次会花 6-8 个 tool 轮次去 `head/cat/jq with wrong filter` 探索 jsonl 格式,质量和延迟都会显著退化(单次摘要 60-90s vs 装了 jq 的 20-30s)。启动日志会打 warning:

```
[summary-worker] WARNING: jq not in PATH — summary quality and latency will both regress.
Install with: brew install jq (macOS) / apt install jq (Debian/Ubuntu)
```

---

## 快速开始

```bash
# 1. 装依赖(macOS 一键)
brew install go jq
npm install -g @anthropic-ai/claude-code   # 或参照官方文档

# 2. 构建
cd claude-code-gateway
go build -o gateway .

# 3. 最小配置 — 写 .env (跟 gateway 二进制同目录)
cat > .env <<'EOF'
GATEWAY_DEFAULT_CWD=~/projects        # 你常在哪建 session
GATEWAY_PROJECT_ROOT=~/projects       # /new <子目录> 解析基准
ADMIN_MODEL=claude-sonnet-4-6         # 摘要 / 模糊匹配模型,推荐 sonnet
# 启用飞书(可选)
# FEISHU_APP_ID=cli_xxx
# FEISHU_APP_SECRET=xxx
EOF

# 4. 启动
./gateway.sh start              # 后台运行 + 健康检查
# 或前台 ./gateway

# 5. 浏览器测试控制台(WebSocket 模式)
open test.html
```

启动后:
- `:8080/health` 健康检查
- `:8080/ws` WebSocket 入口
- 飞书配置好后,在飞书私聊里发 `/help` 看命令列表

完整配置项见 CLAUDE.md「Configuration」表。

---

## 常用命令

```bash
./gateway.sh start              # 启动
./gateway.sh stop               # 停止
./gateway.sh restart            # 重启(rebuild + restart)
./gateway.sh status             # 看运行状态
./gateway.sh logs               # tail 日志

go test -race ./...             # 全测试 + race detector
go vet ./...                    # 静态检查(没用 lint)
```

飞书 bot 启用后,所有运行时配置(摘要间隔、磁盘扫描窗口、ShareExternal 开关等)用 `/config` 命令在线热更新,不需要重启。
