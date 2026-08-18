// Package compute runs Postgres on top of whatever storage a branch has.
//
// Storage and compute are deliberately separate: a branch whose compute is
// stopped still exists and still owns its data.
package compute

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/postkitstack/forklift/internal/branch"
)

// Provider starts and stops a branch's database process.
type Provider interface {
	Start(ctx context.Context, dataDir, pgVersion string) (branch.ComputeRef, error)
	Stop(ctx context.Context, ref branch.ComputeRef) error
	Running(ctx context.Context, ref branch.ComputeRef) bool
	Initdb(ctx context.Context, dataDir, pgVersion string) error
}

// Docker runs each branch as a stock postgres:{version} container.
//
// Stock images matter: the whole reason for working at the block layer rather
// than replacing Postgres's storage manager is that Postgres stays unmodified.
// The container has no idea it is running on a clone.
type Docker struct {
	// Network is a normal user-defined bridge.
	//
	// It is deliberately NOT created with --internal, though that was the first
	// attempt. Safe Mode wants a branch that cannot reach the outside world,
	// and an internal network delivers exactly that — but it also silently
	// drops published ports, so the branch becomes unreachable by the agent
	// that is supposed to use it. Postgres came up healthy and simply could not
	// be connected to.
	//
	// Egress isolation therefore needs a mechanism that distinguishes inbound
	// from outbound: per-container egress firewall rules, or a proxy the branch
	// is forced through. Until that exists, Safe Mode has to be enforced inside
	// Postgres (archive_mode off, subscriptions disabled, cron neutered, FDW
	// user mappings cleared) rather than at the network layer.
	Network  string
	BindHost string // defaults to loopback; never publish Postgres on every interface implicitly
	PortLow  int
	PortHigh int
	Password string
}

func NewDocker() *Docker {
	return &Docker{
		Network: "forklift", BindHost: "127.0.0.1",
		PortLow: 15500, PortHigh: 15600, Password: "forklift",
	}
}

// Initdb initialises an empty data directory using the same image the branch
// will later run, so the on-disk format always matches the server.
func (d *Docker) Initdb(ctx context.Context, dataDir, pgVersion string) error {
	args := []string{
		"run", "--rm",
		"-e", "POSTGRES_PASSWORD=" + d.Password,
		"-e", "PGDATA=/var/lib/postgresql/data",
		"-v", dataDir + ":/var/lib/postgresql/data",
		"--entrypoint", "docker-ensure-initdb.sh",
		d.image(pgVersion),
	}
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Docker) Start(ctx context.Context, dataDir, pgVersion string) (branch.ComputeRef, error) {
	if err := d.ensureNetwork(ctx); err != nil {
		return branch.ComputeRef{}, err
	}
	port, err := d.freePort()
	if err != nil {
		return branch.ComputeRef{}, err
	}

	args := []string{
		"run", "-d",
		"--network", d.Network,
		"-p", d.publishAddress(port),
		"-e", "POSTGRES_PASSWORD=" + d.Password,
		"-e", "PGDATA=/var/lib/postgresql/data",
		"-v", dataDir + ":/var/lib/postgresql/data",
		d.image(pgVersion),
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return branch.ComputeRef{}, fmt.Errorf("start container: %w: %s", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	ref := branch.ComputeRef{Provider: "docker", ContainerID: id, Port: port}

	if err := d.waitReady(ctx, ref); err != nil {
		_ = d.Stop(context.Background(), ref)
		return branch.ComputeRef{}, err
	}
	return ref, nil
}

func (d *Docker) Stop(ctx context.Context, ref branch.ComputeRef) error {
	if ref.ContainerID == "" {
		return nil
	}
	_ = exec.CommandContext(ctx, "docker", "stop", ref.ContainerID).Run()
	return exec.CommandContext(ctx, "docker", "rm", "-f", ref.ContainerID).Run()
}

func (d *Docker) Running(ctx context.Context, ref branch.ComputeRef) bool {
	if ref.ContainerID == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"-f", "{{.State.Running}}", ref.ContainerID).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// waitReady polls pg_isready. A clone always boots into crash recovery — it
// carries no backup_label, so Postgres replays WAL from the last checkpoint —
// and how long that takes scales with WAL written since that checkpoint.
// CHECKPOINT before forking shortens it.
func (d *Docker) waitReady(ctx context.Context, ref branch.ComputeRef) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		err := exec.CommandContext(ctx, "docker", "exec", ref.ContainerID,
			"pg_isready", "-U", "postgres").Run()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "20", ref.ContainerID).CombinedOutput()
	return fmt.Errorf("postgres did not become ready within 90s; last logs:\n%s", strings.TrimSpace(string(logs)))
}

func (d *Docker) ensureNetwork(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", d.Network).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "network", "create",
		d.Network).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("create network: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Docker) image(pgVersion string) string {
	if pgVersion == "" {
		pgVersion = "16"
	}
	return "postgres:" + pgVersion
}

func (d *Docker) publishAddress(port int) string {
	host := d.BindHost
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port)) + ":5432"
}

func (d *Docker) freePort() (int, error) {
	for p := d.PortLow; p <= d.PortHigh; p++ {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port between %d and %d", d.PortLow, d.PortHigh)
}
