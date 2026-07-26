package ingest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// snapshotsDir is the archive subdirectory holding raw encrypted DB backups
// produced by an external backup job. msgbrowse inventories these for display
// only (ADR-0010 §5: never opens or decrypts). msgbrowse-owned snapshots live
// under a separate configurable backups.dir (ADR-0026).
const snapshotsDir = ".snapshots"

// snapshotNameRe matches a snapshot tar: db-YYYYMMDD-HHMMSS.tar.
var snapshotNameRe = regexp.MustCompile(`^db-(\d{8})-(\d{6})\.tar$`)

// snapshotTimeLayout matches the timestamp embedded in a snapshot filename.
const snapshotTimeLayout = "20060102-150405"

// GFS tier age boundaries, measured from "now". These describe which retention
// tier a snapshot currently falls into. msgbrowse-owned snapshots (created,
// pruned, and restorable from the Backups tab) use these as enforced policy;
// the external .snapshots inventory uses them as a display classification.
// See ADR-0026 (msgbrowse owns its own snapshots).
const (
	dailyMaxAge     = 14 * 24 * time.Hour      // daily backups kept ≤ 14 days
	monthlyMaxAge   = 395 * 24 * time.Hour     // ~13 months
	quarterlyMaxAge = 3 * 365 * 24 * time.Hour // ~3 years
)

// classifyTier returns the GFS retention tier a snapshot of the given age falls
// into: daily, monthly, quarterly, or yearly.
func classifyTier(age time.Duration) string {
	switch {
	case age <= dailyMaxAge:
		return "daily"
	case age <= monthlyMaxAge:
		return "monthly"
	case age <= quarterlyMaxAge:
		return "quarterly"
	default:
		return "yearly"
	}
}

// scanSnapshots inventories archiveRoot/.snapshots, returning one [store.Snapshot]
// per recognizable backup tar. Unrecognized files are ignored. A missing
// .snapshots directory is not an error (returns an empty slice). The tars are
// never opened or decrypted.
func scanSnapshots(archiveRoot string, now time.Time) ([]store.Snapshot, error) {
	dir := filepath.Join(archiveRoot, snapshotsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}

	var snaps []store.Snapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := snapshotNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		taken, perr := time.Parse(snapshotTimeLayout, m[1]+"-"+m[2])
		if perr != nil {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			if errIsGone(ierr) {
				continue
			}
			return nil, fmt.Errorf("stat snapshot %q: %w", e.Name(), ierr)
		}
		snaps = append(snaps, store.Snapshot{
			Filename:  e.Name(),
			TakenAt:   taken,
			SizeBytes: info.Size(),
			Tier:      classifyTier(now.Sub(taken)),
		})
	}
	return snaps, nil
}

// errIsGone reports whether err indicates a file vanished mid-scan (a benign
// race against the upstream backup job).
func errIsGone(err error) bool {
	return os.IsNotExist(err) || err == fs.ErrNotExist
}
