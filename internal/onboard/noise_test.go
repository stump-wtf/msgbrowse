package onboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// TestStripExporterNoise covers the benign-line filter directly: the known
// always-benign warning is removed while every other line — including the
// permission-wall hint the classifier keys on — survives in its original order.
func TestStripExporterNoise(t *testing.T) {
	const movWarning = "No MOV converter found, video attachments will not be converted!"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips the lone MOV warning",
			in:   movWarning,
			want: "",
		},
		{
			name: "strips the MOV warning but keeps the real failure",
			in: movWarning + "\n" +
				"Invalid configuration: Unable to read from chat database: unable to open database file: " +
				"/Users/testuser/Library/Messages/chat.db Ensure full disk access is enabled",
			want: "Invalid configuration: Unable to read from chat database: unable to open database file: " +
				"/Users/testuser/Library/Messages/chat.db Ensure full disk access is enabled",
		},
		{
			name: "strips the MOV warning surrounded by whitespace",
			in:   "  " + movWarning + "  ",
			want: "",
		},
		{
			name: "preserves order and other lines",
			in:   "starting export\n" + movWarning + "\nexported 3 conversations",
			want: "starting export\nexported 3 conversations",
		},
		{
			name: "leaves unrelated output untouched",
			in:   "exported 3 conversations\nexported 42 messages",
			want: "exported 3 conversations\nexported 42 messages",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripExporterNoise(tc.in); got != tc.want {
				t.Errorf("stripExporterNoise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExportStripsBenignNoiseFromLog drives the runner with an exporter that
// prints the MOV warning ahead of the real Full Disk Access wall (the exact shape
// from the failing card). The terminal state must still classify as a permission
// failure — the MOV line must not blunt that — while the JobLog the UI renders no
// longer carries the misleading warning.
func TestExportStripsBenignNoiseFromLog(t *testing.T) {
	const movWarning = "No MOV converter found, video attachments will not be converted!"
	const fdaStderr = "Invalid configuration: Unable to read from chat database: unable to open database file: " +
		"/Users/testuser/Library/Messages/chat.db Ensure full disk access is enabled"
	combined := movWarning + "\n" + fdaStderr
	exitErr := errors.New("exit status 1")

	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/imessage-exporter"),
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			return combined, exitErr
		},
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			t.Error("importer must not run after a failed export")
			return ImportResult{}, nil
		}),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.IMessage); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	term := waitFor(t, r, source.IMessage, func(p Progress) bool { return p.Phase.Terminal() })

	// The permission wall still wins: the benign MOV line does not stop the
	// classifier from reaching the Full Disk Access hint beneath it.
	if !errors.Is(term.Err, ErrPermissionDenied) {
		t.Fatalf("terminal error %v does not wrap ErrPermissionDenied", term.Err)
	}
	// The surfaced diagnostic no longer carries the false alarm...
	if strings.Contains(term.Log.Output, movWarning) {
		t.Errorf("JobLog.Output still contains the benign MOV warning:\n%s", term.Log.Output)
	}
	// ...but the real failure text the user needs is intact.
	if !strings.Contains(term.Log.Output, "full disk access is enabled") {
		t.Errorf("JobLog.Output lost the real failure text:\n%s", term.Log.Output)
	}
}
