package bridge

import (
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	claudeRT "github.com/anthropics/claude-code-gateway/internal/runtime/claude"
)

func TestIsValidEffortName(t *testing.T) {
	for _, name := range []string{"low", "medium", "high", "max", "auto"} {
		if !isValidEffortName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	for _, bad := range []string{"", "LOW", "Low", "ultra", "off", "auto ", " "} {
		if isValidEffortName(bad) {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestEffortNameToWire(t *testing.T) {
	cases := map[string]string{
		"auto":   "", // sentinel: "" tells buildArgs to skip --effort
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		"max":    "max",
	}
	for in, want := range cases {
		if got := effortNameToWire(in); got != want {
			t.Errorf("effortNameToWire(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClaudeConfig_WithEffort_ImmutableCopy verifies the override returns a
// new config rather than mutating in place — required by SwitchEffort's
// "no shared state across spawn requests" invariant.
func TestClaudeConfig_WithEffort_ImmutableCopy(t *testing.T) {
	original := claudeRT.Config{Effort: "high"}
	updated := original.WithEffort("low")
	if got := updated.(claudeRT.Config).Effort; got != "low" {
		t.Errorf("expected Effort=low on returned config, got %q", got)
	}
	if original.Effort != "high" {
		t.Errorf("original Effort was mutated: got %q, want high", original.Effort)
	}
}

// TestClaudeConfig_WithEffort_EmptyClears verifies "" is treated as "let
// settings.json / model default decide" — buildArgs already skips an empty
// --effort, this just nails the contract at the config layer.
func TestClaudeConfig_WithEffort_EmptyClears(t *testing.T) {
	original := claudeRT.Config{Effort: "max"}
	cleared := original.WithEffort("")
	if got := cleared.(claudeRT.Config).Effort; got != "" {
		t.Errorf("expected Effort cleared, got %q", got)
	}
}

// TestClaudeConfig_WithEffort_SatisfiesRuntimeConfig keeps WithEffort wired
// to the runtime.Config interface so the session-layer effortOverrider
// type-assertion can find it at runtime — regression guard for an
// accidental signature change that would silently no-op SwitchEffort.
func TestClaudeConfig_WithEffort_SatisfiesRuntimeConfig(t *testing.T) {
	var _ runtime.Config = claudeRT.Config{}.WithEffort("low")
}

// TestAvailableEfforts_OrderAndCompleteness keeps the menu in a stable
// pedagogical order (low → max → auto) — buttons are rendered in this
// order, and reordering them would silently change the UX without any
// test signal. Updating the menu requires updating this test.
func TestAvailableEfforts_OrderAndCompleteness(t *testing.T) {
	want := []string{"low", "medium", "high", "max", "auto"}
	if len(availableEfforts) != len(want) {
		t.Fatalf("availableEfforts length = %d, want %d", len(availableEfforts), len(want))
	}
	for i, w := range want {
		if availableEfforts[i].Name != w {
			t.Errorf("availableEfforts[%d].Name = %q, want %q", i, availableEfforts[i].Name, w)
		}
		if availableEfforts[i].Desc == "" {
			t.Errorf("availableEfforts[%d] (%s) has empty Desc — would render as a blank line in the menu",
				i, w)
		}
	}
}
