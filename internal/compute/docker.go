// Package compute runs Postgres on top of whatever storage a branch has.
//
// Storage and compute are deliberately separate: a branch whose compute is
// stopped still exists and still owns its data.
package compute

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/postkitstack/forklift/internal/branch"
	"github.com/postkitstack/forklift/internal/tool"
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

	exeOnce sync.Once
	exePath string
	exeErr  error
}

func NewDocker() *Docker {
	return &Docker{
		Network: "forklift", BindHost: "127.0.0.1",
		PortLow: 15500, PortHigh: 15600, Password: "forklift",
	}
}

// dockerExe resolves the docker binary once. Resolution goes through the
// shared tool resolver so a docker in an sbin dir off PATH is still found.
func (d *Docker) dockerExe() (string, error) {
	d.exeOnce.Do(func() { d.exePath, d.exeErr = tool.Resolve("docker") })
	return d.exePath, d.exeErr
}

// cmd builds a docker command, or an error if docker could not be found.
func (d *Docker) cmd(ctx context.Context, args ...string) (*exec.Cmd, error) {
	exe, err := d.dockerExe()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, exe, args...), nil
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
	cmd, err := d.cmd(ctx, args...)
	if err != nil {
		return fmt.Errorf("initdb: %w", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
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
	cmd, err := d.cmd(ctx, args...)
	if err != nil {
		return branch.ComputeRef{}, err
	}
	out, err := cmd.CombinedOutput()
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
	if cmd, err := d.cmd(ctx, "stop", ref.ContainerID); err == nil {
		_ = cmd.Run()
	}
	cmd, err := d.cmd(ctx, "rm", "-f", ref.ContainerID)
	if err != nil {
		return err
	}
	return cmd.Run()
}

func (d *Docker) Running(ctx context.Context, ref branch.ComputeRef) bool {
	if ref.ContainerID == "" {
		return false
	}
	cmd, err := d.cmd(ctx, "inspect", "-f", "{{.State.Running}}", ref.ContainerID)
	if err != nil {
		return false
	}
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// waitReady polls pg_isready. A clone always boots into crash recovery — it
// carries no backup_label, so Postgres replays WAL from the last checkpoint —
// and how long that takes scales with WAL written since that checkpoint.
// CHECKPOINT before forking shortens it.
func (d *Docker) waitReady(ctx context.Context, ref branch.ComputeRef) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := d.cmd(ctx, "exec", ref.ContainerID, "pg_isready", "-U", "postgres")
		if err == nil && cmd.Run() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	logs := []byte("docker not found")
	if cmd, err := d.cmd(context.Background(), "logs", "--tail", "20", ref.ContainerID); err == nil {
		logs, _ = cmd.CombinedOutput()
	}
	return fmt.Errorf("postgres did not become ready within 90s; last logs:\n%s", strings.TrimSpace(string(logs)))
}

func (d *Docker) ensureNetwork(ctx context.Context) error {
	if cmd, err := d.cmd(ctx, "network", "inspect", d.Network); err == nil && cmd.Run() == nil {
		return nil
	}
	cmd, err := d.cmd(ctx, "network", "create", d.Network)
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	out, err := cmd.CombinedOutput()
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

// DaemonInfo describes the Docker daemon a command would talk to.
type DaemonInfo struct {
	Context  string
	Rootless bool
	ID       string
}

// InspectDaemon queries the daemon the current process would reach.
//
// The daemon ID from `docker info -f {{.ID}}` is the comparable part: two
// docker binaries can share a context name yet point at different daemons,
// and DOCKER_HOST overrides everything.
func InspectDaemon(ctx context.Context) (DaemonInfo, error) {
	exe, err := tool.Resolve("docker")
	if err != nil {
		return DaemonInfo{}, err
	}
	var info DaemonInfo
	if out, err := exec.CommandContext(ctx, exe, "context", "show").Output(); err == nil {
		info.Context = strings.TrimSpace(string(out))
	}
	if info.Context == "" {
		info.Context = "default"
	}
	out, err := exec.CommandContext(ctx, exe, "info", "-f", "{{.ID}}").Output()
	if err != nil {
		return info, fmt.Errorf("docker info failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	info.ID = strings.TrimSpace(string(out))
	sec, _ := exec.CommandContext(ctx, exe, "info", "-f", "{{.SecurityOptions}}").Output()
	info.Rootless = strings.Contains(string(sec), "rootless")
	return info, nil
}

// EnsureSameDaemonAsUser refuses to proceed when sudo has forked the daemon.
//
// Under sudo we run docker as root. If the invoking user runs rootless Docker
// or a non-default context, root's docker is a different daemon: branches get
// created where the user cannot see them, or fail confusingly mid-create.
// When SUDO_USER is set, compare the daemon ID root sees with the one that
// user sees and fail loudly on a mismatch. If the comparison is impossible —
// no sudo binary, or the user's own query fails — stay silent rather than
// block on something we could not verify.
func EnsureSameDaemonAsUser(ctx context.Context) error {
	user := os.Getenv("SUDO_USER")
	if user == "" || os.Geteuid() != 0 {
		return nil
	}
	rootDaemon, err := InspectDaemon(ctx)
	if err != nil {
		return nil
	}
	sudoExe, err := tool.Resolve("sudo")
	if err != nil {
		return nil
	}
	dockerExe, err := tool.Resolve("docker")
	if err != nil {
		return nil
	}
	// sudo -u does not inherit the target user's environment, so the usual
	// daemon selectors must be forwarded explicitly or we would query root's
	// daemon twice and call it a match.
	cmd := exec.CommandContext(ctx, sudoExe, "-u", user,
		"--preserve-env=DOCKER_HOST,DOCKER_CONTEXT,XDG_RUNTIME_DIR",
		dockerExe, "info", "-f", "{{.ID}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}
	userID := strings.TrimSpace(string(out))
	if userID == "" || userID == rootDaemon.ID {
		return nil
	}
	userDaemon, derr := inspectDaemonAs(ctx, sudoExe, dockerExe, user)
	userCtx := "unknown"
	if derr == nil {
		userCtx = userDaemon.Context
	}
	return fmt.Errorf(
		"sudo is talking to a different Docker daemon than %s; branches created here would be invisible to you.\n"+
			"  as root:      context %s, id %s\n"+
			"  as %s:  context %s, id %s\n"+
			"Run forklift without sudo, or align DOCKER_HOST / docker context first.",
		user, rootDaemon.Context, rootDaemon.ID, user, userCtx, userID)
}

func inspectDaemonAs(ctx context.Context, sudoExe, dockerExe, user string) (DaemonInfo, error) {
	c := exec.CommandContext(ctx, sudoExe, "-u", user,
		"--preserve-env=DOCKER_HOST,DOCKER_CONTEXT,XDG_RUNTIME_DIR",
		dockerExe, "context", "show")
	out, err := c.Output()
	if err != nil {
		return DaemonInfo{}, err
	}
	ctxName := strings.TrimSpace(string(out))
	if ctxName == "" {
		ctxName = "default"
	}
	return DaemonInfo{Context: ctxName}, nil
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
