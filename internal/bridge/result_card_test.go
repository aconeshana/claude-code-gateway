package bridge

import (
	"strings"
	"testing"

	"github.com/anthropics/claude-code-gateway/internal/channel"
	"github.com/anthropics/claude-code-gateway/internal/protocol"
)

// TestResultCard_InterruptedRendersAsStopped covers the /stop UX:
// when the user explicitly interrupts a turn, the CLI exits with
// IsError=true (it sees a non-zero exit) but the user shouldn't see a
// red "Error" card — that's misleading after they themselves asked to
// stop. Renderer must surface "Stopped" with a neutral tone instead.
func TestResultCard_InterruptedRendersAsStopped(t *testing.T) {
	b, _, _ := newTestBridge(t)

	result := &protocol.ResultMessage{
		IsError:      true, // CLI marks every interrupt as error
		DurationMS:   1234,
		NumTurns:     1,
		TotalCostUSD: 0.01,
	}

	card := b.resultCardWithIDAndInterrupt("proj", "abc12345", "", "sonnet", "", 0, "partial output", nil, 0, "", result, true)

	if !strings.HasPrefix(card.Title, "Stopped") {
		t.Errorf("title = %q, want it to start with 'Stopped' (interrupted=true)", card.Title)
	}
	if card.Tone == channel.ToneError {
		t.Errorf("tone = %v, must not be Error after a user-initiated stop", card.Tone)
	}
	if card.Tone != channel.ToneNeutral {
		t.Errorf("tone = %v, want Neutral (interrupted should be calm, not red)", card.Tone)
	}
}

// TestResultCard_RealErrorStillRendersAsError guards against accidentally
// suppressing genuine CLI failures. When interrupted=false but IsError=true
// (CLI crashed, ran out of tokens, etc.), the user must see the red Error
// card and tone — that's how they know something actually went wrong.
func TestResultCard_RealErrorStillRendersAsError(t *testing.T) {
	b, _, _ := newTestBridge(t)
	result := &protocol.ResultMessage{IsError: true}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "", nil, 0, "", result, false)

	if !strings.HasPrefix(card.Title, "Error") {
		t.Errorf("title = %q, want 'Error' for genuine failure", card.Title)
	}
	if card.Tone != channel.ToneError {
		t.Errorf("tone = %v, want Error for genuine failure", card.Tone)
	}
}

// TestResultCard_SuccessUnaffected: the new interrupted parameter must
// not regress the happy path. IsError=false and interrupted=false should
// still render the green Done card.
func TestResultCard_SuccessUnaffected(t *testing.T) {
	b, _, _ := newTestBridge(t)
	result := &protocol.ResultMessage{IsError: false}
	card := b.resultCardWithIDAndInterrupt("proj", "abc", "", "sonnet", "", 0, "all good", nil, 0, "", result, false)

	if !strings.HasPrefix(card.Title, "Done") {
		t.Errorf("title = %q, want 'Done' for success", card.Title)
	}
	if card.Tone != channel.ToneSuccess {
		t.Errorf("tone = %v, want Success", card.Tone)
	}
}
