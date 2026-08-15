package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"charm.land/fang/v2"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/spf13/cobra"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// renderCommandError drives a failing command through the same fang wiring
// Execute uses, and returns what landed on stderr with ANSI and fang's
// wrapping/padding normalized away.
func renderCommandError(t *testing.T, err error) string {
	t.Helper()
	var buf bytes.Buffer
	root := &cobra.Command{
		Use:           "msgbrowse",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return err },
	}
	root.SetOut(io.Discard)
	root.SetErr(&buf)
	root.SetArgs([]string{})
	_ = fang.Execute(
		context.Background(),
		root,
		fang.WithColorSchemeFunc(slateColorScheme),
		fang.WithErrorHandler(slateErrorHandler),
	)
	return strings.Join(strings.Fields(ansiRe.ReplaceAllString(buf.String(), "")), " ")
}

// TestSlateErrorHandlerSkipsDoctorSentinel pins the one error `doctor` returns
// purely to force a non-zero exit: it has already printed its own report, so
// the handler must stay silent (the exit status still comes from Execute).
func TestSlateErrorHandlerSkipsDoctorSentinel(t *testing.T) {
	if got := renderCommandError(t, fmt.Errorf("wrapped: %w", errDoctorFailed)); got != "" {
		t.Errorf("doctor sentinel rendered %q, want no output", got)
	}
}

// TestSlateErrorHandlerAppendsHint covers the actionable-hint path: a known
// failure renders fang's ERROR block *and* the Hint line beneath it.
func TestSlateErrorHandlerAppendsHint(t *testing.T) {
	got := renderCommandError(t, fmt.Errorf("import failed: %w", ingest.ErrExportDirNotFound))
	for _, want := range []string{"ERROR", "import failed", "Hint:", "CONTAINS export/"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestSlateErrorHandlerNoHint keeps the unknown-failure path clean: no Hint
// line is invented for errors errorHint does not recognize.
func TestSlateErrorHandlerNoHint(t *testing.T) {
	got := renderCommandError(t, errors.New("something went sideways"))
	if !strings.Contains(got, "something went sideways") {
		t.Errorf("output missing the error message:\n%s", got)
	}
	if strings.Contains(got, "Hint:") {
		t.Errorf("output has a Hint line for an unmapped error:\n%s", got)
	}
}

// TestSlateErrorHandlerKeepsConfigKeyCase guards the reason slateErrorHandler
// unsets fang's title-case transform: errors that lead with a literal config
// key must print that key verbatim, or the message names a setting the user
// cannot find in config.yaml or the environment.
func TestSlateErrorHandlerKeepsConfigKeyCase(t *testing.T) {
	got := renderCommandError(t, errors.New("data_dir must not be empty"))
	if !strings.Contains(got, "data_dir must not be empty") {
		t.Errorf("config key was rewritten by fang's transform:\n%s", got)
	}
}

// runVersionSurface executes the root command and returns what it printed.
//
// `version` runs through PersistentPreRunE -> config.Load(""), which searches
// ".", "$HOME/.config/msgbrowse", os.UserConfigDir()/msgbrowse and
// "/etc/msgbrowse". A real config.yaml on the developer's or CI runner's box
// therefore decides the result: a malformed one fails this test with "reading
// config: ... yaml: ..." — a failure with nothing to do with versioning. Point
// HOME and XDG_CONFIG_HOME at an empty temp dir so no host file is found: the
// same hermetic-HOME pattern internal/config's loadHermetic and internal/setup
// (PR #214) already use.
func runVersionSurface(t *testing.T, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	root.Version = "ignored-by-the-template"
	if err := root.Execute(); err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return strings.TrimSpace(buf.String())
}

// TestVersionSurfacesAgree pins the `version` subcommand and the --version
// flag fang installs on the root to the same line.
func TestVersionSurfacesAgree(t *testing.T) {
	sub := runVersionSurface(t, "version")
	flag := runVersionSurface(t, "--version")
	if sub != flag {
		t.Errorf("`version` printed %q but `--version` printed %q", sub, flag)
	}
}

// TestVersionSurfacesAgreeOnTemplateMetacharacters guards the version line
// against being re-parsed as a template. Version is set from `git describe
// --tags --always --dirty` and "{"/"}" are legal in git refnames, so "{{" can
// reach the binary. Cobra renders the version template with text/template and
// template.Must, so baking the rendered line into the template *text* made
// --version either print a different line than `version` (an action like
// "{{.Name}}" being evaluated) or panic the process outright (an unparsable
// "{{oops"). Both were reproduced against real ldflags builds. Passing the line
// as template data keeps it inert.
func TestVersionSurfacesAgreeOnTemplateMetacharacters(t *testing.T) {
	restore := Version
	t.Cleanup(func() { Version = restore })
	Version = "v0.0.0-{{.Name}}-{{oops"

	sub := runVersionSurface(t, "version")
	flag := runVersionSurface(t, "--version")
	if sub != flag {
		t.Errorf("`version` printed %q but `--version` printed %q", sub, flag)
	}
	if !strings.Contains(sub, Version) {
		t.Errorf("version line %q does not contain the raw version %q verbatim", sub, Version)
	}
}
