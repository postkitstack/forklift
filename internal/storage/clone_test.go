package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareCloneRemovesPostmasterPID(t *testing.T) {
	// This is the fixup that was found by running a fork, not by reading docs:
	// the snapshot faithfully copies the parent's lock file, which names a PID
	// that is still alive, and Postgres then refuses to start.
	dir := t.TempDir()
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(data, "postmaster.pid")
	if err := os.WriteFile(pid, []byte("1234\n/var/lib/postgresql/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PrepareClone(data, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("PrepareClone: %v", err)
	}
	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Fatalf("postmaster.pid should have been removed, stat err = %v", err)
	}
}

func TestPrepareCloneIsIdempotent(t *testing.T) {
	// Fork retries must not fail just because a previous attempt got partway.
	dir := t.TempDir()
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := PrepareClone(data, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestPrepareCloneKeepsWAL(t *testing.T) {
	// The WAL is precisely what makes a clone recoverable — removing it would
	// turn a working branch into a corrupt one.
	dir := t.TempDir()
	data := filepath.Join(dir, "pgdata")
	wal := filepath.Join(data, "pg_wal")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(wal, "000000010000000000000001")
	if err := os.WriteFile(seg, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareClone(data, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seg); err != nil {
		t.Fatalf("WAL segment must survive PrepareClone: %v", err)
	}
}

func TestPrepareCloneSetsDataDirPermissions(t *testing.T) {
	// Postgres refuses to start unless PGDATA is 0700 or 0750.
	dir := t.TempDir()
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := PrepareClone(data, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("want mode 0700, got %o", perm)
	}
}

func TestPrepareCloneFailsOnMissingDataDir(t *testing.T) {
	err := PrepareClone(filepath.Join(t.TempDir(), "nope"), 0, 0)
	if err == nil {
		t.Fatal("expected an error for a missing data directory")
	}
}

func TestUsagePercent(t *testing.T) {
	cases := []struct {
		name string
		u    Usage
		want float64
	}{
		{"empty pool reports zero", Usage{TotalBytes: 100, UsedBytes: 0}, 0},
		{"half full", Usage{TotalBytes: 200, UsedBytes: 100}, 50},
		{"unknown total does not divide by zero", Usage{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.Percent(); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}
