// The Backups tab's live configuration seam (issue #300, the newLLMApplier
// pattern): the web layer's BackupConfigurator backed by the loaded config
// file. ApplyBackups persists backups.dir + retention (a surgical YAML merge
// via config.SaveBackups), updates the in-memory config so ResolveDir sees
// the new values, and swaps the live snapshot manager — a changed directory
// or policy applies with no restart.
package cli

import (
	"github.com/joestump/msgbrowse/internal/backup"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/web"
)

// backupApplier implements web.BackupConfigurator over the process's loaded
// config. dbPath is the live SQLite path the rebuilt manager snapshots.
type backupApplier struct {
	cfg    *config.Config
	dbPath string
	setMgr func(*backup.Manager)
}

// newBackupApplier builds the applier and binds it to srv's manager seam.
func newBackupApplier(cfg *config.Config, srv *web.Server, db string) web.BackupConfigurator {
	return &backupApplier{cfg: cfg, dbPath: db, setMgr: srv.SetBackupManager}
}

// CurrentBackups returns the effective settings: the configured dir (""
// means the <data_dir>/backups default) and the defaulted retention tiers.
func (a *backupApplier) CurrentBackups() (string, config.RetentionConfig) {
	return a.cfg.Backups.Dir, a.cfg.Backups.EffectiveRetention()
}

// ApplyBackups persists the settings to the resolved config file, updates
// the in-memory config, and swaps the live manager. Nothing is swapped when
// persistence fails.
func (a *backupApplier) ApplyBackups(dir string, retention config.RetentionConfig) error {
	path, err := llmConfigSavePath(a.cfg)
	if err != nil {
		return err
	}
	if err := config.SaveBackups(path, dir, retention); err != nil {
		return err
	}
	a.cfg.Backups.Dir = dir
	a.cfg.Backups.Retention = retention
	a.setMgr(backup.NewManager(
		backup.ResolveDir(*a.cfg),
		a.dbPath,
		a.cfg.SourceFile,
		a.cfg.Backups.EffectiveRetention(),
	))
	return nil
}

// compile-time interface check.
var _ web.BackupConfigurator = (*backupApplier)(nil)
