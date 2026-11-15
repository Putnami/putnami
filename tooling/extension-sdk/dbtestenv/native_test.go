package dbtestenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The native provider, proven against a fake runner and a fake machine so no
// case needs a PostgreSQL install, plus one test against the install of a
// contributor's machine.

// fakeNative records every command and scripts the machine around it.
type fakeNative struct {
	mu       sync.Mutex
	calls    []nativeCommand
	respond  func(c nativeCommand) (string, error)
	binDir   string
	found    bool
	dialErr  error
	alive    bool
	prepared []string
}

func (f *fakeNative) run(_ context.Context, c nativeCommand) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(c)
	}
	return "", nil
}

func (f *fakeNative) programs() []string {
	var out []string
	for _, c := range f.calls {
		program := filepath.Base(c.path)
		if program == "pg_ctl" {
			program += " " + c.args[len(c.args)-1]
		}
		out = append(out, program)
	}
	return out
}

func (f *fakeNative) call(program string) (nativeCommand, bool) {
	for _, c := range f.calls {
		name := filepath.Base(c.path)
		if name == "pg_ctl" {
			name += " " + c.args[len(c.args)-1]
		}
		if name == program {
			return c, true
		}
	}
	return nativeCommand{}, false
}

// testNativeServer is a NativeServer on f, whose state lives in a test
// directory. initdb writes PG_VERSION, the way the real one does, and a
// started server writes its postmaster.pid.
func testNativeServer(t *testing.T, f *fakeNative) *NativeServer {
	t.Helper()
	if f.binDir == "" {
		// A psql beside the fake install keeps the provider off the PATH one.
		f.binDir = t.TempDir()
		if err := os.WriteFile(filepath.Join(f.binDir, "psql"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &NativeServer{
		run: f.run,
		host: nativeHost{
			findBinDir:  func() (string, bool) { return f.binDir, f.found },
			freePort:    func() (int, error) { return 54321, nil },
			dial:        func(context.Context, string, int) error { return f.dialErr },
			prepareRoot: func(dir string) error { return os.MkdirAll(dir, 0o700) },
			prepareDir:  func(dir string) error { f.prepared = append(f.prepared, dir); return os.MkdirAll(dir, 0o700) },
			serviceIDs:  func() (int, int, error) { return -1, -1, nil },
			alive:       func(int) bool { return f.alive },
			lock:        func(string) (func(), error) { return func() {}, nil },
		},
		stateRoot:    t.TempDir(),
		user:         "putnami",
		database:     "putnami_test",
		readyTimeout: 50 * time.Millisecond,
	}
}

// scriptedServer answers like a machine with one install: initdb creates the
// cluster, pg_ctl start writes postmaster.pid, status succeeds while it runs,
// and psql finds the test database once created.
func scriptedServer(t *testing.T) (*fakeNative, *NativeServer) {
	t.Helper()
	f := &fakeNative{found: true}
	n := testNativeServer(t, f)
	running := false
	created := false
	f.respond = func(c nativeCommand) (string, error) {
		switch filepath.Base(c.path) {
		case "initdb":
			dir := c.args[1]
			if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(dir, "postgresql.conf"), nil, 0o600)
		case "pg_ctl":
			dir := c.args[1]
			switch c.args[len(c.args)-1] {
			case "start":
				running = true
				return "", os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte("42\n"+dir+"\n1\n54321\n"), 0o600)
			case "status":
				if running {
					return "pg_ctl: server is running", nil
				}
				return "", errors.New("exit status 3")
			case "stop":
				running = false
				return "", nil
			}
		case "psql":
			statement := c.args[len(c.args)-1]
			if strings.HasPrefix(statement, "CREATE DATABASE") {
				created = true
				return "", nil
			}
			if strings.HasPrefix(statement, "SELECT 1") && created {
				return "1", nil
			}
		}
		return "", nil
	}
	return f, n
}

// The first provision initializes a cluster with the provider's role under a
// random password the data directory keeps, starts it detached as the service
// user on a loopback port, and creates the test database. The password reaches
// no argv: initdb reads it from a file, psql from its environment.
func TestNativeServerInitializesStartsAndCreatesTheDatabase(t *testing.T) {
	f, n := scriptedServer(t)

	conn, digest, err := n.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	dataDir := filepath.Join(n.stateRoot, nativeDirPrefix+digest)
	kept, ok := readPassword(dataDir)
	if !ok || len(kept) != 2*nativePasswordBytes || conn.Password != kept {
		t.Errorf("password %q, kept %q, want a random one the data directory keeps", conn.Password, kept)
	}
	if conn.Host != "127.0.0.1" || conn.Port != 54321 || conn.User != "putnami" || conn.Database != "putnami_test" {
		t.Errorf("connection = %+v", conn)
	}
	if digest == "" || digest != n.Digest() {
		t.Errorf("digest = %q, want the configuration digest %q", digest, n.Digest())
	}
	want := []string{"initdb", "pg_ctl start", "psql", "psql"}
	if got := f.programs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commands = %v, want %v", got, want)
	}

	initdb, _ := f.call("initdb")
	if !initdb.asService || !hasArg(initdb.args, "-U", "putnami") || !hasArg(initdb.args, "--auth=scram-sha-256") {
		t.Errorf("initdb = %+v, want the putnami role, scram auth, as the service user", initdb)
	}
	start, _ := f.call("pg_ctl start")
	if !start.asService || !start.detach || !hasArg(start.args, "-o", "-p 54321") {
		t.Errorf("pg_ctl start = %+v, want a detached start as the service user on the picked port", start)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.args, " "), conn.Password) {
			t.Errorf("%s argv carries the password: %v", filepath.Base(c.path), c.args)
		}
	}
	psql, _ := f.call("psql")
	if !hasArg(psql.env, "PGPASSWORD="+conn.Password) || psql.asService || psql.args[0] != "-X" {
		t.Errorf("psql = %+v, want the password in its environment, no psqlrc, as this user", psql)
	}

	conf, err := os.ReadFile(filepath.Join(dataDir, "postgresql.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"listen_addresses = '127.0.0.1'", "unix_socket_directories = ''", "fsync = off"} {
		if !strings.Contains(string(conf), setting) {
			t.Errorf("postgresql.conf lacks %q:\n%s", setting, conf)
		}
	}
	if _, err := os.Stat(dataDir + ".pw"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the initdb password file is left behind: %v", err)
	}
}

// A running server with the current digest is reused: no initdb, no start.
func TestNativeServerReusesARunningServer(t *testing.T) {
	f, n := scriptedServer(t)
	if _, _, err := n.Provision(); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	f.calls = nil

	first, _ := readPassword(filepath.Join(n.stateRoot, nativeDirPrefix+n.Digest()))
	conn, _, err := n.Provision()
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if conn.Port != 54321 || conn.Password != first {
		t.Errorf("connection %+v, want the running server's port 54321 and its kept password", conn)
	}
	want := []string{"pg_ctl status", "psql"}
	if got := f.programs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commands = %v, want %v", got, want)
	}
}

// A data directory of another install is stopped and removed before the
// current one starts, as the docker provider reaps mismatched containers.
func TestNativeServerReapsAnotherInstallsServer(t *testing.T) {
	_, n := scriptedServer(t)
	stale := filepath.Join(n.stateRoot, nativeDirPrefix+"0123456789abcdef")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale data directory survived: %v", err)
	}
}

// TeardownAll stops every server and removes every data directory.
func TestNativeServerTeardownAllStopsAndRemoves(t *testing.T) {
	f, n := scriptedServer(t)
	_, digest, err := n.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	f.calls = nil
	if err := n.TeardownAll(); err != nil {
		t.Fatalf("TeardownAll: %v", err)
	}
	if _, ok := f.call("pg_ctl stop"); !ok {
		t.Errorf("commands = %v, want a pg_ctl stop", f.programs())
	}
	if _, err := os.Stat(filepath.Join(n.stateRoot, nativeDirPrefix+digest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data directory survived: %v", err)
	}
}

// No install is an error that names where the provider looked, and nothing runs.
func TestNativeServerWithoutAnInstall(t *testing.T) {
	f := &fakeNative{found: false}
	n := testNativeServer(t, f)
	if n.available() {
		t.Error("available without an install")
	}
	if _, _, err := n.Provision(); !errors.Is(err, errNoNativePostgres) {
		t.Errorf("Provision error = %v, want errNoNativePostgres", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("commands ran without an install: %v", f.programs())
	}
}

// A failed initdb leaves no half-initialized directory a later run would
// mistake for a cluster.
func TestNativeServerRemovesAFailedInitialization(t *testing.T) {
	f := &fakeNative{found: true}
	n := testNativeServer(t, f)
	f.respond = func(c nativeCommand) (string, error) {
		if filepath.Base(c.path) == "initdb" {
			return "", errors.New("initdb: cannot be run as root")
		}
		return "", errors.New("exit status 3")
	}
	_, digest, err := n.Provision()
	if err == nil || !strings.Contains(err.Error(), "cannot be run as root") {
		t.Fatalf("Provision error = %v, want initdb's cause", err)
	}
	if _, err := os.Stat(filepath.Join(n.stateRoot, nativeDirPrefix+digest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed data directory survived: %v", err)
	}
}

// The isolator statements run on the server of the digest, through psql over
// loopback, with quoted identifiers.
func TestNativeServerIsolatesDatabases(t *testing.T) {
	f, n := scriptedServer(t)
	_, digest, err := n.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	f.calls = nil
	if err := n.CreateDatabase(context.Background(), digest, `compose_a"b`); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if err := n.DropDatabase(context.Background(), digest, "compose_ab"); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	var statements []string
	for _, c := range f.calls {
		if filepath.Base(c.path) == "psql" {
			statements = append(statements, c.args[len(c.args)-1])
			if !hasArg(c.args, "-p", "54321") {
				t.Errorf("psql argv %v, want the running server's port", c.args)
			}
		}
	}
	want := []string{`CREATE DATABASE "compose_a""b"`, `DROP DATABASE IF EXISTS "compose_ab" WITH (FORCE)`}
	if strings.Join(statements, "|") != strings.Join(want, "|") {
		t.Errorf("statements = %q, want %q", statements, want)
	}
	if err := n.CreateDatabase(context.Background(), "", "x"); err == nil {
		t.Error("CreateDatabase without a digest succeeded")
	}
}

// postmasterPort reads the fourth line.
func TestPostmasterPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "postmaster.pid")
	if err := os.WriteFile(path, []byte("42\n/data\n1700000000\n6543\n\n*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if port, err := postmasterPort(path); err != nil || port != 6543 {
		t.Errorf("postmasterPort = %d, %v; want 6543", port, err)
	}
	if err := os.WriteFile(path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := postmasterPort(path); err == nil {
		t.Error("a postmaster.pid without a port line parsed")
	}
}

// The newest version of an install location comes first.
func TestCompareVersionedPathsOrdersByVersion(t *testing.T) {
	paths := []string{"/usr/lib/postgresql/16/bin", "/usr/lib/postgresql/9/bin", "/usr/lib/postgresql/17/bin"}
	if compareVersionedPaths(paths[1], paths[0]) >= 0 || compareVersionedPaths(paths[2], paths[0]) <= 0 {
		t.Errorf("9 < 16 < 17 does not hold")
	}
	if compareVersionedPaths("/a", "/b") >= 0 {
		t.Error("paths without a number do not fall back to the text")
	}
}

// --- the local provider ------------------------------------------------------

// testLocalServer is a LocalServer whose two halves are fakes.
func testLocalServer(t *testing.T, daemon bool, docker *fakeDocker, native *NativeServer) *LocalServer {
	t.Helper()
	p := testProvisioner(docker, func(context.Context, runner, string, string, string) error { return nil })
	return &LocalServer{
		docker:        p,
		native:        native,
		dockerAnswers: func() bool { return daemon },
	}
}

// With no Docker daemon answering, the local provider serves from the native
// install and names the native resource; with one, Docker serves and the
// native install is never touched.
func TestLocalServerUsesTheNativeInstallWhenNoDockerDaemonAnswers(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "a-native-postgres-serves-when-no-docker-daemon-answers")

	t.Run("no daemon", func(t *testing.T) {
		_, native := scriptedServer(t)
		docker := &fakeDocker{}
		l := testLocalServer(t, false, docker, native)
		conn, digest, err := l.Provision()
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if conn.Port != 54321 || digest != native.Digest() {
			t.Errorf("connection %+v digest %q, want the native server's", conn, digest)
		}
		if l.id() != nativeProviderID || l.LabelKey() != "" || l.ContainerName(digest) != nativeDirPrefix+digest {
			t.Errorf("identity = %q %q %q, want the native one", l.id(), l.LabelKey(), l.ContainerName(digest))
		}
		if len(docker.calls) != 0 {
			t.Errorf("docker ran without a daemon: %v", docker.calls)
		}
		if l.isolatorFor(digest) != DatabaseIsolator(native) {
			t.Error("the native digest does not route to the native isolator")
		}
	})

	t.Run("a daemon", func(t *testing.T) {
		f, native := scriptedServer(t)
		docker := &fakeDocker{respond: func(args []string) (string, error) {
			if verb(args) == "port" {
				return "127.0.0.1:49153", nil
			}
			return "", nil
		}}
		l := testLocalServer(t, true, docker, native)
		conn, _, err := l.Provision()
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if conn.Port != 49153 || l.id() != providerID {
			t.Errorf("connection %+v, provider %q, want the docker container", conn, l.id())
		}
		if len(f.calls) != 0 {
			t.Errorf("the native install ran beside a daemon: %v", f.programs())
		}
	})

	t.Run("neither", func(t *testing.T) {
		native := testNativeServer(t, &fakeNative{found: false})
		l := testLocalServer(t, false, &fakeDocker{}, native)
		if _, _, err := l.Provision(); !errors.Is(err, errNoNativePostgres) {
			t.Errorf("Provision error = %v, want one naming the missing install", err)
		}
	})
}

// The local provider owns the server it starts, so the fail-closed gate holds:
// a CI run or a non-auto mode never starts the native server either.
func TestLocalServerIsGatedLikeDocker(t *testing.T) {
	_, native := scriptedServer(t)
	l := testLocalServer(t, false, &fakeDocker{}, native)
	t.Setenv(EnvCI, "")
	if !mayProvision("auto", l) {
		t.Error("auto off CI with a native install may not provision")
	}
	if mayProvision("require", l) {
		t.Error("require may start a native server")
	}
	t.Setenv(EnvCI, "true")
	if mayProvision("auto", l) {
		t.Error("a CI run may start a native server")
	}
}

// --- against a real install ----------------------------------------------------

// TestNativeServer_AgainstARealInstall starts the PostgreSQL installed on the
// machine, isolates a database on it, and tears it down. It never runs on CI,
// where this package never starts infrastructure of its own.
func TestNativeServer_AgainstARealInstall(t *testing.T) {
	n := NewNativeServer()
	if testing.Short() || os.Getenv(EnvCI) != "" || !n.available() {
		//putnami:allow-skip this package never starts infrastructure of its own on CI, so the test needs a contributor's PostgreSQL install
		t.Skip("needs a local PostgreSQL install outside CI")
	}
	root, err := os.MkdirTemp("", "putnami-native-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if os.Geteuid() == 0 {
		// The service user must reach its data directory.
		if err := os.Chmod(root, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	n.stateRoot = filepath.Join(root, "state")
	t.Cleanup(func() {
		if err := n.TeardownAll(); err != nil {
			t.Errorf("TeardownAll: %v", err)
		}
	})

	conn, digest, err := n.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := dialTCP(context.Background(), conn.Host, conn.Port); err != nil {
		t.Fatalf("the server does not answer on %s:%d: %v", conn.Host, conn.Port, err)
	}
	again, _, err := n.Provision()
	if err != nil || again.Port != conn.Port {
		t.Fatalf("second Provision = %+v, %v; want the running server on port %d", again, err, conn.Port)
	}

	ctx := context.Background()
	name := "compose_native_" + strconv.Itoa(os.Getpid())
	if err := n.CreateDatabase(ctx, digest, name); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	listed, err := n.ListDatabases(ctx, digest, "compose_native_")
	if err != nil || len(listed) != 1 || listed[0] != name {
		t.Fatalf("ListDatabases = %v, %v; want [%s]", listed, err, name)
	}
	if err := n.DropDatabase(ctx, digest, name); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
}

func hasArg(args []string, want ...string) bool {
	for i := range args {
		if i+len(want) <= len(args) && strings.Join(args[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

// The programs inherit only what finds the binaries and sets their locale:
// the run's credentials never reach a server that outlives it.
func TestNativeEnvKeepsOnlyTheAllowlist(t *testing.T) {
	inherited := []string{"PATH=/usr/bin", "LANG=C.UTF-8", "HOME=/root", "AWS_SECRET_ACCESS_KEY=x", "GITHUB_TOKEN=y", "PGDATA=/elsewhere", "TZ=UTC"}
	env := nativeEnv(inherited, "/var/lib/postgresql", []string{"PGPASSWORD=p"})
	want := []string{"PATH=/usr/bin", "LANG=C.UTF-8", "TZ=UTC", "HOME=/var/lib/postgresql", "PGPASSWORD=p"}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Errorf("env = %v, want %v", env, want)
	}
	if env := nativeEnv(inherited, "", nil); !hasArg(env, "HOME=/root") || len(env) != 4 {
		t.Errorf("env without a service home = %v, want the allowlist and the inherited HOME", env)
	}
}

// A data directory whose server still runs and cannot be stopped is kept.
func TestNativeServerKeepsADirectoryUnderALiveServer(t *testing.T) {
	f := &fakeNative{found: false, alive: true}
	n := testNativeServer(t, f)
	dir := filepath.Join(n.stateRoot, nativeDirPrefix+"0123456789abcdef")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.TeardownAll(); err == nil || !strings.Contains(err.Error(), "4242") {
		t.Errorf("TeardownAll = %v, want an error naming the live process", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory under a live server was removed: %v", err)
	}
}
