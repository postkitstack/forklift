package storage

import (
	"context"
	"testing"

	"github.com/postkitstack/forklift/internal/branch"
)

func TestFindMountDevice(t *testing.T) {
	mounts := "/dev/loop3 /var/lib/forklift/pool-old btrfs rw 0 0\n" +
		"/dev/loop7 /var/lib/forklift/pool btrfs rw 0 0\n"
	if got, want := findMountDevice(mounts, "/var/lib/forklift/pool"), "/dev/loop7"; got != want {
		t.Fatalf("findMountDevice: want %q, got %q", want, got)
	}
}

func TestFindMountDeviceHandlesEscapedPath(t *testing.T) {
	mounts := `/dev/loop8 /var/lib/forklift\040test/pool btrfs rw 0 0` + "\n"
	if got, want := findMountDevice(mounts, "/var/lib/forklift test/pool"), "/dev/loop8"; got != want {
		t.Fatalf("findMountDevice: want %q, got %q", want, got)
	}
}

func TestBtrfsRejectsUnsafeHandlesBeforeFilesystemAccess(t *testing.T) {
	b := NewBtrfs(t.TempDir(), 1)
	ctx := context.Background()

	if _, err := b.CreateRoot(ctx, "../escape"); err == nil {
		t.Fatal("CreateRoot accepted an unsafe name")
	}
	unsafe := branch.StorageRef{Provider: "btrfs", Handle: "../escape"}
	if _, _, err := b.Fork(ctx, unsafe, ForkOptions{Name: "child"}); err == nil {
		t.Fatal("Fork accepted an unsafe parent handle")
	}
	if err := b.Delete(ctx, unsafe); err == nil {
		t.Fatal("Delete accepted an unsafe handle")
	}
	if _, err := b.Children(ctx, unsafe); err == nil {
		t.Fatal("Children accepted an unsafe handle")
	}
}
