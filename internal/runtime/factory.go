package runtime

import "encoding/json"

// Factory parses an opaque JSON payload (received from a WebSocket client or
// other transport) into a runtime-specific Config. Implementations are
// expected to be paired with their Runtime (claude.Factory ↔ claude.Runtime).
type Factory interface {
	// Kind returns the value of the payload's "kind" discriminator that this
	// factory handles (e.g. "claude").
	Kind() string

	// Parse decodes config from the JSON payload. payload may be nil/empty,
	// in which case a zero-value Config should be returned.
	Parse(payload json.RawMessage) (Config, error)
}

// Registry maps "kind" strings to Factories. The zero value is not usable;
// use NewRegistry().
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a factory. Subsequent registrations with the same Kind
// overwrite the previous one.
func (r *Registry) Register(f Factory) {
	r.factories[f.Kind()] = f
}

// Parse picks the appropriate factory based on the "kind" field of the
// envelope and delegates to it. If kind is empty, fallbackKind is used.
//
// Envelope shape:
//
//	{"kind": "claude", "config": {...}}
func (r *Registry) Parse(envelope json.RawMessage, fallbackKind string) (Config, error) {
	if len(envelope) == 0 {
		if fallbackKind == "" {
			return nil, nil
		}
		f, ok := r.factories[fallbackKind]
		if !ok {
			return nil, &UnknownKindError{Kind: fallbackKind}
		}
		return f.Parse(nil)
	}

	var env struct {
		Kind   string          `json:"kind"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, err
	}
	kind := env.Kind
	if kind == "" {
		kind = fallbackKind
	}
	if kind == "" {
		return nil, &UnknownKindError{Kind: ""}
	}
	f, ok := r.factories[kind]
	if !ok {
		return nil, &UnknownKindError{Kind: kind}
	}
	return f.Parse(env.Config)
}

// UnknownKindError is returned when a runtime kind is not registered.
type UnknownKindError struct{ Kind string }

func (e *UnknownKindError) Error() string {
	if e.Kind == "" {
		return "runtime: missing kind"
	}
	return "runtime: unknown kind: " + e.Kind
}
