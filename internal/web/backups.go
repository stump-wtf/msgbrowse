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
	"net/http"

	"github.com/joestump/msgbrowse/internal/backup"
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
	ctx := r.Context()
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
		BackupResult:        backupResult,
		ConfirmRestore:      confirmRestore,
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
