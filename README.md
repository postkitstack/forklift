# forklift

Copy-on-write branching for Postgres.

```
$ forklift create agent-a --from main
Forking "agent-a" from "main"...

  branch    agent-a
  endpoint  postgres://postgres:forklift@localhost:15501/postgres?sslmode=disable
```

`main` keeps serving throughout. The clone boots into ordinary crash recovery
and comes up transactionally consistent at the snapshot instant. Branches of
branches work. Postgres itself is unmodified — these are stock
`postgres:{version}` images that have no idea they are running on a clone.

> **Experimental.** This is a research project exploring whether database
> branching belongs in PostKit, deliberately kept separate from the PostKit
> codebase. Nothing here is stable.

## Why this exists

Agent workflows want to fork a database, mutate it speculatively, evaluate, and
throw it away — dozens of times an hour, from forks of forks. Copying data per
fork makes that impossible above a few gigabytes.

"Fork the database" can be implemented at exactly three depths of the stack,
and choosing the depth is the whole architectural decision:

| Layer | Unit copied | Fork cost | Postgres modified? |
|---|---|---|---|
| Logical | rows and DDL | O(data) | no |
| **Block** | **8 KiB pages as device blocks** | **O(1)** | **no** |
| Page | page versions keyed by LSN | O(1) at any LSN | **yes** |

forklift works at the block layer. The page layer — Neon's design — is more
powerful, but the extensible `smgr` API that would let an extension implement
it has never landed in core Postgres. It has been on pgsql-hackers since 2021,
pushed largely by Neon's own engineers, and PG18 shipped without it. Building
there means maintaining a patched Postgres, which gives up the one property
worth protecting: that you can point this at any Postgres.

## Why block-level cloning of a live database is safe

Postgres already survives something strictly worse than being snapshotted:
losing power mid-write. An atomic block snapshot is indistinguishable from a
power cut to the instance that boots on it.

1. `pg_control` holds the last checkpoint's redo LSN.
2. The clone finds no `backup_label`, so it treats the directory as crashed.
3. It replays WAL forward, redoing committed transactions and discarding
   uncommitted ones.
4. Torn 8 KiB pages are repaired from the full-page images `full_page_writes`
   put in the WAL.

**The one hard requirement** is that the snapshot is atomic across the entire
data directory including `pg_wal`. Split those across volumes and snapshot them
separately and you capture two different instants — the bug that passes tests
and corrupts under load.

This is also why `cp -r` of a running PGDATA is *not* a valid clone: `cp` walks
the tree over seconds, so the copy corresponds to no instant that ever existed,
and crash recovery cannot repair that.

## Measured

Snapshotting a live Postgres under a 300k-row insert, then booting a second
Postgres on the clone:

```
main rows after load : 500000     ← parent unaffected, kept serving
agent-a rows         : 200000     ← exactly the snapshot instant
main sees branch write?     0     ← fully independent
hypothesis-1 (depth 2)  200001    ← branch of a branch, inherits the branch-only write
Pool: 1.9% used (192.1 MB of 10.0 GB)   ← three branches, ~180 MB each on paper
```

`agent-a` showing exactly 200000 is the informative number: the snapshot caught
the 300k insert uncommitted and recovery rolled it back. The branch is
transactionally consistent, not merely a set of files that copied cleanly.

## Storage mechanisms

The backend is chosen at runtime, because the preferred mechanism is not
universally available. Probing a plain Linux Docker host found dm-thin absent —
only `multipath, striped, linear, error` device-mapper targets, with an empty
`/lib/modules` — plus no nbd and no reflink, since the filesystem was overlayfs.

`doctor` needs no root, but some probes do — it says so rather than guessing:

```
$ forklift doctor
MECHANISM     STATUS                                   DETAIL
dm-thin       unknown — re-run with sudo to determine  requires root to query dm targets
btrfs         available                                validated: 210ms live snapshot, clean recovery, depth-2 verified
nbd (qcow2)   unavailable                              absent (/dev/nbd0 missing)
reflink       unavailable                              filesystem does not support reflink
loop devices  unknown — re-run with sudo to determine  required by every pool-in-a-file mechanism
docker        available                                context default, rootless no, id a64d6a4e-...

Best available mechanism: btrfs

Note: probes marked "unknown" could not run without root; re-run with sudo to determine them.
```

`unknown` is deliberately distinct from `unavailable`. Querying dm targets needs
`/dev/mapper/control`, and probing loop devices needs to attach one — neither is
possible unprivileged. Reporting those as "unavailable" would tell you a
mechanism does not work on your machine when it does. Under `sudo` they resolve:

```
dm-thin       unavailable  absent; dm targets present: multipath, striped, linear, error
loop devices  available    required by every pool-in-a-file mechanism
```

| Mechanism | Status | Notes |
|---|---|---|
| **dm-thin** | preferred, not yet implemented | Per-device btree, so read cost is flat at any branch depth. The kernel docs are explicit that the older chained design was O(depth) and this exists to remove that. |
| **btrfs** | implemented, validated | Subvolume snapshots. Depth cost unmeasured. |
| qcow2 + nbd | not implemented | Backing chains are O(depth), and qemu sits in the read path. |
| ZFS | not implemented | Same verbs, for hosts that already run a pool. |
| Vendor API | not implemented | Neon and similar already do this better than we can. |

Everything lives behind `storage.Provider`, and `Conformance` in
`internal/storage/conformance_test.go` is the suite each must pass.

The pool lives in a single loopback-mounted file, which is the property that
makes this portable: the host filesystem contributes one file and never has to
support snapshots or reflink itself. That is also the theory for why this should
work on macOS, where a ZFS-on-the-host approach cannot — untested, see below.

## Usage

Requires Linux, root (loop devices, `mount`, btrfs subvolumes), `btrfs-progs`,
and Docker.

`make install` deliberately targets `/usr/local/bin` rather than `GOBIN`.
`go install` puts the binary in `~/go/bin`, which is **not** on sudo's
`secure_path`, so `sudo forklift ...` would fail with `command not found` — and
every pool command needs root. Override with `make install PREFIX=/somewhere`.
If you do run it from an unusual location, forklift prints the exact
`sudo /abs/path/forklift ...` line to re-run.

```bash
make build                              # -> ./bin/forklift
make install                            # -> /usr/local/bin/forklift

sudo forklift doctor                    # what can this machine do?
sudo ./bin/forklift init                # create the pool
sudo ./bin/forklift create main         # empty branch, runs initdb
sudo ./bin/forklift create agent-a --from main
sudo ./bin/forklift list
sudo ./bin/forklift inspect agent-a
sudo ./bin/forklift stop agent-a        # keeps the data, frees the compute
sudo ./bin/forklift start agent-a       # same data, back up
sudo ./bin/forklift delete agent-a
```

Deleting a branch that others were forked from is refused, by name:

```
$ sudo forklift delete agent-a
error: branch agent-a still has dependent branches: hypothesis-1
```

## Layout

```
cmd/forklift/            entry point
internal/
  branch/                domain model — what a branch is
  storage/               Provider interface, btrfs backend, capability detection
  compute/               Docker: stock Postgres on a branch's data directory
  metadata/              branch registry (JSON; Postgres impl belongs here too)
  manager/               lifecycle orchestration and ordering constraints
scripts/
  probe-cow.sh           report a machine's COW capabilities without building
  cow-test.sh            standalone end-to-end proof, no Go required
```

The registry deliberately lives outside the branchable data. If branch records
lived in the database being forked, forking it would fork the registry and every
child would believe it was the authoritative source of truth about all branches.

## Testing

```bash
make test               # unit tests, no root required
make test-integration   # conformance suite against btrfs — run WITHOUT sudo
```

`make test-integration` elevates itself. Do not prefix it with `sudo`: sudo
replaces `PATH` with its `secure_path`, which excludes the Go toolchain, so
`sudo make` fails with `go: not found` before any test runs. The target detects
that and tells you to drop the sudo. If you are already root (a CI container,
say) it runs `go test` directly instead of nesting sudo.

## Known gaps

- **macOS is untested.** The whole portability argument rests on the pool living
  inside the Linux VM, and that has not been verified on Docker Desktop. Run
  `scripts/probe-cow.sh` on a Mac; it is the single most useful open question.
- **dm-thin backend is not written.** It is the preferred mechanism on paper and
  was unavailable on every machine tested so far.
- **Safe Mode is not implemented.** A cloned branch faithfully carries every
  webhook trigger, FDW, `pg_cron` job and stored credential from its source. The
  agent's writes stay in its branch; its *effects* do not. The first attempt used
  an `--internal` Docker network, which does isolate egress — and also silently
  drops published ports, so the branch became unreachable. Egress control needs a
  mechanism that distinguishes inbound from outbound; until then Safe Mode has to
  be enforced inside Postgres.
- **No diff or merge.** Schema diff is tractable. Data merge is not, and probably
  should never be automatic: a three-way comparison sees values, but an agent's
  write is a function of the rows it *read*, and that dependency is invisible at
  the storage layer.
- **Sequences collide across siblings.** Two branches forked from one parent both
  allocate the same sequence values. The fix belongs at fork time — give each
  child a disjoint range — not at merge time.
- **No connection router.** Ports are allocated per branch from 15500–15600, so a
  branch's connection string changes across a stop/start.
