package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/spf13/cobra"
)

// Every test in this file exists because six help regressions shipped through
// four green required checks (#333): before it, the repository had zero
// assertions on --help output and Execute() sat at 0.0% function coverage.
// The expectations are pinned against the pre-fang binary at c905d21, which is
// the last rendering users saw that was not mangled.

// renderHelpFor drives the real command tree through the real Execute path and
// returns the help page it produced. Output is captured through a buffer, so
// the render width is the non-terminal default (120) and the result is stable.
func renderHelpFor(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := execute(context.Background(), root, append(args, "--help")); err != nil {
		t.Fatalf("rendering help for %v: %v", args, err)
	}
	return buf.String()
}

// TestHelpRendersFlagValueTypes pins M3: pflag's value-type placeholder is
// part of a flag's contract — `--port` with no `int`, or
// `--imessage-exporter-args` with no `-- <args>`, does not tell a reader what
// to pass. fang's flag table dropped every one of them.
func TestHelpRendersFlagValueTypes(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"serve", []string{"--port int", "--host string", "--listen-addr string"}},
		{"export", []string{
			"--imessage-exporter-args -- <args>",
			"--signal-export-args -- <args>",
			"--whatsapp-exporter-args -- <args>",
			"--signal-export-bin sigexport",
			"--whatsapp-exporter-bin wtsexporter",
		}},
		// Persistent flags inherit their types too, on every command.
		{"media", []string{"--archive-root string", "--data-dir string"}},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			got := renderHelpFor(t, tc.cmd)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("`msgbrowse %s --help` is missing %q:\n%s", tc.cmd, want, got)
				}
			}
		})
	}
}

// TestHelpRendersCobraStyleDefaults pins the other half of M3: fang reformatted
// cobra's `(default 60)` to a bare `(60)`, which reads as part of the sentence
// rather than as the flag's default.
func TestHelpRendersCobraStyleDefaults(t *testing.T) {
	for _, tc := range []struct{ cmd, want, notWant string }{
		{"serve", "(default true)", "(true)"},
		{"facts", "(default 60)", "(60)"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			got := renderHelpFor(t, tc.cmd)
			if !strings.Contains(got, tc.want) {
				t.Errorf("`msgbrowse %s --help` is missing %q:\n%s", tc.cmd, tc.want, got)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("`msgbrowse %s --help` still renders fang's bare %q form:\n%s", tc.cmd, tc.notWant, got)
			}
		})
	}
}

// TestHelpKeepsLiteralCasing pins M1: fang applies Transform(titleFirstWord) to
// styles.FlagDescription, which renders both flag descriptions and command
// Short strings. `sigexport`, `imessage-exporter` and `wtsexporter` are the
// literal names of binaries on PATH; title-casing them in the help that tells
// a user which tool a flag targets is actively misleading.
func TestHelpKeepsLiteralCasing(t *testing.T) {
	for _, tc := range []struct {
		cmd      []string
		want     []string
		notWant  []string
		surfaces string
	}{
		{
			cmd:      []string{"export"},
			want:     []string{"sigexport-only", "imessage-exporter-only", "wtsexporter-only"},
			notWant:  []string{"Sigexport-Only", "Imessage-Exporter-Only", "Wtsexporter-Only"},
			surfaces: "flag descriptions",
		},
		{
			cmd:      []string{"media"},
			want:     []string{"re-convert even if a derivative already exists"},
			notWant:  []string{"Re-Convert"},
			surfaces: "flag descriptions",
		},
		{
			// `watch`'s Short reaches the root help through cobra's Short
			// rendering, the surface #331 could not reach at all.
			cmd:      nil,
			want:     []string{"Re-ingest automatically when the archive changes"},
			notWant:  []string{"Re-Ingest"},
			surfaces: "command Short strings",
		},
	} {
		name := "root"
		if len(tc.cmd) > 0 {
			name = strings.Join(tc.cmd, "-")
		}
		t.Run(name+"/"+strings.ReplaceAll(tc.surfaces, " ", "-"), func(t *testing.T) {
			got := renderHelpFor(t, tc.cmd...)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%s: missing verbatim %q:\n%s", tc.surfaces, want, got)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("%s: title-case transform still mangles %q:\n%s", tc.surfaces, bad, got)
				}
			}
		})
	}
}

// helpTargets lists every visible command path in the tree, so the whole-tree
// assertions below cannot go stale when a subcommand is added.
func helpTargets(t *testing.T) [][]string {
	t.Helper()
	targets := [][]string{nil}
	for _, c := range NewRootCommand().Commands() {
		if c.Hidden {
			continue
		}
		targets = append(targets, []string{c.Name()})
	}
	if len(targets) < 10 {
		t.Fatalf("only found %d help targets; the command tree walk is broken", len(targets))
	}
	return targets
}

// TestHelpHasNoTrailingWhitespace pins M6. lipgloss pads every styled block out
// to its declared width and colorprofile strips the color but not the padding,
// so piped help came out right-padded on 17 lines of the root page alone. The
// pre-fang binary produced none.
func TestHelpHasNoTrailingWhitespace(t *testing.T) {
	for _, target := range helpTargets(t) {
		name := "root"
		if len(target) > 0 {
			name = target[0]
		}
		t.Run(name, func(t *testing.T) {
			var bad []string
			for i, line := range strings.Split(renderHelpFor(t, target...), "\n") {
				if line != strings.TrimRight(line, " \t") {
					bad = append(bad, fmt.Sprintf("  line %d: %q", i+1, line))
				}
			}
			if len(bad) > 0 {
				t.Errorf("%d line(s) of `msgbrowse %s --help` end in whitespace:\n%s",
					len(bad), name, strings.Join(bad, "\n"))
			}
		})
	}
}

// TestHelpRendersEveryCommand is the cheap backstop the repo never had: every
// command must produce a help page with the sections fang's layout promises.
// Without it, a renderer that silently emitted nothing would satisfy the
// whitespace and casing assertions above vacuously.
func TestHelpRendersEveryCommand(t *testing.T) {
	for _, target := range helpTargets(t) {
		name := "root"
		if len(target) > 0 {
			name = target[0]
		}
		t.Run(name, func(t *testing.T) {
			got := renderHelpFor(t, target...)
			for _, want := range []string{"USAGE", "msgbrowse", "FLAGS", "--help"} {
				if !strings.Contains(got, want) {
					t.Errorf("`msgbrowse %s --help` is missing %q:\n%s", name, want, got)
				}
			}
		})
	}
}

// TestExecuteRendersHelp covers Execute itself, which was at 0.0% function
// coverage — the function that installs the whole help/error layer was
// untested by construction, which is why all six regressions shipped green.
func TestExecuteRendersHelp(t *testing.T) {
	out := captureStdio(t, func() {
		os.Args = []string{"msgbrowse", "--help"}
		if err := Execute(); err != nil {
			t.Errorf("Execute() with --help returned %v, want nil", err)
		}
	})
	for _, want := range []string{"USAGE", "COMMANDS", "FLAGS", "--archive-root string"} {
		if !strings.Contains(out, want) {
			t.Errorf("Execute() help output is missing %q:\n%s", want, out)
		}
	}
	for i, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("Execute() help line %d ends in whitespace: %s", i+1, fmt.Sprintf("%q", line))
		}
	}
}

// TestExecuteRendersErrorBlock keeps the other half of the layer covered: with
// help no longer going through fang.Execute, the ERROR surface has to keep
// working from the same wiring — badge, message, and msgbrowse's own hint.
func TestExecuteRendersErrorBlock(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.AddCommand(&cobra.Command{
		Use:               "boom",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("import failed: %w", ingest.ErrExportDirNotFound)
		},
	})
	if err := execute(context.Background(), root, []string{"boom"}); err == nil {
		t.Fatal("execute returned nil for a failing command")
	}
	got := strings.Join(strings.Fields(ansiRe.ReplaceAllString(buf.String(), "")), " ")
	for _, want := range []string{"ERROR", "import failed", "Hint:", "CONTAINS export/"} {
		if !strings.Contains(got, want) {
			t.Errorf("error block is missing %q:\n%s", want, got)
		}
	}
}

// TestManCommandSurvives pins the one thing dropping fang.Execute could have
// silently removed: fang injected a hidden `man` subcommand, so we inject the
// same one.
func TestManCommandSurvives(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := execute(context.Background(), root, []string{"man"}); err != nil {
		t.Fatalf("`msgbrowse man` returned %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "MSGBROWSE") {
		t.Errorf("`msgbrowse man` did not produce a manpage:\n%.400s", got)
	}
}

// TestHelpRendersExamples covers the EXAMPLES block. No command in the tree
// sets Example today, so this drives a synthetic one — otherwise the section
// would be dead code nobody notices breaking.
func TestHelpRendersExamples(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{
		Use:     "demo",
		Short:   "a demo command",
		Example: "\n# import everything\nmsgbrowse import --data-dir /tmp\n",
	}
	cmd.SetOut(&buf)
	renderHelp(cmd, &buf)
	got := buf.String()
	for _, want := range []string{"EXAMPLES", "# import everything", "msgbrowse import --data-dir /tmp"} {
		if !strings.Contains(got, want) {
			t.Errorf("examples block is missing %q:\n%s", want, got)
		}
	}
}

// TestTerminalIsDark covers the mechanism M2's fix rests on: light/dark is
// resolved from the environment, so nothing is ever written to the terminal to
// ask. Dark is the fallback — it is both the common case and what lipgloss
// itself returns when its query fails.
func TestTerminalIsDark(t *testing.T) {
	for _, tc := range []struct {
		colorfgbg string
		set       bool
		want      bool
	}{
		{set: false, want: true},                           // unset → dark
		{colorfgbg: "15;0", set: true, want: true},         // black background
		{colorfgbg: "0;15", set: true, want: false},        // white background
		{colorfgbg: "0;7", set: true, want: false},         // light grey background
		{colorfgbg: "15;default;0", set: true, want: true}, // three-field form
		{colorfgbg: "nonsense", set: true, want: true},     // unparsable → dark
	} {
		name := "unset"
		if tc.set {
			name = tc.colorfgbg
		}
		t.Run(name, func(t *testing.T) {
			if tc.set {
				t.Setenv("COLORFGBG", tc.colorfgbg)
			} else {
				t.Setenv("COLORFGBG", "")
			}
			if got := terminalIsDark(); got != tc.want {
				t.Errorf("terminalIsDark() with COLORFGBG=%q = %v, want %v", tc.colorfgbg, got, tc.want)
			}
		})
	}
}

// captureStdio runs fn with os.Args, os.Stdout and os.Stderr swapped out, and
// returns what it wrote to stdout. Stderr is redirected too so the logger
// Execute installs cannot see a terminal (charmbracelet/log probes one at
// construction) and so its lines stay out of the test log.
func captureStdio(t *testing.T, fn func()) string {
	t.Helper()
	oldArgs, oldOut, oldErr := os.Args, os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Args, os.Stdout, os.Stderr = oldArgs, oldOut, oldErr
	}()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(outR)
		done <- string(b)
	}()
	go func() {
		_, _ = io.Copy(io.Discard, errR)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	return <-done
}
