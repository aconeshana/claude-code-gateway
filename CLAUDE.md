# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Go 网关,桥接外部客户端(WebSocket 或飞书)与 Claude Code CLI 子进程。每个会话对应一个独立的 CLI 进程,通过 NDJSON stdin/stdout 双向通信。

```
WebSocket 客户端              飞书用户(WS 长连接)
       │                            │
       │ JSON action                │ 飞书 event
       ▼                            ▼
┌────────────────┐         ┌────────────────────┐
│ gateway.Server │         │ channel/feishu.    │
│ gateway.WS-    │         │  Channel           │
│  Handler       │         └──────────┬─────────┘
└───────┬────────┘                    │ channel.Inbound/
        │                             │ Outbound
        │                  ┌──────────▼─────────┐
        │                  │   bridge.Bridge    │  命令路由 + 渲染
        │                  └──────────┬─────────┘
        │                             │
        ▼                             ▼
┌─────────────────────────────────────────────────┐
│                session.Manager                   │  唯一真相源
│  Create / Reactivate / Archive / SetFocus / ... │
│  per-owner 索引 + active/idle/archived 三态     │
└─────────────────────┬───────────────────────────┘
                      │
                      ▼
              runtime.Runtime(claude / fake)
                      │
                      ▼
              Claude Code CLI(每 session 一个进程)
                -p --output-format stream-json
                --input-format stream-json --verbose
```

## Commands

```bash
# 构建
go build -o gateway .

# 全部编译 + 测试
go build ./...
go test -race ./...

# 运行覆盖率
go test -cover ./...

# 启动
CLAUDE_CLI_PATH="claude" GATEWAY_DEFAULT_CWD="/path/to/project" ./gateway

# 自定义配置文件
./gateway -config config.json

# WS 手动测试
websocat ws://localhost:8080/ws

# 浏览器测试控制台
open test.html
```

无 linter 配置,用 `go vet ./...` 静态检查。

## Architecture

### 包结构(按依赖方向自底向上)

```
internal/
  protocol/                 claude-code stream-json 协议(types/codec/permission)
  runtime/                  CLI 运行时抽象
    runtime.go              Runtime / Process / Codec / Config 接口
    factory.go              Registry + Factory(把 JSON envelope 解析为 Config)
    claude/                 claude-code 实现(spawn CLI + 91 行 flag 构造 + codec wrapper)
    fake/                   可编程 fake(单测用)
  session/                  唯一真相源
    session.go              Session 实体 + 双状态机(lifecycle + runtime phase)
    manager.go              Create / Resume / Get / List / Destroy + 默认配置
    lifecycle.go            Archive / Reactivate / SetLabel / SetSummary / SetFocus /
                            ResolveResumable / FindByPrefix / Import* / 摘要追踪
    user_index.go           per-owner 索引(SessionIDs/FocusedID/ResumeHintID)
    persist/                JSONStore: Save/Load gateway_state.json + legacy 兼容
    sessiontest/            buildFakeCLI + EventCollector(供单测复用)
  channel/                  IM 抽象
    channel.go              Channel 接口 + Inbound/Outbound/Card/Button 类型
    fake/                   测试用
    feishu/                 飞书完整实现(独立,不依赖外层 bridge)
      channel.go            实现 Channel 接口,WebSocket 长连接 + Lark API
      cards.go              channel.Card → Lark JSON 渲染
      markdown.go           markdown → lark_md 适配
  bridge/                   channel × session 业务粘合
    bridge.go               Options/New/Start/Shutdown + OnMessage 入口
    command.go              Command/CommandHandler 类型 + dispatch + forwardToCLI
    commands.go             registerCommands() + /new /list /switch /archive /resume /plan /help + Card action
    commands_extra.go       /model /diff /config /shell + /setup + 配置写入
    admin.go                admin AI session(摘要、模糊匹配、NL 配置解析)
    renderer.go             session event → channel.Card,Plan/Elicitation 闭环
    config.go               ConfigFields + NormalizePermissionMode + WriteEnvFile + ParseEnvValues
  gateway/                  WebSocket 入口
    server.go               HTTP + WS 升级 + Bearer 认证
    handler.go              8 action 分发 + Runtime envelope 解析
    messages.go             Client/Server message + payload 结构
```

### 关键设计

**Session 唯一真相源**:`session.Manager` 持有所有 session 元数据(label/summary/status/owner/chat_id),lifecycle(active/idle/archived)与 runtime phase(starting→ready→...→stopped)正交。channel/bridge 不持有任何 session 状态,只调 manager API。

**Runtime 抽象**:`runtime.Runtime` 接口隔离 CLI agent 细节。新增 codex 等只需实现 `Spawn` + `Codec`,manager 完全不变。CreateOpts 支持 `RuntimeConfig runtime.Config`(opaque)字段透传任意 runtime 的 config。

**Channel 抽象**:`channel.Channel` 接口让 IM 实现可插拔。`channel/feishu` 独立完整,future Slack/钉钉 只需新实现一个 channel 包。`channel.Card/Button/Section/Form` 是通用模型,由 channel 实现负责渲染为平台特定格式。

**Bridge 粘合层**:消费 channel.InboundMessage → 命令路由 + session 操作;反向消费 session event → 渲染 channel.Card → ch.Send/Update。无业务状态,所有真相源在 manager。

**Gateway transport**:`CreateSessionPayload.Runtime json.RawMessage` 是 opaque envelope,由 `runtime.Factory` 解析。30+ claude flag 字段不再出现在 gateway 协议中(向后不兼容)。

## Conventions(开发约定)

这些是踩过坑写下来的硬规则,新代码必须遵守:

### Session 状态访问

**字段定义在 `docs/SESSION_STATE.md`** — 所有 `session.Session` / `SessionInfo` / `ExternalAugmentation` 字段的来源、约束、生命周期都在该文档中。**修改 session schema(加字段、改语义、新增 Origin/Status/PermissionMode 值)必须在同一 commit 更新该文档**,这是硬要求。

**唯一变更入口是 `session.Manager` 的 API**。channel/bridge/gateway 任何代码都不能直接写 `Session.Status / Label / Summary / ChatID / OwnerID / WorkingDir / Phase / recentMessages` 等字段。

| 操作 | 必须用的 API |
|------|-------------|
| 创建 / 销毁 | `mgr.Create` / `mgr.Destroy` |
| 状态迁移 | `mgr.Archive` / `mgr.Reactivate` / `mgr.TransitionToIdle` / `mgr.RemoveArchived` |
| 元数据 | `mgr.SetLabel` / `mgr.SetSummary` / `mgr.SetChatID` |
| 焦点 / 索引 | `mgr.SetFocus` / `mgr.SetResumeHint` |
| 摘要计数 | `mgr.AppendRecentMessage` / `mgr.ShouldUpdateSummary` |
| 外部摘要 augmentation | `mgr.SetExternalSummary` / `ExternalSummary` / `CountFreshExternalSummaries` / `PurgeExternalSummaries`(不要直调 persister!) |
| 默认配置 | `mgr.SetDefaultPermissionMode` / `mgr.AddAllowedBaseDir` / `mgr.SetSummaryStore` |
| 查询 | `mgr.Get` / `GetByCLISessionID` / `FindByPrefix` / `FindArchivedByPrefix` / `ListBy` / `ListDiscoverableByOwner` / `FocusedSession` / `ResolveResumable` |

**跨包读取 Session 字段必须走 `sess.Info()`**(内部带 mutex,返回 SessionInfo 快照)。直接读 `sess.Status` 等可变字段会和 manager 的写入产生 race。WorkingDir 创建后不变,直接读可以。

**字符串字面量禁区** — 这些值都有常量,不要再写 magic string:
- Origin: `session.OriginFeishu` / `OriginWS` / `OriginExternal` / `OriginAdmin`
- Status: `session.StatusActive` / `StatusIdle` / `StatusArchived`
- PermissionMode: `claudeRT.PermissionAuto` / `PermissionForward` / `PermissionDefault`(`internal/runtime/claude`)
- Channel kind: `channel.KindFeishu`(`internal/channel`)
- Admin workdir: `claudeRT.AdminWorkdirPrefix`(canonical 定义在 `internal/runtime/claude/discoverer.go`)
- JSONL 路径: `claudeRT.SessionJSONLPath(workingDir, cliID)`(canonical),`persist.SessionJSONLPath` 是兼容旧 import 的 thin proxy

新增字段时,在 `Session` 结构体加字段,同时在 `Info()` 拷贝到 `SessionInfo`,manager 提供 `SetXxx(sessionID, val) error` setter,**同步更新 `docs/SESSION_STATE.md`**。

### Command Registry

`internal/bridge/command.go::Command` 是 slash 命令的注册结构。新增命令:

1. 在 `commands.go::registerCommands()` 追加 `Command{}` 字面量(Name/Aliases/Usage/Desc/Handler 一处声明)
2. 实现 `func (b *Bridge) cmdFoo(ctx, m, args)`,handler 签名是 `CommandHandler`
3. 无参 handler 用 `b.wrapNoArgs(b.cmdFoo)` 适配
4. 透传给 CLI 用 `b.forwardToCLI`(/plan、/commit 等)
5. `/help` 自动从 `b.commands` 生成,不需要手动改帮助文本

按钮回调:在 `handleCardAction` 加 case + 在 button 的 `Action` map 里声明 `"action": "your_name"`。

### Permission Mode

`auto` 是 `auto_allow` 的简写。任何接收用户输入的入口(env var、`/config set`、`save_config` 按钮)都必须调 `bridge.NormalizePermissionMode(v)` 归一化再存。runtime 层只识别 canonical 值 `auto_allow` / `forward` / `default`。

### Workspace 路径

`/new <subdir>` 解析规则:若 `GATEWAY_PROJECT_ROOT/<subdir>` 是目录,自动用作 workingDir 并把 `<subdir>` 当 label。`/new /abs/path` 直接当路径。`/new label /some/dir` 显式 label + dir。

新工作目录必须是 `GATEWAY_DEFAULT_CWD` 或 `GATEWAY_PROJECT_ROOT`(由 `AddAllowedBaseDir` 注册)的子目录,否则 `mgr.Create` 返回错误。

## Configuration

| 环境变量 | 默认 | 说明 |
|---------|-----|------|
| `CLAUDE_CLI_PATH` | `claude` | CLI 二进制路径 |
| `GATEWAY_LISTEN_ADDR` | `:8080` | WS 监听地址 |
| `GATEWAY_DEFAULT_CWD` | `.` | 默认工作目录(也是基础允许目录) |
| `GATEWAY_PROJECT_ROOT` | (空) | 项目基准目录(`/new <子目录>` 解析基准) |
| `GATEWAY_PERMISSION_MODE` | `auto` | `auto`(= `auto_allow`)/ `forward` |
| `GATEWAY_MAX_SESSIONS` | `10` | 最大并发 session 数 |
| `GATEWAY_AUTH_TOKEN` | (空) | WS Bearer Token |
| `SUMMARY_INTERVAL` | `5` | 多少**条用户消息**后自动重生成摘要(0=关闭) |
| `ADMIN_MODEL` | `claude-haiku-4-5` | 摘要 / 模糊匹配用的 admin 模型(**生产建议 `claude-sonnet-4-6`**,质量 10/10 vs haiku 7-8/10) |
| `GATEWAY_SHARE_EXTERNAL_SESSIONS` | `false` | 是否展示 terminal/SDK 等创建的 external session |
| `GATEWAY_DISCOVERY_WINDOW_DAYS` | `7` | 磁盘扫描时间窗口(天),0=全量 |
| `GATEWAY_DISCOVERY_RESCAN_INTERVAL` | `5m` | 重新扫描间隔 |
| `FEISHU_APP_ID` | (空) | 飞书 App ID(非空时启用飞书) |
| `FEISHU_APP_SECRET` | (空) | 飞书 App Secret |
| `FEISHU_ALLOWED_USER_IDS` | (空) | 飞书用户白名单(逗号分隔,空=允许所有) |

加载顺序:默认值 → JSON 文件(`-config`)→ 环境变量覆盖 → `.env` 文件(同二进制目录)。

## WebSocket Protocol

**端点**: `ws://host:port/ws` | **健康检查**: `GET /health`

### Client Actions(8 种)

| action | payload 关键字段 |
|--------|----------------|
| `create_session` | `working_dir`, `permission_mode`, `env_vars`, `owner_id`, `chat_id`, `runtime` |
| `resume_session` | `session_id`, `working_dir`, `owner_id`, `label`, `summary`, `chat_id` |
| `send_message` | `session_id`, `content`(支持 `/model <name>`、`!<cmd>`) |
| `respond_permission` | `session_id`, `request_id`, `tool_use_id`, `behavior`, `updated_input` |
| `control` | `session_id`, `subtype`(interrupt / end_session 等) |
| `destroy_session` | `session_id` |
| `list_sessions` | - |
| `ping` | - |

### Runtime envelope(create_session 的 `runtime` 字段)

```json
{
  "kind": "claude",
  "config": {
    "Model": "claude-sonnet-4-6",
    "MaxTurns": 10,
    "Effort": "high",
    "Betas": ["beta-x"],
    "AllowedTools": ["Bash"]
  }
}
```

字段定义见 `internal/runtime/claude/config.go::Config`。空 envelope 用 fallback kind(默认 `claude`)+ 零值 config。

### Server Events

| event | 触发 |
|-------|------|
| `session_created` | 创建/恢复成功(payload = SessionInfo) |
| `message` | CLI stdout 透传 |
| `permission_request` | forward 模式工具审批 |
| `turn_status` | `{pending_turns, status}` |
| `session_error` | CLI 进程退出 |
| `shell_result` | `!command` 输出 |
| `model_switched` | `/model` 切换成功 |
| `error` | 通用错误 |

## State Persistence

启用飞书时,bridge 把 per-owner session 元数据持久化到 `gateway_state.json`(与 gateway 二进制同目录)。

**自动迁移**:启动时 `gateway_state.json` 不存在但 `feishu_state.json` 存在 → 自动 rename。一次性,旧用户零感知。

**字段**:
```json
{
  "users": {
    "<owner_id>": {
      "active_label": "...",
      "focused_cli_id": "<cli session id>",
      "sessions": [
        {
          "cli_session_id": "...",
          "label": "...",
          "summary": "...",
          "working_dir": "...",
          "chat_id": "...",
          "status": "active|idle|archived",
          "jsonl_path": "~/.claude/projects/.../<id>.jsonl"
        }
      ]
    }
  }
}
```

**Legacy 兼容**:旧的 `dormant_sessions` / `archived` bool 字段在 `persist/json_store.go::readFile` 自动迁移成 `status: archived`。

## Feishu Bot

启用方式:设 `FEISHU_APP_ID` + `FEISHU_APP_SECRET`。架构:`channel/feishu.Channel`(Lark WS 长连接,无需公网 IP)→ `bridge.Bridge`(命令路由 + 渲染)→ `session.Manager`。

### 命令(`/help` 输出)

| 命令 | 说明 |
|------|------|
| `/new [label] [dir]` | 创建 session,设为 focused。单 arg 是 label 时,若 `projectRoot/label` 是目录则自动用作 workingDir |
| `/list` | 活跃 + 归档列表(按状态显示「切换」/「恢复」按钮,归档收在二级菜单) |
| `/switch [prefix]` | 无参=显示菜单(同 /list);带 prefix=智能切换(active→SetFocus,idle/archived→Reactivate) |
| `/resume [prefix]` | `/switch` 的别名,意图是从 idle/归档恢复,行为完全一致 |
| `/archive [prefix]` | 归档 session(active 会先关 runtime,然后保留记录) |
| `/model [name]` | 列出可用模型或切换当前 session 的模型 |
| `/diff` | 工作目录 git diff |
| `/config` | 查看/修改配置,`/config set <KEY> <VAL>` 直接改 |
| `/plan [desc]` | 进入 plan 模式(透传给 Claude CLI) |
| `/help` | 显示帮助(从 `b.commands` registry 自动生成) |
| `!<cmd>` | 在 focused session 的 working_dir 执行 shell(30s 超时) |
| 普通消息 | 发到 focused session;无 session 时自动创建/恢复 |
| 其他 `/xxx` | 透传给 Claude CLI(/commit、/compact 等) |

### Session 生命周期

- **autoresume**:`ResolveResumable` 优先 idle > ResumeHint > 最新归档
- **自动 idle**:CLI 进程退出 → bridge.handleCLIExit → `mgr.TransitionToIdle`(保留状态,下次发消息自动恢复)
- **显式归档**:仅 `/archive` 或按钮触发 `mgr.Archive`(关 runtime + status=archived)
- **持久化**:每次 lifecycle 变化后 `bridge.saveStateIfPossible` 自动 Save
- **安全红线**:archived session 是用户对话的唯一备份,只能用户主动 `RemoveArchived` 删除

### 动态摘要

- **默认摘要**:首条消息截断 30 字(不依赖 AI,立即可见)
- **AI 摘要**:每 `SUMMARY_INTERVAL` 轮通过 admin session 生成(默认 sonnet)
- **同一套 prompt**:`SUMMARY_INTERVAL` 触发的 active session 摘要,跟 disk-discovery 触发的 external session 摘要,都走 `buildSummaryPrompt`(`summary_worker.go`)
- **持久化**:
  - 自己 owner 的 session → `gateway_state.json.users[].sessions[].summary`
  - external(terminal/SDK 等)session → `gateway_state.json.external_summaries[cli_id]`,带 `PromptVersion` 字段
- **PromptVersion 失效**:bump `bridge.SummaryPromptVersion` 常量后,discovery 检测到旧版本会重新入队 worker

### Discovery & 摘要 Worker

启动时 + 每 `GATEWAY_DISCOVERY_RESCAN_INTERVAL`(默认 5m)定时扫 `~/.claude/projects/*/*.jsonl`,把未纳管的 claude session 以 `Origin="external"` 导入 manager,worker 后台生成 AI 摘要。

**Admin-internal session 检测**(避免网关自己跑的 admin session 污染 `/list`):

双层检测,任一命中即标 `IsAdminInternal=true` 跳过 import + summarize。

| 检测层 | 信号 | 何时生效 | 稳定性 |
|--------|------|---------|--------|
| **主:cwd 前缀** | `WorkingDir == /tmp/claude-code-gateway-admin` | admin session 跑在这个隔离目录 | 永久稳定,不依赖 prompt |
| **备:fingerprint 主选** | `[GATEWAY_ADMIN_SESSION_v1]` 出现在 jsonl head/tail | `buildSummaryPrompt` 头部强制注入此 marker | 稳定 — marker 与 prompt 文本解耦 |
| **备:fingerprint legacy** | "总结一个 claude-code session" / `jq -r 'select` 等 | 老 prompt 版本写的 admin session | 会随 prompt 改而失效,只作一次性历史清理 |

**保护机制**:
- `summary_worker_test.go::TestBuildSummaryPrompt_InjectsAdminMarker` 强制 prompt 含 marker — prompt 重写漏掉 marker 单测会红
- `discovery_cache.go::cacheSchemaVersion`:`DiscoveredSession` 加字段时 bump 此版本,旧 cache 自动失效避免 stale 标记
- worker `regenerate` 兜底:`SourceRef` 以 admin workdir 开头直接拒绝

**修改 admin marker 的流程**:bump `AdminSessionMarker` 到 `v2`(`summary_worker.go`)+ 同步更新 `adminPromptFingerprints` 第一项(`runtime/claude/discoverer.go`)。新 marker 立刻保护新 admin session,旧 v1 marker 的历史 session 仍能被 legacy 检测识别。

**`_skip_meta_` 处理**:admin 判定某 session 是 meta-like(空对话 / 跑其他 session 的 worker 任务等)时输出此 sentinel,worker 记录空 summary + 当前 PromptVersion 当"已处理"标记 — UI 渲染 fallback "(短对话,无摘要)",不重试。

## CLI Subprocess

`internal/runtime/claude/args.go::buildArgs` 构造完整 flag 列表:

```
claude -p --output-format stream-json --input-format stream-json --verbose
  PermissionMode != "" → --permission-prompt-tool stdio
  ResumeID 不空 → --resume <id>
  其余:--model / --max-turns / --include-partial-messages / --thinking / --effort /
       --max-budget-usd / --task-budget / --agent / --betas(repeatable) /
       --json-schema / --allowedTools(repeatable) / --disallowedTools(repeatable) /
       --tools(repeatable) / --mcp-config / --fallback-model / --session-id /
       --fork-session / --add-dir(repeatable) / --channels(repeatable) /
       --include-hook-events / --plugin-dir / --no-session-persistence /
       --permission-mode / --allow-dangerously-skip-permissions
```

优雅关闭:`end_session` control_request → 等退出 → 5s SIGTERM → 3s SIGKILL。

## Testing

`internal/session/sessiontest` 提供共享 fixtures:
- `sessiontest.FakeCLIPath(t)` — 编译 `sessiontest/fakecli/main.go`,缓存路径(sync.Once)
- Fake CLI 行为由 env 控制:`FAKE_CLI_SESSION_ID`、`FAKE_CLI_INIT_DELAY_MS`、`FAKE_CLI_EXIT_AFTER_USER`、`FAKE_CLI_FAIL_START`、`FAKE_CLI_PERMISSION_TOOL` 等
- `EventCollector` 收集 subscriber channel 上的事件,提供 `WaitForType` / `WaitDone` / `FindGatewayEvent`

`internal/runtime/fake` 提供 in-process fake runtime,供单测无需 spawn 真实进程时使用:
- `fake.NewRuntime(codec)` + `OnSpawn(fn)` 钩子可拦截 Spawn 调用
- `fake.Process.EmitInit/EmitResult/EmitMessage/Exit` 编程式驱动 session 事件

`internal/channel/fake` 提供 fake channel,记录 Outbound/Updates/Reactions,`Inject` 推 inbound 消息触发 handler。

## test.html

单文件浏览器测试控制台,需要更新以匹配新 `runtime` envelope 协议(旧的 30+ 字段已不再支持)。

## Pitfalls

- `--verbose` 是 CLI 必需参数,不加会启动失败
- `updatedInput` 在 permission allow 响应中必须存在(`omitempty` 会触发 ZodError);protocol/types.go 已用 `json:"updatedInput"` 而非 omitempty
- 工作目录必须是 `GATEWAY_DEFAULT_CWD` 的子目录或被显式 `AddAllowedBaseDir` 的目录,否则 Create 报错
- forward 模式下 CLI 内部有 AI classifier 预筛,只有被判定 `ask` 的工具才发 `control_request`(`Bash rm -rf` 会、`Read` 不会)。`AskUserQuestion` 工具因 `requiresUserInteraction: true` 始终触发
- 关闭 channel 后 manager 不会自动销毁 session — `bridge.Shutdown` 主动调 `TransitionToIdle` 保留对话
- gateway WS payload 协议已从 30+ flat field 改为 `runtime: {kind, config}` envelope —旧 client 不再兼容
