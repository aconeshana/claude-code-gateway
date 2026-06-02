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

## V2 路由优先级(顶层原则)

所有 Session / Thread / Focus 决策按以下顺序应用,**前者优先级最高**:

1. **复用已有 thread**:若 sess 已有 ThreadID 绑定,任何"恢复/激活/转发"行为都路由进该 thread;不开新 thread,不让该 sess 抢主聊天 focus。
2. **保护已有 focus**:若用户主聊天有 Active focus,新创建/恢复的 sess 一律进新 thread;priorFocus 保持。
3. **主聊天作为默认容器**:无 thread、无 priorFocus 时,新 sess 才接管主聊天 focus。
4. **主聊天只承载命令/全局动作**:thread 内只做该 sess 自身的对话。

实现要点:
- 原则 1 由 `afterCreateOrActivate` 在 `priorFocus == nil` 分支显式检查 `newSess.ThreadID` 落地 — 命中则 `ClearFocus` 并把 welcome 发进既有 thread(`openThreadForSession` 内部 Reply API 复用)。
- 原则 2 由 `snapshotFocus` + `afterCreateOrActivate` 主路径落地(priorFocus 还原 + 新 sess 进新 thread)。
- 原则 3 是 mgr.Create / Reactivate 默认副作用(直接 SetFocus 到新 sess)。
- 原则 4 由各命令的 `m.ThreadID != ""` 拒绝分支落地(`/new` `/switch` `/branch` `/list` `/project` 拒绝 thread)。

**核心不变量**:focus 只可能是 None 或指向 Active session(`TransitionToIdle` ClearFocus, `Archive` ClearFocus, `/terminate` 兜底切下一个 active)。

**详细状态机**:见 `docs/state-machine.md`(包含 5 个 mermaid 图 + 命令矩阵 + 修订过的 BUG 列表)。

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

### Project 模型

**项目 = working directory**。一个项目可以有 N 个 session(各自独立的 CLI 历史)。项目集合在内存里聚合,不持久化项目本身(有 session 的 dir 自然"活着")。

`b.projectsForUser(userID)` 来源:
- `mgr.ListDiscoverableByOwner(userID, shareExternal)` 返回的 owned + 可选 external session 的 WorkingDir
- `mgr.ListArchivedByOwner(userID)` 的归档 session WorkingDir
- `b.manualProjects[userID]` — 用户通过 `[➕ 添加项目]` + 目录选择器手动加的 path(transient 内存集合,不持久化)

一二级菜单算法**必须用同一份数据源**,否则 count 不一致(`countSessionsInProject` 跟 `buildProjectCard` 走同一条 filter pipeline)。

### PickDir card flow(目录选择器)

通用目录浏览组件(纯 Card + Button,任何 channel 都能渲染)。card action 链:

```
pick_dir (path, purpose, sort=name|mtime) → handlePickDir
  ├─ ls path,每个子目录一个 [📂 name] 按钮
  ├─ nav: [← 上级] [🏠 家目录] [按时间/名称 排序] [✓ 选这里]
  └─ pickDirReturnButton(purpose) — 按 purpose 决定"返回"按钮的目标 action
       add_project → [← 返回项目] (回 show_projects → buildProjectsCard)

pick_dir_confirm (path, purpose) → handlePickDirConfirm
  switch purpose:
    add_project → addManualProject + 重渲项目卡
    setup_cwd   → WriteEnvFile GATEWAY_DEFAULT_CWD + applyConfigChange
```

加新入口只需 (a) 触发处给按钮设 `purpose=xxx`、(b) 在 `pickDirReturnButton` 加返回 case、(c) 在 `handlePickDirConfirm` 加 purpose 分支。picker 本身不用改。

### Workspace 路径(简化)

`GATEWAY_DEFAULT_CWD` 是主聊天 plain text 兜底 dir(用户没选项目直接发字时新建 session 用)。默认 `~`,不强制 setup。`/new` 不再用 `GATEWAY_PROJECT_ROOT` 解析子目录 — 项目通过 `/project` 显式选,跨级目录由 PickDir flow 自由组合。

### Card 布局(飞书)

通用 `channel.Card/Section/Form` 抽象在 feishu renderer 提供 3 种增强布局,通过 hint 字段触发:

- `Section.ButtonLayout = "fill"` — 多按钮等宽撑满卡宽(column_set + width=fill,N=2 时 flex_mode=bisect)。适用 `/list` 每条 session 行(4 个按钮均匀分布,不再左聚集)
- `Section.ButtonLayout = "trailing"` — markdown 左 + 单按钮右(column_set 2 列:左 weighted weight=5, 右 width=auto)。适用单操作行,行高比堆叠减半
- `Form.SecondaryButtons []Button` — 跟 Submit 横排放在 form 末尾(column_set 左对齐 + width=auto)。适用 `[保存][取消]` 这类二选一表单,避免按钮各占一行
- `Form.LeadingButtons []Button` — 输入框前的非提交按钮(跟 input + Submit 同一 column_set)。适用 `/skills [详情][input][执行]` 这类 inline 操作

设计参考来自 [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) `platform/feishu/card.go` 的 `CardActionLayoutEqualColumns` 和 `CardListItem` 模式。**新加卡片渲染优先用这些 hint,不要直接堆 Markdown + 多个 Buttons section**。

## Configuration

| 环境变量 | 默认 | 说明 |
|---------|-----|------|
| `CLAUDE_CLI_PATH` | `claude` | CLI 二进制路径 |
| `GATEWAY_LISTEN_ADDR` | `:8080` | WS 监听地址 |
| `GATEWAY_DEFAULT_CWD` | `~` | 主聊天 plain text 自动建 session 时的兜底 dir(可热改) |
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
| `/new` | 创建 session(V2 无参数)。**无 focus** → 弹 `/project` 卡;**有 focus** → 在 focus 所在项目下建新 session,自动开 thread |
| `/project` (alias `/projects`) | 项目管理: 列已知项目 + `[进入][新建 session]`,底部 `[➕ 添加项目]` 进目录选择器 |
| `/list` | 活跃 + 归档列表(按项目分组,二级菜单展开;每行有「切换/恢复/归档/刷新摘要/重命名」) |
| `/switch [prefix]` | 切换主聊天 focus。**旧 focus 自动收纳到 thread**(无 thread → sendCard + openThread;已绑 → ping 原 thread),新 focus 提到主聊天 |
| `/resume [prefix]` | 恢复 idle/archived/external session。走 `afterCreateOrActivate`:无 prior focus → 当焦点;有 prior focus → 开 thread + 还原 focus |
| `/branch [name]` (alias `/fork`) | 在当前对话历史上 fork 一个 session,**永远开 thread**(不抢 focus) |
| `/archive [prefix]` | 归档 session(active 会先关 runtime,然后保留记录) |
| `/rename <name>` | 重命名 focused session 的 CustomTitle(也可在 /list 卡里点 `[重命名]` 走 card form) |
| `/model [name]` | 列出可用模型或切换当前 session 的模型 |
| `/diff` | 工作目录 git diff |
| `/config` | 查看/修改配置,`/config set <KEY> <VAL>` 直接改 |
| `/plan [desc]` | 进入 plan 模式(透传给 Claude CLI);ExitPlanMode 时 bot 弹卡显示 plan 内容 + `[批准/拒绝]` |
| `/plan-list` | 浏览 `~/.claude/plans` 目录;thread 里点 `[查看]` 详情卡回到原 thread(payload 编 thread_id) |
| `/stop [prefix]` | 打断当前 turn(等价 CLI ESC) |
| `/terminate [prefix]` | 停 CLI 子进程,session 变 idle(下次发消息自动 reactivate) |
| `/status` | gateway 状态: discovery / summary worker / sessions / focused 项目 + 项目数 |
| `/help` | 显示帮助(从 `b.commands` registry 自动生成) |
| `!<cmd>` | 在 focused session 的 working_dir 执行 shell(30s 超时) |
| 普通消息 | 主聊天 → focused session;thread 里 → 该 thread 绑定的 session;无 focus → 自动创建/恢复 |
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

### Thread 路由(Thread = Session)

利用飞书原生"话题"(thread)做并行 session 的物理隔离 — 用户不再需要反复 `/switch`。

**核心模型 — 主聊天 focus 稳定 + 自动开 thread**:

| 用户行为 | bot 行为 |
|----------|----------|
| 首次 `/new`(没有 active focus) | 创建 sess,设为主聊天 focus,**不开 thread** |
| `/new` 已经有 focus | 创建 sess,**自动开 thread**(reply_in_thread),focus 不变 |
| `/resume <prefix>` 没有 active focus | 恢复 sess,设为主聊天 focus |
| `/resume <prefix>` 已经有 focus | 恢复 sess,自动开 thread,focus 不变 |
| `/branch <name>` | 永远开 thread(必然已有焦点)。focus 仍是父 session |
| `/switch <prefix>` | 旧 focus 收纳到话题(若无 thread)→ 设新 focus |
| 主聊天 plain text | 路由到 focused session(任何 session,包括 thread-bound) |
| 话题里 plain text | 通过 thread_id 路由到绑定的 session |
| 话题里 `/new` `/switch` `/branch` | **拒绝**(响应"请回主聊天") |
| 任何位置 `/list` `/status` `/config` `/diff` 等 | 响应在原位置(thread 内自洽) |

**关键约束**:
- focus 概念稳定 — 主聊天的"上下文"由 focus 表达,且只通过显式 `/switch` 改变
- 一个 session 可以**同时**是 focus(主聊天)+ 绑某个 thread,两个入口都活
- 钉钉 channel 不实现 `channel.ThreadOpener`,降级到主聊天(无 thread 路径)
- **thread inbound 不抢主聊天 focus**: `resolveOrCreateSession` thread 分支用 `snapshotFocus + restoreFocus` 包住 Reactivate/Create 调用 (mgr 内部 SetFocus 副作用会被还原)

**LastInbound — 出站跟随入站位置**:
`session.Session` 持有 transient 字段 `lastInbound{ChatID,ThreadID,RootMessageID}`,在 handleText/handleBlocks 入口由 `sess.SetLastInbound(...)` 设置。出站 helper 链路:
- `replyText / replyCard(m, ...)` — inbound 触发的回复,按 m.ThreadID 决定 anchor
- `sendTextForSession / sendCardForSession(sess, ...)` — streamSession 用,**优先用 sess.LastInbound**(用户最后一次发消息的位置),fallback 到 sess.RootMessageID
- 效果: thread-bound session 在主聊天被发了消息,bot 输出在主聊天;反之亦然

**Switch 收纳的两条路径**(`switchFocusTo` in `commands.go`):
- 旧 focus **没绑 thread** → sendCard 主聊天发"📦 Session 收纳"卡 + `openThreadForSession` 开新 thread + `SetLastInbound(thread)` 让 in-flight 输出转向
- 旧 focus **已绑 thread** → 不重开(避免重复话题入口卡),`SetLastInbound(thread)` + 在原 thread reply 一条"📌 主聊天 focus 已切走"(原入口卡显示新回复指示)

**Card payload 用 CLI session id**(`sessionPayloadID`):
mgr.Reactivate 会换掉 gateway-internal session.ID。卡上的 button payload 用 CLI session id(stable across reactivate),handler 用 `b.resolveSessionByPayload(id)` 双查兜底(先 `mgr.Get`,再 `mgr.GetByCLISessionID`),拿到 sess 后用 `sess.ID` 调 manager API。

**MessageCount(`/list` 显示 `· N 条`)**:
- 来源 1: Discoverer.parseSession 用 `countUserTurns` 流式数 `"type":"user"` 行(admin internal 跳过省 I/O)
- 来源 2: 实时自增 — `sess.SendMessage / SendMessageBlocks` 内部 `s.MessageCount++`
- 来源 3: Lazy 回填 — `filterAliveSessions` 渲染 /list 前对 alive 但 count=0 的 session 跑一次 `countUserTurnsInJSONL`,写回 `sess.SetMessageCount(n)`。一次性开销 ~50ms/文件,有缓存后近 0

**Ghost session 处理**:
`filterAliveSessions` 检查 `claudeRT.SessionJSONLPath(...)` 是否存在,缺失的(典型 `/branch` fork 没对话过)从 /list 视图过滤掉。`resolveOrCreateSession` step 2 (ResolveResumable) 撞到 ghost 时调 `mgr.Archive` 自动清,避免无限循环。

**ThreadOpener capability**(`channel/channel.go`):
- 可选接口,bot 通过 type assertion 检测:`if opener, ok := ch.(channel.ThreadOpener); ok`
- 飞书实现 `OpenThread(ctx, anchorMsgID, OutboundMessage) → (msgID, threadID, err)`
- 内部用 Lark Reply API + `reply_in_thread=true`

**bridge helper**(`internal/bridge/bridge.go`):
- `snapshotFocus(ownerID)` — 在 mgr.Create / Reactivate **之前**捕获,因为 mgr 内部 SetFocus 会覆盖
- `afterCreateOrActivate(ctx, newSess, ownerID, anchorMsgID, welcome, priorFocus, forceThread)` — 统一规则:无 priorFocus 当 focus,有 priorFocus 还原 + 新 sess 进 thread
- `openThreadForSession(ctx, sess, anchorMsgID, welcome)` — 调 ThreadOpener + BindThread
- `replyText / replyCard(ctx, m, ...)` — inbound 触发的回复(命令、错误提示等),自动从 `m.ThreadID` 决定是否进 thread
- `sendTextForSession / sendCardForSession(ctx, sess, chatID, ...)` — session 流式输出(`renderer.go`),从 `sess.RootMessageID` 取 anchor

**数据模型**:
- `session.Session` 加 `ThreadID, RootMessageID string`,持久化到 `gateway_state.json.users[].sessions[].thread_id / root_message_id`
- `channel.InboundMessage` 加 `ThreadID, RootID, ParentID`(飞书 SDK 已暴露 `larkim.EventMessage.{ThreadId,RootId,ParentId}`)
- `channel.OutboundMessage` 加 `ReplyToMessageID, OpenThread`:非空时 feishu channel 调 Reply API,钉钉忽略

**错误恢复**:thread root 被删 → Reply API 返回 230020/230002/230001(`feishu.ErrReplyAnchorMissing`)→ `handleOutboundError` 清空 `session.ThreadID` + 通知用户"话题已失效,已切回主聊天" + 重试 Create 到主聊天。

**新增命令处理函数务必用 `replyXxx` 而非 `sendXxx`**,否则在 thread 里发命令时响应会飞到主聊天,造成断裂。所有"焦点切换性"命令(`/new` `/resume` `/branch`)都应调 `snapshotFocus` + `afterCreateOrActivate`,不能直接 SetFocus。

**所有 session-related 文案务必带 displayId(短 8 字符 hash)**,用户在主聊天看到话题入口卡片时能立刻识别"这是哪个 session"。

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
