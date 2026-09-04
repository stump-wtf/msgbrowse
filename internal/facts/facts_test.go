package facts

import (
	"strings"
	"testing"
)

// TestSystemPromptCarriesDurableDefinition (#448): the prompt must define
// "durable" (still true in a year), name the ephemeral exclusions, and ask
// for consolidation — that wording is the first line of defense against
// "Was late" facts. Bumping FactPromptVersion is what re-scans the archive.
func TestSystemPromptCarriesDurableDefinition(t *testing.T) {
	for _, want := range []string{
		"still true in a year",
		"was late",
		"one fact per subject",
		"Consolidate",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if FactPromptVersion < 2 {
		t.Errorf("FactPromptVersion = %d; the #448 prompt rewrite must bump it", FactPromptVersion)
	}
}
