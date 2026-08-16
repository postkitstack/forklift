#!/usr/bin/env bash
# PostKit branching — COW capability probe.
#
# Run this on any machine you want to support (especially macOS + Docker Desktop,
# which is the case we could not test in CI). It reports which copy-on-write
# mechanisms the Docker VM's kernel can actually provide.
#
#   ./probe-cow.sh
#
# Requires: Docker running. Uses one throwaway privileged container.

set -uo pipefail

echo "host      : $(uname -s) $(uname -m)"
echo "docker    : $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unavailable)"
echo "vm kernel : $(docker run --rm alpine uname -r 2>/dev/null)"
echo "─────────────────────────────────────────────────────────────"

docker run --rm -i --privileged -v /lib/modules:/lib/modules:ro debian:trixie-slim bash -s <<'PROBE'
export DEBIAN_FRONTEND=noninteractive
# Real tools, so an "unavailable" means the kernel lacks it, not the image.
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq kmod dmsetup btrfs-progs util-linux coreutils >/dev/null 2>&1

r() { printf "%-22s %s\n" "$1" "$2"; }

# --- device-mapper thin provisioning: the preferred mechanism ---
modprobe dm_thin_pool 2>/dev/null
if dmsetup targets 2>/dev/null | grep -q "^thin-pool"; then
  r "dm-thin" "AVAILABLE  <- preferred: flat read cost at any depth"
else
  avail=$(dmsetup targets 2>/dev/null | awk '{print $1}' | paste -sd, -)
  r "dm-thin" "unavailable (dm targets present: ${avail:-none})"
fi

# --- btrfs subvolumes: validated fallback ---
modprobe btrfs 2>/dev/null
if grep -qw btrfs /proc/filesystems; then
  r "btrfs" "AVAILABLE  <- fallback: subvolume snapshots, validated working"
else
  r "btrfs" "unavailable"
fi

# --- qcow2 over nbd ---
modprobe nbd 2>/dev/null
if [ -e /dev/nbd0 ]; then
  r "nbd (qcow2)" "available  (userspace daemon in the read path)"
else
  r "nbd (qcow2)" "unavailable"
fi

# --- loop devices: required by every pool-in-a-file approach ---
truncate -s 16M /t.img
if losetup -f --show /t.img >/dev/null 2>&1; then
  r "loop devices" "OK (required by all of the above)"
else
  r "loop devices" "FAILED - blocks every pool-in-a-file backend"
fi

# --- reflink: the unprivileged floor ---
mkdir -p /rl && cd /rl && echo x > a
if cp --reflink=always a b 2>/dev/null; then
  r "reflink (VM fs)" "OK on $(stat -f -c %T .)"
else
  r "reflink (VM fs)" "unsupported on $(stat -f -c %T .)"
fi
PROBE

echo "─────────────────────────────────────────────────────────────"
echo "Decision: dm-thin > btrfs > nbd. If none, the reflink cold-clone"
echo "backend is the floor, and it needs no privileges at all."
