package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/postkitstack/forklift/internal/storage"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report which copy-on-write mechanisms this machine can provide",
		Long: `Probes the kernel for every COW mechanism forklift knows about.

Worth running before committing to a machine: the preferred mechanism is not
universally available. A plain Linux Docker host was found exposing only
multipath/striped/linear/error device-mapper targets, no nbd, and no reflink.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			ms := storage.Detect(ctx)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "MECHANISM\tSTATUS\tDETAIL")
			for _, m := range ms {
				fmt.Fprintf(w, "%s\t%s\t%s\n", m.Name, stateWord(m.State), m.Detail)
			}
			fmt.Fprintf(w, "loop devices\t%s\t%s\n",
				stateWord(storage.LoopDevicesState(ctx)),
				"required by every pool-in-a-file mechanism")
			w.Flush()

			fmt.Println()
			if best := storage.Best(ms); best != "" {
				fmt.Printf("Best available mechanism: %s\n", best)
			} else {
				fmt.Println("No COW mechanism available on this machine.")
			}
			if os.Geteuid() != 0 {
				fmt.Println("\nNote: probes marked \"unknown\" could not run without root; re-run with sudo to determine them.")
			}
			return nil
		},
	}
}

// stateWord renders a probe result. Unknown must stay visibly distinct from
// unavailable: telling a user a mechanism is absent when we merely lacked
// permission to check makes doctor lie.
func stateWord(s storage.State) string {
	switch s {
	case storage.StateAvailable:
		return "available"
	case storage.StateUnavailable:
		return "unavailable"
	default:
		return "unknown — re-run with sudo to determine"
	}
}

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the storage pool",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			m, err := buildManager()
			if err != nil {
				return err
			}
			if err := m.Init(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("Pool ready at %s\n", flagRoot)
			return nil
		},
	}
}

func newCreateCmd() *cobra.Command {
	var from, pgVersion string
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a branch, either empty or forked from another",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			m, err := buildManager()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := m.Init(ctx); err != nil {
				return err
			}

			start := time.Now()
			name := args[0]

			b, err := func() (interface{ Endpoint() string }, error) {
				if from == "" {
					fmt.Printf("Creating empty branch %q (postgres:%s)...\n", name, pgVersion)
					return m.CreateRoot(ctx, name, pgVersion)
				}
				fmt.Printf("Forking %q from %q...\n", name, from)
				return m.Fork(ctx, from, name)
			}()
			if err != nil {
				return err
			}

			fmt.Printf("\n  branch    %s\n", name)
			fmt.Printf("  endpoint  %s\n", b.Endpoint())
			fmt.Printf("  elapsed   %s\n", time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "parent branch to fork from; omit to create an empty branch")
	c.Flags().StringVar(&pgVersion, "pg", "16", "Postgres major version (empty branches only)")
	return c
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List branches",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := buildManager()
			if err != nil {
				return err
			}
			branches, err := m.Repo.List()
			if err != nil {
				return err
			}
			if len(branches) == 0 {
				fmt.Println("No branches. Create one with: forklift create main")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPARENT\tSTATUS\tPORT\tCREATED")
			for _, b := range branches {
				parent := b.Parent
				if parent == "" {
					parent = "-"
				}
				port := "-"
				if b.Compute.Port != 0 {
					port = fmt.Sprint(b.Compute.Port)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					b.Name, parent, b.Status, port, b.CreatedAt.Format(time.RFC3339))
			}
			w.Flush()

			if u, err := m.Storage.Usage(cmd.Context()); err == nil && u.TotalBytes > 0 {
				fmt.Printf("\nPool: %.1f%% used (%s of %s)\n",
					u.Percent(), humanBytes(u.UsedBytes), humanBytes(u.TotalBytes))
			}
			return nil
		},
	}
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show one branch in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := buildManager()
			if err != nil {
				return err
			}
			b, err := m.Repo.Get(args[0])
			if err != nil {
				return err
			}
			kids, _ := m.Repo.Children(b.Name)

			fmt.Printf("name       %s\n", b.Name)
			fmt.Printf("parent     %s\n", orDash(b.Parent))
			fmt.Printf("status     %s\n", b.Status)
			fmt.Printf("endpoint   %s\n", orDash(b.Endpoint()))
			fmt.Printf("storage    %s:%s\n", b.Storage.Provider, b.Storage.Handle)
			fmt.Printf("datadir    %s\n", b.Storage.DataDir)
			fmt.Printf("pg version %s\n", b.PGVersion)
			fmt.Printf("created    %s\n", b.CreatedAt.Format(time.RFC3339))
			if b.Fork != nil {
				fmt.Printf("forked at  %s\n", b.Fork.At.Format(time.RFC3339))
			}
			if len(kids) > 0 {
				fmt.Printf("children   ")
				for i, k := range kids {
					if i > 0 {
						fmt.Print(", ")
					}
					fmt.Print(k.Name)
				}
				fmt.Println("\n           (this branch cannot be deleted while they exist)")
			}
			return nil
		},
	}
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped branch's Postgres",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			m, err := buildManager()
			if err != nil {
				return err
			}
			b, err := m.Repo.Get(args[0])
			if err != nil {
				return err
			}
			if err := m.Start(cmd.Context(), b); err != nil {
				return err
			}
			fmt.Printf("Started %s at %s\n", b.Name, b.Endpoint())
			return nil
		},
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a branch's Postgres, keeping its data",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := buildManager()
			if err != nil {
				return err
			}
			if err := m.Stop(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Stopped %s. Its data is untouched; start it again with: forklift start %s\n",
				args[0], args[0])
			return nil
		},
	}
}

func newDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a branch and its data",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			m, err := buildManager()
			if err != nil {
				return err
			}
			if err := m.Delete(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted %s\n", args[0])
			return nil
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

var _ = context.Background
