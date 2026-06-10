package bridge

import "bytes"

// limitedWriter caps how many bytes are written to its buffer.
// Used by runGit (diff_command.go) and handleShell (shell_command.go) to
// avoid runaway memory consumption from misbehaving subprocesses.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return 0, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}
