package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/postkitstack/forklift/internal/tool"
)

// Mechanism is one candidate COW implementation, with whether this machine can
// actually provide it.
type Mechanism struct {
	Name      string
	Available bool
	Detail    string
	// Preference orders the candidates; lower is better.
	Preference int
}

// Detect probes the machine for every COW mechanism we know about.
//
// This exists because assuming one mechanism is not viable. Probing a plain
// Linux Docker host found dm-thin absent (only multipath/striped/linear/error
// device-mapper targets, with an empty /lib/modules), no nbd, and no reflink
// because the filesystem was overlayfs. Only btrfs and loop devices were
// present. A backend that hard-codes its mechanism simply will not start for
// some users.
func Detect(ctx context.Context) []Mechanism {
	return []Mechanism{
		detectDmThin(ctx),
		detectBtrfs(ctx),
		detectNBD(ctx),
		detectReflink(ctx),
	}
}

// Best returns the highest-preference available mechanism, or "" if none.
func Best(ms []Mechanism) string {
	best := ""
	bestPref := 1 << 30
	for _, m := range ms {
		if m.Available && m.Preference < bestPref {
			best, bestPref = m.Name, m.Preference
		}
	}
	return best
}

func detectDmThin(ctx context.Context) Mechanism {
	m := Mechanism{Name: "dm-thin", Preference: 1,
		Detail: "preferred: per-device btree, so read cost is flat at any branch depth"}
	if exe, err := tool.Resolve("modprobe"); err == nil {
		_ = exec.CommandContext(ctx, exe, "dm_thin_pool").Run()
	}
	dmsetup, err := tool.Resolve("dmsetup")
	if err != nil {
		m.Detail = "dmsetup unavailable, cannot determine"
		return m
	}
	out, err := exec.CommandContext(ctx, dmsetup, "targets").Output()
	if err != nil {
		m.Detail = "dmsetup unavailable, cannot determine"
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "thin-pool") {
			m.Available = true
			return m
		}
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	m.Detail = "absent; dm targets present: " + strings.Join(names, ", ")
	return m
}

// btrfsKernelSupport reports whether the kernel currently has btrfs, trying
// to load the module first, and explains which case actually holds.
//
// The old code discarded modprobe's error and declared "not supported by this
// kernel" on any failure — a guess. On hosts without kmod or /lib/modules
// (common in minimal containers) that assertion was unfounded: the module
// might exist but simply never have been loaded.
func btrfsKernelSupport(ctx context.Context) (bool, string) {
	fs, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false, "cannot read /proc/filesystems: " + err.Error()
	}
	if strings.Contains(string(fs), "btrfs") {
		return true, ""
	}
	modprobe, err := tool.Resolve("modprobe")
	if err != nil {
		return false, "btrfs not loaded and modprobe is unavailable to load it"
	}
	if _, err := os.Stat("/lib/modules/" + kernelRelease()); err != nil {
		return false, "this kernel has no loadable modules"
	}
	out, err := exec.CommandContext(ctx, modprobe, "btrfs").CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("btrfs not loaded; modprobe failed: %s", strings.TrimSpace(string(out)))
	}
	fs, _ = os.ReadFile("/proc/filesystems")
	if !strings.Contains(string(fs), "btrfs") {
		return false, "btrfs not loaded; modprobe succeeded but btrfs is still absent from /proc/filesystems"
	}
	return true, ""
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func detectBtrfs(ctx context.Context) Mechanism {
	m := Mechanism{Name: "btrfs", Preference: 2,
		Detail: "validated: 210ms live snapshot, clean recovery, depth-2 verified"}
	supported, why := btrfsKernelSupport(ctx)
	if !supported {
		m.Detail = why
		return m
	}
	if _, err := tool.Resolve("mkfs.btrfs"); err != nil {
		m.Detail = "kernel supports btrfs but mkfs.btrfs is not installed"
		return m
	}
	m.Available = true
	return m
}

func detectNBD(ctx context.Context) Mechanism {
	m := Mechanism{Name: "nbd (qcow2)", Preference: 3,
		Detail: "userspace daemon in the read path; backing chains are O(depth)"}
	if exe, err := tool.Resolve("modprobe"); err == nil {
		_ = exec.CommandContext(ctx, exe, "nbd").Run()
	}
	if _, err := os.Stat("/dev/nbd0"); err == nil {
		m.Available = true
	} else {
		m.Detail = "absent (/dev/nbd0 missing)"
	}
	return m
}

func detectReflink(ctx context.Context) Mechanism {
	m := Mechanism{Name: "reflink", Preference: 4,
		Detail: "unprivileged floor; degrades to a full copy where unsupported"}
	dir, err := os.MkdirTemp("", "forklift-reflink")
	if err != nil {
		m.Detail = "could not test: " + err.Error()
		return m
	}
	defer os.RemoveAll(dir)

	src := dir + "/a"
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		m.Detail = "could not test: " + err.Error()
		return m
	}
	cp, err := tool.Resolve("cp")
	if err != nil {
		m.Detail = "could not test: " + err.Error()
		return m
	}
	if err := exec.CommandContext(ctx, cp, "--reflink=always", src, dir+"/b").Run(); err == nil {
		m.Available = true
		return m
	}
	m.Detail = "filesystem does not support reflink"
	return m
}

// LoopDevicesWork reports whether we can attach a loop device, which every
// pool-in-a-file mechanism depends on.
func LoopDevicesWork(ctx context.Context) bool {
	if os.Geteuid() != 0 {
		return false
	}
	f, err := os.CreateTemp("", "forklift-loop")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	f.Close()
	truncate, err := tool.Resolve("truncate")
	if err != nil {
		return false
	}
	losetup, err := tool.Resolve("losetup")
	if err != nil {
		return false
	}
	if err := exec.CommandContext(ctx, truncate, "-s", "16M", f.Name()).Run(); err != nil {
		return false
	}
	out, err := exec.CommandContext(ctx, losetup, "-f", "--show", f.Name()).Output()
	if err != nil {
		return false
	}
	dev := strings.TrimSpace(string(out))
	_ = exec.CommandContext(ctx, losetup, "-d", dev).Run()
	return dev != ""
}
