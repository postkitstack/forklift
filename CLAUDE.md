# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`forklift` gives Postgres copy-on-write branching: a branch is a COW snapshot of a whole
cluster plus its own compute. Experimental, and deliberately separate from the PostKit
codebase — it exists to test whether branching belongs there.

## Commands

```bash
make build              # -> bin/forklift
make test               # unit tests, no root
make test-integration   # storage conformance suite — run WITHOUT sudo
make install            # -> /usr/local/bin/forklift (on sudo's secure_path)
make doctor             # report this machine's COW capabilities
make vet fmt clean
```

Go lives at `/usr/local/go/bin/go` and is often **not** on the default PATH:
`export PATH=$PATH:/usr/local/go/bin`.

**Never prefix a make target with `sudo`.** Targets that need root elevate
themselves. `sudo` replaces PATH with its `secure_path`, which excludes the Go
toolchain, so `sudo make test-integration` fails with `go: not found` before any
test runs — a failure that has already been misdiagnosed once as "this machine
cannot run the suite".

Running a single test:

```bash
go test ./internal/branch/ -run TestValidateName -v
sudo -E env "PATH=$PATH" go test -count=1 ./internal/storage/ -run Conformance -v
```

Use `-count=1` on anything you are using as evidence: cached `ok` lines have
already been mistaken for a passing rewrite that the tests never executed.

Anything that touches the pool (`init`, `create`, `start`, `delete`, and the integration
tests) needs **root** — loop devices, `mount`, btrfs subvolumes. `list`, `inspect` and
`doctor` do not.

Manual end-to-end check, if you changed storage or compute:

```bash
sudo ./bin/forklift init && sudo ./bin/forklift create main
sudo ./bin/forklift create agent-a --from main   # fork
sudo ./bin/forklift create deep --from agent-a   # fork of a fork
sudo ./bin/forklift delete main                  # must be REFUSED, naming its children
```

`scripts/probe-cow.sh` reports a machine's COW capabilities without building;
`scripts/cow-test.sh` is a standalone end-to-end proof needing no Go.

## Architecture

### The layer thesis

"Fork a database" can be built at three depths, and the choice of depth is the whole
design. Logical (rows/DDL, O(data)); **block** (8 KiB pages as device blocks, O(1),
Postgres unmodified); page (page versions by LSN, O(1) at any LSN, but requires patching
Postgres — the `smgr` hook never landed upstream). forklift works at the block layer, and
that constraint explains most decisions here: branches run **stock** `postgres:{version}`
images that have no idea they are on a clone.

### Three interfaces, one orchestrator

- `storage.Provider` (`internal/storage/provider.go`) — the COW backend. `btrfs.go` is the
  only implementation; dm-thin/ZFS/vendor-API are intended to slot in beside it.
- `compute.Provider` (`internal/compute/docker.go`) — runs Postgres on a branch's data dir.
- `metadata.Repository` (`internal/metadata/repo.go`) — the branch registry (JSON today).

`internal/manager` is the **only** place that knows the ordering constraints between the
three; `internal/cli` just wires flags. `internal/branch` holds the domain types and has no
dependencies on the others.

### Invariants worth knowing before changing anything

**A snapshot must be atomic across the entire PGDATA, including `pg_wal`.** That is what
makes cloning a *running* Postgres safe: the clone finds no `backup_label`, enters ordinary
crash recovery, replays from the last checkpoint, and repairs torn pages from full-page
images. Split PGDATA and WAL across devices and you capture two instants — works in testing,
corrupts under load. `CHECKPOINT` before a fork is an optimisation, never a correctness
requirement.

**A cloned PGDATA needs fixups before Postgres will start.** `storage.PrepareClone` removes
`postmaster.pid` (snapshotted faithfully, naming a PID still alive in the parent), fixes
ownership, and sets 0700. Every block-level provider must call it — hence its living in
`storage`, not in `btrfs.go`. It must never touch `pg_wal`; the WAL is what makes the clone
recoverable.

**`branch.ValidateName` must be called at the storage provider boundary**, not only in the
CLI or manager. Names reach `filepath.Join` against the pool root, so an unvalidated name
escapes the pool. `btrfs.go` re-validates every handle it receives, including ones read back
from the registry.

**The registry must live outside the branchable data.** If branch records lived in the
database being forked, forking it would fork the registry and every child would believe it
was authoritative. This is why `metadata` is a file, not a table.

**Do not put branch containers on a `--internal` Docker network.** It does isolate egress —
and silently drops published ports, leaving a branch that is healthy and unreachable. Until
an inbound/outbound-aware mechanism exists, Safe Mode has to be enforced inside Postgres
(`archive_mode` off, subscriptions disabled, cron neutered, FDW mappings cleared). Ports
publish on `127.0.0.1` via `Docker.BindHost`; never make that `0.0.0.0` implicitly.

**Refuse, don't corrupt.** A COW child depends on its parent's blocks, so deleting a parent
with children returns `storage.ErrHasChildren` naming the blockers. Likewise the pool
watermark (`Manager.PoolWatermark`, 85%) fails the *fork* rather than letting a full pool
take running databases read-only.

**Resolve external binaries through `internal/tool`, never bare `exec.LookPath`.**
`losetup`, `dmsetup` and `mkfs.btrfs` live in `/usr/sbin`, which is off a normal
non-root PATH, so a bare lookup false-negatives for unprivileged callers like
`doctor`. The resolver tries PATH then `/usr/local/sbin`, `/usr/sbin`, `/sbin`.
This is the single most repeated bug in this codebase's history — four separate
instances — and the resolver exists so a new call site cannot reintroduce it.

**Detection is tri-state: available / unavailable / unknown.** A probe that could
not run for lack of privilege must return `unknown`, never `unavailable`, and
`Best()` must never select an unknown mechanism. Telling someone a mechanism is
unavailable when you merely could not look is worse than saying nothing — they
will go and change their machine to fix a problem that does not exist.

**Never operate against a Docker daemon the invoking user cannot see.** Under
`sudo`, `docker` talks to root's daemon; a user running rootless Docker or a
non-default context would get branches created somewhere invisible to them.
When `SUDO_USER` is set, compare daemon IDs and refuse on a confirmed mismatch.
Querying the user's daemon needs
`--preserve-env=DOCKER_HOST,DOCKER_CONTEXT,XDG_RUNTIME_DIR`, or you inspect
root's environment and conclude they match when they do not.

### Adding a storage backend

Implement `storage.Provider`, add detection to `storage.Detect` with a `Preference` rank,
and make `Conformance` in `internal/storage/conformance_test.go` pass. That suite is the
contract — it covers parent isolation, depth-2 forks, and delete-refusal. Implement
`storage.Teardown` if the backend holds a mount or loop device, or test cleanup fails with
"device or resource busy".

Mechanism preference is `dm-thin > btrfs > nbd > reflink`. dm-thin ranks first because its
per-device btree keeps read cost flat at any branch depth, where qcow2 backing chains and
classic LVM are O(depth) — but it was **absent on every machine probed so far**, which is
why backends are selected at runtime rather than assumed.

## Known gaps (see README for detail)

macOS is untested and is the most valuable open question — the whole portability argument
rests on the pool living inside the Linux VM. No dm-thin backend. No Safe Mode. No
diff/merge, and data merge probably should never be automatic: a three-way comparison sees
values, but an agent's write is a function of the rows it *read*, and that dependency is
invisible at the storage layer. Sibling branches collide on sequence values; the fix belongs
at fork time (disjoint ranges), not merge time.
