// Package manager orchestrates the branch lifecycle across storage, compute
// and the registry. It is the only place that knows the ordering constraints
// between the three.
package manager

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/postkitstack/forklift/internal/branch"
	"github.com/postkitstack/forklift/internal/compute"
	"github.com/postkitstack/forklift/internal/metadata"
	"github.com/postkitstack/forklift/internal/storage"

	_ "github.com/lib/pq"
)

// Manager owns branch lifecycle.
type Manager struct {
	Storage storage.Provider
	Compute compute.Provider
	Repo    metadata.Repository

	// PoolWatermark is the pool usage percentage above which new forks are
	// refused. A thin pool that fills does not degrade gracefully — writes
	// error or the pool goes read-only, and a Postgres that cannot write WAL
	// stops. Refusing the fork is always better than killing the database.
	PoolWatermark float64
}

func New(s storage.Provider, c compute.Provider, r metadata.Repository) *Manager {
	return &Manager{Storage: s, Compute: c, Repo: r, PoolWatermark: 85}
}

// Init prepares the storage pool.
func (m *Manager) Init(ctx context.Context) error { return m.Storage.Init(ctx) }

// CreateRoot makes a new empty branch and initdbs into it.
func (m *Manager) CreateRoot(ctx context.Context, name, pgVersion string) (*branch.Branch, error) {
	if _, err := m.Repo.Get(name); err == nil {
		return nil, fmt.Errorf("branch %q already exists", name)
	}
	ref, err := m.Storage.CreateRoot(ctx, name)
	if err != nil {
		return nil, err
	}
	b := &branch.Branch{
		Name: name, Status: branch.StatusCreating,
		Storage: ref, PGVersion: pgVersion, CreatedAt: time.Now(),
	}
	if err := m.Repo.Put(b); err != nil {
		return nil, err
	}
	if err := m.Compute.Initdb(ctx, ref.DataDir, pgVersion); err != nil {
		b.Status = branch.StatusFailed
		_ = m.Repo.Put(b)
		return nil, err
	}
	return b, m.Start(ctx, b)
}

// Fork creates a child branch from parent.
//
// Order matters throughout. The pool check happens before any storage is
// allocated; the CHECKPOINT happens before the snapshot so the clone's replay
// is short; the registry record is written before compute starts so a crash
// mid-fork leaves a row we can clean up rather than orphaned storage.
func (m *Manager) Fork(ctx context.Context, parentName, name string) (*branch.Branch, error) {
	if _, err := m.Repo.Get(name); err == nil {
		return nil, fmt.Errorf("branch %q already exists", name)
	}
	parent, err := m.Repo.Get(parentName)
	if err != nil {
		return nil, err
	}

	if err := m.checkPool(ctx); err != nil {
		return nil, err
	}

	// Shortens the clone's crash recovery. Best effort: an atomic snapshot is
	// crash-consistent whether or not this succeeds.
	if parent.Compute.Port != 0 && m.Compute.Running(ctx, parent.Compute) {
		_ = checkpoint(parent.Endpoint())
	}

	ref, fp, err := m.Storage.Fork(ctx, parent.Storage, storage.ForkOptions{
		Name: name, Checkpoint: true,
	})
	if err != nil {
		return nil, err
	}

	b := &branch.Branch{
		Name: name, Parent: parentName, Status: branch.StatusCreating,
		Storage: ref, Fork: fp, PGVersion: parent.PGVersion, CreatedAt: time.Now(),
	}
	if err := m.Repo.Put(b); err != nil {
		_ = m.Storage.Delete(ctx, ref)
		return nil, err
	}
	return b, m.Start(ctx, b)
}

// Start brings up a branch's compute.
func (m *Manager) Start(ctx context.Context, b *branch.Branch) error {
	ref, err := m.Compute.Start(ctx, b.Storage.DataDir, b.PGVersion)
	if err != nil {
		b.Status = branch.StatusFailed
		_ = m.Repo.Put(b)
		return err
	}
	b.Compute = ref
	b.Status = branch.StatusReady
	return m.Repo.Put(b)
}

// Stop stops a branch's compute without touching its storage.
func (m *Manager) Stop(ctx context.Context, name string) error {
	b, err := m.Repo.Get(name)
	if err != nil {
		return err
	}
	if err := m.Compute.Stop(ctx, b.Compute); err != nil {
		return err
	}
	b.Compute = branch.ComputeRef{}
	b.Status = branch.StatusStopped
	return m.Repo.Put(b)
}

// Delete removes a branch entirely.
//
// Refuses when children exist. A COW child depends on its parent's blocks, so
// destroying the parent either fails or corrupts the children — and either way
// the user deserves to be told which branches are in the way, by name.
func (m *Manager) Delete(ctx context.Context, name string) error {
	b, err := m.Repo.Get(name)
	if err != nil {
		return err
	}
	kids, err := m.Repo.Children(name)
	if err != nil {
		return err
	}
	if len(kids) > 0 {
		names := make([]string, len(kids))
		for i, k := range kids {
			names[i] = k.Name
		}
		return &storage.ErrHasChildren{Handle: name, Children: names}
	}

	b.Status = branch.StatusDeleting
	_ = m.Repo.Put(b)

	if err := m.Compute.Stop(ctx, b.Compute); err != nil {
		return err
	}
	if err := m.Storage.Delete(ctx, b.Storage); err != nil {
		return err
	}
	return m.Repo.Delete(name)
}

func (m *Manager) checkPool(ctx context.Context) error {
	u, err := m.Storage.Usage(ctx)
	if err != nil {
		return nil // usage reporting is advisory; don't block a fork on it
	}
	if pct := u.Percent(); pct >= m.PoolWatermark {
		return fmt.Errorf(
			"storage pool is %.1f%% full (watermark %.0f%%); refusing to fork. "+
				"Delete branches or grow the pool — a pool that fills will take running databases read-only",
			pct, m.PoolWatermark)
	}
	return nil
}

// checkpoint flushes dirty buffers so the clone has less WAL to replay.
func checkpoint(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, "CHECKPOINT")
	return err
}
