package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/protocol"
	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// Runtime is the claude-code implementation of runtime.Runtime.
type Runtime struct {
	mu      sync.RWMutex
	cliPath string
}

// NewRuntime creates a runtime that spawns claude-code at the given path.
func NewRuntime(cliPath string) *Runtime {
	return &Runtime{cliPath: cliPath}
}

// SetCLIPath updates the binary path used for future Spawn calls. Existing
// processes are not affected.
func (r *Runtime) SetCLIPath(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cliPath = path
}

func (r *Runtime) currentCLIPath() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cliPath
}

func (r *Runtime) Name() string { return "claude-code" }

func (r *Runtime) Codec() runtime.Codec { return Codec{} }

func (r *Runtime) Spawn(ctx context.Context, req runtime.SpawnRequest, cb runtime.Callbacks) (runtime.Process, error) {
	cfg, ok := req.Config.(Config)
	if !ok {
		if pcfg, isPtr := req.Config.(*Config); isPtr && pcfg != nil {
			cfg = *pcfg
		} else if req.Config != nil {
			return nil, fmt.Errorf("claude runtime: expected *claude.Config or claude.Config, got %T", req.Config)
		}
	}

	args := buildArgs(cfg, req)
	cmd := exec.CommandContext(ctx, r.currentCLIPath(), args...)
	cmd.Dir = req.WorkingDir
	if len(req.Env) > 0 {
		cmd.Env = append(cmd.Environ(), req.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start CLI: %w", err)
	}

	p := &process{
		cmd:    cmd,
		stdin:  stdin,
		done:   make(chan struct{}),
		onMsg:  cb.OnMessage,
		onExit: cb.OnExit,
	}

	go p.readStdout(stdout)
	go p.readStderr(stderr)
	go p.waitExit()

	return p, nil
}

// process implements runtime.Process for claude-code.
type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdinMu sync.Mutex

	done    chan struct{}
	exitErr error

	onMsg  func(json.RawMessage)
	onExit func(error)

	idMu      sync.RWMutex
	runtimeID string
}

func (p *process) Write(raw []byte) error {
	select {
	case <-p.done:
		return errors.New("process exited")
	default:
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if _, err := p.stdin.Write(raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		if _, err := p.stdin.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

func (p *process) GracefulStop(timeout time.Duration) error {
	endReq, _ := json.Marshal(protocol.NewStdinControlRequest(
		json.RawMessage(`{"subtype":"end_session"}`),
	))
	_ = p.Write(endReq)

	select {
	case <-p.done:
		return p.exitErr
	case <-time.After(timeout):
	}

	if err := p.signal(syscall.SIGTERM); err != nil {
		return p.Kill()
	}
	select {
	case <-p.done:
		return p.exitErr
	case <-time.After(3 * time.Second):
	}
	return p.Kill()
}

func (p *process) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *process) signal(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Signal(sig)
}

func (p *process) Done() <-chan struct{} { return p.done }

func (p *process) RuntimeID() string {
	p.idMu.RLock()
	defer p.idMu.RUnlock()
	return p.runtimeID
}

func (p *process) readStdout(stdout io.ReadCloser) {
	decoder := protocol.NewDecoder(stdout)
	for {
		raw, err := decoder.Decode()
		if err != nil {
			return
		}
		p.captureRuntimeID(raw)
		if p.onMsg != nil {
			p.onMsg(raw)
		}
	}
}

func (p *process) captureRuntimeID(raw json.RawMessage) {
	msgType, subtype, err := protocol.ParseType(raw)
	if err != nil || msgType != protocol.MsgTypeSystem || subtype != protocol.SubtypeInit {
		return
	}
	var init protocol.SystemInitMessage
	if err := json.Unmarshal(raw, &init); err != nil || init.SessionID == "" {
		return
	}
	p.idMu.Lock()
	if p.runtimeID == "" {
		p.runtimeID = init.SessionID
	}
	p.idMu.Unlock()
}

func (p *process) readStderr(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		log.Printf("[CLI stderr] %s", scanner.Text())
	}
}

func (p *process) waitExit() {
	p.exitErr = p.cmd.Wait()
	close(p.done)
	if p.onExit != nil {
		p.onExit(p.exitErr)
	}
}
