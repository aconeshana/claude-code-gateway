// Package runtime defines the abstraction for a CLI-based agent runtime that
// the session manager spawns and talks to. The default implementation is
// runtime/claude, but additional runtimes (codex, etc.) can be plugged in.
package runtime

import (
	"context"
	"encoding/json"
	"time"
)

// Config is the marker interface for runtime-specific configuration.
// Implementations (e.g. claude.Config, fake.Config) provide their own concrete
// type and the session manager passes it through to Runtime.Spawn without
// inspecting fields. RuntimeName must match the receiving Runtime's Name().
type Config interface {
	RuntimeName() string
}

// SpawnRequest carries the parameters needed to start a runtime instance.
type SpawnRequest struct {
	WorkingDir string
	Env        []string // KEY=VALUE pairs appended to the inherited environment
	Config     Config   // runtime-specific config (opaque)
	ResumeID   string   // runtime-internal id to resume; empty for a fresh start
}

// Callbacks are invoked by Process during its lifetime.
type Callbacks struct {
	// OnMessage is called for every line of structured output from the runtime.
	// The bytes are owned by the receiver and must be copied if retained.
	OnMessage func(raw json.RawMessage)
	// OnExit is called exactly once when the runtime exits, with the underlying
	// error from the OS process (nil for clean exit).
	OnExit func(err error)
}

// Runtime spawns Process instances for a given backend (claude-code, codex, …).
type Runtime interface {
	// Name returns a short identifier ("claude-code", "codex", …) used in logs.
	Name() string

	// Spawn starts a runtime instance. Callbacks must be set before the runtime
	// emits any messages; OnMessage/OnExit may be invoked concurrently.
	Spawn(ctx context.Context, req SpawnRequest, cb Callbacks) (Process, error)

	// Codec returns the encoder/decoder this runtime understands. Codec
	// instances must be safe to call concurrently from multiple goroutines.
	Codec() Codec
}

// Process represents a running runtime instance. Implementations must be safe
// to call from multiple goroutines.
type Process interface {
	// Write enqueues a single encoded message for delivery to the runtime via
	// stdin (or equivalent). Returns an error if the process has exited.
	Write(raw []byte) error

	// GracefulStop sends a shutdown request and waits up to timeout for the
	// runtime to exit cleanly. After timeout it falls back to Kill().
	GracefulStop(timeout time.Duration) error

	// Kill terminates the runtime immediately (SIGKILL for process-based
	// runtimes). Safe to call after the process has already exited.
	Kill() error

	// Done is closed when the runtime has exited (cleanly or otherwise).
	Done() <-chan struct{}

	// RuntimeID returns the runtime-internal identifier for this instance
	// (e.g. claude-code's session_id from the init message). Returns "" until
	// the runtime has populated it. Implementations should make this stable
	// after first non-empty value.
	RuntimeID() string
}

// Codec converts between high-level session intents and the wire format the
// runtime understands. The session layer holds a Codec via Runtime.Codec().
type Codec interface {
	// EncodeUserText encodes a user message containing plain text.
	EncodeUserText(text, uuid string) ([]byte, error)

	// EncodeUserBlocks encodes a user message containing structured content
	// blocks (typically used for image attachments).
	EncodeUserBlocks(blocks []interface{}, uuid string) ([]byte, error)

	// EncodeControlResponse encodes a response to a runtime-initiated control
	// request (e.g. permission decision). For "deny" behavior the message is
	// the rejection reason; updatedInput may be nil.
	EncodeControlResponse(requestID, toolUseID, behavior, message string, updatedInput map[string]interface{}) ([]byte, error)

	// EncodeControl wraps an arbitrary control request payload (the payload is
	// the JSON for the inner request object, e.g. `{"subtype":"interrupt"}`).
	EncodeControl(payload json.RawMessage) ([]byte, error)

	// EncodeKeepAlive encodes a periodic keep-alive message.
	EncodeKeepAlive() ([]byte, error)

	// ParseEvent extracts the high-level event metadata from a raw runtime
	// output line. The original bytes are returned in Event.Raw so the session
	// layer can forward them to subscribers verbatim.
	ParseEvent(raw json.RawMessage) (Event, error)
}

// EventKind classifies a runtime output line. New kinds may be added as new
// runtimes are introduced; consumers should default to KindUnknown for any
// kind they do not recognize.
type EventKind int

const (
	KindUnknown EventKind = iota
	KindInit
	KindAssistant
	KindResult
	KindControlRequest
	KindControlResponse
	KindControlCancel
	KindKeepAlive
	KindToolProgress
)

// Event is the codec-agnostic view of a single runtime output line.
type Event struct {
	Kind      EventKind
	Subtype   string
	RuntimeID string          // populated for init events
	Raw       json.RawMessage // original bytes; safe to forward to subscribers
}
