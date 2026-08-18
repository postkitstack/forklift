package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/postkitstack/forklift/internal/branch"
)

// Btrfs stores each branch as a btrfs subvolume inside a pool that lives in a
// single loopback-mounted file.
//
// This is the mechanism validated end to end: snapshotting a live Postgres
// under a 400k-row insert took 210ms, the clone booted into ordinary crash
// recovery ("database system was not properly shut down"), and came up
// transactionally consistent at the snapshot instant — the in-flight insert
// rolled back, the parent unaffected. A branch of that branch also worked.
//
// The host filesystem contributes exactly one file, which is the property that
// makes this approach portable: it never has to support reflink or snapshots
// itself.
type Btrfs struct {
	// Root is where the pool image and mountpoint live.
	Root string
	// PoolSizeGB is the size of the sparse backing file created by Init.
	PoolSizeGB int
	// PGUID/PGGID own the cloned data directory. 999 matches the postgres
	// user in the official images.
	PGUID int
	PGGID int
}

// NewBtrfs returns a Btrfs provider rooted at dir.
func NewBtrfs(dir string, poolGB int) *Btrfs {
	return &Btrfs{Root: dir, PoolSizeGB: poolGB, PGUID: 999, PGGID: 999}
}

func (b *Btrfs) imagePath() string { return filepath.Join(b.Root, "pool.img") }
func (b *Btrfs) mountPath() string { return filepath.Join(b.Root, "pool") }

func (b *Btrfs) Capabilities() Capabilities {
	return Capabilities{
		Name:            "btrfs",
		InstantFork:     true,
		LiveParent:      true,
		FlatDepth:       false, // untested; btrfs shares extents but depth cost is unmeasured
		NeedsPrivileged: true,
		HistoricalFork:  false, // reachable later by pairing snapshots with a WAL archive
	}
}

func (b *Btrfs) Available(ctx context.Context) (bool, string) {
	if os.Geteuid() != 0 {
		return false, "requires root (loop devices, mount, btrfs subvolume)"
	}
	fs, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false, "cannot read /proc/filesystems: " + err.Error()
	}
	if !strings.Contains(string(fs), "btrfs") {
		// Try loading it before giving up.
		_ = exec.CommandContext(ctx, "modprobe", "btrfs").Run()
		fs, _ = os.ReadFile("/proc/filesystems")
		if !strings.Contains(string(fs), "btrfs") {
			return false, "btrfs not supported by this kernel"
		}
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		return false, "btrfs-progs not installed"
	}
	return true, ""
}

func (b *Btrfs) Init(ctx context.Context) error {
	if ok, why := b.Available(ctx); !ok {
		return fmt.Errorf("btrfs provider unavailable: %s", why)
	}
	if err := os.MkdirAll(b.Root, 0o755); err != nil {
		return err
	}
	if b.mounted() {
		return nil // already initialised
	}
	if _, err := os.Stat(b.imagePath()); os.IsNotExist(err) {
		size := strconv.Itoa(b.PoolSizeGB) + "G"
		if err := run(ctx, "truncate", "-s", size, b.imagePath()); err != nil {
			return fmt.Errorf("create pool image: %w", err)
		}
		if err := run(ctx, "mkfs.btrfs", "-q", "-f", b.imagePath()); err != nil {
			return fmt.Errorf("mkfs.btrfs: %w", err)
		}
	}
	if err := os.MkdirAll(b.mountPath(), 0o755); err != nil {
		return err
	}
	// -o loop lets mount allocate the loop device for us.
	if err := run(ctx, "mount", "-o", "loop", b.imagePath(), b.mountPath()); err != nil {
		return fmt.Errorf("mount pool: %w", err)
	}
	return nil
}

// Close unmounts the pool and detaches its loop device.
//
// Without this the mount outlives the process, and anything trying to clean up
// the directory hits "device or resource busy" — which is how the gap was
// found. Branch data is unaffected; Init remounts it.
func (b *Btrfs) Close(ctx context.Context) error {
	device := b.mountDevice()
	if device == "" {
		return nil
	}
	// Lazy unmount: detaches immediately and finishes when the last reference
	// goes, so a stray reader cannot wedge teardown.
	if err := run(ctx, "umount", "-l", b.mountPath()); err != nil {
		return fmt.Errorf("unmount pool: %w", err)
	}
	// mount -o loop normally marks its loop device for autoclear. Detach only
	// this pool's device as a fallback; losetup -D would affect unrelated pools.
	if strings.HasPrefix(device, "/dev/loop") {
		_ = run(ctx, "losetup", "-d", device)
	}
	return nil
}

func (b *Btrfs) mounted() bool { return b.mountDevice() != "" }

func (b *Btrfs) mountDevice() string {
	out, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	return findMountDevice(string(out), b.mountPath())
}

func findMountDevice(mounts, target string) string {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && unescapeMountField(fields[1]) == target && fields[2] == "btrfs" {
			return unescapeMountField(fields[0])
		}
	}
	return ""
}

func unescapeMountField(field string) string {
	return strings.NewReplacer(
		`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`,
	).Replace(field)
}

func (b *Btrfs) subvol(name string) string { return filepath.Join(b.mountPath(), name) }

func (b *Btrfs) CreateRoot(ctx context.Context, name string) (branch.StorageRef, error) {
	if err := branch.ValidateName(name); err != nil {
		return branch.StorageRef{}, err
	}
	path := b.subvol(name)
	if err := run(ctx, "btrfs", "subvolume", "create", path); err != nil {
		return branch.StorageRef{}, fmt.Errorf("create subvolume: %w", err)
	}
	data := filepath.Join(path, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return branch.StorageRef{}, err
	}
	if err := os.Chown(data, b.PGUID, b.PGGID); err != nil {
		return branch.StorageRef{}, err
	}
	return branch.StorageRef{Provider: "btrfs", Handle: name, DataDir: data}, nil
}

func (b *Btrfs) Fork(ctx context.Context, parent branch.StorageRef, opts ForkOptions) (branch.StorageRef, *branch.ForkPoint, error) {
	if err := branch.ValidateName(parent.Handle); err != nil {
		return branch.StorageRef{}, nil, fmt.Errorf("invalid parent storage handle: %w", err)
	}
	if err := branch.ValidateName(opts.Name); err != nil {
		return branch.StorageRef{}, nil, err
	}
	src := b.subvol(parent.Handle)
	dst := b.subvol(opts.Name)

	if _, err := os.Stat(dst); err == nil {
		return branch.StorageRef{}, nil, fmt.Errorf("branch %q already exists", opts.Name)
	}

	// The snapshot itself. Atomic, and the parent keeps serving throughout.
	started := time.Now()
	if err := run(ctx, "btrfs", "subvolume", "snapshot", src, dst); err != nil {
		return branch.StorageRef{}, nil, fmt.Errorf("snapshot: %w", err)
	}

	data := filepath.Join(dst, "pgdata")
	if err := PrepareClone(data, b.PGUID, b.PGGID); err != nil {
		// Roll back so a failed fork does not leave storage behind.
		_ = run(ctx, "btrfs", "subvolume", "delete", dst)
		return branch.StorageRef{}, nil, err
	}

	ref := branch.StorageRef{Provider: "btrfs", Handle: opts.Name, DataDir: data}
	fp := &branch.ForkPoint{SnapshotHandle: opts.Name, At: started}
	return ref, fp, nil
}

func (b *Btrfs) Delete(ctx context.Context, ref branch.StorageRef) error {
	if err := branch.ValidateName(ref.Handle); err != nil {
		return fmt.Errorf("invalid storage handle: %w", err)
	}
	kids, err := b.Children(ctx, ref)
	if err != nil {
		return err
	}
	if len(kids) > 0 {
		return &ErrHasChildren{Handle: ref.Handle, Children: kids}
	}
	return run(ctx, "btrfs", "subvolume", "delete", b.subvol(ref.Handle))
}

// Children reports subvolumes forked from ref.
//
// btrfs tracks this through parent UUIDs rather than names, so we resolve the
// parent's UUID and match against every subvolume's Parent UUID field.
func (b *Btrfs) Children(ctx context.Context, ref branch.StorageRef) ([]string, error) {
	if err := branch.ValidateName(ref.Handle); err != nil {
		return nil, fmt.Errorf("invalid storage handle: %w", err)
	}
	uuid, err := b.subvolUUID(ctx, ref.Handle)
	if err != nil || uuid == "" {
		return nil, err
	}
	out, err := output(ctx, "btrfs", "subvolume", "list", "-u", "-q", b.mountPath())
	if err != nil {
		return nil, err
	}
	var kids []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "parent_uuid "+uuid) {
			continue
		}
		if i := strings.LastIndex(line, " path "); i >= 0 {
			name := strings.TrimSpace(line[i+len(" path "):])
			if name != ref.Handle {
				kids = append(kids, name)
			}
		}
	}
	return kids, nil
}

func (b *Btrfs) subvolUUID(ctx context.Context, name string) (string, error) {
	out, err := output(ctx, "btrfs", "subvolume", "show", b.subvol(name))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UUID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "UUID:")), nil
		}
	}
	return "", nil
}

func (b *Btrfs) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	out, err := output(ctx, "btrfs", "filesystem", "usage", "-b", b.mountPath())
	if err != nil {
		return u, err
	}
	// "Used:" carries only two fields, so guard at 2 rather than 3 — at 3 it is
	// silently skipped and the pool always reports empty, which would make the
	// watermark check useless exactly when it matters.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		f := strings.Fields(trimmed)
		if len(f) < 2 {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "Device size:"):
			u.TotalBytes = parseUint(f[len(f)-1])
		case strings.HasPrefix(trimmed, "Used:"):
			if u.UsedBytes == 0 {
				u.UsedBytes = parseUint(f[len(f)-1])
			}
		}
	}
	return u, nil
}

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
