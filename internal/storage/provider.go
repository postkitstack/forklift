// Package storage defines the copy-on-write backends a branch can live on.
//
// The interface exists because probing real machines showed the preferred
// mechanism is not universally available: a plain Linux Docker host exposed
// only multipath/striped/linear/error device-mapper targets, no nbd, and no
// reflink (overlayfs). Assuming any single mechanism means not starting at all
// for some users, so the backend is chosen at runtime by Detect().
package storage

import (
	"context"

	"github.com/postkitstack/forklift/internal/branch"
)

// Capabilities describes what a provider can actually do, so the manager can
// refuse an operation with a useful message instead of failing halfway.
type Capabilities struct {
	Name string

	// InstantFork is false for providers that copy data (the reflink fallback
	// on a non-reflink filesystem, or a logical dump).
	InstantFork bool

	// LiveParent reports whether the parent can keep serving during a fork.
	// False for anything that must stop the source first.
	LiveParent bool

	// FlatDepth reports whether read cost is independent of how deep the
	// branch chain is. dm-thin's per-device btree gives this; qcow2 backing
	// chains and classic LVM do not.
	FlatDepth bool

	// NeedsPrivileged reports whether the provider requires CAP_SYS_ADMIN
	// (loop devices, /dev/mapper, mount).
	NeedsPrivileged bool

	// HistoricalFork reports whether the provider can fork at a past point
	// without replaying WAL itself.
	HistoricalFork bool
}

// ForkOptions carries per-fork parameters.
type ForkOptions struct {
	// Name of the new branch.
	Name string
	// Checkpoint asks the caller to CHECKPOINT the parent first. It is not
	// required for correctness — an atomic snapshot is crash-consistent
	// regardless — but it shortens the clone's WAL replay on startup.
	Checkpoint bool
}

// Provider is the contract every storage backend implements.
//
// Fork must be atomic with respect to the parent's data directory: the entire
// PGDATA including pg_wal has to be captured at one instant. A non-atomic copy
// (cp -r of a live directory) corresponds to no point in time that ever
// existed, and Postgres crash recovery cannot repair that.
type Provider interface {
	// Capabilities reports what this provider can do.
	Capabilities() Capabilities

	// Available reports whether this provider can run on this machine, with a
	// human-readable reason when it cannot.
	Available(ctx context.Context) (bool, string)

	// Init prepares the pool. Idempotent.
	Init(ctx context.Context) error

	// CreateRoot makes an empty branch to initdb into.
	CreateRoot(ctx context.Context, name string) (branch.StorageRef, error)

	// Fork snapshots parent and returns storage for the child.
	//
	// Implementations must apply the three fixups a cloned PGDATA needs before
	// Postgres will start on it, all discovered by actually doing this:
	//   1. remove postmaster.pid — it names the parent's live PID
	//   2. regenerate the filesystem UUID where the mechanism inherits it
	//   3. chown the clone to the database user
	// PrepareClone is the shared helper for (1) and (3).
	Fork(ctx context.Context, parent branch.StorageRef, opts ForkOptions) (branch.StorageRef, *branch.ForkPoint, error)

	// Delete removes a branch's storage. It must refuse when other branches
	// still depend on it rather than orphaning or corrupting them.
	Delete(ctx context.Context, ref branch.StorageRef) error

	// Children lists storage handles that were forked from ref and still exist.
	Children(ctx context.Context, ref branch.StorageRef) ([]string, error)

	// Usage reports pool consumption, so callers can refuse new forks before
	// the pool fills. A full pool does not degrade — writes fail or the pool
	// goes read-only, and a Postgres that cannot write WAL stops.
	Usage(ctx context.Context) (Usage, error)
}

// Teardown is implemented by providers that hold OS-level resources — a mount,
// a loop device — which outlive the process unless released. Providers backed
// by a remote API do not need it, so it is separate from Provider.
type Teardown interface {
	Close(ctx context.Context) error
}

// Usage is pool-level space accounting.
type Usage struct {
	TotalBytes uint64
	UsedBytes  uint64
}

// Percent returns used capacity as a percentage, or 0 when unknown.
func (u Usage) Percent() float64 {
	if u.TotalBytes == 0 {
		return 0
	}
	return float64(u.UsedBytes) / float64(u.TotalBytes) * 100
}

// ErrHasChildren is returned by Delete when dependants exist.
type ErrHasChildren struct {
	Handle   string
	Children []string
}

func (e *ErrHasChildren) Error() string {
	return "branch " + e.Handle + " still has dependent branches: " +
		join(e.Children, ", ")
}

func join(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}
