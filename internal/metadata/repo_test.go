package metadata

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/postkitstack/forklift/internal/branch"
)

func newRepo(t *testing.T) *JSONRepo {
	t.Helper()
	return NewJSONRepo(filepath.Join(t.TempDir(), "branches.json"))
}

func put(t *testing.T, r *JSONRepo, name, parent string, age time.Duration) {
	t.Helper()
	b := &branch.Branch{
		Name:      name,
		Parent:    parent,
		Status:    branch.StatusReady,
		CreatedAt: time.Now().Add(-age),
	}
	if err := r.Put(b); err != nil {
		t.Fatalf("Put(%s): %v", name, err)
	}
}

func TestMissingRegistryIsEmptyNotAnError(t *testing.T) {
	// `forklift list` on a fresh machine should say "no branches", not fail.
	r := newRepo(t)
	got, err := r.List()
	if err != nil {
		t.Fatalf("List on a missing file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 branches, got %d", len(got))
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	r := newRepo(t)
	put(t, r, "main", "", 0)

	got, err := r.Get("main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "main" || !got.IsRoot() {
		t.Fatalf("unexpected branch: %+v", got)
	}
}

func TestGetUnknownReturnsNotFound(t *testing.T) {
	r := newRepo(t)
	_, err := r.Get("ghost")
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := err.(*ErrNotFound); !ok {
		t.Fatalf("want *ErrNotFound so callers can distinguish it, got %T", err)
	}
}

func TestListIsOrderedByCreation(t *testing.T) {
	r := newRepo(t)
	put(t, r, "third", "", 1*time.Second)
	put(t, r, "first", "", 3*time.Second)
	put(t, r, "second", "", 2*time.Second)

	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("position %d: want %s, got %s", i, w, got[i].Name)
		}
	}
}

func TestChildrenFindsDependants(t *testing.T) {
	// Delete protection depends on this being right.
	r := newRepo(t)
	put(t, r, "main", "", 3*time.Second)
	put(t, r, "agent-a", "main", 2*time.Second)
	put(t, r, "agent-b", "main", 1*time.Second)
	put(t, r, "deep", "agent-a", 0)

	kids, err := r.Children("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("want 2 direct children of main, got %d", len(kids))
	}
	// Grandchildren are not direct children.
	for _, k := range kids {
		if k.Name == "deep" {
			t.Fatal("Children must return direct descendants only")
		}
	}
}

func TestDeleteRemovesOnlyTheNamedBranch(t *testing.T) {
	r := newRepo(t)
	put(t, r, "main", "", time.Second)
	put(t, r, "agent-a", "main", 0)

	if err := r.Delete("agent-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get("agent-a"); err == nil {
		t.Fatal("agent-a should be gone")
	}
	if _, err := r.Get("main"); err != nil {
		t.Fatalf("main should survive: %v", err)
	}
}

func TestPersistsAcrossInstances(t *testing.T) {
	// The registry outlives the process — that is the entire point of storing
	// it outside the branchable database.
	path := filepath.Join(t.TempDir(), "branches.json")
	first := NewJSONRepo(path)
	put(t, first, "main", "", 0)

	second := NewJSONRepo(path)
	if _, err := second.Get("main"); err != nil {
		t.Fatalf("a second instance should see the branch: %v", err)
	}
}

func TestCorruptRegistryIsReportedNotSilentlyDiscarded(t *testing.T) {
	// Silently starting from empty would strand storage that nothing knows how
	// to reclaim, which is worse than refusing to run.
	path := filepath.Join(t.TempDir(), "branches.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJSONRepo(path).List(); err == nil {
		t.Fatal("expected a corrupt registry to surface as an error")
	}
}
