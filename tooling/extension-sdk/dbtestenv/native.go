// The native provider behind Up/Down: a PostgreSQL this run starts from the
// binaries installed on the machine, for a host where no Docker daemon answers.
//
// A container without a Docker daemon often has a PostgreSQL package installed
// (Debian's postgresql-16, Homebrew's postgresql@16). This provider finds its
// initdb, pg_ctl and postgres, initializes a throwaway data directory under a
// state root this OS user owns, and starts the server on an ephemeral loopback
// port with no Unix socket. It mirrors the docker Provisioner: the same role and
// database names, a digest-keyed identity, reuse of a still-running server whose
// digest matches, the reaping of every other one before it starts its own, and
// the same retained-until-`--infra-down` lifetime.
//
// PostgreSQL refuses to run as root. Under root, initdb, pg_ctl and the server
// run as the `postgres` OS user the distribution packages create, and the data
// directory belongs to it; without that user the provider is not usable.
//
// SECURITY. Unlike a container, this server is a process of the host, so it
// gets the guards a container boundary gave the docker one for free:
//
//   - The superuser password is random per data directory, generated at initdb
//     and kept in a 0600 file inside it, so no source-visible secret opens a
//     server whose superuser can run programs as the service user. It reaches
//     initdb through a 0600 file created exclusively and removed right after,
//     psql through PGPASSWORD, and the returned pdb.Connection; never an argv,
//     a directory name, a lease or an error.
//   - The state root must be a real directory this OS user owns that nobody
//     else can write, or nothing is started in it.
//   - The programs run with an allowlisted environment: the server outlives
//     this process, under another OS user, and must not carry the credentials
//     of the run that started it.
//   - Under root the service user's own groups replace root's.
package dbtestenv

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	pdb "go.putnami.dev/protocol/database"

	"go.putnami.dev/sdk/extension/procstat"
)

// nativeProviderID names the resource kind a native lease reclaims: a data
// directory and the server running on it.
const nativeProviderID = "postgres-native"

// nativeImagePrefix marks a native server's identity in serverConfig.Image, so
// a native digest can never equal a docker one.
const nativeImagePrefix = "native:"

// nativeDirPrefix names each data directory: nativeDirPrefix + digest, the
// same deterministic name the docker container carries.
const nativeDirPrefix = namePrefix

// nativeServiceUser is the OS user PostgreSQL runs as when this process is
// root. Distribution packages create it.
const nativeServiceUser = "postgres"

const (
	// nativeStartTimeout bounds pg_ctl's wait for the server to accept
	// connections, in seconds as pg_ctl takes it.
	nativeStartTimeout = 60
	// nativeCommandTimeout bounds one initdb, pg_ctl or psql invocation.
	nativeCommandTimeout = 2 * time.Minute
	// nativeDialTimeout bounds one readiness dial of a reused server.
	nativeDialTimeout = 2 * time.Second
)

// nativeBinaries are the programs a bin directory must hold to serve.
var nativeBinaries = []string{"initdb", "pg_ctl", "postgres"}

// nativeBinDirGlobs are the install locations searched after PATH, newest
// version first: Debian and Ubuntu, RHEL's PGDG packages, and Homebrew on both
// architectures.
var nativeBinDirGlobs = []string{
	"/usr/lib/postgresql/*/bin",
	"/usr/pgsql-*/bin",
	"/opt/homebrew/opt/postgresql@*/bin",
	"/usr/local/opt/postgresql@*/bin",
}

// errNoNativePostgres is what Provision reports when no install was found.
var errNoNativePostgres = errors.New("no PostgreSQL install found: initdb, pg_ctl and postgres are neither on PATH nor in " +
	strings.Join(nativeBinDirGlobs, ", "))

// nativeCommand is one program invocation. asService runs it as the service
// user when this process is root; detach starts it in its own session so the
// server it leaves running outlives this process and its process group.
type nativeCommand struct {
	path      string
	args      []string
	env       []string
	dir       string
	asService bool
	detach    bool
}

// nativeRunner runs a command and returns its trimmed stdout. It is the seam
// tests replace to assert argv without a PostgreSQL install.
type nativeRunner func(ctx context.Context, cmd nativeCommand) (string, error)

// nativeHost is what the provider needs from the machine besides running
// programs: where the binaries are, a free port, a readiness dial, and the
// platform's file locking and ownership. Tests replace it whole.
type nativeHost struct {
	findBinDir func() (string, bool)
	freePort   func() (int, error)
	dial       func(ctx context.Context, host string, port int) error
	// prepareRoot creates the state root, traversable by the service user.
	prepareRoot func(dir string) error
	// prepareDir creates dir at mode 0700, owned by the service user under
	// root.
	prepareDir func(dir string) error
	// serviceIDs is the uid and gid files the service user reads must have:
	// the service user's under root, -1 and -1 otherwise.
	serviceIDs func() (int, int, error)
	// alive reports whether a process runs under pid.
	alive func(pid int) bool
	// lock takes an exclusive lock on path and returns its release.
	lock func(path string) (func(), error)
}

// NativeServer starts a PostgreSQL from the binaries installed on the machine.
// The zero value is not usable; construct one with NewNativeServer.
type NativeServer struct {
	run          nativeRunner
	host         nativeHost
	stateRoot    string
	user         string
	database     string
	readyTimeout time.Duration
}

// NewNativeServer returns a NativeServer wired to the machine: the binaries
// found on PATH or in nativeBinDirGlobs, the OS temporary directory, and the
// same role and database as the docker Provisioner.
func NewNativeServer() *NativeServer {
	return &NativeServer{
		run:          nativeExec,
		host:         machineNativeHost(),
		stateRoot:    nativeStateRoot(),
		user:         "putnami",
		database:     "putnami_test",
		readyTimeout: DefaultReadyTimeout,
	}
}

// nativeStateRoot is the directory every data directory lives in, one per OS
// user. A non-root user keeps it in its own cache directory; root keeps it
// under the temporary directory, which the service user can traverse, and
// prepareRoot refuses one root does not own or others can write.
func nativeStateRoot() string {
	if os.Getuid() != 0 {
		if cache, err := os.UserCacheDir(); err == nil {
			return filepath.Join(cache, "putnami", "test-pg")
		}
	}
	return filepath.Join(os.TempDir(), "putnami-test-pg-"+strconv.Itoa(os.Getuid()))
}

// passwordFile names the file inside a data directory that keeps its
// superuser password.
const passwordFile = "putnami.password"

// nativePasswordBytes is the entropy of a generated superuser password.
const nativePasswordBytes = 24

// id names the resource kind a native lease reclaims.
func (n *NativeServer) id() string { return nativeProviderID }

// ownsServer reports true: this provider starts the server it hands back, so
// the auto-only, never-on-CI gate applies to it in full.
func (n *NativeServer) ownsServer() bool { return true }

// bindingDefaults states no policy, like the docker provider: the server lives
// across runs and belongs to this machine, so the runtime keeps its own
// defaults.
func (n *NativeServer) bindingDefaults() TestPolicy { return TestPolicy{} }

// available reports whether an install was found. A PATH and glob lookup only.
func (n *NativeServer) available() bool {
	if n == nil || n.host.findBinDir == nil {
		return false
	}
	_, ok := n.host.findBinDir()
	return ok
}

// LabelKey is empty: a native server carries no label to filter by; its data
// directory name is its identity.
func (n *NativeServer) LabelKey() string { return "" }

// ContainerName is the data directory name for a digest.
func (n *NativeServer) ContainerName(digest string) string { return nativeDirPrefix + digest }

// config is the digest-affecting identity of the server: the install it runs
// and the role and database it serves.
func (n *NativeServer) config(binDir string) serverConfig {
	return serverConfig{Image: nativeImagePrefix + binDir, User: n.user, Database: n.database}
}

// Digest is the configuration digest of the server this provider would start,
// or "" when no install is found.
func (n *NativeServer) Digest() string {
	binDir, ok := n.host.findBinDir()
	if !ok {
		return ""
	}
	return n.config(binDir).digest()
}

// Provision returns a ready connection to the server for the current install,
// reusing a running one or initializing and starting a new one.
func (n *NativeServer) Provision() (pdb.Connection, string, error) {
	binDir, ok := n.host.findBinDir()
	if !ok {
		return pdb.Connection{}, "", errNoNativePostgres
	}
	digest := n.config(binDir).digest()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProvisionTimeout)
	defer cancel()

	if err := n.host.prepareRoot(n.stateRoot); err != nil {
		return pdb.Connection{}, digest, fmt.Errorf("prepare %s: %w", n.stateRoot, err)
	}
	unlock, err := n.host.lock(n.lockPath())
	if err != nil {
		return pdb.Connection{}, digest, fmt.Errorf("lock %s: %w", n.stateRoot, err)
	}
	defer unlock()

	// Recovery before provisioning, as the docker provider does: a server for
	// another install or configuration is stopped and its directory removed.
	_ = n.removeDirs(func(other string) bool { return other != digest })

	port, password, err := n.ensureRunning(ctx, binDir, digest)
	if err != nil {
		return pdb.Connection{}, digest, err
	}
	if err := n.ensureDatabase(ctx, binDir, port, password); err != nil {
		return pdb.Connection{}, digest, err
	}
	return pdb.Connection{
		Host:     "127.0.0.1",
		Port:     port,
		User:     n.user,
		Password: password,
		Database: n.database,
	}, digest, nil
}

// ensureRunning returns the port and superuser password of the server on the
// digest's data directory, starting it, and initializing the directory first,
// when it is not running.
func (n *NativeServer) ensureRunning(ctx context.Context, binDir, digest string) (int, string, error) {
	dataDir := n.dataDir(digest)
	password, havePassword := readPassword(dataDir)
	if port, ok := n.runningPort(ctx, binDir, dataDir); ok {
		if err := n.waitReady(ctx, port); err == nil && havePassword {
			return port, password, nil
		}
		// A server that does not answer, or whose password is lost, is
		// stopped and, without its password, initialized again.
		_ = n.stop(ctx, binDir, dataDir)
	}
	if !n.initialized(dataDir) || !havePassword {
		var err error
		if password, err = n.initdb(ctx, binDir, dataDir); err != nil {
			return 0, "", err
		}
	}
	port, err := n.host.freePort()
	if err != nil {
		return 0, "", fmt.Errorf("pick a port for the native postgres: %w", err)
	}
	logFile := filepath.Join(dataDir, "server.log")
	if _, err := n.run(ctx, nativeCommand{
		path: filepath.Join(binDir, "pg_ctl"),
		args: []string{"-D", dataDir, "-l", logFile, "-o", "-p " + strconv.Itoa(port),
			"-w", "-t", strconv.Itoa(nativeStartTimeout), "start"},
		dir:       n.stateRoot,
		asService: true,
		detach:    true,
	}); err != nil {
		return 0, "", fmt.Errorf("start the native postgres (log: %s): %w", logFile, err)
	}
	if err := n.waitReady(ctx, port); err != nil {
		return 0, "", err
	}
	return port, password, nil
}

// readPassword reads the superuser password a data directory keeps.
func readPassword(dataDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, passwordFile))
	if err != nil {
		return "", false
	}
	password := strings.TrimSpace(string(data))
	return password, password != ""
}

// newPassword returns a random superuser password.
func newPassword() (string, error) {
	raw := make([]byte, nativePasswordBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// initdb initializes dataDir with the superuser role, under a new random
// password it keeps in the directory and returns, and the settings a throwaway
// test server wants: loopback TCP only, no Unix socket, and no durability it
// would pay for and never use.
func (n *NativeServer) initdb(ctx context.Context, binDir, dataDir string) (string, error) {
	// A server a temp cleaner left running on a damaged directory is stopped
	// before the directory goes.
	if _, err := os.Stat(filepath.Join(dataDir, "postmaster.pid")); err == nil {
		_ = n.stop(ctx, binDir, dataDir)
	}
	_ = os.RemoveAll(dataDir)
	if err := n.host.prepareDir(dataDir); err != nil {
		return "", fmt.Errorf("create %s: %w", dataDir, err)
	}
	password, err := newPassword()
	if err != nil {
		return "", fmt.Errorf("generate the native postgres password: %w", err)
	}
	// initdb wants an empty directory, so its password file lives beside it.
	pwFile := dataDir + ".pw"
	if err := n.writeServiceFile(pwFile, password+"\n"); err != nil {
		return "", fmt.Errorf("write the initdb password file: %w", err)
	}
	defer func() { _ = os.Remove(pwFile) }()
	if _, err := n.run(ctx, nativeCommand{
		path: filepath.Join(binDir, "initdb"),
		args: []string{"-D", dataDir, "-U", n.user, "--pwfile=" + pwFile,
			"--auth=scram-sha-256", "--encoding=UTF8", "--no-locale"},
		dir:       n.stateRoot,
		asService: true,
	}); err != nil {
		_ = os.RemoveAll(dataDir)
		return "", fmt.Errorf("initialize the native postgres data directory: %w", err)
	}
	settings := strings.Join([]string{
		"",
		"# putnami: a throwaway test server",
		"listen_addresses = '127.0.0.1'",
		"unix_socket_directories = ''",
		"fsync = off",
		"synchronous_commit = off",
		"full_page_writes = off",
		"",
	}, "\n")
	f, err := os.OpenFile(filepath.Join(dataDir, "postgresql.conf"), os.O_APPEND|os.O_WRONLY|syscallNoFollow, 0)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return "", fmt.Errorf("configure the native postgres: %w", err)
	}
	_, werr := f.WriteString(settings)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.RemoveAll(dataDir)
		return "", fmt.Errorf("configure the native postgres: %w", err)
	}
	// The password stays with this process's user, inside the 0700 directory.
	if err := writeExclusive(filepath.Join(dataDir, passwordFile), password+"\n", -1, -1); err != nil {
		_ = os.RemoveAll(dataDir)
		return "", fmt.Errorf("keep the native postgres password: %w", err)
	}
	return password, nil
}

// ensureDatabase creates the test database once.
func (n *NativeServer) ensureDatabase(ctx context.Context, binDir string, port int, password string) error {
	out, err := n.psql(ctx, binDir, port, password, "SELECT 1 FROM pg_database WHERE datname = "+pgQuoteLiteral(n.database))
	if err != nil {
		return fmt.Errorf("read the databases of the native postgres: %w", err)
	}
	if strings.TrimSpace(out) == "1" {
		return nil
	}
	if _, err := n.psql(ctx, binDir, port, password, "CREATE DATABASE "+pgQuoteIdent(n.database)); err != nil {
		return fmt.Errorf("create database %s: %w", n.database, err)
	}
	return nil
}

// psql runs one statement against the maintenance database over loopback TCP.
// The password travels in PGPASSWORD, never in the argv, and -X keeps the
// invoking user's ~/.psqlrc from reshaping the output.
func (n *NativeServer) psql(ctx context.Context, binDir string, port int, password, statement string) (string, error) {
	path := filepath.Join(binDir, "psql")
	if !isExecutable(path) {
		found, err := exec.LookPath("psql")
		if err != nil {
			return "", errors.New("psql is neither beside initdb nor on PATH")
		}
		path = found
	}
	return n.run(ctx, nativeCommand{
		path: path,
		args: []string{"-X", "-h", "127.0.0.1", "-p", strconv.Itoa(port), "-U", n.user, "-d", "postgres",
			"-v", "ON_ERROR_STOP=1", "-Atc", statement},
		env: []string{"PGPASSWORD=" + password, "PGCONNECT_TIMEOUT=5"},
		dir: n.stateRoot,
	})
}

// runningPort reports the port of the server running on dataDir, read from the
// postmaster.pid its postmaster writes, when pg_ctl says one is running.
func (n *NativeServer) runningPort(ctx context.Context, binDir, dataDir string) (int, bool) {
	if !n.initialized(dataDir) {
		return 0, false
	}
	if _, err := n.run(ctx, nativeCommand{
		path:      filepath.Join(binDir, "pg_ctl"),
		args:      []string{"-D", dataDir, "status"},
		dir:       n.stateRoot,
		asService: true,
	}); err != nil {
		return 0, false
	}
	port, err := postmasterPort(filepath.Join(dataDir, "postmaster.pid"))
	if err != nil {
		return 0, false
	}
	return port, true
}

// postmasterPort reads the port line (the fourth) of a postmaster.pid file.
func postmasterPort(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		if line == 4 {
			return strconv.Atoi(strings.TrimSpace(scanner.Text()))
		}
	}
	return 0, fmt.Errorf("%s has no port line", path)
}

// postmasterPID reads the process id line (the first) of a postmaster.pid file.
func postmasterPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	return pid, err == nil && pid > 0
}

// waitReady waits, bounded, until the server accepts TCP connections.
func (n *NativeServer) waitReady(ctx context.Context, port int) error {
	dctx, cancel := context.WithTimeout(ctx, n.readyTimeout)
	defer cancel()
	for {
		probeCtx, probeCancel := context.WithTimeout(dctx, nativeDialTimeout)
		err := n.host.dial(probeCtx, "127.0.0.1", port)
		probeCancel()
		if err == nil {
			return nil
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("native postgres test server not ready within %s", n.readyTimeout)
		case <-time.After(readyPollInterval):
		}
	}
}

// stop stops the server on dataDir; stopping a stopped server fails, which the
// callers ignore.
func (n *NativeServer) stop(ctx context.Context, binDir, dataDir string) error {
	_, err := n.run(ctx, nativeCommand{
		path:      filepath.Join(binDir, "pg_ctl"),
		args:      []string{"-D", dataDir, "-m", "fast", "-w", "stop"},
		dir:       n.stateRoot,
		asService: true,
	})
	return err
}

// TeardownAll stops every server under the state root and removes its data
// directory. It backs `--infra-down`.
func (n *NativeServer) TeardownAll() error {
	return n.removeWhere(func(string) bool { return true })
}

// removeWhere takes the lock, then stops and removes the servers match selects.
func (n *NativeServer) removeWhere(match func(digest string) bool) error {
	if _, err := os.Stat(n.stateRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := n.host.lock(n.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	return n.removeDirs(match)
}

// removeDirs stops and removes the data directories match selects. A
// directory whose server still runs and cannot be stopped, because its install
// is gone, is kept and reported rather than removed under the live server.
func (n *NativeServer) removeDirs(match func(digest string) bool) error {
	entries, err := os.ReadDir(n.stateRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProvisionTimeout)
	defer cancel()
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, nativeDirPrefix) {
			continue
		}
		digest := strings.TrimPrefix(name, nativeDirPrefix)
		if !match(digest) {
			continue
		}
		dataDir := filepath.Join(n.stateRoot, name)
		if binDir, ok := n.host.findBinDir(); ok && n.initialized(dataDir) {
			if _, running := n.runningPort(ctx, binDir, dataDir); running {
				if stopErr := n.stop(ctx, binDir, dataDir); stopErr != nil {
					errs = append(errs, fmt.Errorf("stop the native postgres in %s: %w", dataDir, stopErr))
					continue
				}
			}
		}
		if pid, ok := postmasterPID(filepath.Join(dataDir, "postmaster.pid")); ok && n.host.alive(pid) {
			errs = append(errs, fmt.Errorf("the postgres in %s still runs as process %d", dataDir, pid))
			continue
		}
		if err := os.RemoveAll(dataDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// initialized reports whether dataDir holds an initialized cluster.
func (n *NativeServer) initialized(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "PG_VERSION"))
	return err == nil
}

func (n *NativeServer) dataDir(digest string) string {
	return filepath.Join(n.stateRoot, n.ContainerName(digest))
}

func (n *NativeServer) lockPath() string { return filepath.Join(n.stateRoot, "lock") }

// writeServiceFile writes a new 0600 file the service user can read.
func (n *NativeServer) writeServiceFile(path, content string) error {
	uid, gid, err := n.host.serviceIDs()
	if err != nil {
		return err
	}
	return writeExclusive(path, content, uid, gid)
}

// writeExclusive creates path anew, refusing to follow a symbolic link or to
// open a file that already exists, writes content at mode 0600, and gives the
// open file to uid and gid unless they are -1.
func writeExclusive(path, content string, uid, gid int) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscallNoFollow, 0o600)
	if err != nil {
		return err
	}
	if uid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			_ = f.Close()
			return err
		}
	}
	_, werr := f.WriteString(content)
	return errors.Join(werr, f.Close())
}

// --- isolation --------------------------------------------------------------

var _ DatabaseIsolator = (*NativeServer)(nil)

// CreateDatabase creates name on the server provisioned for digest.
func (n *NativeServer) CreateDatabase(ctx context.Context, digest, name string) error {
	if err := validateIdentifier(name); err != nil {
		return err
	}
	if _, err := n.psqlFor(ctx, digest, "CREATE DATABASE "+pgQuoteIdent(name)); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// DropDatabase drops name on the server provisioned for digest, terminating
// the sessions still open on it.
func (n *NativeServer) DropDatabase(ctx context.Context, digest, name string) error {
	if err := validateIdentifier(name); err != nil {
		return err
	}
	if _, err := n.psqlFor(ctx, digest, "DROP DATABASE IF EXISTS "+pgQuoteIdent(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// ListDatabases lists the databases of the server provisioned for digest whose
// name starts with prefix.
func (n *NativeServer) ListDatabases(ctx context.Context, digest, prefix string) ([]string, error) {
	query := "SELECT datname FROM pg_database WHERE datname LIKE " + pgQuoteLiteral(escapeLikePattern(prefix)+"%") +
		` ESCAPE '\' ORDER BY datname`
	out, err := n.psqlFor(ctx, digest, query)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	return nonEmptyLines(out), nil
}

// psqlFor runs a statement on the running server of digest.
func (n *NativeServer) psqlFor(ctx context.Context, digest, statement string) (string, error) {
	if digest == "" {
		return "", errors.New("no server digest: provision the server first")
	}
	binDir, ok := n.host.findBinDir()
	if !ok {
		return "", errNoNativePostgres
	}
	dataDir := n.dataDir(digest)
	port, running := n.runningPort(ctx, binDir, dataDir)
	if !running {
		return "", fmt.Errorf("no native postgres is running for digest %s", digest)
	}
	password, ok := readPassword(dataDir)
	if !ok {
		return "", fmt.Errorf("the native postgres for digest %s keeps no password", digest)
	}
	return n.psql(ctx, binDir, port, password, statement)
}

// Serves reports whether a data directory for digest exists, which is how a
// composition's recorded digest finds the provider that started its server.
func (n *NativeServer) Serves(digest string) bool {
	if digest == "" {
		return false
	}
	info, err := os.Lstat(n.dataDir(digest))
	return err == nil && info.IsDir()
}

// --- the machine ---------------------------------------------------------------

// findNativeBinDir returns the first directory holding every nativeBinaries
// program: the directory of pg_ctl or initdb on PATH, then the newest of the
// install locations nativeBinDirGlobs names.
func findNativeBinDir() (string, bool) {
	var candidates []string
	for _, program := range []string{"pg_ctl", "initdb"} {
		if path, err := exec.LookPath(program); err == nil {
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				candidates = append(candidates, filepath.Dir(resolved))
			}
			candidates = append(candidates, filepath.Dir(path))
		}
	}
	for _, pattern := range nativeBinDirGlobs {
		matches, _ := filepath.Glob(pattern)
		slices.SortFunc(matches, compareVersionedPaths)
		slices.Reverse(matches)
		candidates = append(candidates, matches...)
	}
	for _, dir := range candidates {
		if holdsNativeBinaries(dir) {
			return dir, true
		}
	}
	return "", false
}

// holdsNativeBinaries reports whether dir holds every program a server needs.
func holdsNativeBinaries(dir string) bool {
	for _, program := range nativeBinaries {
		if !isExecutable(filepath.Join(dir, program)) {
			return false
		}
	}
	return true
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

// compareVersionedPaths orders paths by the first number each holds, so
// postgresql/9 sorts before postgresql/16; equal numbers fall back to the text.
func compareVersionedPaths(a, b string) int {
	va, vb := firstNumber(a), firstNumber(b)
	if va != vb {
		return va - vb
	}
	return strings.Compare(a, b)
}

func firstNumber(s string) int {
	start := strings.IndexAny(s, "0123456789")
	if start < 0 {
		return -1
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[start:end])
	if err != nil {
		return -1
	}
	return n
}

// freeLoopbackPort asks the kernel for a free loopback port.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if err := l.Close(); err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("listener address %s is not TCP", l.Addr())
	}
	return addr.Port, nil
}

// nativeExec is the production runner. A failure carries the program's last
// stderr line: none of these programs receives the password on its argv, so
// its output cannot echo one.
func nativeExec(ctx context.Context, c nativeCommand) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, nativeCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.path, c.args...)
	cmd.Dir = c.dir
	attr, home, err := nativeSysProcAttr(c.asService, c.detach)
	if err != nil {
		return "", err
	}
	cmd.Env = nativeEnv(os.Environ(), home, c.env)
	cmd.SysProcAttr = attr
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if c.detach {
		// The server pg_ctl leaves running keeps no handle on this process.
		cmd.Stdout = nil
		cmd.Stdin = nil
		if err := procstat.Run(cmd); err != nil {
			return "", commandError(c.path, err, stderr.String())
		}
		return "", nil
	}
	out, err := procstat.Output(cmd)
	if err != nil {
		return "", commandError(c.path, err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// commandError names the program and its last stderr line.
func commandError(path string, err error, stderr string) error {
	lines := nonEmptyLines(stderr)
	if len(lines) == 0 {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return fmt.Errorf("%s: %w: %s", filepath.Base(path), err, lines[len(lines)-1])
}

// nativeInheritedEnv names the only variables the programs inherit: what
// finds the binaries and sets their locale and time zone. The server outlives
// this process, possibly under another OS user, so the rest of the run's
// environment, its credentials included, must not reach it.
var nativeInheritedEnv = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ"}

// nativeEnv is the environment of one program: the allowlisted inherited
// variables, HOME (home when set, the inherited one otherwise), then extra.
func nativeEnv(inherited []string, home string, extra []string) []string {
	var env []string
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if slices.Contains(nativeInheritedEnv, name) || (name == "HOME" && home == "") {
			env = append(env, entry)
		}
	}
	if home != "" {
		env = append(env, "HOME="+home)
	}
	return append(env, extra...)
}
