package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// PrepareClone applies the fixups a cloned PGDATA needs before Postgres will
// start on it. Every block-level provider needs these, which is why they live
// here rather than in any one backend.
//
// These were found by running the thing, not by reading documentation:
//
//   - postmaster.pid is faithfully snapshotted and names a PID that is still
//     alive in the parent. Postgres refuses to start rather than risk two
//     postmasters on one data directory — correct behaviour, but it means the
//     fork owns removing it.
//   - the clone is owned by whoever owned the parent's files, which is not
//     necessarily the uid the new container runs as.
//
// Deliberately not removed: postmaster.opts (harmless), and nothing under
// pg_wal — the WAL is exactly what makes the clone recoverable.
func PrepareClone(dataDir string, uid, gid int) error {
	if _, err := os.Stat(dataDir); err != nil {
		return fmt.Errorf("clone has no data directory at %s: %w", dataDir, err)
	}

	// 1. The parent's lock file.
	pidFile := filepath.Join(dataDir, "postmaster.pid")
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove postmaster.pid: %w", err)
	}

	// 2. Ownership, recursively — Postgres checks the data directory's owner
	// and permissions at startup and refuses if they are wrong.
	if err := chownTree(dataDir, uid, gid); err != nil {
		return fmt.Errorf("chown clone: %w", err)
	}

	// 3. Postgres requires 0700 or 0750 on PGDATA.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return fmt.Errorf("chmod clone: %w", err)
	}
	return nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Lchown so symlinks are retargeted rather than followed.
		return os.Lchown(path, uid, gid)
	})
}
