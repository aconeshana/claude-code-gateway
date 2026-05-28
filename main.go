package main

import (
	"fmt"
	"os"
)

const usage = `Usage: ccg <command> [options]

Commands:
  serve      Run the gateway in the foreground (default when no command given)
  start      Start the gateway as a background daemon
  stop       Stop the running daemon
  restart    Restart the daemon
  status     Show daemon status and health check
  logs       Tail daemon logs (flags: -n N, --no-follow)
  register   Run Feishu app registration wizard

Options for serve / start:
  -config <path>   Path to JSON config file

Environment overrides (see CLAUDE.md for full list):
  CCG_LOG_FILE      Override daemon log file path (default: /var/log/ccg.log)
  FEISHU_APP_ID     Feishu App ID (set automatically by 'register')
  FEISHU_APP_SECRET Feishu App Secret

Examples:
  ccg                        # run in foreground
  ccg start                  # start daemon (prompts for Feishu setup if needed)
  ccg start -config my.json  # start daemon with custom config
  ccg register               # create a new Feishu PersonalAgent app
  ccg status
  ccg logs -n 200
  ccg stop
`

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			cmdServe()
			return
		case "start":
			cmdStart(os.Args[2:])
			return
		case "stop":
			cmdStop()
			return
		case "restart":
			cmdRestart(os.Args[2:])
			return
		case "status":
			cmdStatus()
			return
		case "logs":
			cmdLogs(os.Args[2:])
			return
		case "register":
			cmdRegister()
			return
		case "help", "--help", "-h":
			fmt.Print(usage)
			return
		}
	}
	cmdServe()
}
