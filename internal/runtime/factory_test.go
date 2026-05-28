package runtime_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/runtime/claude"
)

func TestRegistry_ParseClaudeEnvelope(t *testing.T) {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})

	envelope := json.RawMessage(`{"kind":"claude","config":{"Model":"sonnet","Effort":"high"}}`)
	cfg, err := r.Parse(envelope, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cc, ok := cfg.(claude.Config)
	if !ok {
		t.Fatalf("got %T, want claude.Config", cfg)
	}
	if cc.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", cc.Model)
	}
	if cc.Effort != "high" {
		t.Errorf("Effort = %q, want high", cc.Effort)
	}
}

func TestRegistry_ParseFallbackKind(t *testing.T) {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})

	// Empty envelope → use fallback
	cfg, err := r.Parse(nil, "claude")
	if err != nil {
		t.Fatalf("Parse(empty): %v", err)
	}
	if _, ok := cfg.(claude.Config); !ok {
		t.Errorf("fallback didn't yield claude.Config")
	}
}

func TestRegistry_ParseUnknownKindError(t *testing.T) {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})

	envelope := json.RawMessage(`{"kind":"codex","config":{}}`)
	_, err := r.Parse(envelope, "")
	if err == nil {
		t.Fatal("Parse with unknown kind returned nil err")
	}
	var unknown *runtime.UnknownKindError
	if !errors.As(err, &unknown) {
		t.Errorf("err = %v, want UnknownKindError", err)
	}
	if unknown.Kind != "codex" {
		t.Errorf("Kind = %q, want codex", unknown.Kind)
	}
}

func TestRegistry_ParseEmptyKindAndNoFallback(t *testing.T) {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})

	_, err := r.Parse(json.RawMessage(`{"kind":""}`), "")
	if err == nil {
		t.Fatal("Parse with empty kind+no fallback returned nil")
	}
}

func TestRegistry_ParseMalformedEnvelope(t *testing.T) {
	r := runtime.NewRegistry()
	r.Register(claude.Factory{})

	_, err := r.Parse(json.RawMessage(`{not json`), "claude")
	if err == nil {
		t.Fatal("Parse with bad JSON returned nil")
	}
}

func TestClaudeFactory_ParseEmptyPayload(t *testing.T) {
	cfg, err := claude.Factory{}.Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if _, ok := cfg.(claude.Config); !ok {
		t.Errorf("got %T, want claude.Config", cfg)
	}

	cfg2, err := claude.Factory{}.Parse(json.RawMessage("null"))
	if err != nil {
		t.Fatalf("Parse(null): %v", err)
	}
	if _, ok := cfg2.(claude.Config); !ok {
		t.Errorf("got %T, want claude.Config", cfg2)
	}
}

func TestClaudeFactory_ParseBadJSON(t *testing.T) {
	_, err := claude.Factory{}.Parse(json.RawMessage(`{"MaxTurns":"not-a-number"}`))
	if err == nil {
		t.Error("Parse with type mismatch returned nil")
	}
}
