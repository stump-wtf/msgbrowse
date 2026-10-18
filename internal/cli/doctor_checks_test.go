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
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// Pipeline-level tests for the doctor checks (issue #52): the leaf
// classifiers already had coverage; these exercise every check* function
// across its pass / warn / fail paths through the existing seams — temp
// directories for the filesystem checks, a real store for the DB checks, and
// PATH control for the exec.LookPath checks. The end-to-end runDoctor and
// exit-code contract live in doctor_e2e_test.go.

// doctorReport runs one check body into a buffer and returns the output plus
// the tallying report.
func doctorReport() (*bytes.Buffer, *report) {
	buf := &bytes.Buffer{}
	return buf, &report{w: buf}
}

// addDoctorConversation inserts one conversation row ((source, name) is
// UNIQUE).
func addDoctorConversation(t *testing.T, st *store.Store, src, name string) int64 {
	t.Helper()
	res, err := st.DB().Exec(`INSERT INTO conversations(source, name) VALUES(?, ?)`, src, name)
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("conversation id: %v", err)
	}
	return id
}

// addDoctorMessage inserts one message row and returns its id and hash.
func addDoctorMessage(t *testing.T, st *store.Store, convID int64, src, hash, body string) (int64, string) {
	t.Helper()
	res, err := st.DB().Exec(`INSERT INTO messages(hash, conversation_id, source, ts, ts_unix, sender, body)
		VALUES(?, ?, ?, '2022-03-01 10:00:00', 1646128800, 'Alice', ?)`, hash, convID, src, body)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("message id: %v", err)
	}
	return id, hash
}

// addDoctorImage attaches one image row to a message.
func addDoctorImage(t *testing.T, st *store.Store, msgID int64, rel string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO attachments(message_id, kind, rel_path, original_name)
		VALUES(?, 'image', ?, '')`, msgID, rel); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
}

// seedDoctorImage writes a conversation/message/image fixture of one source
// into a real store: conv "doctor-<src>" (names are UNIQUE across sources),
// message hash "doctor-<src>".
func seedDoctorImage(t *testing.T, st *store.Store, src, rel string) {
	t.Helper()
	convID := addDoctorConversation(t, st, src, "doctor-"+src)
	msgID, _ := addDoctorMessage(t, st, convID, src, "doctor-"+src, "photo")
	addDoctorImage(t, st, msgID, rel)
}

// mustWrite makes parent dirs and writes a small file.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ensureManagedSignalRoot creates <dataDir>/archives/signal so the EFFECTIVE
// root resolves for a config with no configured archive_root (issue #160
// managed-root discovery).
func ensureManagedSignalRoot(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, "archives", "signal"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeConverterOnPATH puts a stub binary named bin on PATH alone, making
// imageconv.Detect deterministic regardless of the host's tooling.
func fakeConverterOnPATH(t *testing.T, bin string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestReportAddAndSummary(t *testing.T) {
	buf, r := doctorReport()
	r.add(statusPass, "pass line", "ignored hint")
	if got := buf.String(); got != glyphPass+" pass line\n" {
		t.Errorf("pass line = %q, want glyph + title with no hint", got)
	}
	r.add(statusWarn, "warn line", "fix it")
	if got := buf.String(); !strings.Contains(got, glyphWarn+" warn line\n    fix it\n") {
		t.Errorf("warn output = %q, want glyph, title and indented hint", got)
	}
	r.add(statusFail, "fail line", "")
	if got := buf.String(); !strings.Contains(got, glyphFail+" fail line\n") {
		t.Errorf("fail output = %q, want glyph + title", got)
	}
	buf.Reset()
	if r.summary() != true {
		t.Error("summary with one fail must report failure (drives the non-zero exit)")
	}
	if got := buf.String(); got != "doctor: 1 warning, 1 problem\n" {
		t.Errorf("summary = %q, want the pluralized one-liner", got)
	}
}

func TestFirstErr(t *testing.T) {
	errA := errors.New("count conversations failed")
	errB := errors.New("count messages failed")
	cases := []struct {
		name string
		errs []error
		want error
	}{
		{name: "all nil", errs: []error{nil, nil}, want: nil},
		{name: "first non-nil wins", errs: []error{errA, errB}, want: errA},
		{name: "skips leading nils", errs: []error{nil, errB}, want: errB},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstErr(c.errs...); !errors.Is(got, c.want) && got != c.want {
				t.Errorf("firstErr = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCheckDataDir(t *testing.T) {
	emptyDB := func(t *testing.T) *config.Config {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "msgbrowse.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		return &config.Config{DataDir: dir}
	}
	cases := []struct {
		name      string
		cfg       func(t *testing.T) *config.Config
		wantFails int
		wantWarns int
		wantOut   []string
		wantStore bool
	}{
		{
			name:      "unset data_dir fails",
			cfg:       func(*testing.T) *config.Config { return &config.Config{} },
			wantFails: 1,
			wantOut:   []string{"data_dir is not set"},
		},
		{
			name: "missing dir warns without creating it",
			cfg: func(t *testing.T) *config.Config {
				return &config.Config{DataDir: filepath.Join(t.TempDir(), "nope")}
			},
			wantWarns: 1,
			wantOut:   []string{"does not exist yet"},
		},
		{
			name: "file instead of dir fails",
			cfg: func(t *testing.T) *config.Config {
				p := filepath.Join(t.TempDir(), "regular")
				mustWrite(t, p, "x")
				return &config.Config{DataDir: p}
			},
			wantFails: 1,
			wantOut:   []string{"is not a directory"},
		},
		{
			name: "writable dir with no db warns",
			cfg:  func(t *testing.T) *config.Config { return &config.Config{DataDir: t.TempDir()} },
			// pass (dir) + warn (no database yet)
			wantWarns: 1,
			wantOut:   []string{"exists and is writable", "no database yet"},
		},
		{
			name:      "current schema with imported data passes and returns the store",
			cfg:       emptyDB,
			wantOut:   []string{"exists and is writable", "database schema is current", "conversations"},
			wantStore: true,
		},
		{
			name: "drifted schema warns but still returns the store",
			cfg: func(t *testing.T) *config.Config {
				cfg := emptyDB(t)
				st, err := store.Open(dbPath(cfg))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`PRAGMA user_version = 1`); err != nil {
					t.Fatal(err)
				}
				st.Close()
				return cfg
			},
			// drift warning + the empty-corpus warning
			wantWarns: 2,
			wantOut:   []string{"binary expects"},
			wantStore: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(t)
			if c.name == "current schema with imported data passes and returns the store" {
				st, err := store.Open(dbPath(cfg))
				if err != nil {
					t.Fatal(err)
				}
				convID := addDoctorConversation(t, st, source.Signal, "DataDir")
				addDoctorMessage(t, st, convID, source.Signal, "datadir-msg", "hello")
				st.Close()
			}
			buf, r := doctorReport()
			st := checkDataDir(context.Background(), r, cfg)
			if st != nil {
				defer st.Close()
			}
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
			if got := st != nil; got != c.wantStore {
				t.Errorf("store returned = %v, want %v", got, c.wantStore)
			}
			// The missing-dir case must not create anything (read-only
			// diagnostic: a typo'd --data-dir is reported, not made real).
			if c.name == "missing dir warns without creating it" {
				if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
					t.Errorf("checkDataDir created the data dir it was supposed to report")
				}
			}
		})
	}
}

func TestCheckSignalArchive(t *testing.T) {
	cases := []struct {
		name       string
		root       func(t *testing.T) string
		wantFails  int
		wantWarns  int
		wantOut    []string
		wantNoFail bool
	}{
		{
			name:      "unset root warns",
			root:      func(*testing.T) string { return "" },
			wantWarns: 1,
			wantOut:   []string{"archive_root (Signal) is not set"},
		},
		{
			name:      "missing root fails",
			root:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			wantFails: 1,
			wantOut:   []string{"archive_root"},
		},
		{
			name: "file instead of dir fails",
			root: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file")
				mustWrite(t, p, "x")
				return p
			},
			wantFails: 1,
			wantOut:   []string{"is not a directory"},
		},
		{
			name:      "root without export/ warns",
			root:      func(t *testing.T) string { return t.TempDir() },
			wantWarns: 1,
			wantOut:   []string{"has no export/ subdirectory"},
		},
		{
			name: "root pointing at export/ fails with the parent-folder hint",
			root: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "Signal-Archive", "export"), 0o755); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "Signal-Archive", "export")
			},
			wantFails: 1,
			wantOut:   []string{"points AT export/", "PARENT folder"},
		},
		{
			name: "healthy root passes",
			root: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "export"), 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantOut: []string{"contains export/"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, r := doctorReport()
			checkSignalArchive(r, &config.Config{ArchiveRoot: c.root(t)})
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
		})
	}
}

func TestCheckIMessageArchive(t *testing.T) {
	cases := []struct {
		name      string
		root      func(t *testing.T) string
		wantFails int
		wantWarns int
		wantOut   []string
	}{
		{
			name:      "unset root warns",
			root:      func(*testing.T) string { return "" },
			wantWarns: 1,
			wantOut:   []string{"imessage_archive_root is not set"},
		},
		{
			name:      "missing root fails",
			root:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			wantFails: 1,
			wantOut:   []string{"imessage_archive_root"},
		},
		{
			name:      "dir without txt files warns with the -f txt hint",
			root:      func(t *testing.T) string { return t.TempDir() },
			wantWarns: 1,
			wantOut:   []string{"has no *.txt files", "-f txt"},
		},
		{
			name: "flat txt output passes",
			root: func(t *testing.T) string {
				dir := t.TempDir()
				mustWrite(t, filepath.Join(dir, "Alice.txt"), "chat")
				mustWrite(t, filepath.Join(dir, "sub", "ignored.txt"), "nested dirs are not counted")
				return dir
			},
			wantOut: []string{"has 1 *.txt file(s)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, r := doctorReport()
			checkIMessageArchive(r, &config.Config{IMessageArchiveRoot: c.root(t)})
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
		})
	}
}

func TestCheckAttachments(t *testing.T) {
	cases := []struct {
		name      string
		store     bool
		seed      func(t *testing.T, st *store.Store, dir string)
		roots     func(dir string) (sig, imsg string)
		wantFails int
		wantWarns int
		wantOut   []string
	}{
		{
			name:  "no database prints nothing",
			store: false,
		},
		{
			name:    "empty store passes",
			store:   true,
			seed:    func(t *testing.T, _ *store.Store, _ string) {},
			wantOut: []string{"no image attachments to check"},
		},
		{
			name:  "copy-mode present passes",
			store: true,
			seed: func(t *testing.T, st *store.Store, dir string) {
				ensureManagedSignalRoot(t, dir)
				seedDoctorImage(t, st, source.Signal, "media/pic.jpg")
				mustWrite(t, filepath.Join(dir, "archives", "signal", "export", "doctor-signal", "media", "pic.jpg"), "jpeg")
			},
			wantOut: []string{"Signal attachments: 1 ok, 0 absolute, 0 missing (of 1 sampled)"},
		},
		{
			name:  "majority absolute paths fails with the copy-mode hint",
			store: true,
			seed: func(t *testing.T, st *store.Store, _ string) {
				seedDoctorImage(t, st, source.IMessage, "/Users/joe/Library/Messages/Attachments/ab/cd/IMG_0001.HEIC")
			},
			wantFails: 1,
			wantOut:   []string{"iMessage attachments: 0 ok, 1 absolute, 0 missing", "copy-method"},
		},
		{
			name:  "missing files warn",
			store: true,
			seed: func(t *testing.T, st *store.Store, _ string) {
				seedDoctorImage(t, st, source.Signal, "media/gone.jpg")
			},
			wantWarns: 1,
			wantOut:   []string{"Signal attachments: 0 ok, 0 absolute, 1 missing"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{DataDir: dir}
			var st *store.Store
			if c.store {
				var err error
				st, err = store.Open(filepath.Join(dir, "msgbrowse.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
			}
			if c.seed != nil {
				c.seed(t, st, dir)
			}
			buf, r := doctorReport()
			checkAttachments(context.Background(), r, cfg, st)
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
		})
	}
}

func TestCheckConverter(t *testing.T) {
	cases := []struct {
		name      string
		store     bool
		seed      func(t *testing.T, st *store.Store, dir string)
		converter func(t *testing.T)
		wantFails int
		wantWarns int
		wantOut   []string
		wantNone  []string
	}{
		{
			name:      "no database prints only the converter line",
			store:     false,
			converter: func(t *testing.T) { t.Setenv("PATH", "") },
			wantWarns: 1,
			wantOut:   []string{"no image converter found"},
		},
		{
			name:      "browser-renderable images need no derivative",
			store:     true,
			converter: func(t *testing.T) { fakeConverterOnPATH(t, "magick") },
			seed: func(t *testing.T, st *store.Store, dir string) {
				ensureManagedSignalRoot(t, dir)
				seedDoctorImage(t, st, source.Signal, "media/pic.jpg")
				mustWrite(t, filepath.Join(dir, "archives", "signal", "export", "doctor-signal", "media", "pic.jpg"), "jpeg")
			},
			wantNone: []string{"lack a cached derivative"},
		},
		{
			name:      "convertible file without a derivative warns",
			store:     true,
			converter: func(t *testing.T) { fakeConverterOnPATH(t, "magick") },
			seed: func(t *testing.T, st *store.Store, dir string) {
				ensureManagedSignalRoot(t, dir)
				seedDoctorImage(t, st, source.Signal, "media/pic.heic")
				mustWrite(t, filepath.Join(dir, "archives", "signal", "export", "doctor-signal", "media", "pic.heic"), "heic")
			},
			wantWarns: 1,
			wantOut:   []string{"1 convertible image(s) lack a cached derivative", "msgbrowse media"},
		},
		{
			name:      "convertible file with a cached derivative passes",
			store:     true,
			converter: func(t *testing.T) { fakeConverterOnPATH(t, "magick") },
			seed: func(t *testing.T, st *store.Store, dir string) {
				ensureManagedSignalRoot(t, dir)
				seedDoctorImage(t, st, source.Signal, "media/pic.heic")
				abs := filepath.Join(dir, "archives", "signal", "export", "doctor-signal", "media", "pic.heic")
				mustWrite(t, abs, "heic")
				// Mirror what `msgbrowse media` leaves behind: a cached JPEG
				// derivative keyed off the source path under <data>/derived.
				derivedDir := imageconv.DerivedDir(dir)
				if err := os.MkdirAll(filepath.Dir(imageconv.DerivedPath(derivedDir, abs)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(imageconv.DerivedPath(derivedDir, abs), []byte("jpeg"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantNone: []string{"lack a cached derivative"},
		},
		{
			name:      "no converter plus pending derivatives warns twice with the install hint",
			store:     true,
			converter: func(t *testing.T) { t.Setenv("PATH", "") },
			seed: func(t *testing.T, st *store.Store, dir string) {
				ensureManagedSignalRoot(t, dir)
				seedDoctorImage(t, st, source.Signal, "media/pic.heic")
				mustWrite(t, filepath.Join(dir, "archives", "signal", "export", "doctor-signal", "media", "pic.heic"), "heic")
			},
			wantWarns: 2,
			wantOut:   []string{"no image converter found", "install an image converter first"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.converter != nil {
				c.converter(t)
			}
			cfg := &config.Config{DataDir: dir}
			var st *store.Store
			if c.store {
				var err error
				st, err = store.Open(filepath.Join(dir, "msgbrowse.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
			}
			if c.seed != nil {
				c.seed(t, st, dir)
			}
			buf, r := doctorReport()
			checkConverter(context.Background(), r, cfg, st)
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
			for _, not := range c.wantNone {
				if strings.Contains(buf.String(), not) {
					t.Errorf("output %q must not contain %q", buf, not)
				}
			}
		})
	}
}

func TestCheckEmbeddings(t *testing.T) {
	const model = "embed-test"
	cases := []struct {
		name      string
		store     bool
		seed      func(t *testing.T, st *store.Store)
		wantFails int
		wantWarns int
		wantOut   []string
	}{
		{
			name:  "no database prints nothing",
			store: false,
		},
		{
			name:  "unembedded messages warn with the embed hint",
			store: true,
			seed: func(t *testing.T, st *store.Store) {
				convID := addDoctorConversation(t, st, source.Signal, "Embeds")
				addDoctorMessage(t, st, convID, source.Signal, "embed-1", "hello there")
			},
			wantWarns: 1,
			wantOut:   []string{"1 message(s) not embedded", "msgbrowse embed"},
		},
		{
			name:  "fully embedded corpus passes",
			store: true,
			seed: func(t *testing.T, st *store.Store) {
				convID := addDoctorConversation(t, st, source.Signal, "Embeds")
				_, hash := addDoctorMessage(t, st, convID, source.Signal, "embed-2", "hello there")
				if err := st.PutEmbedding(context.Background(), hash, model, []float32{0.5, 0.5}); err != nil {
					t.Fatal(err)
				}
			},
			wantOut: []string{"all messages are embedded"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{DataDir: t.TempDir(), LLM: config.LLMConfig{EmbedModel: model}}
			var st *store.Store
			if c.store {
				var err error
				st, err = store.Open(filepath.Join(cfg.DataDir, "msgbrowse.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
			}
			if c.seed != nil {
				c.seed(t, st)
			}
			buf, r := doctorReport()
			checkEmbeddings(context.Background(), r, cfg, st)
			if r.fails != c.wantFails {
				t.Errorf("fails = %d, want %d (out: %s)", r.fails, c.wantFails, buf)
			}
			if r.warnings != c.wantWarns {
				t.Errorf("warnings = %d, want %d (out: %s)", r.warnings, c.wantWarns, buf)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output %q missing %q", buf, want)
				}
			}
		})
	}
}

func TestCheckExporters(t *testing.T) {
	exporterBins := []string{"sigexport", "imessage-exporter", "wtsexporter"}
	t.Run("all present on PATH pass", func(t *testing.T) {
		dir := t.TempDir()
		for _, bin := range exporterBins {
			if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", dir)
		buf, r := doctorReport()
		checkExporters(r)
		if r.warnings != 0 || r.fails != 0 {
			t.Errorf("tally = %d/%d, want 0/0 (out: %s)", r.warnings, r.fails, buf)
		}
		for _, bin := range exporterBins {
			if !strings.Contains(buf.String(), "exporter \""+bin+"\" found on PATH") {
				t.Errorf("output missing pass line for %s: %s", bin, buf)
			}
		}
	})
	t.Run("all missing warn", func(t *testing.T) {
		t.Setenv("PATH", "")
		buf, r := doctorReport()
		checkExporters(r)
		if r.warnings != len(exporterBins) || r.fails != 0 {
			t.Errorf("tally = %d/%d, want %d/0 (out: %s)", r.warnings, r.fails, len(exporterBins), buf)
		}
		if !strings.Contains(buf.String(), "wtsexporter") {
			t.Errorf("output missing wtsexporter line: %s", buf)
		}
	})
}

func TestCheckLLMEndpoint(t *testing.T) {
	t.Run("flag off prints nothing", func(t *testing.T) {
		buf, r := doctorReport()
		checkLLMEndpoint(r, &config.Config{}, false)
		if buf.Len() != 0 || r.fails+r.warnings != 0 {
			t.Errorf("without --check-llm doctor must stay silent, got %q", buf)
		}
	})
	t.Run("reachable endpoint passes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		buf, r := doctorReport()
		checkLLMEndpoint(r, &config.Config{LLM: config.LLMConfig{BaseURL: srv.URL}}, true)
		if r.fails+r.warnings != 0 {
			t.Errorf("reachable endpoint must pass, got %q", buf)
		}
		if !strings.Contains(buf.String(), "reachable (TCP connect only; no data sent)") {
			t.Errorf("output %q missing the loopback pass line", buf)
		}
	})
	t.Run("refused connection warns", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		buf, r := doctorReport()
		checkLLMEndpoint(r, &config.Config{LLM: config.LLMConfig{BaseURL: url}}, true)
		if r.warnings != 1 || r.fails != 0 {
			t.Errorf("unreachable endpoint must warn without failing, got %d/%d (out: %s)", r.warnings, r.fails, buf)
		}
		if !strings.Contains(buf.String(), "not reachable") {
			t.Errorf("output %q missing the reachability warning", buf)
		}
	})
	t.Run("unparseable base_url warns", func(t *testing.T) {
		buf, r := doctorReport()
		checkLLMEndpoint(r, &config.Config{LLM: config.LLMConfig{BaseURL: "http://exa mple.com"}}, true)
		if r.warnings != 1 || r.fails != 0 {
			t.Errorf("bad base_url must warn, got %d/%d (out: %s)", r.warnings, r.fails, buf)
		}
		if !strings.Contains(buf.String(), "could not parse llm.base_url") {
			t.Errorf("output %q missing the parse warning", buf)
		}
	})
}
