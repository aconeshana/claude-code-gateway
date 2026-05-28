package claude

import (
	"encoding/json"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
)

// Factory is the runtime.Factory implementation for claude-code.
type Factory struct{}

func (Factory) Kind() string { return "claude" }

// Parse decodes the JSON payload into a claude.Config. nil/empty payload
// yields a zero-value Config.
func (Factory) Parse(payload json.RawMessage) (runtime.Config, error) {
	if len(payload) == 0 || string(payload) == "null" {
		return Config{}, nil
	}
	var cfg Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
