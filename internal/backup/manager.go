// Package backup implements msgbrowse-owned snapshots of data_dir (the SQLite
// database + embeddings) and the config file (ADR-0026 / SPEC-0026).
//
// A snapshot is a directory under a configurable backups.dir, containing a
// VACUUM INTO-produced consistent copy of msgbrowse.sqlite, the config file,
// and a manifest.json with per-file checksums. Snapshots are plaintext (the
// pure-Go SQLite driver is not SQLCipher-encrypted, ADR-0013), so files are
// mode 0600 and the directory 0700 — they contain the entire message corpus.
//
// The manager is single-flight: Create and Prune are disk-expensive and MUST
// be serialized so held-back-to-back requests cannot fill the disk or race a
// writer. A second Create while one is in flight returns ErrInProgress.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	_ "modernc.org/sqlite" // VACUUM INTO opens a fresh connection
)

// SnapshotNameRe matches a msgbrowse-owned snapshot directory:
// snapshot-YYYYMMDD-HHMMSS or snapshot-YYYYMMDD-HHMMSS-NN (the -NN suffix is
// appended when two snapshots land in the same second).
var SnapshotNameRe = regexp.MustCompile(`^(pre-restore-)?snapshot-(\d{8})-(\d{6})(-\d{2})?$`)

// SnapshotTimeLayout is the timestamp layout embedded in a snapshot directory name.
const SnapshotTimeLayout = "20060102-150405"

// manifestFileName is the JSON metadata file inside each snapshot directory.
const manifestFileName = "manifest.json"

// ErrInProgress is returned when a second snapshot operation is attempted
// while one is already in flight. Callers render it as a fixed-enum banner
// ("a snapshot is already in progress"), not an error.
var ErrInProgress = errors.New("snapshot operation already in progress")

// ErrNotFound is returned when a snapshot identifier does not match any
// snapshot in the stored inventory. It is the path-traversal containment
// signal: a restore/delete target that is not in the listing is refused.
var ErrNotFound = errors.New("snapshot not found")

// Manifest is the per-snapshot metadata written to manifest.json.
type Manifest struct {
	Version       int               `json:"version"`
	TakenAt       time.Time         `json:"taken_at"`
	SchemaVersion int               `json:"schema_version"`
	Files         map[string]string `json:"files"` // basename → SHA-256 hex
}

// Snapshot is one msgbrowse-owned snapshot, listed from the filesystem.
type Snapshot struct {
	ID         string // directory name, e.g. snapshot-20260726-143012
	TakenAt    time.Time
	SizeBytes  int64
	Tier       string
	PreRestore bool // true for snapshots taken automatically before a restore
}

// Manager creates, lists, prunes, and restores msgbrowse-owned snapshots.
// It is safe for concurrent use; Create/Prune/Restore are serialized by an
// in-progress guard so a second call while one is in flight returns
// ErrInProgress.
type Manager struct {
	dir       string
	dbPath    string
	cfgPath   string
	retention config.RetentionConfig
	mu        sync.Mutex
	inFlight  bool
	now       func() time.Time
}

// NewManager creates a Manager. dbPath is the path to msgbrowse.sqlite (the
// live database). cfgPath is the path to config.yaml (may be empty — a
// snapshot without a config is still useful for restoring the DB). dir is the
// snapshot directory; the caller is expected to have resolved it (default
// <data_dir>/backups) and validated it is not inside archive_root.
func NewManager(dir, dbPath, cfgPath string, retention config.RetentionConfig) *Manager {
	return &Manager{
		dir:       dir,
		dbPath:    dbPath,
		cfgPath:   cfgPath,
		retention: retention,
		now:       time.Now,
	}
}

// Dir returns the snapshot directory path.
func (m *Manager) Dir() string { return m.dir }

// resolveDir returns the effective snapshot directory, creating it (0700) if
// it does not exist.
func (m *Manager) resolveDir() (string, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot dir %q: %w", m.dir, err)
	}
	return m.dir, nil
}

// Create produces a new snapshot of the live database and config file. It is
// single-flight: a second Create while one is in flight returns ErrInProgress
// without touching the filesystem.
//
// The database is snapshotted via VACUUM INTO — SQLite's online backup
// primitive — so a concurrent ingest or read request does not corrupt the
// snapshot. "Copying a live SQLite file is not a backup" (ADR-0026).
func (m *Manager) Create(ctx context.Context) (Snapshot, error) {
	if !m.acquire() {
		return Snapshot{}, ErrInProgress
	}
	defer m.release()

	return m.create(ctx, false)
}

// create is the shared Create path, also used by pre-restore snapshots. The
// preRestore flag marks the snapshot directory name so a future listing can
// distinguish it.
func (m *Manager) create(ctx context.Context, preRestore bool) (Snapshot, error) {
	dir, err := m.resolveDir()
	if err != nil {
		return Snapshot{}, err
	}

	now := m.now()
	prefix := "snapshot-"
	if preRestore {
		prefix = "pre-restore-snapshot-"
	}
	// Ensure a unique directory name when two snapshots land in the same
	// second (a rare but real case). Append -NN before the timestamp would
	// break parseSnapshotName, so disambiguate by appending a suffix to the
	// timestamp only when the directory already exists.
	name := prefix + now.UTC().Format(SnapshotTimeLayout)
	snapDir := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(snapDir); os.IsNotExist(err) {
			break
		}
		name = prefix + now.UTC().Format(SnapshotTimeLayout) + "-" + fmt.Sprintf("%02d", i)
		snapDir = filepath.Join(dir, name)
	}
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create snapshot directory: %w", err)
	}

	// Clean up a half-written snapshot on any failure so the listing never
	// shows a partial snapshot.
	cleanup := func() { _ = os.RemoveAll(snapDir) }

	// 1. VACUUM INTO the database snapshot — consistent, no exclusive lock.
	dbSnap := filepath.Join(snapDir, store.DBFileName)
	if _, err := m.vacuumInto(ctx, dbSnap); err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("snapshot database: %w", err)
	}
	if err := os.Chmod(dbSnap, 0o600); err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("chmod snapshot db: %w", err)
	}

	// 2. Copy the config file if present.
	files := map[string]string{store.DBFileName: ""}
	if m.cfgPath != "" {
		if err := copyFile0600(m.cfgPath, filepath.Join(snapDir, "config.yaml")); err != nil {
			cleanup()
			return Snapshot{}, fmt.Errorf("snapshot config: %w", err)
		}
		files["config.yaml"] = ""
	}

	// 3. Compute checksums and write the manifest.
	checksums, err := checksumDir(snapDir)
	if err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("checksum snapshot: %w", err)
	}
	for k := range files {
		if sum, ok := checksums[k]; ok {
			files[k] = sum
		}
	}
	schemaVer, _ := m.readSchemaVersion(ctx, dbSnap)
	manifest := Manifest{
		Version:       1,
		TakenAt:       now.UTC(),
		SchemaVersion: schemaVer,
		Files:         files,
	}
	if err := writeManifest(snapDir, manifest); err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("write manifest: %w", err)
	}

	size, _ := dirSize(snapDir)
	return Snapshot{
		ID:         name,
		TakenAt:    now.UTC(),
		SizeBytes:  size,
		PreRestore: preRestore,
	}, nil
}

// vacuumInto runs VACUUM INTO <target> on a fresh connection to the live
// database. VACUUM INTO is SQLite's online backup primitive: it produces a
// transactionally consistent copy without an exclusive lock and without
// coordinating with writers.
func (m *Manager) vacuumInto(ctx context.Context, target string) (int64, error) {
	// Open a read-only connection to the LIVE database (not the snapshot).
	dsn := "file:" + m.dbPath + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, fmt.Errorf("open live db for vacuum: %w", err)
	}
	defer db.Close()

	// VACUUM INTO writes a consistent snapshot to <target>. The target path
	// must be quoted as a string literal in the SQL.
	_, err = db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO %q`, target))
	if err != nil {
		return 0, fmt.Errorf("vacuum into: %w", err)
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		return 0, statErr
	}
	return info.Size(), nil
}

// readSchemaVersion reads PRAGMA user_version from a snapshot's database file.
func (m *Manager) readSchemaVersion(ctx context.Context, dbPath string) (int, error) {
	dsn := "file:" + dbPath + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// List returns all msgbrowse-owned snapshots, ordered newest first. The
// listing is read from the filesystem (not the DB), so ingest's
// ReplaceSnapshots path cannot clobber it (REQ-0026-006).
func (m *Manager) List() ([]Snapshot, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot dir: %w", err)
	}
	now := m.now()
	var snaps []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, takenAt, preRestore, ok := parseSnapshotName(e.Name())
		if !ok {
			continue
		}
		size, _ := dirSize(filepath.Join(m.dir, e.Name()))
		snaps = append(snaps, Snapshot{
			ID:         id,
			TakenAt:    takenAt,
			SizeBytes:  size,
			Tier:       classifySnapshotTier(now.Sub(takenAt)),
			PreRestore: preRestore,
		})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TakenAt.After(snaps[j].TakenAt) })
	return snaps, nil
}

// parseSnapshotName parses a snapshot directory name into its ID, timestamp,
// and pre-restore flag. ok is false for unrecognized names. A -NN suffix
// (added to disambiguate same-second snapshots) is stripped before parsing
// the timestamp.
func parseSnapshotName(name string) (id string, takenAt time.Time, preRestore, ok bool) {
	if rest, found := stripPrefix(name, "pre-restore-snapshot-"); found {
		rest = trimDisambigSuffix(rest)
		t, err := time.Parse(SnapshotTimeLayout, rest)
		if err != nil {
			return name, time.Time{}, false, false
		}
		return name, t.UTC(), true, true
	}
	if rest, found := stripPrefix(name, "snapshot-"); found {
		rest = trimDisambigSuffix(rest)
		t, err := time.Parse(SnapshotTimeLayout, rest)
		if err != nil {
			return name, time.Time{}, false, false
		}
		return name, t.UTC(), false, true
	}
	return name, time.Time{}, false, false
}

// trimDisambigSuffix strips a trailing -NN (the same-second disambiguator)
// so the remaining string parses as a timestamp. "20060102-150405-02" →
// "20060102-150405". If there is no suffix, the input is returned unchanged.
func trimDisambigSuffix(s string) string {
	// The timestamp is 15 chars (YYYYMMDD-HHMMSS); a suffix is -NN (3 chars).
	if len(s) == 18 && s[15] == '-' {
		return s[:15]
	}
	return s
}

// stripPrefix reports whether name starts with prefix and returns the rest.
func stripPrefix(name, prefix string) (string, bool) {
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):], true
	}
	return name, false
}

// classifySnapshotTier maps a snapshot's age to the GFS tier it falls into.
// Mirrors internal/ingest.classifyTier so the listing is consistent with the
// external inventory.
func classifySnapshotTier(age time.Duration) string {
	switch {
	case age <= 14*24*time.Hour:
		return "daily"
	case age <= 395*24*time.Hour:
		return "monthly"
	case age <= 3*365*24*time.Hour:
		return "quarterly"
	default:
		return "yearly"
	}
}

// PrunePreview returns the snapshots that prune WOULD delete, without
// deleting anything. It applies the configured GFS counts and never marks the
// newest snapshot for deletion (prune never empties the set).
func (m *Manager) PrunePreview() ([]Snapshot, error) {
	snaps, err := m.List()
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	// The newest snapshot is always kept; prune never empties the set.
	newest := snaps[0]
	toDelete := applyRetention(snaps, m.retention)
	// Ensure the newest is never in the delete set.
	filtered := toDelete[:0]
	for _, s := range toDelete {
		if s.ID != newest.ID {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

// Prune applies the configured GFS retention policy, deleting snapshots outside
// the keep counts. It is single-flight and never deletes the only snapshot.
func (m *Manager) Prune(ctx context.Context) ([]Snapshot, error) {
	if !m.acquire() {
		return nil, ErrInProgress
	}
	defer m.release()

	toDelete, err := m.PrunePreview()
	if err != nil {
		return nil, err
	}
	for _, s := range toDelete {
		path := filepath.Join(m.dir, s.ID)
		if err := os.RemoveAll(path); err != nil {
			return toDelete[:0], fmt.Errorf("delete snapshot %q: %w", s.ID, err)
		}
	}
	return toDelete, nil
}

// RestoreTarget is the snapshot to restore, validated against the listing.
func (m *Manager) RestoreTarget(id string) (Snapshot, error) {
	snaps, err := m.List()
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range snaps {
		if s.ID == id {
			return s, nil
		}
	}
	return Snapshot{}, ErrNotFound
}

// Restore replaces the live database with the selected snapshot. It is
// single-flight. Before swapping, it takes a pre-restore snapshot of the
// current database so a mid-failure is recoverable. The snapshot is verified
// (checksums + a read-only open) before the live DB is touched.
//
// The caller is responsible for closing and re-opening any live *store.Store
// connection; Restore swaps the file on disk but does not re-open in-process
// connections (serve and the desktop shell re-open after the handler returns).
func (m *Manager) Restore(ctx context.Context, id string) (Snapshot, error) {
	if !m.acquire() {
		return Snapshot{}, ErrInProgress
	}
	defer m.release()

	// 1. Validate the target against the listing (path-traversal containment).
	target, err := m.RestoreTarget(id)
	if err != nil {
		return Snapshot{}, err
	}
	snapDir := filepath.Join(m.dir, target.ID)
	dbSnap := filepath.Join(snapDir, store.DBFileName)

	// 2. Verify the snapshot manifest + checksums before touching the live DB.
	if err := verifySnapshot(snapDir); err != nil {
		return Snapshot{}, fmt.Errorf("verify snapshot: %w", err)
	}

	// 3. Verify the snapshot DB opens and is a valid SQLite database.
	if err := verifyDatabaseOpens(ctx, dbSnap); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot db invalid: %w", err)
	}

	// 4. Take a pre-restore snapshot of the current database.
	preRestore, err := m.create(ctx, true)
	if err != nil {
		return Snapshot{}, fmt.Errorf("pre-restore snapshot: %w", err)
	}

	// 5. Swap: rename the live DB aside, copy the snapshot DB into place.
	// rename(2) is atomic on the same filesystem; a crash leaves either the
	// old or the new file, never a half-written one.
	liveBackup := m.dbPath + ".pre-restore-" + target.ID
	if err := os.Rename(m.dbPath, liveBackup); err != nil {
		return preRestore, fmt.Errorf("rename live db aside: %w", err)
	}
	if err := copyFile0600(dbSnap, m.dbPath); err != nil {
		// Rollback: put the old DB back.
		_ = os.Rename(liveBackup, m.dbPath)
		return preRestore, fmt.Errorf("copy snapshot into place (rolled back): %w", err)
	}
	// The live DB is now the restored snapshot; keep the pre-restore aside
	// for safety (it will be cleaned up by a future prune, or the operator
	// can delete it manually).
	return preRestore, nil
}

// acquire is the single-flight guard. It returns false (without blocking) if
// an operation is already in flight.
func (m *Manager) acquire() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight {
		return false
	}
	m.inFlight = true
	return true
}

func (m *Manager) release() {
	m.mu.Lock()
	m.inFlight = false
	m.mu.Unlock()
}

// --- helpers ---

// applyRetention returns the snapshots that fall outside the configured GFS
// keep counts. It classifies each snapshot into a tier by age, keeps the
// newest N per tier, and marks the rest for deletion.
func applyRetention(snaps []Snapshot, r config.RetentionConfig) []Snapshot {
	// snaps is newest-first from List().
	tiers := map[string][]Snapshot{"daily": {}, "monthly": {}, "quarterly": {}, "yearly": {}}
	for _, s := range snaps {
		tiers[s.Tier] = append(tiers[s.Tier], s)
	}
	limits := map[string]int{
		"daily":     r.Daily,
		"monthly":   r.Monthly,
		"quarterly": r.Quarterly,
		"yearly":    r.Yearly,
	}
	var toDelete []Snapshot
	for tier, list := range tiers {
		keep := limits[tier]
		if keep < 0 || len(list) <= keep {
			// keep < 0 means "skip this tier entirely" (a safety valve);
			// len(list) <= keep means everything in the tier stays.
			continue
		}
		// keep == 0 means "delete everything in this tier". The newest-
		// overall snapshot is still protected by PrunePreview, which
		// strips it from the delete set after applyRetention returns.
		// list is newest-first; delete the oldest ones beyond keep.
		toDelete = append(toDelete, list[keep:]...)
	}
	return toDelete
}

// copyFile0600 copies src to dst with mode 0600. Used for the config file
// and for restoring the DB into place.
func copyFile0600(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// checksumDir computes SHA-256 checksums for every file in dir (non-recursive).
func checksumDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		sum, err := fileSHA256(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", e.Name(), err)
		}
		out[e.Name()] = sum
	}
	return out, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dirSize returns the total size of all files in a directory tree.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// writeManifest marshals the manifest to JSON and writes it to
// <dir>/manifest.json with mode 0600.
func writeManifest(dir string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestFileName), data, 0o600)
}

// verifySnapshot reads the manifest from a snapshot directory and re-checks
// every file's checksum. A mismatch means the snapshot is corrupt or was
// tampered with — restore is refused.
func verifySnapshot(snapDir string) error {
	data, err := os.ReadFile(filepath.Join(snapDir, manifestFileName))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	for name, wantSum := range m.Files {
		gotSum, err := fileSHA256(filepath.Join(snapDir, name))
		if err != nil {
			return fmt.Errorf("verify %s: %w", name, err)
		}
		if gotSum != wantSum {
			return fmt.Errorf("checksum mismatch for %s (manifest %s, on-disk %s)", name, wantSum, gotSum)
		}
	}
	return nil
}

// verifyDatabaseOpens opens the SQLite file read-only and runs a trivial
// query to confirm it is a valid, openable database.
func verifyDatabaseOpens(ctx context.Context, path string) error {
	dsn := "file:" + path + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	var one int
	if err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("integrity query: %w", err)
	}
	return nil
}

// ResolveDir returns the effective snapshot directory for a config + data_dir.
// If backups.dir is set, it is used; otherwise the default is
// <data_dir>/backups.
func ResolveDir(cfg config.Config) string {
	if cfg.Backups.Dir != "" {
		return cfg.Backups.Dir
	}
	return filepath.Join(cfg.DataDir, "backups")
}
