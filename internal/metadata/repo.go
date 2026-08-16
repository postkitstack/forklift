// Package metadata stores the branch registry.
//
// The registry must live OUTSIDE the branchable data, and the reason is
// structural rather than stylistic: if branch records lived in the database
// being forked, forking it would fork the registry, and every child would
// believe it was the authoritative source of truth about all branches.
//
// It does not, however, have to be a database. A JSON file is the default here
// so that `forklift list` needs no running server — the same reason PostKit
// keeps session state in .postkit/db/session.json. A Postgres-backed
// implementation belongs behind this interface for the concurrent multi-agent
// case, where file locking stops being adequate.
package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/postkitstack/forklift/internal/branch"
)

// Repository is the branch registry contract.
type Repository interface {
	Get(name string) (*branch.Branch, error)
	List() ([]*branch.Branch, error)
	Put(b *branch.Branch) error
	Delete(name string) error
	// Children returns branches whose Parent is name.
	Children(name string) ([]*branch.Branch, error)
}

// ErrNotFound is returned when a branch is not in the registry.
type ErrNotFound struct{ Name string }

func (e *ErrNotFound) Error() string { return "no such branch: " + e.Name }

type file struct {
	Version  int                       `json:"version"`
	Branches map[string]*branch.Branch `json:"branches"`
}

// JSONRepo persists the registry to a single JSON file.
type JSONRepo struct {
	path string
	mu   sync.Mutex
}

// NewJSONRepo returns a repository backed by path.
func NewJSONRepo(path string) *JSONRepo { return &JSONRepo{path: path} }

func (r *JSONRepo) load() (*file, error) {
	f := &file{Version: 1, Branches: map[string]*branch.Branch{}}
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("registry at %s is corrupt: %w", r.path, err)
	}
	if f.Branches == nil {
		f.Branches = map[string]*branch.Branch{}
	}
	return f, nil
}

// save writes atomically — a torn registry file would be worse than a lost
// update, since it would strand storage that nothing knows how to reclaim.
func (r *JSONRepo) save(f *file) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *JSONRepo) Get(name string) (*branch.Branch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	b, ok := f.Branches[name]
	if !ok {
		return nil, &ErrNotFound{Name: name}
	}
	return b, nil
}

func (r *JSONRepo) List() ([]*branch.Branch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	out := make([]*branch.Branch, 0, len(f.Branches))
	for _, b := range f.Branches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r *JSONRepo) Put(b *branch.Branch) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	f.Branches[b.Name] = b
	return r.save(f)
}

func (r *JSONRepo) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	if _, ok := f.Branches[name]; !ok {
		return &ErrNotFound{Name: name}
	}
	delete(f.Branches, name)
	return r.save(f)
}

func (r *JSONRepo) Children(name string) ([]*branch.Branch, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	var kids []*branch.Branch
	for _, b := range all {
		if b.Parent == name {
			kids = append(kids, b)
		}
	}
	return kids, nil
}
