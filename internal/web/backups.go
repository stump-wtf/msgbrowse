// The Backups tab (ADR-0026 / SPEC-0026): msgbrowse owns its own snapshots.
// The tab renders two listings: msgbrowse-owned snapshots (actionable: Create,
// Prune, Restore) and the external .snapshots inventory (read-only, never
// opened or decrypted — ADR-0010 §5). The three mutating POSTs follow the
// Status page's POST /status/index precedent: checkSetupPOST gate (same-origin
// + per-session token + MaxBytesReader, 403 before any filesystem work),
// fixed-enum result banner, and hx-boost swap of #main-content.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/joestump/msgbrowse/internal/backup"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
)

// BackupManager is the seam the web layer depends on for snapshot operations.
// nil (browser mode, no data_dir) renders the tab's unavailable state and
// makes the mutating POSTs report themselves so. The concrete implementation
// is *backup.Manager; the interface exists so the web layer can be tested
// without a real filesystem.
type BackupManager interface {
	List() ([]backup.Snapshot, error)
	Create(ctx context.Context) (backup.Snapshot, error)
	PrunePreview() ([]backup.Snapshot, error)
	Prune(ctx context.Context) ([]backup.Snapshot, error)
	RestoreTarget(id string) (backup.Snapshot, error)
	Restore(ctx context.Context, id string) (backup.Snapshot, error)
	Dir() string
}

// SetBackupManager wires the snapshot manager. Called after NewServer and
// before serving begins, mirroring SetIndexer / SetJournalBuilder. nil or
// unset means the Backups tab renders its unavailable state and the mutating
// POSTs report "unavailable".
func (s *Server) SetBackupManager(m *backup.Manager) {
	s.backupMgr = m
}

// BackupConfigurator is the live-settings seam behind the Backups tab's
// configuration form (issue #300, the SetLLMConfig pattern): serve and the
// desktop shell wire an applier that persists backups.dir + retention into
// the loaded config file and rebuilds the snapshot manager; tests wire
// fakes. With no configurator wired the form still renders (showing the
// boot config values) but a save reports itself unavailable rather than
// pretending.
type BackupConfigurator interface {
	// CurrentBackups returns the effective snapshot settings: the configured
	// dir ("" means the <data_dir>/backups default) and the effective
	// retention tier counts (zero-config tiers already defaulted).
	CurrentBackups() (dir string, retention config.RetentionConfig)
	// ApplyBackups persists the settings to the config file, updates the
	// in-memory config, and swaps the live snapshot manager. Nothing is
	// swapped when persistence fails.
	ApplyBackups(dir string, retention config.RetentionConfig) error
}

// SetBackupConfig wires the live backup settings source. Call it after
// NewServer and before serving begins, mirroring SetLLMConfig.
func (s *Server) SetBackupConfig(c BackupConfigurator) { s.backupConfig = c }

// backupResult is the fixed-enum banner from a Create / Prune / Restore POST.
// It is never derived from request input — the mapping from outcome to this
// enum is a static table, not user input.
const (
	backupResultOK             = "created"
	backupResultPruned         = "pruned"
	backupResultRestored       = "restored"
	backupResultNeedsRestart   = "restored-needs-restart"
	backupResultInProgress     = "inprogress"
	backupResultUnwritable     = "unwritable"
	backupResultInvalid        = "invalid"
	backupResultUnavailable    = "unavailable"
	backupResultNotFound       = "notfound"
	backupResultRolledBack     = "rolled-back"
	backupResultNoSnapshots    = "nosnapshots"
	backupResultNothingToPrune = "nothing-to-prune"
)

// backupsData drives the Backups tab. It carries both the owned snapshot
// listing (from the filesystem via backup.Manager) and the external .snapshots
// inventory (from the DB via store.ListSnapshots), plus the fixed-enum banner
// from a just-completed POST.
type backupsData struct {
	baseData
	// Owned snapshots — actionable (Create / Prune / Restore).
	OwnedSnapshots     []backup.Snapshot
	OwnedFootprint     int64
	BackupMgrAvailable bool
	BackupDir          string

	// External snapshots — read-only (ADR-0010 §5: never opened or decrypted).
	ExternalSnapshots   []store.Snapshot
	ExternalFootprint   int64
	HasSnapshotPipeline bool

	// Banner from a just-completed POST (fixed-enum).
	BackupResult   string
	ConfirmRestore string // snapshot ID in confirm state, "" when not confirming
	PrunePreview   []backup.Snapshot

	// Configuration form (issue #300): the configured snapshot dir ("" =
	// default <data_dir>/backups), the retention tier counts, and the
	// fixed-enum result/field errors from a just-completed config save.
	ConfigAvailable bool
	ConfigDir       string
	DefaultDir      string // the <data_dir>/backups default, shown as placeholder
	Retention       config.RetentionConfig
	CfgSaveResult   string // "", "ok", "unavailable", "error", "unwritable", "inarchive"
	ErrDir          string // "", "toolong", "invalid", "inarchive"
	ErrRetention    string // "", "invalid"
	// SetupToken is the per-session token the configuration form submits
	// through the same checkSetupPOST gate the other privileged POSTs use.
	// Minted on every render so the Create/Prune/Restore forms carry a real
	// token too (previously their branch only rendered with a manager wired).
	SetupToken string
}

// handleBackups renders the Backups tab. A safe GET with no privileged work;
// the mutating POSTs are separate handlers.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	s.renderBackups(w, r, "", "")
}

// handleBackupsCreate is POST /backups/create — the privileged "Create backup"
// control. Gate FIRST (checkSetupPOST), then write one snapshot via the
// manager, then re-render with a fixed-enum banner.
func (s *Server) handleBackupsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	if s.backupMgr == nil {
		s.renderBackups(w, r, backupResultUnavailable, "")
		return
	}
	s.backupMu.Lock()
	if s.backupInFlight {
		s.backupMu.Unlock()
		s.renderBackups(w, r, backupResultInProgress, "")
		return
	}
	s.backupInFlight = true
	s.backupMu.Unlock()
	defer func() {
		s.backupMu.Lock()
		s.backupInFlight = false
		s.backupMu.Unlock()
	}()

	snap, err := s.backupMgr.Create(r.Context())
	if err != nil {
		if errors.Is(err, backup.ErrInProgress) {
			s.renderBackups(w, r, backupResultInProgress, "")
			return
		}
		s.log.Error("backup create failed", "error", err)
		s.renderBackups(w, r, backupResultUnwritable, "")
		return
	}
	_ = snap
	s.renderBackups(w, r, backupResultOK, "")
}

// handleBackupsPrune is POST /backups/prune — apply the configured GFS
// retention policy. Gate FIRST, then preview, then delete.
func (s *Server) handleBackupsPrune(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	if s.backupMgr == nil {
		s.renderBackups(w, r, backupResultUnavailable, "")
		return
	}
	s.backupMu.Lock()
	if s.backupInFlight {
		s.backupMu.Unlock()
		s.renderBackups(w, r, backupResultInProgress, "")
		return
	}
	s.backupInFlight = true
	s.backupMu.Unlock()
	defer func() {
		s.backupMu.Lock()
		s.backupInFlight = false
		s.backupMu.Unlock()
	}()

	// Preview first (the SPEC requires naming what would be deleted BEFORE
	// deleting). The POST without --apply is the preview; with --apply is
	// the delete. For the web flow, the button is the delete — but the
	// preview is shown in the banner BEFORE the next render.
	preview, err := s.backupMgr.PrunePreview()
	if err != nil {
		s.log.Error("backup prune preview failed", "error", err)
		s.renderBackups(w, r, backupResultUnwritable, "")
		return
	}
	if len(preview) == 0 {
		s.renderBackups(w, r, backupResultNothingToPrune, "")
		return
	}
	deleted, err := s.backupMgr.Prune(r.Context())
	if err != nil {
		s.log.Error("backup prune failed", "error", err)
		s.renderBackups(w, r, backupResultUnwritable, "")
		return
	}
	_ = deleted
	s.renderBackups(w, r, backupResultPruned, "")
}

// handleBackupsRestore is POST /backups/restore — the two-step guarded
// restore. Without confirm=1, nothing is mutated: the row re-renders in a
// confirm state. With confirm=1, the current DB is snapshotted first, then
// the selected snapshot is restored.
func (s *Server) handleBackupsRestore(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	if s.backupMgr == nil {
		s.renderBackups(w, r, backupResultUnavailable, "")
		return
	}
	id := r.PostFormValue("snapshot")
	if id == "" {
		s.renderBackups(w, r, backupResultInvalid, "")
		return
	}

	// Step 1: no confirm=1 → re-render in confirm state (no mutation).
	if r.PostFormValue("confirm") != "1" {
		// Validate the target exists (path-traversal containment).
		if _, err := s.backupMgr.RestoreTarget(id); err != nil {
			s.renderBackups(w, r, backupResultNotFound, "")
			return
		}
		s.renderBackups(w, r, "", id)
		return
	}

	// Step 2: confirm=1 → take pre-restore snapshot, then swap.
	s.backupMu.Lock()
	if s.backupInFlight {
		s.backupMu.Unlock()
		s.renderBackups(w, r, backupResultInProgress, "")
		return
	}
	s.backupInFlight = true
	s.backupMu.Unlock()
	defer func() {
		s.backupMu.Lock()
		s.backupInFlight = false
		s.backupMu.Unlock()
	}()

	preRestore, err := s.backupMgr.Restore(r.Context(), id)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			s.renderBackups(w, r, backupResultNotFound, "")
			return
		}
		s.log.Error("backup restore failed", "error", err)
		s.renderBackups(w, r, backupResultRolledBack, "")
		return
	}
	_ = preRestore
	s.renderBackups(w, r, backupResultNeedsRestart, "")
}

// renderBackups assembles the Backups page and renders it (full document or
// boosted partial). backupResult is the fixed-enum banner from a
// just-completed POST; confirmRestore is the snapshot ID in confirm state.
func (s *Server) renderBackups(w http.ResponseWriter, r *http.Request, backupResult, confirmRestore string) {
	s.renderBackupsWithConfig(w, r, backupsData{BackupResult: backupResult, ConfirmRestore: confirmRestore})
}

// renderBackupsWithConfig is renderBackups with the config-form fields
// pre-populated: a zero cfgData (plain renders) shows the CURRENT settings;
// a cfgData from a failed config save echoes the submitted values and their
// field errors.
func (s *Server) renderBackupsWithConfig(w http.ResponseWriter, r *http.Request, cfgData backupsData) {
	ctx := r.Context()
	echo := cfgData.ErrDir != "" || cfgData.ErrRetention != "" || cfgData.Retention.Daily < 0
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Backups · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Backups · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}

	// External snapshots (read-only, from the DB — the ingest inventory).
	extSnaps, err := s.store.ListSnapshots(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var extFootprint int64
	for _, sn := range extSnaps {
		extFootprint += sn.SizeBytes
	}

	// Owned snapshots (actionable, from the filesystem via the manager).
	data := backupsData{
		baseData:            base,
		ExternalSnapshots:   extSnaps,
		ExternalFootprint:   extFootprint,
		HasSnapshotPipeline: len(extSnaps) > 0 || s.signalSnapshotsDirExists(),
		BackupResult:        cfgData.BackupResult,
		ConfirmRestore:      cfgData.ConfirmRestore,
		CfgSaveResult:       cfgData.CfgSaveResult,
		ErrDir:              cfgData.ErrDir,
		ErrRetention:        cfgData.ErrRetention,
	}

	// Configuration form: the live/boot settings, unless a failed save is
	// being echoed back (then keep the submitted values so the user can
	// correct instead of retyping; retention is re-defaulted for display).
	if s.backupConfig != nil {
		data.ConfigAvailable = true
	}
	data.DefaultDir = filepath.Join(s.rootsCfg.DataDir, "backups")
	if echo {
		data.ConfigDir = cfgData.ConfigDir
		data.Retention = cfgData.Retention
		// A failed tier parse leaves -1 sentinels; clamp them for display —
		// the field-error banner carries the failure, the inputs re-render 0.
		if data.Retention.Daily < 0 {
			data.Retention.Daily = 0
		}
		if data.Retention.Monthly < 0 {
			data.Retention.Monthly = 0
		}
		if data.Retention.Quarterly < 0 {
			data.Retention.Quarterly = 0
		}
		if data.Retention.Yearly < 0 {
			data.Retention.Yearly = 0
		}
	} else {
		data.ConfigDir, data.Retention = s.currentBackupsConfig()
	}

	if s.backupMgr != nil {
		data.BackupMgrAvailable = true
		owned, err := s.backupMgr.List()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.OwnedSnapshots = owned
		for _, sn := range owned {
			data.OwnedFootprint += sn.SizeBytes
		}
		if preview, err := s.backupMgr.PrunePreview(); err == nil {
			data.PrunePreview = preview
		}
	}

	tok, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	data.SetupToken = tok
	s.render(w, r, "backups", data)
}

// backupDirDisplay returns a human-readable path for the snapshot directory,
// or "" when no manager is wired.
func (s *Server) backupDirDisplay() string {
	if s.backupMgr == nil {
		return ""
	}
	return s.backupMgr.Dir()
}

// backupDirMaxLen / backupRetentionMax bound the accepted config form values.
const (
	backupDirMaxLen        = 2048
	backupRetentionMax     = 1000
	backupCfgResultOK      = "ok"
	backupCfgResultErr     = "error"
	backupCfgResultUnavail = "unavailable"
	backupCfgResultUnwrit  = "unwritable"
)

// currentBackupsConfig resolves the settings the form displays: the live
// configurator when wired, else the boot-time config snapshot.
func (s *Server) currentBackupsConfig() (string, config.RetentionConfig) {
	if s.backupConfig != nil {
		return s.backupConfig.CurrentBackups()
	}
	return s.backupBoot.Dir, s.backupBoot.EffectiveRetention()
}

// validateBackupDir checks a submitted snapshot directory: bounded length,
// printable runes only, and not inside archive_root (the read-only archive;
// ADR-0010 §4 — the same rule config.Load enforces at boot). Empty is VALID
// (it means the <data_dir>/backups default). Returns "" or a fixed error
// enum ("toolong" / "invalid" / "inarchive").
func (s *Server) validateBackupDir(dir string) string {
	if dir == "" {
		return ""
	}
	if len(dir) > backupDirMaxLen {
		return "toolong"
	}
	for _, r := range dir {
		if unicode.IsControl(r) {
			return "invalid"
		}
	}
	if !filepath.IsAbs(dir) {
		return "invalid"
	}
	archive := s.rootsCfg.ArchiveRoot
	if archive == "" {
		return ""
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "invalid"
	}
	absArchive, err := filepath.Abs(archive)
	if err != nil {
		return ""
	}
	if rel, err := filepath.Rel(absArchive, absDir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "inarchive"
	}
	return ""
}

// validateBackupRetention parses one retention tier count from the form.
// Empty means "use the default"; the value must be a non-negative integer
// bounded well beyond any sane policy. Returns the count or -1 on error.
func parseRetentionTier(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > backupRetentionMax {
		return -1
	}
	return n
}

// handleBackupsConfigSave is POST /backups/config — the privileged save
// behind the tab's configuration form (issue #300). Gate first
// (checkSetupPOST: same-origin + per-session token + body cap, 403 before
// any work), validate second, probe the directory is writable third, and
// only then apply: persist backups.dir + retention to the config file and
// swap the live manager. Success re-renders with the saved banner and the
// now-effective values.
func (s *Server) handleBackupsConfigSave(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}

	data := backupsData{}
	data.ConfigDir = strings.TrimSpace(r.PostFormValue("dir"))
	data.Retention.Daily = parseRetentionTier(r.PostFormValue("retention_daily"))
	data.Retention.Monthly = parseRetentionTier(r.PostFormValue("retention_monthly"))
	data.Retention.Quarterly = parseRetentionTier(r.PostFormValue("retention_quarterly"))
	data.Retention.Yearly = parseRetentionTier(r.PostFormValue("retention_yearly"))

	data.ErrDir = s.validateBackupDir(data.ConfigDir)
	if data.Retention.Daily < 0 || data.Retention.Monthly < 0 || data.Retention.Quarterly < 0 || data.Retention.Yearly < 0 {
		data.ErrRetention = "invalid"
	}
	if data.ErrDir != "" || data.ErrRetention != "" {
		s.renderBackupsWithConfig(w, r, data)
		return
	}

	// Nothing can be saved without a configurator, so report that before the
	// probe below — otherwise an unavailable save still leaves a freshly
	// created directory behind on disk.
	if s.backupConfig == nil {
		data.CfgSaveResult = backupCfgResultUnavail
		data.ConfigDir, data.Retention = s.currentBackupsConfig()
		s.renderBackupsWithConfig(w, r, data)
		return
	}

	// The directory must be creatable/writable before the settings are
	// persisted — otherwise the tab would save a path every later Create
	// fails on. Empty dir resolves to the <data_dir>/backups default.
	effective := data.ConfigDir
	if effective == "" {
		effective = filepath.Join(s.rootsCfg.DataDir, "backups")
	}
	if err := probeWritableDir(effective); err != nil {
		s.log.Warn("backup config dir not writable", "dir", effective, "error", err)
		data.CfgSaveResult = backupCfgResultUnwrit
		data.ConfigDir, data.Retention = s.currentBackupsConfig()
		s.renderBackupsWithConfig(w, r, data)
		return
	}
	if err := s.backupConfig.ApplyBackups(data.ConfigDir, data.Retention); err != nil {
		s.log.Error("backup config save failed", "error", err)
		data.CfgSaveResult = backupCfgResultErr
		data.ConfigDir, data.Retention = s.currentBackupsConfig()
		s.renderBackupsWithConfig(w, r, data)
		return
	}
	s.log.Info("backup config saved and applied live", "dir", data.ConfigDir,
		"daily", data.Retention.Daily, "monthly", data.Retention.Monthly,
		"quarterly", data.Retention.Quarterly, "yearly", data.Retention.Yearly)

	s.renderBackupsWithConfig(w, r, backupsData{CfgSaveResult: backupCfgResultOK})
}

// probeWritableDir verifies dir exists or can be created (0700) and that a
// file can be written inside it — the same contract backup.Manager relies on
// at Create time, checked BEFORE the settings are persisted.
func probeWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
