package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func sentimentCommand(t *testing.T) *cobra.Command {
	t.Helper()
	for _, c := range NewRootCommand().Commands() {
		if c.Name() == "sentiment" {
			return c
		}
	}
	t.Fatal("the sentiment command is not registered on the root")
	return nil
}

// TestSentimentCommandSurface pins the flag surface SPEC-0027 REQ "CLI command"
// specifies, and that it mirrors `facts`. The defaults matter: --concurrency 4
// is named in the REQ, and a silently different default would change first-run
// behaviour on a large archive.
func TestSentimentCommandSurface(t *testing.T) {
	cmd := sentimentCommand(t)
	for _, tc := range []struct{ flag, want string }{
		{"concurrency", "4"},
		{"batch-size", "40"},
		{"reset", "false"},
		{"conversation", "0"},
	} {
		f := cmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("sentiment has no --%s flag", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
}

// TestSentimentCommandDocumentsItsPosture guards the user-facing claims that are
// also privacy commitments: it scores text rather than people, it honours the
// exclude list and opt-outs, and it is resumable.
func TestSentimentCommandDocumentsItsPosture(t *testing.T) {
	long := sentimentCommand(t).Long
	for _, want := range []string{
		"scores TEXT, not people",
		"exclude_conversations",
		"opted",
		"llm.base_url",
		"resumes where it stopped",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("sentiment --help does not mention %q", want)
		}
	}
}
