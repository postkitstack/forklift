package storage_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/postkitstack/forklift/internal/storage"
)

// Conformance is the suite every storage provider must pass.
//
// It exists so that adding dm-thin, ZFS or a vendor API later is a matter of
// making this pass rather than re-deriving what correct behaviour means. The
// interesting requirements are the ones that are easy to get wrong: a fork must
// not disturb the parent, depth must work, and deleting a branch that others
// depend on must be refused rather than silently corrupting them.
func Conformance(t *testing.T, newProvider func(root string) storage.Provider) {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	p := newProvider(root)

	if ok, why := p.Available(ctx); !ok {
		t.Skipf("provider unavailable: %s", why)
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Providers holding a mount must release it, or TempDir cleanup fails with
	// "device or resource busy".
	if td, ok := p.(storage.Teardown); ok {
		t.Cleanup(func() {
			if err := td.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}

	t.Run("CreateRoot yields a usable data directory", func(t *testing.T) {
		ref, err := p.CreateRoot(ctx, "base")
		if err != nil {
			t.Fatalf("CreateRoot: %v", err)
		}
		if ref.DataDir == "" {
			t.Fatal("DataDir must be set")
		}
		if _, err := os.Stat(ref.DataDir); err != nil {
			t.Fatalf("DataDir must exist: %v", err)
		}
	})

	base, err := p.CreateRoot(ctx, "parent")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	// Something to observe across the fork.
	marker := filepath.Join(base.DataDir, "marker")
	if err := os.WriteFile(marker, []byte("from-parent"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("Fork copies parent state", func(t *testing.T) {
		child, _, err := p.Fork(ctx, base, storage.ForkOptions{Name: "child"})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(child.DataDir, "marker"))
		if err != nil {
			t.Fatalf("child should carry the parent's data: %v", err)
		}
		if string(got) != "from-parent" {
			t.Fatalf("want %q, got %q", "from-parent", string(got))
		}
	})

	t.Run("writes to the child do not reach the parent", func(t *testing.T) {
		child, _, err := p.Fork(ctx, base, storage.ForkOptions{Name: "isolated"})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if err := os.WriteFile(filepath.Join(child.DataDir, "marker"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "from-parent" {
			t.Fatalf("parent was modified by a write to the child: %q", string(got))
		}
	})

	t.Run("a branch of a branch works", func(t *testing.T) {
		first, _, err := p.Fork(ctx, base, storage.ForkOptions{Name: "depth1"})
		if err != nil {
			t.Fatalf("depth-1 fork: %v", err)
		}
		if err := os.WriteFile(filepath.Join(first.DataDir, "depth"), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		second, _, err := p.Fork(ctx, first, storage.ForkOptions{Name: "depth2"})
		if err != nil {
			t.Fatalf("depth-2 fork: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(second.DataDir, "depth"))
		if err != nil {
			t.Fatalf("depth-2 should inherit depth-1's writes: %v", err)
		}
		if string(got) != "1" {
			t.Fatalf("want %q, got %q", "1", string(got))
		}
	})

	t.Run("deleting a branch with children is refused", func(t *testing.T) {
		parent, err := p.CreateRoot(ctx, "gcparent")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Fork(ctx, parent, storage.ForkOptions{Name: "gcchild"}); err != nil {
			t.Fatal(err)
		}
		err = p.Delete(ctx, parent)
		if err == nil {
			t.Fatal("deleting a parent with a live child must be refused")
		}
		var hc *storage.ErrHasChildren
		if !asErrHasChildren(err, &hc) {
			t.Fatalf("want ErrHasChildren so callers can name the blockers, got %T: %v", err, err)
		}
		if len(hc.Children) == 0 {
			t.Fatal("ErrHasChildren must name the dependants")
		}
	})

	t.Run("a childless branch deletes cleanly", func(t *testing.T) {
		ref, err := p.CreateRoot(ctx, "disposable")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Delete(ctx, ref); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("Usage reports a real pool size", func(t *testing.T) {
		u, err := p.Usage(ctx)
		if err != nil {
			t.Skipf("usage unsupported: %v", err)
		}
		if u.TotalBytes == 0 {
			t.Fatal("TotalBytes must be non-zero, or the watermark check is inert")
		}
	})
}

func asErrHasChildren(err error, target **storage.ErrHasChildren) bool {
	if e, ok := err.(*storage.ErrHasChildren); ok {
		*target = e
		return true
	}
	return false
}

// TestBtrfsConformance runs the suite against the btrfs provider.
//
// Skipped unless running as root on a machine with btrfs, since the pool needs
// loop devices and mount. `make test-integration` runs it.
func TestBtrfsConformance(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: loop devices, mount, btrfs subvolumes")
	}
	if _, err := exec.LookPath("mkfs.btrfs"); err != nil {
		t.Skip("btrfs-progs not installed")
	}
	Conformance(t, func(root string) storage.Provider {
		b := storage.NewBtrfs(root, 2)
		// The suite runs as root and inspects files directly, so keep the
		// clone owned by the caller rather than the postgres uid.
		b.PGUID, b.PGGID = os.Getuid(), os.Getgid()
		return b
	})
}
