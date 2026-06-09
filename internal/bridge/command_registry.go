package bridge

import (
	"context"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/session"
)

// registerCommands populates b.commands with all built-in slash commands.
// To add a new command: append a Command literal here and implement the handler.
func (b *Bridge) registerCommands() {
	b.commands = []Command{
		{
			Name:    "/new",
			Usage:   "/new",
			Desc:    "创建新 session(无 focus 时弹项目选择;有 focus 时在当前项目下新建)",
			Handler: b.cmdNew,
		},
		{
			Name:    "/list",
			Aliases: []string{"/sessions"},
			Usage:   "/list",
			Desc:    "查看所有 session(活跃 + 归档,带切换/归档按钮)",
			Handler: b.wrapNoArgs(b.cmdList),
		},
		{
			Name:    "/switch",
			Usage:   "/switch [prefix]",
			Desc:    "切换到 active session(无参显示菜单)。idle/归档请用 /resume",
			Handler: b.cmdSwitch,
		},
		{
			Name:    "/archive",
			Aliases: []string{"/destroy"},
			Usage:   "/archive [prefix]",
			Desc:    "归档 session(默认归档 active;归档后不会自动加载)",
			Handler: b.cmdArchive,
		},
		{
			Name:    "/resume",
			Usage:   "/resume [prefix]",
			Desc:    "恢复 session(idle/归档/external 都行,跟 claude --resume 一致)",
			Handler: b.cmdResume,
		},
		{
			Name:    "/branch",
			Aliases: []string{"/fork"},
			Usage:   "/branch [名字]",
			Desc:    "在当前对话历史上创建分支 session,可选名字",
			Handler: b.cmdBranch,
		},
		{
			Name:    "/model",
			Usage:   "/model [name]",
			Desc:    "查看或切换模型(haiku, sonnet, opus)",
			Handler: b.cmdModel,
		},
		{
			Name:    "/effort",
			Usage:   "/effort [low|medium|high|max|auto]",
			Desc:    "切换 reasoning effort 等级(影响推理深度/速度/成本)",
			Handler: b.cmdEffort,
		},
		{
			Name:    "/permissions",
			Aliases: []string{"/allowed-tools"},
			Usage:   "/permissions [allow|deny|ask <pattern>]",
			Desc:    "查看 / 添加 / 删除 工具权限规则(allow / deny / ask)",
			Handler: b.cmdPermissions,
		},
		{
			Name:    "/add-dir",
			Usage:   "/add-dir [路径]",
			Desc:    "将目录加入当前 session 的允许列表(无参显示已添加目录;有 active session 时立即 respawn 生效)",
			Handler: b.cmdAddDir,
		},
		{
			Name:    "/diff",
			Usage:   "/diff",
			Desc:    "查看工作目录未提交 git 变更",
			Handler: b.wrapNoArgs(b.cmdDiff),
		},
		{
			Name:    "/config",
			Usage:   "/config [set <KEY> <VAL>]",
			Desc:    "查看/修改配置(/config set <KEY> <VAL> 直接改)",
			Handler: b.cmdConfig,
		},
		{
			Name:    "/rename",
			Usage:   "/rename <新名字>",
			Desc:    "重命名当前 session(更新网关显示标题并透传给 Claude CLI)",
			Handler: b.cmdRename,
		},
		{
			Name:    "/plan",
			Usage:   "/plan [description]",
			Desc:    "进入 plan 模式(透传给 Claude CLI)",
			Handler: b.forwardToCLI,
		},
		{
			Name:    "/plan-list",
			Usage:   "/plan-list",
			Desc:    "浏览 ~/.claude/plans 下已有的 plan 文件(按最近修改)",
			Handler: b.wrapNoArgs(b.cmdPlanList),
		},
		{
			Name:    "/project",
			Aliases: []string{"/projects"},
			Usage:   "/project",
			Desc:    "查看/添加项目(working directory),从中选 session",
			Handler: b.wrapNoArgs(b.cmdProject),
		},
		{
			Name:    "/stop",
			Usage:   "/stop [prefix]",
			Desc:    "打断当前会话正在执行的任务(等价 CLI 里的 ESC)",
			Handler: b.cmdStop,
		},
		{
			Name:    "/terminate",
			Usage:   "/terminate [prefix]",
			Desc:    "停止 CLI 子进程,会话变为 idle(再发消息会自动恢复)",
			Handler: b.cmdTerminate,
		},
		{
			Name:    "/status",
			Usage:   "/status",
			Desc:    "查看 gateway 状态(发现进度、摘要 worker、活跃 session)",
			Handler: b.wrapNoArgs(b.cmdStatus),
		},
		{
			Name:    "/recap",
			Usage:   "/recap",
			Desc:    "生成当前 session 的摘要(正在做什么 + 下一步)",
			Handler: b.wrapNoArgs(b.cmdRecap),
		},
		{
			Name:    "/export",
			Usage:   "/export",
			Desc:    "将当前 session 的完整对话导出为文件(飞书发文件消息;其他渠道回复文本)",
			Handler: b.wrapNoArgs(b.cmdExport),
		},
		{
			Name:    "/skills",
			Usage:   "/skills",
			Desc:    "列出可用 skills(项目 + 全局 .claude/skills/)",
			Handler: b.wrapNoArgs(b.cmdSkills),
		},
		{
			Name:    "/agents",
			Usage:   "/agents",
			Desc:    "列出可用 agents(~/.claude/agents/ + 项目 .claude/agents/)",
			Handler: b.wrapNoArgs(b.cmdAgents),
		},
		{
			Name:    "/commands",
			Usage:   "/commands",
			Desc:    "列出自定义 slash 命令(~/.claude/commands/ + 项目 .claude/commands/)",
			Handler: b.wrapNoArgs(b.cmdCommands),
		},
		{
			Name:    "/mcp",
			Usage:   "/mcp",
			Desc:    "列出 MCP servers(~/.claude.json + 项目 .mcp.json)",
			Handler: b.wrapNoArgs(b.cmdMCP),
		},
		{
			Name:    "/hooks",
			Usage:   "/hooks",
			Desc:    "列出 hooks(~/.claude/settings.json + 项目 .claude/settings.json)",
			Handler: b.wrapNoArgs(b.cmdHooks),
		},
		{
			Name:    "/memory",
			Usage:   "/memory",
			Desc:    "列出 memory 文件(CLAUDE.md / rules / local),查看当前生效的指令",
			Handler: b.wrapNoArgs(b.cmdMemory),
		},
		{
			Name:    "/btw",
			Usage:   "/btw <问题>",
			Desc:    "基于当前 session 上下文问旁支问题(不写入主对话)",
			Handler: b.cmdBTW,
		},
		{
			Name:    "/help",
			Usage:   "/help",
			Desc:    "显示此帮助",
			Handler: b.wrapNoArgs(b.cmdHelp),
		},
		{
			Name:    "/cron",
			Usage:   "/cron [list|add|remove|enable|disable|history]",
			Desc:    "管理定时任务(无参数显示管理卡片)",
			Handler: b.cmdCron,
		},
	}
}

// wrapNoArgs adapts a no-args handler to the CommandHandler signature.
func (b *Bridge) wrapNoArgs(fn func(ctx context.Context, m channel.InboundMessage)) CommandHandler {
	return func(ctx context.Context, m channel.InboundMessage, _ string) {
		fn(ctx, m)
	}
}

// sessionPayloadID returns the stable identifier used in card-action
// payloads. We prefer the CLI session id because mgr.Reactivate replaces
// the gateway-internal session.ID — a card rendered before a reactivate
// would otherwise carry a stale id that mgr.Get can't find.
func sessionPayloadID(info session.SessionInfo) string {
	if info.CLISessionID != "" {
		return info.CLISessionID
	}
	return info.ID
}
