// The provider a run gets when the pipeline names no server: a Postgres this
// run starts on the machine, in a Docker container when a Docker daemon
// answers, from the native install otherwise.
//
// The choice is made inside Provision, not in SelectProvider, because it costs
// a call to the Docker daemon, and the fail-closed gate must decide whether the
// run may start a server before anything is asked of Docker. Both halves own
// the server they start, so the auto-only, never-on-CI gate applies whichever
// one serves.
package dbtestenv

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	pdb "go.putnami.dev/protocol/database"
)

// dockerProbeTimeout bounds the call that asks whether a Docker daemon answers.
const dockerProbeTimeout = 10 * time.Second

// LocalServer starts a Postgres on this machine: Docker's when its daemon
// answers, the native install's otherwise. Construct one with NewLocalServer.
type LocalServer struct {
	docker *Provisioner
	native *NativeServer
	// dockerAnswers reports whether a Docker daemon answers. Called only once
	// the gate allowed the run to start a server.
	dockerAnswers func() bool
	// chosen is the half the last Provision used.
	chosen Provider
}

// NewLocalServer returns the local provider wired to the machine.
func NewLocalServer() *LocalServer {
	return &LocalServer{
		docker:        NewProvisioner(),
		native:        NewNativeServer(),
		dockerAnswers: dockerDaemonAnswers,
	}
}

// dockerDaemonAnswers asks the Docker daemon for its version, bounded. A docker
// binary with no daemon behind it, the common case in a container, answers no.
func dockerDaemonAnswers() bool {
	if !DockerAvailable() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// current is the half that serves: the one the last Provision chose, or
// Docker before any.
func (l *LocalServer) current() Provider {
	if l.chosen != nil {
		return l.chosen
	}
	return l.docker
}

func (l *LocalServer) id() string                  { return l.current().id() }
func (l *LocalServer) ownsServer() bool            { return true }
func (l *LocalServer) bindingDefaults() TestPolicy { return TestPolicy{} }

// available reports whether either half can serve: a docker binary on PATH or a
// native install. Lookups only, never a daemon call.
func (l *LocalServer) available() bool {
	return l.docker.available() || l.native.available()
}

// LabelKey is the label of the half that serves.
func (l *LocalServer) LabelKey() string { return l.current().LabelKey() }

// ContainerName is the resource name of the half that serves.
func (l *LocalServer) ContainerName(digest string) string { return l.current().ContainerName(digest) }

// Provision starts or reuses a Docker container when a Docker daemon answers,
// and the native server otherwise.
func (l *LocalServer) Provision() (pdb.Connection, string, error) {
	if l.docker.available() && l.dockerAnswers() {
		l.chosen = l.docker
		return l.docker.Provision()
	}
	if !l.native.available() {
		return pdb.Connection{}, "", fmt.Errorf("no Docker daemon answers, and %w", errNoNativePostgres)
	}
	l.chosen = l.native
	conn, digest, err := l.native.Provision()
	if err != nil {
		return conn, digest, fmt.Errorf("no Docker daemon answers, and the native postgres did not start: %w", err)
	}
	return conn, digest, nil
}

// TeardownAll removes what either half started: the Docker containers when a
// Docker daemon answers, and every native server.
func (l *LocalServer) TeardownAll() error {
	var errs []error
	if l.docker.available() && l.dockerAnswers() {
		errs = append(errs, l.docker.TeardownAll())
	}
	errs = append(errs, l.native.TeardownAll())
	return errors.Join(errs...)
}

var _ DatabaseIsolator = (*LocalServer)(nil)

// isolatorFor routes a digest to the half that started its server: the
// native one when it keeps a data directory for that digest, which survives an
// upgrade of the install, Docker otherwise.
func (l *LocalServer) isolatorFor(digest string) DatabaseIsolator {
	if l.native.Serves(digest) {
		return l.native
	}
	return l.docker
}

// CreateDatabase creates name on the server of digest.
func (l *LocalServer) CreateDatabase(ctx context.Context, digest, name string) error {
	return l.isolatorFor(digest).CreateDatabase(ctx, digest, name)
}

// DropDatabase drops name on the server of digest.
func (l *LocalServer) DropDatabase(ctx context.Context, digest, name string) error {
	return l.isolatorFor(digest).DropDatabase(ctx, digest, name)
}

// ListDatabases lists the databases of the server of digest that start with
// prefix.
func (l *LocalServer) ListDatabases(ctx context.Context, digest, prefix string) ([]string, error) {
	return l.isolatorFor(digest).ListDatabases(ctx, digest, prefix)
}
