package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennwc/btrfs"
	"github.com/postkitstack/forklift/internal/branch"
)

// Subvolume operations go through the pure-Go github.com/dennwc/btrfs library
// (kernel ioctls underneath) rather than shelling out to the btrfs CLI: the
// CLI's human-readable output is not a stable interface, and parsing it broke
// across btrfs-progs versions. Only mkfs.btrfs, mount/umount/losetup and
// modprobe still run as external commands — the kernel exposes no ioctl for
// formatting or mounting.

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
	if _, err := exec.LookPath("mkfs.btrfs"); err != nil {
		return false, "btrfs-progs not installed (mkfs.btrfs is required to format the pool)"
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
		f, err := os.Create(b.imagePath())
		if err != nil {
			return fmt.Errorf("create pool image: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("create pool image: %w", err)
		}
		if err := os.Truncate(b.imagePath(), int64(b.PoolSizeGB)<<30); err != nil {
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
	if err := btrfs.CreateSubVolume(path); err != nil {
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
	if err := btrfs.SnapshotSubVolume(src, dst, false); err != nil {
		return branch.StorageRef{}, nil, fmt.Errorf("snapshot: %w", err)
	}

	data := filepath.Join(dst, "pgdata")
	if err := PrepareClone(data, b.PGUID, b.PGGID); err != nil {
		// Roll back so a failed fork does not leave storage behind.
		_ = btrfs.DeleteSubVolume(dst)
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
	if err := btrfs.DeleteSubVolume(b.subvol(ref.Handle)); err != nil {
		return fmt.Errorf("delete subvolume: %w", err)
	}
	return nil
}

// Children reports subvolumes forked from ref.
//
// btrfs tracks this through parent UUIDs rather than names, so we resolve the
// parent's UUID and match it against every subvolume's ParentUUID in a single
// ioctl-driven walk of the pool mount (no CLI text parsing).
func (b *Btrfs) Children(ctx context.Context, ref branch.StorageRef) ([]string, error) {
	if err := branch.ValidateName(ref.Handle); err != nil {
		return nil, fmt.Errorf("invalid storage handle: %w", err)
	}
	fs, err := btrfs.Open(b.mountPath(), true)
	if err != nil {
		return nil, fmt.Errorf("open pool mount: %w", err)
	}
	defer fs.Close()
	uuid, err := b.subvolUUID(fs, ref.Handle)
	if err != nil {
		return nil, err
	}
	if uuid == (btrfs.UUID{}) {
		return nil, nil
	}
	subs, err := fs.ListSubvolumes(nil)
	if err != nil {
		return nil, fmt.Errorf("list subvolumes: %w", err)
	}
	var kids []string
	for _, sub := range subs {
		if sub.ParentUUID == uuid && sub.Path != ref.Handle {
			kids = append(kids, sub.Path)
		}
	}
	return kids, nil
}

func (b *Btrfs) subvolUUID(fs *btrfs.FS, name string) (btrfs.UUID, error) {
	info, err := fs.SubvolumeByPath(name)
	if err != nil {
		return btrfs.UUID{}, fmt.Errorf("resolve subvolume %q: %w", name, err)
	}
	return info.UUID, nil
}

func (b *Btrfs) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	fs, err := btrfs.Open(b.mountPath(), true)
	if err != nil {
		return u, fmt.Errorf("open pool mount: %w", err)
	}
	defer fs.Close()
	usage, err := fs.Usage()
	if err != nil {
		return u, fmt.Errorf("query pool usage: %w", err)
	}
	u.UsedBytes = usage.TotalUsed
	u.TotalBytes = usage.Total
	return u, nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
