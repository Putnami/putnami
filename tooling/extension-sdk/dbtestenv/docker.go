// The Postgres provider behind Up/Down: ephemeral, local test infrastructure
// run by shelling out to the docker CLI.
//
// Migrated verbatim in behavior from the CLI's internal/testinfra, since
// deleted — same pinned image, same digest-keyed identity,
// same reuse and reaping rules, same bounded timeouts. What changed is WHO owns
// it: a provider lives with the extension that needs it, not in the
// orchestrator, and the credential now reaches the test task through the
// invocation-scoped sensitive artifact instead of a private env seam the
// orchestrator had to hide from its own cache key.
//
// Everything here is daemon-free at unit-test time: every docker invocation goes
// through the runner seam, which tests replace with a fake to assert argv,
// digest-keyed reuse and reaping without a live daemon. Only digests, hostnames
// and ports ever surface in container names, labels, filters, logs or errors —
// credentials live solely in the returned pdb.Connection and the `docker run`
// env argv, never in a label, name, filter, log line or error.
package dbtestenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	pdb "go.putnami.dev/protocol/database"
)

// postgresImagePinned is the digest-pinned postgres:17 image the provisioner
// runs. The pin — not the moving :17 tag — is what makes the reuse digest (and
// therefore container identity) reproducible across machines and over time;
// bumping it (for a CVE fix or a new point release) is an explicit, reviewable
// change, exactly like go/extension/internal/jobs/pkg/docker.go's
// dockerBaseImagePinned. Refresh with:
//
//	docker buildx imagetools inspect postgres:17
//
// (or the Docker Hub tag API) and paste the manifest-list digest below.
const postgresImagePinned = "postgres:17@sha256:cb875afe6d2e8593c28c22d37d0fd7aaf035c43a42e2f7792cd4c09ceb6beac5"

// labelKey namespaces every container this provisioner owns. Its value is the
// config digest — never a credential — so session reuse and orphan reaping are
// pure label lookups. The name follows the framework's putnami.test.* convention.
const labelKey = "putnami.test.workspace"

// namePrefix builds the deterministic container name (namePrefix + digest). A
// deterministic name gives concurrent provisions atomicity for free: docker
// enforces unique names, so two racing runs with the same config digest cannot
// both create a container — the loser falls back to reusing the winner's.
const namePrefix = "putnami-test-pg-"

// containerPort is the port Postgres listens on inside the container; it is
// published to an ephemeral host port bound to loopback only.
const containerPort = "5432"

const (
	// DefaultReadyTimeout bounds the in-process protocol readiness wait for the
	// provisioned Postgres: a single named default timeout — no magic numbers,
	// no unbounded waits — scaled to accepting queries rather than a full test
	// run.
	DefaultReadyTimeout = 60 * time.Second

	// DefaultProvisionTimeout bounds a whole Provision call (image pull + start +
	// readiness) so a wedged docker daemon can never hang a run.
	DefaultProvisionTimeout = 5 * time.Minute

	// dockerQueryTimeout bounds cheap docker queries (ps, port, rm).
	dockerQueryTimeout = 20 * time.Second

	// dockerRunTimeout bounds `docker run -d`, which may pull the image first.
	dockerRunTimeout = 4 * time.Minute

	// readyPollInterval is the gap between Postgres readiness probes.
	readyPollInterval = 200 * time.Millisecond
)

// errStartFailed is the credential-free error a failed `docker run` surfaces. The
// docker argv carries the Postgres password, so start never wraps the raw
// exec error together with the arguments — only this sentinel escapes.
var errStartFailed = errors.New("docker: failed to start postgres test container")

// runner runs a docker CLI invocation and returns its trimmed stdout. It is the
// single seam tests replace with a fake to assert argv without a live daemon.
// A real runner shells out to the docker binary under a bounded context.
type runner func(ctx context.Context, args ...string) (string, error)

// probeFunc probes whether Postgres in container can serve queries. It receives
// the runner and non-secret server identity so production can invoke pg_isready
// while tests can simulate slow readiness without a daemon.
type probeFunc func(ctx context.Context, run runner, container, user, database string) error

// serverConfig is the digest-affecting Postgres server identity: two configs
// that would yield different servers key to different containers. The password
// is deliberately NOT a field here — it never enters a serializable struct, so
// no credential can leak through the digest path. v1 uses a constant password,
// so it never varies the server's identity anyway; a future configurable
// password would be added here (and would then legitimately move the digest).
type serverConfig struct {
	Image    string `json:"image"`
	User     string `json:"user"`
	Database string `json:"database"`
}

// digest is the stable, canonical identity of the config. Truncated to 16 hex
// chars — collision-free at this scale and short enough for a container name.
func (c serverConfig) digest() string { return digestOf(c) }

// Provisioner provisions a Postgres server for a test run by shelling out to the
// docker CLI. The zero value is not usable; construct one with NewProvisioner.
type Provisioner struct {
	run             runner
	probe           probeFunc
	dockerAvailable func() bool
	cfg             serverConfig
	// password is the superuser secret. It is held outside serverConfig (and thus
	// outside the digest and every serialization path) so it can only ever surface
	// in the run env argv and the returned pdb.Connection — never in a label,
	// name, filter, digest or log line.
	password         string
	readyTimeout     time.Duration
	provisionTimeout time.Duration
}

// NewProvisioner returns a Provisioner wired to the real docker CLI and
// pg_isready, pinned to postgresImagePinned with deterministic local superuser
// credentials. The credentials are constant, so the reuse digest is stable
// across runs; they are throwaway secrets for a loopback-only container and
// never reach a label, name or event.
func NewProvisioner() *Provisioner {
	return &Provisioner{
		run:             execRunner,
		probe:           postgresReady,
		dockerAvailable: DockerAvailable,
		cfg: serverConfig{
			Image:    postgresImagePinned,
			User:     "putnami",
			Database: "putnami_test",
		},
		password:         "local-dev-secret",
		readyTimeout:     DefaultReadyTimeout,
		provisionTimeout: DefaultProvisionTimeout,
	}
}

// available reports whether this provisioner can reach its Docker runner.
// Production uses the PATH lookup; tests replace it together with the runner
// so fake-Docker cases never depend on the host having a docker binary.
func (p *Provisioner) available() bool {
	return p != nil && p.dockerAvailable != nil && p.dockerAvailable()
}

// DockerAvailable reports whether a docker binary is on PATH. It is a PATH
// lookup, never a daemon call, and it is the last of the three fail-closed gates
// Up applies before anything may touch docker.
func DockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// LabelKey is the container label every resource this provisioner owns carries.
// Exposed so a lease can record the filter a later run reclaims by, without that
// lease having to know how a container is named.
func (p *Provisioner) LabelKey() string { return labelKey }

// ContainerName is the deterministic container name for a config digest.
func (p *Provisioner) ContainerName(digest string) string { return namePrefix + digest }

// Provision returns a ready-to-use Postgres connection and the configuration
// digest its container is labeled with.
//
// It first reaps stale workspace-labeled containers from prior configs, then
// reuses a still-running container whose label matches the current digest, or
// starts a new one under a deterministic name, and finally waits (bounded) for
// Postgres to accept queries. Every docker invocation is context-bounded, so an
// interrupted run leaves no unbounded process; label-reaping cleans up whatever
// --rm could not after a SIGKILL.
func (p *Provisioner) Provision() (pdb.Connection, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.provisionTimeout)
	defer cancel()
	conn, err := p.provision(ctx)
	return conn, p.cfg.digest(), err
}

func (p *Provisioner) provision(ctx context.Context) (pdb.Connection, error) {
	digest := p.cfg.digest()

	// RECOVERY BEFORE PROVISIONING. A SIGKILL cannot stop a container, so
	// whatever a killed run left behind is still here. Reaping the mismatched
	// ones first — rather than on a timer — is what makes "the next invocation
	// recovers the orphan" true. A failure here must not block provisioning the
	// current config.
	_ = p.reapStale(ctx, digest)

	// Session reuse: a running container with the current digest is interchangeable.
	if container, host, port, ok := p.findRunning(ctx, digest); ok {
		if err := p.waitReady(ctx, container); err != nil {
			return pdb.Connection{}, err
		}
		return p.connection(host, port), nil
	}

	container, host, port, err := p.start(ctx, digest)
	if err != nil {
		return pdb.Connection{}, err
	}
	if err := p.waitReady(ctx, container); err != nil {
		return pdb.Connection{}, err
	}
	return p.connection(host, port), nil
}

// TeardownAll reaps every workspace-labeled container regardless of digest. It
// backs the explicit `--infra-down` teardown path: no provisioning, just cleanup.
func (p *Provisioner) TeardownAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), p.provisionTimeout)
	defer cancel()
	return p.teardownAll(ctx)
}

func (p *Provisioner) teardownAll(ctx context.Context) error {
	all, err := p.listIDs(ctx, true, "label="+labelKey)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	return p.remove(ctx, all)
}

// reapStale removes every workspace-labeled container whose digest is not keep.
// It compares two label filters rather than parsing per-label templates, so it
// works across docker versions. --rm handles clean stops; this handles the
// SIGKILL leftovers --rm cannot.
func (p *Provisioner) reapStale(ctx context.Context, keep string) error {
	all, err := p.listIDs(ctx, true, "label="+labelKey)
	if err != nil {
		return err
	}
	current, err := p.listIDs(ctx, true, "label="+labelKey+"="+keep)
	if err != nil {
		return err
	}
	keepSet := make(map[string]bool, len(current))
	for _, id := range current {
		keepSet[id] = true
	}
	var stale []string
	for _, id := range all {
		if !keepSet[id] {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return p.remove(ctx, stale)
}

// findRunning returns the ID, host and mapped port of a still-running container
// whose label matches digest, or ok=false when none exists.
func (p *Provisioner) findRunning(ctx context.Context, digest string) (container, host string, port int, ok bool) {
	ids, err := p.listIDs(ctx, false, "status=running", "label="+labelKey+"="+digest)
	if err != nil || len(ids) == 0 {
		return "", "", 0, false
	}
	host, port, err = p.mappedPort(ctx, ids[0])
	if err != nil {
		return "", "", 0, false
	}
	return ids[0], host, port, true
}

// start runs a fresh Postgres container under the deterministic name and
// returns its reference, host and mapped port. It first clears a stopped
// leftover under that name with a non-force `docker rm` — which never disturbs
// a concurrent winner's running container. If `docker run` fails on a name
// conflict, a concurrent run won the race; start reuses that container instead.
// The docker argv carries the password, so a run failure surfaces only the
// credential-free errStartFailed sentinel.
func (p *Provisioner) start(ctx context.Context, digest string) (string, string, int, error) {
	name := p.ContainerName(digest)

	// Clear only a STOPPED leftover under this name (a prior run that exited
	// before --rm could reap it). This is a non-force `docker rm`: docker refuses
	// to remove a *running* container, so if a concurrent provision won the
	// deterministic-name race and its container is already live — validated and
	// handed back to its own caller — this rm no-ops instead of yanking it away
	// (which would leave that winner's caller dialing a dead port). The `docker
	// run` below then name-conflicts and start() falls back to reusing the winner
	// via findRunning.
	_, _ = p.docker(ctx, dockerQueryTimeout, "rm", name)

	args := []string{
		"run", "-d", "--rm",
		"--name", name,
		"--label", labelKey + "=" + digest,
		// Bind to loopback only and let docker pick the host port: no external
		// exposure, and concurrent runs never fight over a fixed port.
		"--publish", "127.0.0.1::" + containerPort,
		"--env", "POSTGRES_USER=" + p.cfg.User,
		"--env", "POSTGRES_PASSWORD=" + p.password,
		"--env", "POSTGRES_DB=" + p.cfg.Database,
		p.cfg.Image,
	}
	if _, err := p.docker(ctx, dockerRunTimeout, args...); err != nil {
		// A name conflict means a concurrent provision won; reuse its container.
		if container, host, port, ok := p.findRunning(ctx, digest); ok {
			return container, host, port, nil
		}
		return "", "", 0, errStartFailed
	}

	host, port, err := p.mappedPort(ctx, name)
	if err != nil {
		return "", "", 0, err
	}
	return name, host, port, nil
}

// mappedPort resolves the loopback host and ephemeral host port docker mapped the
// container's Postgres port onto, via `docker port <ref> 5432/tcp`.
func (p *Provisioner) mappedPort(ctx context.Context, ref string) (string, int, error) {
	out, err := p.docker(ctx, dockerQueryTimeout, "port", ref, containerPort+"/tcp")
	if err != nil {
		return "", 0, err
	}
	return parseHostPort(firstLine(out))
}

// waitReady blocks until Postgres reports that it can accept queries or the
// bounded readiness deadline (min of readyTimeout and the caller's ctx)
// elapses. Each probe is itself context-bounded. Never unbounded.
func (p *Provisioner) waitReady(ctx context.Context, container string) error {
	dctx, cancel := context.WithTimeout(ctx, p.readyTimeout)
	defer cancel()
	for {
		probeCtx, probeCancel := context.WithTimeout(dctx, dockerQueryTimeout)
		err := p.probe(probeCtx, p.run, container, p.cfg.User, p.cfg.Database)
		probeCancel()
		if err == nil {
			return nil
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("postgres test server not ready within %s", p.readyTimeout)
		case <-time.After(readyPollInterval):
		}
	}
}

// connection builds the base pdb.Connection from the mapped endpoint and the
// server credentials. The credentials live only here (and in the run argv) —
// never in a label, name, filter or event.
func (p *Provisioner) connection(host string, port int) pdb.Connection {
	return pdb.Connection{
		Host:     host,
		Port:     port,
		User:     p.cfg.User,
		Password: p.password,
		Database: p.cfg.Database,
	}
}

// listIDs lists container IDs matching every filter. all includes stopped
// containers (`ps -a`), needed to catch SIGKILL leftovers during reaping.
func (p *Provisioner) listIDs(ctx context.Context, all bool, filters ...string) ([]string, error) {
	args := []string{"ps"}
	if all {
		args = append(args, "-a")
	}
	args = append(args, "--format", "{{.ID}}")
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out, err := p.docker(ctx, dockerQueryTimeout, args...)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// remove force-removes the given container IDs.
func (p *Provisioner) remove(ctx context.Context, ids []string) error {
	args := append([]string{"rm", "-f"}, ids...)
	_, err := p.docker(ctx, dockerQueryTimeout, args...)
	return err
}

// docker runs one docker invocation under a bounded sub-context so no single
// call can hang indefinitely.
func (p *Provisioner) docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return p.run(cctx, args...)
}

// execRunner is the production runner: it invokes the docker CLI under ctx and
// returns trimmed stdout. stderr is deliberately discarded from the surfaced
// value — the run argv carries the password, so nothing derived from a failed
// invocation (which could echo arguments) is ever returned to a caller.
func execRunner(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// postgresReady is the production readiness probe. The image ships pg_isready;
// forcing its loopback TCP connection avoids treating socket-only initialization
// as readiness. The caller supplies a bounded context for this invocation.
func postgresReady(ctx context.Context, run runner, container, user, database string) error {
	_, err := run(ctx, "exec", container, "pg_isready", "-h", "127.0.0.1", "-U", user, "-d", database)
	return err
}

// parseHostPort parses a `docker port` line (e.g. "127.0.0.1:49153") into a
// loopback host and port. A wildcard bind (0.0.0.0 / ::) is dialed via loopback.
func parseHostPort(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, errors.New("empty docker port mapping")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, fmt.Errorf("parse docker port mapping %q: %w", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("parse mapped port %q: %w", portStr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// firstLine returns the first non-empty trimmed line of s, or "".
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// nonEmptyLines returns every trimmed, non-empty line of s in order.
func nonEmptyLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}
