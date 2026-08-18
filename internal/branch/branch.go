// Package branch defines the domain model: what a branch is, independent of how
// it is stored or where its compute runs.
//
// The central claim of this project is that "branch" is the primitive and every
// storage mechanism (btrfs, dm-thin, ZFS, a vendor API) is an implementation
// detail behind an interface. These types are what all of them agree on.
package branch

import (
	"fmt"
	"time"
)

// Status is a branch's lifecycle state.
//
// Storage and compute are deliberately separate concerns: a branch whose
// Postgres is stopped still exists and still owns its data. Agents die,
// branches survive.
type Status string

const (
	StatusCreating Status = "creating"
	StatusReady    Status = "ready"   // storage exists, compute running
	StatusStopped  Status = "stopped" // storage exists, compute stopped
	StatusFailed   Status = "failed"
	StatusDeleting Status = "deleting"
)

// StorageRef identifies a branch's data wherever the provider put it.
// Opaque to everything except the provider that created it.
type StorageRef struct {
	Provider string `json:"provider"` // "btrfs", "dmthin", "zfs", "neon"
	Handle   string `json:"handle"`   // subvolume path, LV name, dataset, branch id
	DataDir  string `json:"dataDir"`  // absolute path to PGDATA for the compute layer
}

// ComputeRef identifies the running Postgres, if any.
type ComputeRef struct {
	Provider    string `json:"provider"` // "docker"
	ContainerID string `json:"containerId,omitempty"`
	Port        int    `json:"port,omitempty"`
}

// ForkPoint records where a branch diverged from its parent.
//
// Retaining this for as long as the child lives is what makes a later
// three-way diff possible: without the base, you can only compare parent and
// branch, which cannot distinguish "they changed it" from "we both did".
type ForkPoint struct {
	SnapshotHandle string    `json:"snapshotHandle"`
	LSN            string    `json:"lsn,omitempty"`
	At             time.Time `json:"at"`
}

// Branch is one forkable Postgres state.
type Branch struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"` // empty for a root branch
	Status Status `json:"status"`

	Storage StorageRef `json:"storage"`
	Compute ComputeRef `json:"compute"`
	Fork    *ForkPoint `json:"fork,omitempty"`

	PGVersion string    `json:"pgVersion"`
	CreatedAt time.Time `json:"createdAt"`

	// Owner is an optional agent id. Branches are owned by the database
	// system, not the agent — an agent merely checks one out.
	Owner     string     `json:"owner,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

const maxNameLength = 63

// ValidateName rejects branch names that could escape a storage provider's
// namespace or behave ambiguously in CLI output. Storage providers must still
// call this at their boundary rather than trusting their callers.
func ValidateName(name string) error {
	valid := len(name) > 0 && len(name) <= maxNameLength && isASCIILetterOrDigit(name[0])
	for i := 1; valid && i < len(name); i++ {
		c := name[i]
		valid = isASCIILetterOrDigit(c) || c == '.' || c == '_' || c == '-'
	}
	if !valid {
		return fmt.Errorf(
			"invalid branch name %q: use 1-%d ASCII letters, digits, '.', '_' or '-', starting with a letter or digit",
			name, maxNameLength)
	}
	return nil
}

func isASCIILetterOrDigit(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// IsRoot reports whether this branch has no parent.
func (b *Branch) IsRoot() bool { return b.Parent == "" }

// Endpoint is the connection string for the branch's Postgres.
func (b *Branch) Endpoint() string {
	if b.Compute.Port == 0 {
		return ""
	}
	return "postgres://postgres:forklift@localhost:" +
		itoa(b.Compute.Port) + "/postgres?sslmode=disable"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
