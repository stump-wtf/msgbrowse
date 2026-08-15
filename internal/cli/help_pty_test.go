//go:build linux

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// M2 — the 4-second `--help` — is invisible to an ordinary Go test, because
// `go test` hands the process a pipe and fang gated its terminal-background
// query on term.IsTerminal(os.Stdout.Fd()). Piped help measured 0.006s on the
// broken build; only a caller holding a pty that never answers OSC 11 paid the
// 4.03s. So this file allocates a real pty, execs the command tree on it, and
// asserts on what actually reached the terminal.
//
// Linux only: it drives /dev/ptmx directly rather than taking on a pty
// dependency. The tests skip when no pty can be opened.

// ptyHelperEnv marks the re-exec'd child process below.
const (
	ptyHelperEnv  = "MSGBROWSE_HELP_PTY_HELPER"
	ptyHelperArgs = "MSGBROWSE_HELP_PTY_ARGS"
)

// TestHelpPTYHelper is not a test: it is the child process the pty tests exec.
// It renders help onto whatever stdout it was given and exits, deliberately
// skipping Execute's configureLogger — charmbracelet/log runs its own terminal
// probe at construction, which is a separate, pre-existing cost (it is on
// `msgbrowse version` too) and would mask the help path being measured here.
func TestHelpPTYHelper(t *testing.T) {
	if os.Getenv(ptyHelperEnv) != "1" {
		t.Skip("helper process for the pty tests in this file")
	}
	args := append(strings.Fields(os.Getenv(ptyHelperArgs)), "--help")
	_ = execute(context.Background(), NewRootCommand(), args)
	os.Exit(0)
}

// openPTY allocates a pty pair. It skips the calling test rather than failing
// it when the platform will not give us one (containers without /dev/ptmx).
func openPTY(t *testing.T) (ptmx, pts *os.File) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available (%v); the OSC 11 regression is only observable on a terminal", err)
	}
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(),
		syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = ptmx.Close()
		t.Skipf("cannot unlock pty: %v", errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(),
		syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		_ = ptmx.Close()
		t.Skipf("cannot resolve pty number: %v", errno)
	}
	pts, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = ptmx.Close()
		t.Skipf("cannot open pty slave: %v", err)
	}
	return ptmx, pts
}

// runHelpOnPTY execs the helper above with its stdio bound to a fresh pty that
// never answers any query, and returns everything the child wrote plus how
// long it took.
func runHelpOnPTY(t *testing.T, term string, args ...string) (string, time.Duration) {
	t.Helper()
	ptmx, pts := openPTY(t)
	defer func() { _ = ptmx.Close() }()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelpPTYHelper$") //nolint:gosec
	cmd.Env = append(cleanColorEnv(),
		ptyHelperEnv+"=1",
		ptyHelperArgs+"="+strings.Join(args, " "),
		"TERM="+term,
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	captured := make(chan string, 1)
	go func() {
		// The read ends with EIO once the last slave fd closes; whatever was
		// read up to that point is what reached the terminal.
		b, _ := io.ReadAll(ptmx)
		captured <- string(b)
	}()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		t.Fatalf("starting the pty helper: %v", err)
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(start)
	_ = pts.Close()
	if waitErr != nil {
		t.Fatalf("pty helper exited with %v", waitErr)
	}

	select {
	case out := <-captured:
		return out, elapsed
	case <-time.After(10 * time.Second):
		t.Fatal("timed out draining the pty")
		return "", elapsed
	}
}

// cleanColorEnv strips the color-control variables from the inherited
// environment so the styled/plain expectations below depend only on TERM.
func cleanColorEnv() []string {
	drop := map[string]bool{
		"NO_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true,
		"COLORFGBG": true, "TERM": true, "FORCE_COLOR": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// osc11Query is the prefix of the terminal-background query lipgloss writes
// (and blocks for ~4s waiting on a reply to). fang ran it on every help
// invocation, from mustColorscheme, with no option to skip it.
const osc11Query = "\x1b]11;"

// TestHelpDoesNotQueryTerminalBackground is the M2 regression test. It runs the
// help path on a pty that never answers and asserts both halves of the fix:
// the query is never written, and help returns immediately instead of blocking
// for the query's ~4s timeout.
//
// The two TERM values are load-bearing. `dumb` reproduces the issue's own
// repro; `xterm-256color` is the control — it proves the child really took the
// styled terminal path, so a renderer that quietly emitted nothing (or fell
// back to the pipe path, where the bug is invisible) cannot pass this
// vacuously.
func TestHelpDoesNotQueryTerminalBackground(t *testing.T) {
	for _, tc := range []struct {
		term       string
		wantStyled bool
	}{
		{"dumb", false},
		{"xterm-256color", true},
	} {
		t.Run("TERM="+tc.term, func(t *testing.T) {
			out, elapsed := runHelpOnPTY(t, tc.term)

			if strings.Contains(out, osc11Query) {
				t.Errorf("help wrote an OSC 11 background query to the terminal (%q); "+
					"it must never block a tty-attached caller on a reply", osc11Query)
			}
			// The query's timeout is ~4s. Anything close to a second means it
			// is back.
			if elapsed > time.Second {
				t.Errorf("help on a non-answering pty took %s, want well under 1s", elapsed)
			}

			// Non-vacuity: the child must actually have rendered a help page.
			for _, want := range []string{"USAGE", "FLAGS", "--archive-root"} {
				if !strings.Contains(out, want) {
					t.Fatalf("no help page reached the terminal (missing %q):\n%q", want, out)
				}
			}
			if styled := strings.Contains(out, "\x1b["); styled != tc.wantStyled {
				t.Errorf("TERM=%s rendered styled=%v, want %v — the pty path under test "+
					"is not the one users get", tc.term, styled, tc.wantStyled)
			}
		})
	}
}
