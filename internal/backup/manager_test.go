package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	_ "modernc.org/sqlite"
)

// newTestManager builds a Manager backed by a temp-dir data_dir, a fresh
// store.Open database, and a fake config file. It returns the manager, the
// store (for closing), the DB path, and the snapshot dir.
func newTestManager(t *testing.T) (*Manager, *store.Store, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, store.DBFileName)
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	snapDir := filepath.Join(dataDir, "backups")
	m := NewManager(snapDir, dbPath, cfgPath, config.DefaultRetention)
	return m, st, dbPath, snapDir
}

func TestCreateProducesSnapshot(t *testing.T) {
	m, _, _, snapDir := newTestManager(t)
	ctx := context.Background()

	snap, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(snap.ID, "snapshot-") {
		t.Fatalf("snapshot ID = %q, want snapshot- prefix", snap.ID)
	}
	snapPath := filepath.Join(snapDir, snap.ID)
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot dir missing: %v", err)
	}
	// The snapshot must contain the DB and the config.
	if _, err := os.Stat(filepath.Join(snapPath, store.DBFileName)); err != nil {
		t.Fatalf("snapshot db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapPath, "config.yaml")); err != nil {
		t.Fatalf("snapshot config missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapPath, "manifest.json")); err != nil {
		t.Fatalf("snapshot manifest missing: %v", err)
	}
}

func TestCreateFileMode(t *testing.T) {
	m, _, _, snapDir := newTestManager(t)
	ctx := context.Background()

	snap, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snapPath := filepath.Join(snapDir, snap.ID)

	// DB file must be 0600.
	info, err := os.Stat(filepath.Join(snapPath, store.DBFileName))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db perm = %o, want 0600", perm)
	}
	// Snapshot directory must be 0700.
	dirInfo, err := os.Stat(snapPath)
	if err != nil {
		t.Fatalf("stat snap dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("snapshot dir perm = %o, want 0700", perm)
	}
}

func TestCreateIsConsistentUnderConcurrentWrites(t *testing.T) {
	// The load-bearing test: a snapshot taken while a writer is active must
	// be a consistent, openable database. VACUUM INTO, not a filesystem copy.
	m, st, dbPath, snapDir := newTestManager(t)
	ctx := context.Background()

	// Seed a conversation + message so the DB has content.
	_, err := st.UpsertConversation(ctx, "signal", "Test Chat")
	if err != nil {
		t.Fatalf("upsert conversation: %v", err)
	}

	// Start a writer goroutine that writes messages continuously.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, _ = st.UpsertConversation(ctx, "signal", "Concurrent Writer "+strings.Repeat("x", i))
		}
	}()

	// Take a snapshot while the writer is active.
	snap, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create during concurrent writes: %v", err)
	}
	<-done // let the writer finish

	// The snapshot DB must open cleanly and be queryable.
	snapDB := filepath.Join(snapDir, snap.ID, store.DBFileName)
	snapStore, err := store.OpenReadOnly(snapDB)
	if err != nil {
		t.Fatalf("open snapshot db read-only: %v", err)
	}
	defer snapStore.Close()

	// PRAGMA integrity_check must pass.
	var result string
	if err := snapStore.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if result != "ok" {
		t.Fatalf("snapshot db integrity_check = %q, want ok", result)
	}

	// The live DB and the snapshot DB must both be openable after.
	liveInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat live db: %v", err)
	}
	if liveInfo.Size() == 0 {
		t.Fatal("live db is empty after snapshot")
	}
}

func TestCreateSingleFlight(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	ctx := context.Background()

	// Block the first Create by holding the lock manually.
	if !m.acquire() {
		t.Fatal("acquire failed on first attempt")
	}
	defer m.release()

	_, err := m.Create(ctx)
	if err != ErrInProgress {
		t.Fatalf("Create while in flight = %v, want ErrInProgress", err)
	}
}

func TestCreateNoPartialFileOnFailure(t *testing.T) {
	// If the target directory is unwritable, Create must fail and leave no
	// partial snapshot in the listing.
	m, _, _, _ := newTestManager(t)
	ctx := context.Background()

	// Make the snapshot dir unwritable.
	snapDir := m.dir
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatalf("mkdir snap dir: %v", err)
	}
	if err := os.Chmod(snapDir, 0o500); err != nil {
		t.Fatalf("chmod snap dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapDir, 0o700) })

	_, err := m.Create(ctx)
	if err == nil {
		t.Fatal("Create on unwritable dir succeeded; want error")
	}

	// Listing must be empty — no partial snapshot.
	snaps, err := m.List()
	if err != nil {
		t.Fatalf("List after failed Create: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("List after failed Create = %d snapshots, want 0", len(snaps))
	}
}

func TestListOrderedNewestFirst(t *testing.T) {
	m, _, _, _ := newTestManager(t)

	// Create three snapshots with controlled timestamps.
	base := time.Date(2026, 7, 26, 14, 30, 0, 0, time.UTC)
	t1 := base
	t2 := base.Add(1 * time.Hour)
	t3 := base.Add(2 * time.Hour)

	createAt(t, m, t1)
	createAt(t, m, t2)
	createAt(t, m, t3)

	snaps, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("List = %d snapshots, want 3", len(snaps))
	}
	if !snaps[0].TakenAt.After(snaps[1].TakenAt) {
		t.Fatalf("List not newest-first: [0]=%s [1]=%s", snaps[0].TakenAt, snaps[1].TakenAt)
	}
	if !snaps[1].TakenAt.After(snaps[2].TakenAt) {
		t.Fatalf("List not newest-first: [1]=%s [2]=%s", snaps[1].TakenAt, snaps[2].TakenAt)
	}
}

func TestListExcludesUnrecognized(t *testing.T) {
	m, _, _, snapDir := newTestManager(t)
	ctx := context.Background()

	// Create a real snapshot.
	if _, err := m.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create an unrecognized directory.
	if err := os.MkdirAll(filepath.Join(snapDir, "not-a-snapshot"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Create a file (not a directory).
	if err := os.WriteFile(filepath.Join(snapDir, "random.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	snaps, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("List = %d snapshots, want 1 (unrecognized excluded)", len(snaps))
	}
}

func TestPrunePreviewDoesNotDelete(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	ctx := context.Background()

	// Create two snapshots.
	s1, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	s2, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	// PrunePreview must not delete anything.
	preview, err := m.PrunePreview()
	if err != nil {
		t.Fatalf("PrunePreview: %v", err)
	}
	// With only 2 snapshots and defaults (14 daily), nothing should be marked.
	if len(preview) != 0 {
		t.Fatalf("PrunePreview = %d, want 0", len(preview))
	}

	snaps, _ := m.List()
	if len(snaps) != 2 {
		t.Fatalf("after PrunePreview, List = %d, want 2", len(snaps))
	}
	_ = s1
	_ = s2
}

func TestPruneNeverEmptiesTheSet(t *testing.T) {
	// Even with a policy that would delete everything, prune keeps the newest.
	// Mock the filesystem with snapshots old enough that zero-keep policy
	// would mark them all.
	dir := t.TempDir()
	m := NewManager(dir, "", "", config.RetentionConfig{
		Daily: 0, Monthly: 0, Quarterly: 0, Yearly: 0,
	})
	ctx := context.Background()

	now := time.Now()
	// Create 3 "old" snapshots (4+ months ago = monthly tier) and 1 "new".
	for i := 1; i <= 3; i++ {
		old := now.AddDate(0, -i*2, 0)
		name := "snapshot-" + old.UTC().Format(SnapshotTimeLayout)
		d := filepath.Join(dir, name)
		_ = os.MkdirAll(d, 0o700)
		_ = os.WriteFile(filepath.Join(d, "dummy"), []byte("x"), 0o600)
	}
	newName := "snapshot-" + now.UTC().Format(SnapshotTimeLayout)
	newDir := filepath.Join(dir, newName)
	_ = os.MkdirAll(newDir, 0o700)
	_ = os.WriteFile(filepath.Join(newDir, "dummy"), []byte("x"), 0o600)

	deleted, err := m.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// The newest must survive — prune never empties the set.
	snaps, _ := m.List()
	if len(snaps) != 1 {
		t.Fatalf("after Prune, %d snapshots survive, want 1 (the newest)", len(snaps))
	}
	if snaps[0].ID != newName {
		t.Fatalf("surviving snapshot = %q, want %q", snaps[0].ID, newName)
	}
	if len(deleted) == 0 {
		t.Fatal("Prune deleted nothing despite a zero-keep policy")
	}
}

func TestRestoreTargetRejectsPathTraversal(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	ctx := context.Background()

	// Create a real snapshot so the listing is non-empty.
	if _, err := m.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A path-traversal attempt must return ErrNotFound.
	_, err := m.RestoreTarget("../../etc/passwd")
	if err != ErrNotFound {
		t.Fatalf("RestoreTarget with traversal = %v, want ErrNotFound", err)
	}
	_, err = m.RestoreTarget("snapshot-../something")
	if err != ErrNotFound {
		t.Fatalf("RestoreTarget with separator = %v, want ErrNotFound", err)
	}
	// A snapshot that does not exist must also return ErrNotFound.
	_, err = m.RestoreTarget("snapshot-99999999-999999")
	if err != ErrNotFound {
		t.Fatalf("RestoreTarget nonexistent = %v, want ErrNotFound", err)
	}
}

func TestRestoreTakesPreRestoreSnapshot(t *testing.T) {
	m, st, _, _ := newTestManager(t)
	ctx := context.Background()

	// Seed the DB with content.
	_, err := st.UpsertConversation(ctx, "signal", "Original Chat")
	if err != nil {
		t.Fatalf("upsert original: %v", err)
	}

	// Create a snapshot (this is the "backup" we will restore from).
	snap, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the live DB AFTER the snapshot (so restore changes observable state).
	_, err = st.UpsertConversation(ctx, "signal", "Post-snapshot Chat")
	if err != nil {
		t.Fatalf("upsert post-snapshot: %v", err)
	}

	// Restore the snapshot. This must take a pre-restore snapshot first.
	preRestore, err := m.Restore(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !preRestore.PreRestore {
		t.Fatal("Restore did not return a pre-restore snapshot")
	}
	if !strings.HasPrefix(preRestore.ID, "pre-restore-") {
		t.Fatalf("pre-restore ID = %q, want pre-restore- prefix", preRestore.ID)
	}

	// The pre-restore snapshot must be in the listing.
	snaps, _ := m.List()
	found := false
	for _, s := range snaps {
		if s.ID == preRestore.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-restore snapshot not found in listing")
	}
}

func TestRestoreSnapshotIsConsistentAndOpenable(t *testing.T) {
	// The snapshot restored into place must be a valid, openable database —
	// the integrity test that matters after a restore.
	m, st, _, _ := newTestManager(t)
	ctx := context.Background()

	_, err := st.UpsertConversation(ctx, "signal", "Before Snapshot")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	snap, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the DB, then restore.
	_, err = st.UpsertConversation(ctx, "signal", "After Snapshot")
	if err != nil {
		t.Fatalf("upsert after: %v", err)
	}

	if _, err := m.Restore(ctx, snap.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Open the restored DB. It must open cleanly and pass integrity_check.
	restored, err := store.OpenReadOnly(m.dbPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restored.Close()
	var result string
	if err := restored.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		t.Fatalf("integrity_check on restored db: %v", err)
	}
	if result != "ok" {
		t.Fatalf("restored db integrity_check = %q, want ok", result)
	}
}

func TestResolveDirDefaultsToDataDirBackups(t *testing.T) {
	cfg := config.Config{DataDir: "/tmp/msgbrowse-data"}
	got := ResolveDir(cfg)
	want := "/tmp/msgbrowse-data/backups"
	if got != want {
		t.Fatalf("ResolveDir default = %q, want %q", got, want)
	}
}

func TestResolveDirUsesConfiguredDir(t *testing.T) {
	cfg := config.Config{
		DataDir: "/tmp/msgbrowse-data",
		Backups: config.BackupsConfig{Dir: "/var/backups/msgbrowse"},
	}
	got := ResolveDir(cfg)
	if got != "/var/backups/msgbrowse" {
		t.Fatalf("ResolveDir configured = %q, want /var/backups/msgbrowse", got)
	}
}

// createAt creates a snapshot with a controlled timestamp by temporarily
// overriding the manager's clock.
func createAt(t *testing.T, m *Manager, at time.Time) {
	t.Helper()
	orig := m.now
	m.now = func() time.Time { return at }
	defer func() { m.now = orig }()
	ctx := context.Background()
	if _, err := m.Create(ctx); err != nil {
		t.Fatalf("Create at %s: %v", at, err)
	}
}
