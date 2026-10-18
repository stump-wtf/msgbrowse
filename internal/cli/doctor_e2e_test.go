package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/whatsapp"
)

// End-to-end coverage for the doctor pipeline and its exit-code contract
// (issue #52): runDoctor over healthy / warning / failing archives must
// return exactly "any check failed", and that boolean must survive the whole
// CLI path — RunE → errDoctorFailed → execute → os.Exit — as exit 0 for
// healthy and warning runs and exit 1 only when a check failed (✗).

// doctorFixture is a temp data dir with a real (closed) database plus the
// config pointing at it, ready for runDoctor or a CLI invocation.
type doctorFixture struct {
	cfg     *config.Config
	archive string // effective Signal archive root (managed layout under DataDir)
}

// newDoctorFixture creates <tmp>/data with a migrated database. runDoctor
// opens it read-only, so the seeding handle is closed first.
func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "msgbrowse.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	return &doctorFixture{
		cfg:     &config.Config{DataDir: data},
		archive: filepath.Join(data, "archives", "signal"),
	}
}

// embedMessages gives every message in the store an embedding under cfg's
// embed model, so checkEmbeddings reports the healthy posture.
func (f *doctorFixture) embedMessages(t *testing.T) {
	t.Helper()
	st, err := store.Open(filepath.Join(f.cfg.DataDir, "msgbrowse.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query(`SELECT hash FROM messages WHERE is_system = 0 AND TRIM(body) <> ''`)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	for _, h := range hashes {
		if err := st.PutEmbedding(context.Background(), h, f.cfg.LLM.EmbedModel, []float32{0.25, 0.25}); err != nil {
			t.Fatal(err)
		}
	}
}

// seedSignalImage writes one copy-mode image attachment into the store with
// its file present inside the managed Signal archive root, on a message in
// conversation conv with the given hash.
func (f *doctorFixture) seedSignalImage(t *testing.T, conv, hash, rel string) int64 {
	t.Helper()
	if err := os.MkdirAll(f.archive, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.archive, "export", conv, rel), "jpeg")
	st, err := store.Open(filepath.Join(f.cfg.DataDir, "msgbrowse.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	convID := addDoctorConversation(t, st, "signal", conv)
	msgID, _ := addDoctorMessage(t, st, convID, "signal", hash, "photo")
	addDoctorImage(t, st, msgID, rel)
	return convID
}

// healthyDoctor returns an archive whose every source check passes: a
// migrated DB with an imported, embedded message and a copy-mode image, a
// Signal root with export/, a flat iMessage txt root, and a WhatsApp export
// whose single media reference resolves inside the root. The optional
// converter binary is the only PATH dependency and only ever adds a pass.
func healthyDoctor(t *testing.T) *doctorFixture {
	t.Helper()
	f := newDoctorFixture(t)
	f.cfg.LLM.EmbedModel = "local-embed"

	convID := f.seedSignalImage(t, "doctor-health", "doctor-health", "media/pic.jpg")
	st, err := store.Open(filepath.Join(f.cfg.DataDir, "msgbrowse.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO messages(hash, conversation_id, source, ts, ts_unix, sender, body, is_system)
		VALUES('doctor-health-sys', ?, 'signal', '2022-03-01 10:01:00', 1646128860, 'Alice', '', 1)`, convID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	f.embedMessages(t)

	managed := filepath.Join(f.cfg.DataDir, "archives")
	mustWrite(t, filepath.Join(managed, "imessage", "Alice.txt"), "chat")
	mustWrite(t, filepath.Join(managed, "whatsapp", whatsapp.ResultFile), `{
		"15550001111@s.whatsapp.net": {"name": "A", "type": "android", "media_base": "", "messages": {
			"1": {"from_me": false, "timestamp": 1748768400, "data": "Media/img.jpg", "media": true, "mime": "image/jpeg"}
		}}
	}`)
	mustWrite(t, filepath.Join(managed, "whatsapp", "Media", "img.jpg"), "jpeg")

	// The converter and exporter lines are PATH lookups, not archive health;
	// stub them so the healthy fixture means zero warnings on any host.
	toolDir := t.TempDir()
	for _, bin := range []string{"magick", "sigexport", "imessage-exporter", "wtsexporter"} {
		if err := os.WriteFile(filepath.Join(toolDir, bin), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", toolDir)
	return f
}

func TestRunDoctorEndToEnd(t *testing.T) {
	cases := []struct {
		name       string
		fixture    func(t *testing.T) *doctorFixture
		wantFailed bool
		wantOut    []string
		wantNone   []string
	}{
		{
			name:    "healthy archive: no warnings, no problems, exit 0",
			fixture: healthyDoctor,
			wantOut: []string{
				"database schema is current",
				"1 conversations, 2 messages imported",
				"contains export/",
				"attachments: 1 ok, 0 absolute, 0 missing",
				"all messages are embedded",
				"doctor: 0 warnings, 0 problems",
			},
			wantNone: []string{glyphFail},
		},
		{
			name: "warning archive: problems stay at zero, exit 0",
			fixture: func(t *testing.T) *doctorFixture {
				return newDoctorFixture(t)
			},
			wantOut: []string{
				"⚠",
				"0 problems",
				"doctor:",
			},
			wantNone: []string{glyphFail},
		},
		{
			name: "failing archive: a failed check sets the flag, exit 1",
			fixture: func(t *testing.T) *doctorFixture {
				f := newDoctorFixture(t)
				f.cfg.ArchiveRoot = filepath.Join(t.TempDir(), "moved-away")
				return f
			},
			wantFailed: true,
			wantOut:    []string{glyphFail + " archive_root", "1 problem"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.fixture(t)
			buf := &bytes.Buffer{}
			failed := runDoctor(context.Background(), buf, f.cfg, false)
			if failed != c.wantFailed {
				t.Errorf("runDoctor failed = %v, want %v (out:\n%s)", failed, c.wantFailed, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output missing %q (out:\n%s)", want, buf)
				}
			}
			for _, not := range c.wantNone {
				if strings.Contains(buf.String(), not) {
					t.Errorf("output must not contain %q (out:\n%s)", not, buf)
				}
			}
		})
	}
}

// isolateDoctorEnv makes the CLI config path deterministic: HOME/XDG point at
// an empty temp dir (no operator config file), and no MSGBROWSE_* env value
// leaks into the resolved config.
func isolateDoctorEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	for _, key := range []string{
		"MSGBROWSE_ARCHIVE_ROOT", "MSGBROWSE_IMESSAGE_ARCHIVE_ROOT", "MSGBROWSE_WHATSAPP_ARCHIVE_ROOT",
		"MSGBROWSE_DATA_DIR", "MSGBROWSE_LLM_BASE_URL", "MSGBROWSE_LLM_EMBED_MODEL", "MSGBROWSE_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

// runDoctorCLI drives `msgbrowse doctor` exactly as main() would — cobra
// RunE included — and reports the returned error (main turns a non-nil error
// into exit code 1) together with the command's stdout.
func runDoctorCLI(t *testing.T, args ...string) (error, string) {
	t.Helper()
	isolateDoctorEnv(t)
	buf := &bytes.Buffer{}
	root := NewRootCommand()
	root.SetOut(buf)
	root.SetErr(buf)
	err := execute(context.Background(), root, append([]string{"doctor"}, args...))
	return err, buf.String()
}

func TestDoctorCommandExitCodeContract(t *testing.T) {
	t.Run("healthy archive exits 0", func(t *testing.T) {
		f := healthyDoctor(t)
		err, out := runDoctorCLI(t, "--data-dir", f.cfg.DataDir)
		if err != nil {
			t.Fatalf("healthy doctor returned an error (main would exit 1): %v (out:\n%s)", err, out)
		}
		if !strings.Contains(out, "doctor: 0 warnings, 0 problems") {
			t.Errorf("output = %q, want the all-clean summary", out)
		}
	})
	t.Run("warnings alone still exit 0", func(t *testing.T) {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "msgbrowse.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		err, out := runDoctorCLI(t, "--data-dir", dir)
		if err != nil {
			t.Fatalf("warning-only doctor returned an error (main would exit 1): %v (out:\n%s)", err, out)
		}
		if !strings.Contains(out, "0 problems") {
			t.Errorf("output = %q, want a summary with zero problems", out)
		}
	})
	t.Run("a failed check exits 1 via errDoctorFailed", func(t *testing.T) {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "msgbrowse.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		err, out := runDoctorCLI(t, "--data-dir", dir, "--archive-root", filepath.Join(t.TempDir(), "gone"))
		if !errors.Is(err, errDoctorFailed) {
			t.Fatalf("err = %v, want errDoctorFailed so main exits 1 (out:\n%s)", err, out)
		}
		if !strings.Contains(out, glyphFail) || !strings.Contains(out, "problem") {
			t.Errorf("output = %q, want the failing check line and a non-zero problem count", out)
		}
	})
	t.Run("an unusable data_dir exits non-zero before any check runs", func(t *testing.T) {
		// config.Validate rejects an empty data_dir; that guard is what makes
		// checkDataDir's own fail line unreachable from the CLI, and it must
		// stay non-zero for scripts.
		err, _ := runDoctorCLI(t, "--data-dir", "")
		if err == nil {
			t.Fatal("empty --data-dir must fail config validation, got exit 0")
		}
	})
}

// TestRunDoctorWithOptionalLLMProbe covers the one check gated behind
// --check-llm inside the full pipeline: a reachable endpoint keeps the run
// clean, an unreachable one warns without turning the exit code non-zero.
func TestRunDoctorWithOptionalLLMProbe(t *testing.T) {
	t.Run("reachable endpoint keeps the report clean", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		f := healthyDoctor(t)
		f.cfg.LLM.BaseURL = srv.URL
		buf := &bytes.Buffer{}
		if failed := runDoctor(context.Background(), buf, f.cfg, true); failed {
			t.Errorf("healthy archive with a reachable LLM must not fail (out:\n%s)", buf)
		}
		if !strings.Contains(buf.String(), "llm endpoint") || strings.Contains(buf.String(), glyphFail) {
			t.Errorf("output = %q, want the LLM pass line and no failures", buf)
		}
	})
	t.Run("unreachable endpoint warns but exits 0", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		f := healthyDoctor(t)
		f.cfg.LLM.BaseURL = url
		buf := &bytes.Buffer{}
		if failed := runDoctor(context.Background(), buf, f.cfg, true); failed {
			t.Errorf("an unreachable LLM is a warning, not a failure (out:\n%s)", buf)
		}
		if !strings.Contains(buf.String(), "not reachable") {
			t.Errorf("output = %q, want the reachability warning", buf)
		}
	})
}
