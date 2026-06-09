//go:build !windows

// Package shellexec provides a platform-independent helper for running a
// shell command string. On Unix it uses "sh -c"; on Windows "cmd.exe /c".
package shellexec

import (
	"context"
	"os/exec"
)

// Command returns an *exec.Cmd that runs cmdStr through the platform shell.
func Command(ctx context.Context, cmdStr string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", cmdStr)
}
