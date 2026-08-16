#!/bin/bash
# Validate the central claim: an atomic block/fs-level COW snapshot of a RUNNING
# Postgres produces a consistent, independently writable branch.
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq btrfs-progs util-linux >/dev/null 2>&1

PG=/usr/lib/postgresql/*/bin
PGBIN=$(echo $PG)

echo "### 1. Build a COW pool on a loop device (host fs contributes one file)"
truncate -s 3G /pool.img
LOOP=$(losetup -f --show /pool.img)
mkfs.btrfs -q -f "$LOOP"
mkdir -p /mnt/pool && mount "$LOOP" /mnt/pool
btrfs subvolume create /mnt/pool/main >/dev/null
mkdir -p /mnt/pool/main/pgdata
chown -R postgres:postgres /mnt/pool
echo "pool ready on $LOOP"
echo

echo "### 2. initdb + start the 'main' branch"
su postgres -c "$PGBIN/initdb -D /mnt/pool/main/pgdata" >/dev/null 2>&1
su postgres -c "$PGBIN/pg_ctl -D /mnt/pool/main/pgdata -o '-p 5433' -l /tmp/main.log -w start" >/dev/null
su postgres -c "psql -p 5433 -qc 'create table t(id serial primary key, v text)'"
su postgres -c "psql -p 5433 -qc \"insert into t(v) select 'row-'||g from generate_series(1,300000) g\""
echo -n "main rows before snapshot: "
su postgres -c "psql -p 5433 -tAc 'select count(*) from t'"
echo

echo "### 3. Start continuous write load, then snapshot WHILE IT RUNS"
su postgres -c "psql -p 5433 -qc \"insert into t(v) select 'load-'||g from generate_series(1,400000) g\"" &
LOADPID=$!
sleep 0.4
T0=$(date +%s%N)
btrfs subvolume snapshot /mnt/pool/main /mnt/pool/agent-a >/dev/null
T1=$(date +%s%N)
echo "snapshot wall time: $(( (T1 - T0) / 1000000 )) ms"
wait $LOADPID 2>/dev/null || true
echo -n "main rows after load:  "
su postgres -c "psql -p 5433 -tAc 'select count(*) from t'"
echo

echo "### 4. Pool usage — is it actually sharing blocks?"
btrfs filesystem df /mnt/pool | grep -E "^Data"
echo -n "apparent size of the two subvolumes: "
du -sh --apparent-size /mnt/pool/main /mnt/pool/agent-a 2>/dev/null | tr '\n' ' '; echo
echo -n "real bytes consumed in the pool file: "
du -h --block-size=1M /pool.img | cut -f1
echo

echo "### 5. Boot a SECOND Postgres on the snapshot"
rm -f /mnt/pool/agent-a/pgdata/postmaster.pid   # stale lock from the live parent
chown -R postgres:postgres /mnt/pool/agent-a
if su postgres -c "$PGBIN/pg_ctl -D /mnt/pool/agent-a/pgdata -o '-p 5434' -l /tmp/branch.log -w start" >/dev/null; then
  echo "branch started"
else
  echo "BRANCH FAILED TO START"; cat /tmp/branch.log; exit 1
fi
echo "--- recovery lines from the branch's log ---"
grep -iE "recovery|redo|consistent|starting|ready" /tmp/branch.log | head -8
echo

echo "### 6. Is the branch consistent, and independent?"
echo -n "branch rows:           "
su postgres -c "psql -p 5434 -tAc 'select count(*) from t'"
su postgres -c "psql -p 5434 -qc \"insert into t(v) values ('written-on-branch-only')\""
echo -n "branch rows after write: "
su postgres -c "psql -p 5434 -tAc 'select count(*) from t'"
echo -n "main sees branch write?  "
su postgres -c "psql -p 5433 -tAc \"select count(*) from t where v='written-on-branch-only'\""
echo -n "branch integrity check:  "
su postgres -c "psql -p 5434 -tAc 'select count(*) from t where v is null'" && echo "(0 nulls = clean)"
echo

echo "### 7. Branch-of-a-branch (depth 2)"
btrfs subvolume snapshot /mnt/pool/agent-a /mnt/pool/hypothesis-1 >/dev/null
rm -f /mnt/pool/hypothesis-1/pgdata/postmaster.pid
chown -R postgres:postgres /mnt/pool/hypothesis-1
su postgres -c "$PGBIN/pg_ctl -D /mnt/pool/hypothesis-1/pgdata -o '-p 5435' -l /tmp/h1.log -w start" >/dev/null \
  && echo "depth-2 branch started" || { echo "DEPTH-2 FAILED"; cat /tmp/h1.log; }
echo -n "depth-2 rows (should include branch-only write): "
su postgres -c "psql -p 5435 -tAc 'select count(*) from t'"
echo
echo "### ALL CHECKS COMPLETE"
