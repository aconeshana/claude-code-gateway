// Package fake provides a programmable runtime.Runtime implementation for
// tests. Unlike runtime/claude it does not spawn any subprocess; messages are
// emitted directly via Process.EmitMessage.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// Config is a fake.runtime.Config (opaque marker, no fields needed).
type Config struct{}

func (Config) RuntimeName() string { return "fake" }

// SpawnInfo records the inputs to a single Spawn call.
type SpawnInfo struct {
	Request runtime.SpawnRequest
	Process *Process
}

// Runtime is a programmable runtime.Runtime. By default Spawn returns a
// Process that is "running" until Exit is called and has a fixed RuntimeID
// derived from the spawn count. Tests can override behavior via OnSpawn.
type Runtime struct {
	mu      sync.Mutex
	spawned []SpawnInfo
	onSpawn func(req runtime.SpawnRequest, cb runtime.Callbacks) (*Process, error)
	codec   runtime.Codec
}

// NewRuntime creates a fake runtime. If codec is nil, ParseEvent / Encode
// methods on the returned Codec return inert results suitable for tests that
// don't exercise the codec.
func NewRuntime(codec runtime.Codec) *Runtime {
	if codec == nil {
		codec = noopCodec{}
	}
	return &Runtime{codec: codec}
}

func (r *Runtime) Name() string         { return "fake" }
func (r *Runtime) Codec() runtime.Codec { return r.codec }

func (r *Runtime) Spawn(ctx context.Context, req runtime.SpawnRequest, cb runtime.Callbacks) (runtime.Process, error) {
	r.mu.Lock()
	hook := r.onSpawn
	r.mu.Unlock()

	var (
		p   *Process
		err error
	)
	if hook != nil {
		p, err = hook(req, cb)
	} else {
		p = newDefaultProcess(cb)
	}
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.spawned = append(r.spawned, SpawnInfo{Request: req, Process: p})
	r.mu.Unlock()
	return p, nil
}

// OnSpawn registers a callback invoked instead of the default behavior.
func (r *Runtime) OnSpawn(fn func(req runtime.SpawnRequest, cb runtime.Callbacks) (*Process, error)) {
	r.mu.Lock()
	r.onSpawn = fn
	r.mu.Unlock()
}

// Spawns returns the recorded spawn calls in order. The returned slice is a
// copy and safe to inspect.
func (r *Runtime) Spawns() []SpawnInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SpawnInfo, len(r.spawned))
	copy(out, r.spawned)
	return out
}

// LastSpawn returns the most recent SpawnInfo, or false if none.
func (r *Runtime) LastSpawn() (SpawnInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.spawned) == 0 {
		return SpawnInfo{}, false
	}
	return r.spawned[len(r.spawned)-1], true
}

// Process is the fake runtime.Process. Tests drive it via EmitMessage / Exit.
type Process struct {
	mu        sync.Mutex
	idMu      sync.RWMutex
	runtimeID string
	written   [][]byte
	done      chan struct{}
	closed    bool
	exitErr   error
	cb        runtime.Callbacks
}

func newDefaultProcess(cb runtime.Callbacks) *Process {
	return &Process{
		done: make(chan struct{}),
		cb:   cb,
	}
}

// NewProcess creates a fake Process without registering it with a Runtime;
// useful when an OnSpawn hook wants to construct its own Process.
func NewProcess(cb runtime.Callbacks) *Process {
	return newDefaultProcess(cb)
}

func (p *Process) Write(raw []byte) error {
	select {
	case <-p.done:
		return errors.New("process exited")
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.written = append(p.written, cp)
	return nil
}

// Written returns a copy of all bytes written via Write.
func (p *Process) Written() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.written))
	for i, b := range p.written {
		c := make([]byte, len(b))
		copy(c, b)
		out[i] = c
	}
	return out
}

func (p *Process) GracefulStop(timeout time.Duration) error {
	p.Exit(nil)
	return nil
}

func (p *Process) Kill() error {
	p.Exit(errors.New("killed"))
	return nil
}

func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) RuntimeID() string {
	p.idMu.RLock()
	defer p.idMu.RUnlock()
	return p.runtimeID
}

// SetRuntimeID changes the value returned by RuntimeID.
func (p *Process) SetRuntimeID(id string) {
	p.idMu.Lock()
	p.runtimeID = id
	p.idMu.Unlock()
}

// EmitMessage invokes the OnMessage callback with the given raw bytes.
// Safe to call concurrently.
func (p *Process) EmitMessage(raw json.RawMessage) {
	if p.cb.OnMessage != nil {
		p.cb.OnMessage(raw)
	}
}

// EmitInit emits a claude-style system/init message and updates RuntimeID.
func (p *Process) EmitInit(sessionID string) {
	p.SetRuntimeID(sessionID)
	msg, _ := json.Marshal(map[string]interface{}{
		"type":           "system",
		"subtype":        "init",
		"session_id":     sessionID,
		"model":          "fake",
		"cwd":            "/tmp",
		"tools":          []string{},
		"permissionMode": "default",
		"uuid":           "init",
	})
	p.EmitMessage(msg)
}

// EmitResult emits a claude-style result message.
func (p *Process) EmitResult(sessionID string, numTurns int) {
	msg, _ := json.Marshal(map[string]interface{}{
		"type":           "result",
		"subtype":        "success",
		"duration_ms":    1,
		"is_error":       false,
		"num_turns":      numTurns,
		"result":         "ok",
		"total_cost_usd": 0.0,
		"session_id":     sessionID,
		"uuid":           "result",
	})
	p.EmitMessage(msg)
}

// Exit closes Done and invokes OnExit (idempotent).
func (p *Process) Exit(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.exitErr = err
	close(p.done)
	p.mu.Unlock()
	if p.cb.OnExit != nil {
		p.cb.OnExit(err)
	}
}

// noopCodec returns inert results for codec methods; used when tests don't
// care about encoding details.
type noopCodec struct{}

func (noopCodec) EncodeUserText(text, uuid string) ([]byte, error) { return []byte(text), nil }
func (noopCodec) EncodeUserBlocks(blocks []interface{}, uuid string) ([]byte, error) {
	return json.Marshal(blocks)
}
func (noopCodec) EncodeControlResponse(reqID, toolUseID, behavior, msg string, ui map[string]interface{}) ([]byte, error) {
	return []byte(behavior), nil
}
func (noopCodec) EncodeControl(payload json.RawMessage) ([]byte, error) { return payload, nil }
func (noopCodec) EncodeKeepAlive() ([]byte, error)                      { return []byte(`{"type":"keep_alive"}`), nil }
func (noopCodec) ParseEvent(raw json.RawMessage) (runtime.Event, error) {
	return runtime.Event{Raw: raw, Kind: runtime.KindUnknown}, nil
}
