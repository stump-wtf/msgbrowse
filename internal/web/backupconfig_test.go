package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
)

// fakeBackupConfigurator is a test double for the BackupConfigurator seam
// (issue #300). It records every ApplyBackups call so the security tests can
// assert a rejected POST applied NOTHING (the checkSetupPOST contract).
type fakeBackupConfigurator struct {
	cur     string
	ret     config.RetentionConfig
	applied []struct {
		dir string
		ret config.RetentionConfig
	}
	applyErr error
}

func (f *fakeBackupConfigurator) CurrentBackups() (string, config.RetentionConfig) {
	return f.cur, f.ret
}

func (f *fakeBackupConfigurator) ApplyBackups(dir string, ret config.RetentionConfig) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, struct {
		dir string
		ret config.RetentionConfig
	}{dir, ret})
	f.cur, f.ret = dir, ret
	return nil
}

// backupsConfigPOST posts the configuration form to /backups/config.
func backupsConfigPOST(t *testing.T, srv *Server, origin, token string, fields map[string]string) httpResp {
	t.Helper()
	return wrapRec(llmPOSTTo(t, srv, "/backups/config", origin, token, fields))
}

type httpResp = struct {
	Code int
	Body string
}

func wrapRec(rec *httptest.ResponseRecorder) httpResp {
	return httpResp{Code: rec.Code, Body: rec.Body.String()}
}

// TestBackupsConfigFormRenders (issue #300): the configuration card shows the
// current effective values, the default-directory hint, and a disabled save
// when no configurator is wired; wiring one enables it.
func TestBackupsConfigFormRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/backups").Body.String()
	for _, want := range []string{
		"Backup configuration",
		`name="dir"`,
		`name="retention_daily"`,
		`name="retention_yearly"`,
		"14 daily / 12 monthly / 4 quarterly / 2 yearly",
	} {
		if !contains(body, want) {
			t.Errorf("backups page missing config-form marker %q", want)
		}
	}
	if !contains(body, `disabled`) {
		t.Error("save button enabled with no configurator wired")
	}

	fc := &fakeBackupConfigurator{cur: "/srv/snaps", ret: config.RetentionConfig{Daily: 7, Monthly: 6, Quarterly: 3, Yearly: 1}}
	srv.SetBackupConfig(fc)
	body = get(t, srv, "/backups").Body.String()
	if !contains(body, `value="/srv/snaps"`) {
		t.Error("config form missing the configured dir value")
	}
	if !contains(body, `value="7"`) || !contains(body, `value="1"`) {
		t.Error("config form missing retention values")
	}
	if strings.Contains(body, `<button type="submit" class="setup-btn" disabled`) {
		t.Error("save button disabled with configurator wired")
	}
}

// TestBackupsConfigSave: a valid POST applies the settings and re-renders
// with the saved banner; the form then shows the applied values.
func TestBackupsConfigSave(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := filepath.Join(t.TempDir(), "snaps")
	fc := &fakeBackupConfigurator{ret: config.DefaultRetention}
	srv.SetBackupConfig(fc)

	rec := backupsConfigPOST(t, srv, selfOrigin, mintToken(t, srv), map[string]string{
		"dir":                 dir,
		"retention_daily":     "7",
		"retention_monthly":   "6",
		"retention_quarterly": "3",
		"retention_yearly":    "1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("config save = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d times, want 1", len(fc.applied))
	}
	if fc.applied[0].dir != dir || fc.applied[0].ret.Daily != 7 || fc.applied[0].ret.Yearly != 1 {
		t.Errorf("applied %+v, want dir %s daily 7 yearly 1", fc.applied[0], dir)
	}
	if !contains(rec.Body, "Backup settings saved") {
		t.Error("config save missing the saved banner")
	}
}

// TestBackupsConfigSaveValidation: a relative dir, a dir inside archive_root,
// and a non-numeric retention tier all fail validation with the fixed-enum
// field errors and apply NOTHING.
func TestBackupsConfigSaveValidation(t *testing.T) {
	srv, _, archive := newTestServer(t)
	fc := &fakeBackupConfigurator{ret: config.DefaultRetention}
	srv.SetBackupConfig(fc)
	valid := map[string]string{"retention_daily": "7", "retention_monthly": "6", "retention_quarterly": "4", "retention_yearly": "2"}

	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{"relative dir", map[string]string{"dir": "relative/path"}, "Enter an absolute path."},
		{"dir in archive", map[string]string{"dir": mustAbsPath(t, filepath.Join(archive, "backups"))}, "read-only archive root"},
		{"bad tier", map[string]string{"retention_daily": "abc"}, "whole numbers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]string{}
			for k, v := range valid {
				fields[k] = v
			}
			for k, v := range tc.extra {
				fields[k] = v
			}
			rec := backupsConfigPOST(t, srv, selfOrigin, mintToken(t, srv), fields)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d", tc.name, rec.Code)
			}
			if !contains(rec.Body, tc.want) {
				t.Errorf("%s: missing error %q", tc.name, tc.want)
			}
			if len(fc.applied) != 0 {
				t.Errorf("%s: applied despite validation failure", tc.name)
			}
		})
	}
}

// TestBackupsConfigSaveSecurity (the checkSetupPOST contract): a cross-origin
// POST or a missing/never-minted token is rejected 403 and applies NOTHING.
func TestBackupsConfigSaveSecurity(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeBackupConfigurator{ret: config.DefaultRetention}
	srv.SetBackupConfig(fc)
	fields := map[string]string{"dir": t.TempDir(), "retention_daily": "7", "retention_monthly": "6", "retention_quarterly": "4", "retention_yearly": "2"}

	rec := backupsConfigPOST(t, srv, "https://evil.example", mintToken(t, srv), fields)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", rec.Code)
	}
	rec = backupsConfigPOST(t, srv, selfOrigin, "not-a-token", fields)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bad-token POST = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Error("a rejected POST applied settings")
	}
}

// TestBackupsConfigSaveUnavailable: with no configurator wired, a valid POST
// reports the fixed-enum unavailable banner instead of pretending.
func TestBackupsConfigSaveUnavailable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := backupsConfigPOST(t, srv, selfOrigin, mintToken(t, srv), map[string]string{
		"dir":                 t.TempDir(),
		"retention_daily":     "7",
		"retention_monthly":   "6",
		"retention_quarterly": "4",
		"retention_yearly":    "2",
	})
	if !contains(rec.Body, "Save unavailable") {
		t.Error("missing the unavailable banner with no configurator wired")
	}
}

func mustAbsPath(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %q: %v", p, err)
	}
	return a
}
