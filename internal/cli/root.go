// Package cli wires the commands.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/postkitstack/forklift/internal/compute"
	"github.com/postkitstack/forklift/internal/manager"
	"github.com/postkitstack/forklift/internal/metadata"
	"github.com/postkitstack/forklift/internal/storage"
	"github.com/spf13/cobra"
)

var (
	flagRoot   string
	flagPoolGB int
)

// Execute runs the CLI.
func Execute(version string) error {
	root := &cobra.Command{
		Use:   "forklift",
		Short: "Copy-on-write branching for Postgres",
		Long: `forklift forks a running Postgres in milliseconds.

A branch is a copy-on-write snapshot of a whole cluster plus its own compute.
The parent keeps serving throughout, the clone boots into ordinary crash
recovery, and branches of branches work. Postgres itself is unmodified — these
are stock postgres:{version} images that have no idea they are running on a
clone.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagRoot, "root", defaultRoot(),
		"directory holding the storage pool and registry")
	root.PersistentFlags().IntVar(&flagPoolGB, "pool-size", 20,
		"size of the pool backing file, in GB, when it is first created")

	root.AddCommand(
		newDoctorCmd(),
		newInitCmd(),
		newCreateCmd(),
		newListCmd(),
		newInspectCmd(),
		newStartCmd(),
		newStopCmd(),
		newDeleteCmd(),
	)
	return root.Execute()
}

func defaultRoot() string {
	if v := os.Getenv("FORKLIFT_ROOT"); v != "" {
		return v
	}
	return "/var/lib/forklift"
}

// buildManager assembles the object graph. Storage is chosen here, which is
// the one place the mechanism decision is made.
func buildManager() (*manager.Manager, error) {
	store := storage.NewBtrfs(flagRoot, flagPoolGB)
	repo := metadata.NewJSONRepo(filepath.Join(flagRoot, "branches.json"))
	return manager.New(store, compute.NewDocker(), repo), nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		// Resolve the binary by absolute path: `sudo forklift` fails outright
		// when the binary is not on sudo's secure_path (go install lands it in
		// ~/go/bin), so a bare "Re-run with sudo" would suggest a command that
		// cannot work.
		exe, err := os.Executable()
		if err != nil || exe == "" {
			exe = "forklift"
		}
		return fmt.Errorf(
			"this command needs root: the storage pool uses loop devices, mount and btrfs subvolumes.\n"+
				"Re-run:  sudo %s %s\n"+
				"or point --root at a pool you already own",
			exe, strings.Join(os.Args[1:], " "))
	}
	return nil
}
