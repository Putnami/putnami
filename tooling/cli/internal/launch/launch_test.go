package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/clibin"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/store"
)

const testSHA = "abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcab"

// capture records what the injected exec hook was asked to do.
type capture struct {
	calls int
	path  string
	argv  []string
	env   []string
}

// touch creates an empty file and returns its path.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCLILock(t *testing.T, wsRoot, goos, goarch, sha string) {
	t.Helper()
	lf := lockfile.NewLockFile()
	e := lockfile.LockEntry{Version: "1.4.2"}
	e.SetPlatformIntegrity(lockfile.PlatformKey(goos, goarch), sha)
	lf.SetCLI(e)
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		t.Fatal(err)
	}
}

// writeCorruptLock writes a putnami.lock.json that exists but does not parse, so
// the launcher can neither read a pin nor prove there is none.
func writeCorruptLock(t *testing.T, wsRoot string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsRoot, lockfile.LockFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func baseConfig(cap *capture, self string) config {
	return config{
		self:    self,
		goos:    "linux",
		goarch:  "amd64",
		args:    []string{"build", "--all"},
		getenv:  func(string) string { return "" },
		homeDir: func() (string, error) { return "", errors.New("no home directory in this test") },
		environ: func() []string { return []string{"PATH=/usr/bin"} },
		resolve: func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
			return "", errors.New("resolve should not have been called")
		},
		exec: func(path string, argv []string, env []string) error {
			cap.calls++
			cap.path = path
			cap.argv = argv
			cap.env = env
			return nil
		},
	}
}

// mustRun asserts relaunch chose to keep running the current binary with no
// error — the fail-OPEN outcome, reserved for "there is no pin to betray".
func mustRun(t *testing.T, ws string, c config, why string) {
	t.Helper()
	launched, err := relaunch(context.Background(), ws, c)
	if err != nil {
		t.Fatalf("%s: unexpected hard error: %v", why, err)
	}
	if launched {
		t.Fatalf("%s: expected to run the current binary, got a relaunch", why)
	}
}

// mustFailClosed asserts relaunch refused to run anything: no exec, and an error
// carrying both a recovery command (WithNext) and the expected exit class.
func mustFailClosed(t *testing.T, ws string, c config, cap *capture, wantExit int, wantNext string) error {
	t.Helper()
	launched, err := relaunch(context.Background(), ws, c)
	if err == nil {
		t.Fatal("expected a hard error, got fail-open")
	}
	if launched {
		t.Fatal("a failed launch must never report success")
	}
	if cap.calls != 0 {
		t.Fatalf("exec called %d times on a failed resolution, want 0", cap.calls)
	}
	if got := protocolcli.ExitCodeForError(err); got != wantExit {
		t.Errorf("exit code = %d, want %d (err: %v)", got, wantExit, err)
	}
	if got := protocolcli.SuggestedNext(err); got != wantNext {
		t.Errorf("suggested next = %q, want %q", got, wantNext)
	}
	return err
}

func TestIsExemptCommand(t *testing.T) {
	if !IsExemptCommand("pin") {
		t.Error("pin must be exempt from relaunch")
	}
	// mcp must NOT be exempt: `putnami mcp` should relaunch into the pinned
	// engine so an agent's MCP server runs the same version as the CLI.
	for _, c := range []string{"build", "test", "version", "upgrade", "mcp", ""} {
		if IsExemptCommand(c) {
			t.Errorf("%q should not be exempt from relaunch", c)
		}
	}
}

func TestIsExemptInvocation(t *testing.T) {
	exempt := [][]string{
		{"pin"},
		{"pin", "1.2.3"},
		{"upgrade", "--from-source"},
		{"upgrade", "--from-source", "--global"},
		{"upgrade", "--global", "--from-source"},
		{"version", "list"},
		{"version", "list", "--global"},
		{"version", "use", "go-dev"},
		{"version", "use", "go-dev", "--global"},
		{"extensions", "install", "--user", "@acme/audit"},
		{"extensions", "--user", "@acme/audit"},
		{"extensions", "remove", "--user", "@acme/audit"},
		{"extensions", "list", "--user"},
	}
	for _, args := range exempt {
		if !IsExemptInvocation(args) {
			t.Errorf("%v must be exempt from relaunch (CLI-management/recovery path)", args)
		}
	}

	// A plain channel upgrade, the release-version subcommands, and everything
	// else relaunch into the pinned engine as before.
	notExempt := [][]string{
		{},
		{"upgrade"},
		{"upgrade", "--local"},
		{"upgrade", "--global"},
		{"upgrade", "--cli"},
		{"version"},
		{"version", "get"},
		{"version", "set", "1.2.3"},
		{"version", "tag"},
		{"build"},
		{"mcp"},
		{"extensions", "install", "@acme/audit"},
		{"extensions", "list"},
		{"extensions", "install", "--", "--user"},
	}
	for _, args := range notExempt {
		if IsExemptInvocation(args) {
			t.Errorf("%v should not be exempt from relaunch", args)
		}
	}
}

func TestRestartEnvBypassesWorkspacePinAndClearsLaunchGuard(t *testing.T) {
	env := restartEnv([]string{
		"PATH=/usr/bin",
		NoRelaunchEnv + "=0",
		LaunchedEnv + "=prior-pin",
	})
	if slices.Contains(env, NoRelaunchEnv+"=0") {
		t.Fatalf("restart env retained stale %s: %v", NoRelaunchEnv, env)
	}
	if slices.ContainsFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, LaunchedEnv+"=")
	}) {
		t.Fatalf("restart env retained %s: %v", LaunchedEnv, env)
	}
	if !slices.Contains(env, NoRelaunchEnv+"=1") {
		t.Fatalf("restart env did not bypass workspace pin: %v", env)
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("restart env dropped PATH: %v", env)
	}
	if len(env) != 2 {
		t.Fatalf("restart env = %v, want PATH plus %s", env, NoRelaunchEnv)
	}
}

func TestRelaunchNoLock(t *testing.T) {
	var cap capture
	mustRun(t, t.TempDir(), baseConfig(&cap, "/self"), "no lock file")
	if cap.calls != 0 {
		t.Error("exec must not be called without a lock")
	}
}

func TestRelaunchNoCLIPin(t *testing.T) {
	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetExtension("@putnami/go", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	var cap capture
	mustRun(t, ws, baseConfig(&cap, "/self"), "a lock without a CLI pin")
	if cap.calls != 0 {
		t.Error("exec must not be called without a CLI pin")
	}
}

// A pin whose digests cover other platforms only used to fall through to the
// running binary — the silent-drift case. It is now a hard error pointing at
// `putnami pin`, which is exempt from the launcher and appends this platform's
// digest to the same entry.
func TestRelaunchNoPlatformDigestFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "darwin", "arm64", testSHA) // pinned for a different platform
	var cap capture
	c := baseConfig(&cap, "/self") // goos/goarch = linux/amd64
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		t.Fatal("resolve must not be called without a digest to verify against")
		return "", nil
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "putnami pin 1.4.2")
	for _, want := range []string{"1.4.2", "linux/amd64", lockfile.LockFilename} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q must name %q", err, want)
		}
	}
}

func TestRelaunchSentinelGuardsLoop(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.getenv = func(k string) string {
		if k == LaunchedEnv {
			return testSHA
		}
		return ""
	}
	mustRun(t, ws, c, "the relaunch loop guard")
	if cap.calls != 0 {
		t.Error("the sentinel env must hard-guard against a relaunch loop")
	}
}

func TestConsumeLaunchedClearsSentinel(t *testing.T) {
	t.Setenv(LaunchedEnv, testSHA)
	if !ConsumeLaunched() {
		t.Fatal("ConsumeLaunched should report the sentinel")
	}
	if got := os.Getenv(LaunchedEnv); got != "" {
		t.Fatalf("%s still set after consume: %q", LaunchedEnv, got)
	}
	if ConsumeLaunched() {
		t.Fatal("ConsumeLaunched should be false after the sentinel is removed")
	}
}

func TestRelaunchNoRelaunchOptOut(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	for _, v := range []string{"1", "true", "YES", "on"} {
		var cap capture
		c := baseConfig(&cap, "/self")
		c.getenv = func(k string) string {
			if k == NoRelaunchEnv {
				return v
			}
			return ""
		}
		c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
			t.Fatalf("resolve called despite %s=%s", NoRelaunchEnv, v)
			return "", nil
		}
		mustRun(t, ws, c, NoRelaunchEnv+"="+v)
		if cap.calls != 0 {
			t.Errorf("%s=%q must suppress relaunch", NoRelaunchEnv, v)
		}
	}
}

// The opt-out is the documented way out of every fail-closed branch below, so it
// must be honored BEFORE the lock is read — otherwise an unreadable or
// unsupported lock would brick the very command meant to recover from it.
func TestRelaunchNoRelaunchOptOutBeatsUnreadableLock(t *testing.T) {
	ws := t.TempDir()
	writeCorruptLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.getenv = func(k string) string {
		if k == NoRelaunchEnv {
			return "1"
		}
		return ""
	}
	mustRun(t, ws, c, NoRelaunchEnv+" with a corrupt lock")
}

// Same for the loop guard: the child of a successful relaunch must not re-read
// the lock and hard-fail on it.
func TestRelaunchLaunchedSentinelBeatsUnreadableLock(t *testing.T) {
	ws := t.TempDir()
	writeCorruptLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.getenv = func(k string) string {
		if k == LaunchedEnv {
			return testSHA
		}
		return ""
	}
	mustRun(t, ws, c, LaunchedEnv+" with a corrupt lock")
}

func TestRelaunchNoRelaunchFalsyIgnored(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "self"))
	resolved := touch(t, filepath.Join(ws, "resolved"))
	for _, v := range []string{"", "0", "false", "no"} {
		var cap capture
		c := baseConfig(&cap, self)
		c.getenv = func(k string) string {
			if k == NoRelaunchEnv {
				return v
			}
			return ""
		}
		c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
			return resolved, nil
		}
		launched, err := relaunch(context.Background(), ws, c)
		if err != nil {
			t.Fatalf("%s=%q: unexpected error: %v", NoRelaunchEnv, v, err)
		}
		if !launched || cap.calls != 1 {
			t.Errorf("%s=%q (falsy) must NOT suppress relaunch", NoRelaunchEnv, v)
		}
	}
}

// Network down mid-resolve: the pinned engine is unreachable, so the run stops
// with the API exit class and the copy-pasteable opt-out for this invocation.
func TestRelaunchDownloadFailureFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return "", protocolcli.Classify(errors.New("clibin: download: dial tcp: no route to host"), clibin.ErrDownload)
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitAPI,
		"PUTNAMI_NO_RELAUNCH=1 putnami build --all")
	if !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("message %q must keep the underlying cause", err)
	}
}

// Corrupt or substituted registry bytes: only an explicit re-pin can move the
// trust anchor, so that — not the opt-out — is the suggested next command.
func TestRelaunchIntegrityFailureFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return "", protocolcli.Classify(errors.New("clibin: integrity check failed"), clibin.ErrIntegrity)
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitFailure, "putnami pin 1.4.2")
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("message %q must keep the underlying cause", err)
	}
}

// A store that will not take the verified bytes (full/read-only disk, corrupt
// tree) is a local failure: opt out for this run while it is repaired.
func TestRelaunchStoreFailureFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return "", protocolcli.Classify(errors.New("create artifact staging dir: read-only file system"), clibin.ErrStore)
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitFailure,
		"PUTNAMI_NO_RELAUNCH=1 putnami build --all")
	if !strings.Contains(err.Error(), "artifact store") {
		t.Errorf("message %q must name the artifact store", err)
	}
}

// clibin reports a missing platform digest with its own sentinel when the lock
// is read on its side; the launcher must give the same answer either way.
func TestRelaunchResolveNoPlatformDigestFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return "", clibin.ErrNoPlatformDigest
	}
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "putnami pin 1.4.2")
}

// Anything clibin has not classified (an insecure registry URL, say) still fails
// closed rather than falling through to the running binary.
func TestRelaunchUnclassifiedResolveFailureFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return "", errors.New("insecure registry URL")
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitFailure,
		"PUTNAMI_NO_RELAUNCH=1 putnami build --all")
	if !strings.Contains(err.Error(), "insecure registry URL") {
		t.Errorf("message %q must keep the underlying cause", err)
	}
}

func TestRelaunchAlreadyPinnedBinary(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "putnami"))
	var cap capture
	c := baseConfig(&cap, self)
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return self, nil // resolved path IS the running binary
	}
	mustRun(t, ws, c, "already running the pinned binary")
	if cap.calls != 0 {
		t.Error("must not relaunch when already running the pinned binary")
	}
}

func TestRelaunchReexecsIntoResolved(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "self"))
	resolved := touch(t, filepath.Join(ws, "resolved"))
	var cap capture
	c := baseConfig(&cap, self)
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return resolved, nil
	}

	launched, err := relaunch(context.Background(), ws, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !launched {
		t.Fatal("expected a relaunch into the resolved binary")
	}
	if cap.calls != 1 {
		t.Fatalf("exec called %d times, want 1", cap.calls)
	}
	if cap.path != resolved {
		t.Errorf("exec path = %q, want %q", cap.path, resolved)
	}
	wantArgv := []string{resolved, "build", "--all"}
	if strings.Join(cap.argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Errorf("argv = %v, want %v", cap.argv, wantArgv)
	}
	if !slices.Contains(cap.env, LaunchedEnv+"="+testSHA) {
		t.Errorf("env missing %s=%s: %v", LaunchedEnv, testSHA, cap.env)
	}
}

func TestRelaunchPreservesCapabilityTransportForPinnedChild(t *testing.T) {
	const token = "pinned-child-runtime-capability"
	const after = "lint,test,build,validate,validate-workspace"
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "self"))
	resolved := touch(t, filepath.Join(ws, "resolved"))
	var cap capture
	c := baseConfig(&cap, self)
	c.environ = func() []string {
		return []string{
			"PATH=/usr/bin",
			"PUTNAMI_CLOUD_TOKEN=" + token,
			"PUTNAMI_CLOUD_CAPABILITY_AFTER=" + after,
		}
	}
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return resolved, nil
	}

	launched, err := relaunch(context.Background(), ws, c)
	if err != nil || !launched {
		t.Fatalf("relaunch = %v, %v; want pinned child", launched, err)
	}
	for _, want := range []string{
		"PUTNAMI_CLOUD_TOKEN=" + token,
		"PUTNAMI_CLOUD_CAPABILITY_AFTER=" + after,
		LaunchedEnv + "=" + testSHA,
	} {
		if !slices.Contains(cap.env, want) {
			t.Errorf("pinned child env missing %q: %v", want, cap.env)
		}
	}
}

// A resolved binary that will not exec (corrupt store entry, noexec mount, wrong
// platform) must not silently degrade into "run whatever this process is".
func TestRelaunchExecErrorFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "self"))
	resolved := touch(t, filepath.Join(ws, "resolved"))
	var cap capture
	c := baseConfig(&cap, self)
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return resolved, nil
	}
	c.exec = func(string, []string, []string) error { return errors.New("permission denied") }

	launched, err := relaunch(context.Background(), ws, c)
	if launched {
		t.Error("a failed exec must never claim success")
	}
	if err == nil {
		t.Fatal("a failed exec must be a hard error, not a fall-through")
	}
	if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitFailure {
		t.Errorf("exit code = %d, want %d", got, protocolcli.ExitFailure)
	}
	if got := protocolcli.SuggestedNext(err); got != "PUTNAMI_NO_RELAUNCH=1 putnami build --all" {
		t.Errorf("suggested next = %q", got)
	}
	// The store path is named so the offending entry can be removed by hand.
	for _, want := range []string{"1.4.2", resolved, "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q must name %q", err, want)
		}
	}
}

// A lock file this binary cannot parse cannot rule a pin OUT, so it fails closed
// rather than guessing "probably unpinned".
func TestRelaunchUnreadableLockFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeCorruptLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		t.Fatal("resolve must not be called on an unreadable lock")
		return "", nil
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage,
		"PUTNAMI_NO_RELAUNCH=1 putnami build --all")
	if !strings.Contains(err.Error(), lockfile.LockFilename) {
		t.Errorf("message %q must name the lock file", err)
	}
}

// A lock written by a NEWER CLI (B0d's FormatVersionV2 and beyond) may carry a
// pin whose vocabulary this binary does not understand. Its recovery is the
// opposite of the generic one — upgrade the CLI — and plain `putnami upgrade`
// would re-enter the launcher and hit this same wall, so the suggestion is the
// opt-out form.
func TestRelaunchUnsupportedLockVersionFailsClosed(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, lockfile.LockFilename),
		fmt.Appendf(nil, `{"version": %d}`, lockfile.MaxSupportedVersion+1), 0o644); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, "/self")
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage,
		"PUTNAMI_NO_RELAUNCH=1 putnami upgrade")
	var unsupported *lockfile.UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error %v must wrap *lockfile.UnsupportedVersionError", err)
	}
	if !strings.Contains(err.Error(), "upgrade the CLI") {
		t.Errorf("message %q must say the CLI is too old", err)
	}
}

// A lock written by an OLDER CLI is the mirror case, and since the vNext floor
// every v1 lock lands here on the FIRST command run in the workspace. Its
// recovery is not the generic "opt out of the pin for this run" — that would
// ignore the pin forever, one command at a time — but the one conversion that
// fixes the workspace. `migrate vnext` is exempt from the launcher, so the
// plain form is safe to suggest.
func TestRelaunchOutdatedLockVersionSuggestsMigration(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, lockfile.LockFilename),
		fmt.Appendf(nil, `{"version": %d}`, lockfile.MinSupportedVersion-1), 0o644); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, "/self")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		t.Fatal("resolve must not be called on a lock below the format floor")
		return "", nil
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "putnami migrate vnext --apply")
	var outdated *lockfile.OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("error %v must wrap *lockfile.OutdatedVersionError", err)
	}
	// The suggestion must name a command the launcher lets through, or the
	// remedy would re-enter this same wall.
	if !IsExemptInvocation([]string{"migrate", "vnext", "--apply"}) {
		t.Error("the suggested migration is not exempt from the launcher")
	}
}

// A hand-edited lock can pin "a CLI" with no version at all. `putnami pin
// <version>` cannot be suggested then — there is no version to re-pin — so the
// hint has to degrade to dropping the pin rather than emitting a broken command.
func TestRelaunchVersionlessPinSuggestsRemove(t *testing.T) {
	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{}) // no version, no digests
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, "/self")
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "putnami pin --remove")
}

// The opt-out hint reproduces the invocation verbatim, so a copy-paste re-runs
// the same command instead of a mangled one.
func TestOptOutCommandQuotesArguments(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "PUTNAMI_NO_RELAUNCH=1 putnami"},
		{[]string{"build", "--all"}, "PUTNAMI_NO_RELAUNCH=1 putnami build --all"},
		{[]string{"run", "--msg", "hello world"}, `PUTNAMI_NO_RELAUNCH=1 putnami run --msg 'hello world'`},
		{[]string{"run", ""}, "PUTNAMI_NO_RELAUNCH=1 putnami run ''"},
		{[]string{"run", "it's"}, `PUTNAMI_NO_RELAUNCH=1 putnami run 'it'\''s'`},
	}
	for _, tc := range cases {
		if got := optOutCommand(tc.args); got != tc.want {
			t.Errorf("optOutCommand(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A Go test binary must never be replaced. syscall.Exec ends the process, so
// every test after the one that triggered it vanishes while the run still exits
// 0 and prints "ok", and no coverage profile is written — the package then
// measures 0%. That is the regression that made internal/cli report 0% on every
// file while 272 test functions were declared and only 45 ever ran.
func TestRelaunchRefusesToReplaceATestBinary(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	for _, self := range []string{
		"/tmp/go-build123/b001/cli.test",
		`C:\tmp\go-build123\b001\cli.test.exe`,
	} {
		var cap capture
		c := baseConfig(&cap, self)
		c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
			return "/store/putnami", nil
		}
		err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, optOutCommand(c.args))
		if !strings.Contains(err.Error(), "test binary") {
			t.Errorf("self=%q error should name the test binary, got %v", self, err)
		}
	}
}

// The refusal is placed at the exec and nowhere earlier, so a test that means to
// assert the launcher's fail-closed behavior still reaches it. A pin with no
// digest for this platform must still be that error, not the test-binary one.
func TestRelaunchTestBinaryStillSeesEarlierFailures(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "plan9", "s390x", testSHA)
	var cap capture
	c := baseConfig(&cap, "/tmp/b001/cli.test")
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, pinCommand("1.4.2"))
	if strings.Contains(err.Error(), "test binary") {
		t.Errorf("platform-digest failure must not be masked by the test-binary guard: %v", err)
	}
}

// The suffix is the identity, not a substring: a real CLI whose path merely
// contains ".test" must still honor its pin.
func TestIsTestBinary(t *testing.T) {
	for self, want := range map[string]bool{
		"/tmp/go-build/b001/cli.test": true,
		"cli.test":                    true,
		`C:\tmp\b001\cli.test.exe`:    true,
		"/usr/local/bin/putnami":      false,
		"/home/me/.test/bin/putnami":  false,
		"/tmp/cli.test/bin/putnami":   false,
		"/tmp/cli.testing":            false,
		"":                            false,
	} {
		if got := isTestBinary(self); got != want {
			t.Errorf("isTestBinary(%q) = %v, want %v", self, got, want)
		}
	}
}

// ─── source workspace ─────────────────────────────────────────────────────
//
// A workspace whose lock records `cli.source: "workspace"` builds its own
// engine. There is no published binary to relaunch into, so the launcher's only
// answers are "you already are that binary" and "refuse". These tests pin the
// refusal, because the alternative — falling through to whatever binary is
// running — is the dishonest green the sentinel exists to prevent.

// writeSourceWorkspaceLock writes the sentinel exactly as a self-hosting
// workspace commits it: a source and nothing else.
func writeSourceWorkspaceLock(t *testing.T, wsRoot string) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		t.Fatal(err)
	}
}

// workspaceEngine creates the file `./putnamiw` publishes — .putnami/bin/putnami —
// and returns its path. The launcher accepts a from-source run only when the
// running binary IS that file, so a test that must be ADMITTED has to run as it;
// a test that must be REFUSED passes some other path.
func workspaceEngine(t *testing.T, wsRoot string) string {
	t.Helper()
	dir := filepath.Join(wsRoot, ".putnami", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return touch(t, filepath.Join(dir, "putnami"))
}

// envFunc builds a getenv stub over a fixed map.
func envFunc(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

// sourceStoreBlob creates the immutable from-source blob `./putnamiw` publishes —
// $PUTNAMI_HOME/artifacts/cli-source/<key>/putnami — and returns the home it put
// it under, the key, and the blob path. A test that must be ADMITTED runs AS that
// blob with the key in FromSourceKeyEnv; the blob is content-addressed and never
// rewritten, which is why a concurrent ./putnamiw cannot take the proof away.
func sourceStoreBlob(t *testing.T, key string) (home, blob string) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, "artifacts", "cli-source", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, touch(t, filepath.Join(dir, "putnami"))
}

// repointEngineLink points .putnami/bin/putnami at some OTHER file, which is what
// a concurrent `./putnamiw` at a different tree state does to every checkout that
// shares the worktree — legitimately, since that is how the wrapper keeps the
// engine current.
func repointEngineLink(t *testing.T, wsRoot, target string) {
	t.Helper()
	dir := filepath.Join(wsRoot, ".putnami", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "putnami")
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// The engine pin is what survives concurrency. `./putnamiw`
// re-points .putnami/bin/putnami on every run whose source key differs, so a
// second invocation anywhere in the same checkout used to retroactively un-prove
// a CLI that was STILL RUNNING — and its re-entrant children were refused while
// naming the correct engine. The blob the pin names is content-addressed and
// first-writer-wins, so nothing can move it out from under a live process.
func TestRelaunchSourceWorkspaceAdmitsThePinnedBlobAfterAConcurrentRelink(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"
	home, blob := sourceStoreBlob(t, key)

	// Another run published a different key and re-pointed the shared link at it.
	_, other := sourceStoreBlob(t, "fcb9c9eecafebabe")
	repointEngineLink(t, ws, other)

	var cap capture
	c := baseConfig(&cap, blob)
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   home,
	})
	mustRun(t, ws, c, "a pinned from-source blob whose workspace link was re-pointed mid-run")
	if cap.calls != 0 {
		t.Error("a source workspace has nothing to exec into")
	}
	spectest.Proves(t, "cli/engine-provenance", "foreign-engine-refused",
		"a-concurrent-relink-does-not-un-prove-a-running-from-source-engine")
}

// The pin is inherited, so a re-entrant child — a job or hook that calls back
// into `putnami` — presents the same blob path and the same key as the parent the
// wrapper admitted. This is the case that was failing in CI: the child was the
// right engine and was refused anyway.
func TestRelaunchSourceWorkspaceAdmitsAReentrantChildOfAPinnedEngine(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "d670fa3a0badc0de"
	home, blob := sourceStoreBlob(t, key)
	// The workspace was never linked at all: the child proves itself from the pin
	// alone, without reading any workspace-global state.
	var cap capture
	c := baseConfig(&cap, blob)
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   home,
	})
	mustRun(t, ws, c, "a re-entrant child running the pinned blob")
}

// Naming a key does not lower the bar the pair exists for. The LOCATION is the
// evidence: only publish_source_cli writes the from-source store, and a download
// lands under artifacts/cli/<sha> instead. So the one-line shortcut
// `PUTNAMI_FROM_SOURCE=$PWD PUTNAMI_FROM_SOURCE_KEY=<any> /usr/local/bin/putnami
// build --all` must still be refused.
func TestRelaunchSourceWorkspaceRefusesAPinnedKeyOnAForeignBinary(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"
	home, _ := sourceStoreBlob(t, key)

	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   home,
	})
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
	if !strings.Contains(err.Error(), "not this binary") {
		t.Errorf("message %q must say the pinned blob is not this binary", err)
	}
	spectest.Proves(t, "cli/engine-provenance", "foreign-engine-refused",
		"a-borrowed-engine-pin-on-a-foreign-binary-is-still-refused")
}

// A pin for a blob in a DIFFERENT store proves nothing about this machine's
// from-source store. The key is resolved under PUTNAMI_HOME, which is how
// putnamiw locates the store it published to, so a run whose home points
// elsewhere must not match.
func TestRelaunchSourceWorkspaceResolvesThePinUnderPutnamiHome(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"
	home, blob := sourceStoreBlob(t, key)

	var cap capture
	c := baseConfig(&cap, blob)
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   home,
	})
	mustRun(t, ws, c, "a pinned blob under the home putnamiw published to")

	// Same blob, same key, a home that does not contain it.
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   t.TempDir(),
	})
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")

	// No PUTNAMI_HOME: .putnami under the user home directory.
	userHome := t.TempDir()
	dir := filepath.Join(userHome, ".putnami", "artifacts", "cli-source", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c.self = touch(t, filepath.Join(dir, "putnami"))
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
	})
	c.homeDir = func() (string, error) { return userHome, nil }
	mustRun(t, ws, c, "a pinned blob under the user home directory")
}

// Without PUTNAMI_HOME and without a user home directory there is no
// from-source store: a blob planted under the worktree's own .putnami does not
// prove anything, because no store writer publishes there.
func TestRelaunchSourceWorkspaceHasNoStoreWithoutAHome(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"
	dir := filepath.Join(ws, ".putnami", "artifacts", "cli-source", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, touch(t, filepath.Join(dir, "putnami")))
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
	})
	if root := sourceStoreRoot(c); root != "" {
		t.Fatalf("store root without a home = %q, want none", root)
	}
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
}

// The launcher and the artifact store resolve the same home: os.UserHomeDir,
// which reads USERPROFILE on Windows and HOME elsewhere. Here the platform's
// variable is the only one set.
func TestSourceStoreRootSharesTheArtifactStoreHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(putnamiHomeEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", "")
	if runtime.GOOS == "windows" {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	c := config{getenv: os.Getenv, homeDir: os.UserHomeDir}
	got := sourceStoreRoot(c)
	artifacts := store.ResolveArtifactStoreRoot(t.TempDir())
	if got != filepath.Join(home, ".putnami", "artifacts", "cli-source") || filepath.Dir(got) != artifacts {
		t.Fatalf("from-source store = %q, artifact store = %q, want both under %s", got, artifacts, filepath.Join(home, ".putnami"))
	}
}

// A Windows engine is putnami.exe, both as the pinned store blob and as the
// workspace link; a blob without the suffix is not the engine there.
func TestRelaunchSourceWorkspaceFindsTheWindowsEngineName(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"
	home := t.TempDir()
	dir := filepath.Join(home, "artifacts", "cli-source", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, touch(t, filepath.Join(dir, "putnami.exe")))
	c.goos = "windows"
	c.getenv = envFunc(map[string]string{
		FromSourceEnv:    ws,
		FromSourceKeyEnv: key,
		putnamiHomeEnv:   home,
	})
	mustRun(t, ws, c, "a pinned putnami.exe blob on Windows")

	c.self = touch(t, filepath.Join(dir, "putnami"))
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")

	engineDir := filepath.Join(ws, ".putnami", "bin")
	if err := os.MkdirAll(engineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c.self = touch(t, filepath.Join(engineDir, "putnami.exe"))
	c.getenv = envFunc(map[string]string{FromSourceEnv: ws})
	mustRun(t, ws, c, "the workspace's .putnami/bin/putnami.exe on Windows")
	if got := engineProof(ws, c.self, "windows"); got != ".putnami/bin/putnami.exe is this binary" {
		t.Fatalf("engineProof = %q", got)
	}
}

// A key is ONE path segment in the store it indexes. A key carrying separators or
// dot segments would let the pin name a binary the store's only writer
// (publish_source_cli) never wrote, so those are refused before any filesystem
// access.
//
// Every case plants a real binary at the path its key resolves to and runs AS
// that binary, so each one is admitted when the guard is removed. A fixture that
// merely points the store root somewhere empty would pass with no guard at all.
func TestRelaunchSourceWorkspaceRefusesAPinKeyThatEscapesTheStore(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	home := t.TempDir()
	storeRoot := filepath.Join(home, "artifacts", "cli-source")

	for _, key := range []string{
		filepath.Join("..", "..", "elsewhere"), // climbs out of the store entirely
		".",                                    // the store root itself
		"..",                                   // the artifacts dir above it
		filepath.Join("sub", "key"),            // a nested path no key can be
	} {
		// The binary the pin would reach. Planting it is what makes the case
		// load-bearing: without the guard, relaunch admits this run.
		reached := filepath.Join(storeRoot, key, "putnami")
		if err := os.MkdirAll(filepath.Dir(reached), 0o755); err != nil {
			t.Fatal(err)
		}
		blob := touch(t, reached)

		var cap capture
		c := baseConfig(&cap, blob)
		c.getenv = envFunc(map[string]string{
			FromSourceEnv:    ws,
			FromSourceKeyEnv: key,
			putnamiHomeEnv:   home,
		})
		if _, err := relaunch(context.Background(), ws, c); err == nil {
			t.Errorf("key %q reached %s and was admitted", key, reached)
		}
	}
}

func TestRelaunchSourceWorkspaceRunsBinaryBuiltFromIt(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, workspaceEngine(t, ws))
	c.getenv = envFunc(map[string]string{FromSourceEnv: ws})
	mustRun(t, ws, c, "a binary ./putnamiw built from this workspace")
	if cap.calls != 0 {
		t.Error("a source workspace has nothing to exec into")
	}
}

// The marker and the discovered root are compared as RESOLVED paths, because
// this repository is worked in through symlinked checkouts and git worktrees
// where the same tree has several spellings. A raw string compare would refuse
// a legitimate from-source run.
func TestRelaunchSourceWorkspaceResolvesSymlinkedRoots(t *testing.T) {
	real := t.TempDir()
	writeSourceWorkspaceLock(t, real)
	link := filepath.Join(t.TempDir(), "worktree")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var cap capture
	c := baseConfig(&cap, workspaceEngine(t, real))
	// The launcher discovered the symlinked spelling; putnamiw exported the real
	// one (or the other way round — both directions must match).
	c.getenv = envFunc(map[string]string{FromSourceEnv: real})
	mustRun(t, link, c, "a symlinked spelling of the same workspace root")

	c.getenv = envFunc(map[string]string{FromSourceEnv: link})
	mustRun(t, real, c, "the real spelling of a symlinked workspace root")

	// Trailing separators and "." segments are still the same directory.
	c.getenv = envFunc(map[string]string{FromSourceEnv: real + string(filepath.Separator) + "."})
	mustRun(t, real, c, "a non-canonical spelling of the workspace root")
}

func TestRelaunchSourceWorkspaceRefusesAForeignBinary(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		t.Fatal("a source workspace resolves nothing: there is no published pin")
		return "", nil
	}
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
	for _, want := range []string{
		lockfile.LockFilename,
		lockfile.SourceWorkspace,
		FromSourceEnv,
		"/usr/local/bin/putnami",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q must name %q", err, want)
		}
	}
	spectest.Proves(t, "cli/engine-provenance", "foreign-engine-refused",
		"a-binary-not-built-from-this-tree-is-refused-with-the-putnamiw-recovery")
}

// PUTNAMI_NO_RELAUNCH is the escape from a broken PIN. A source workspace has no
// pinned binary to be broken, so the opt-out must not become a license to gate a
// commit with a foreign engine. Its escape is ./putnamiw (plus the
// IsExemptInvocation set, which App.Run checks before calling Relaunch at all).
func TestRelaunchSourceWorkspaceIgnoresTheNoRelaunchOptOut(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	for _, v := range []string{"1", "true", "YES", "on"} {
		var cap capture
		c := baseConfig(&cap, "/usr/local/bin/putnami")
		c.getenv = envFunc(map[string]string{NoRelaunchEnv: v})
		err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
		if !strings.Contains(err.Error(), lockfile.SourceWorkspace) {
			t.Errorf("%s=%s: message %q must name the sentinel", NoRelaunchEnv, v, err)
		}
	}
}

// A marker naming a DIFFERENT workspace root proves nothing about this one: it
// is what a nested invocation from another checkout would carry.
func TestRelaunchSourceWorkspaceRefusesAMarkerForAnotherWorkspace(t *testing.T) {
	ws := t.TempDir()
	other := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{FromSourceEnv: other})
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
}

// An empty marker must not match an empty workspace root, or "unset" would mean
// "matches everything".
func TestRelaunchSourceWorkspaceEmptyMarkerNeverMatches(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{FromSourceEnv: "   "})
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
}

// The marker alone is not evidence. It is an ordinary environment variable, so
// `PUTNAMI_FROM_SOURCE=$PWD /usr/local/bin/putnami build --all` would satisfy a
// marker-only check with ANY binary — a one-line shortcut that restores the
// dishonest green this whole shape removes, and exactly what somebody wiring the
// cloud runner would reach for. The running binary must also BE the engine file
// ./putnamiw publishes.
func TestRelaunchSourceWorkspaceRefusesACorrectMarkerOnAForeignBinary(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	workspaceEngine(t, ws) // the real engine exists; the running binary is not it

	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{FromSourceEnv: ws})
	err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
	if !strings.Contains(err.Error(), lockfile.SourceWorkspace) {
		t.Errorf("message %q must name the sentinel", err)
	}
	spectest.Proves(t, "cli/engine-provenance", "foreign-engine-refused",
		"a-correct-marker-on-a-foreign-binary-is-still-refused")
}

// A workspace that has never been built through ./putnamiw has no engine file,
// so nothing can satisfy the pair. The refusal must still name the recovery
// rather than crash on the missing path.
func TestRelaunchSourceWorkspaceRefusesWhenNoEngineHasBeenBuilt(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{FromSourceEnv: ws})
	mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
}

// The refusal has to say WHICH proof is missing. The two fail for unrelated
// reasons — a marker the process ladder dropped between the wrapper and a
// re-entrant child, an engine link some other command re-pointed — and the
// recovery differs: one is "run through ./putnamiw", the other is "something
// rewrote this workspace's engine link". A message that says only "at least one
// is missing" makes the reader reproduce the run to find out.
func TestRelaunchSourceWorkspaceRefusalNamesTheMissingProof(t *testing.T) {
	ws := t.TempDir()
	other := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	engineDir := filepath.Join(ws, ".putnami", "bin")

	cases := []struct {
		name  string
		setup func(t *testing.T) string // returns the marker value
		want  []string
	}{
		{
			name:  "no marker and no engine",
			setup: func(*testing.T) string { return "" },
			want:  []string{FromSourceEnv + " is unset", ".putnami/bin/putnami does not exist"},
		},
		{
			name:  "marker for another workspace",
			setup: func(*testing.T) string { return other },
			want:  []string{"names " + other + ", not this workspace root " + ws},
		},
		{
			name: "engine is another file",
			setup: func(t *testing.T) string {
				workspaceEngine(t, ws)
				return ws
			},
			want: []string{
				FromSourceEnv + " names this workspace root",
				".putnami/bin/putnami is another file, not this binary",
			},
		},
		{
			name: "engine link points at bytes that are gone",
			setup: func(t *testing.T) string {
				if err := os.MkdirAll(engineDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(other, "reaped", "putnami"),
					filepath.Join(engineDir, "putnami")); err != nil {
					t.Fatal(err)
				}
				return ws
			},
			want: []string{"points at", "which does not exist"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.RemoveAll(engineDir); err != nil {
				t.Fatal(err)
			}
			marker := tc.setup(t)
			var cap capture
			c := baseConfig(&cap, "/usr/local/bin/putnami")
			c.getenv = envFunc(map[string]string{FromSourceEnv: marker})
			err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q must state %q", err, want)
				}
			}
		})
	}
}

// The pin clause has its own three outcomes, and they have different causes: no
// pin at all is a marker that outlived its key (a Restart, or a hand-written
// FromSourceEnv); a pin whose blob is gone is the artifact GC or a `cache clean`;
// a pin whose blob is another file is a foreign engine under a borrowed key. A
// message that lumps them together sends the reader to the wrong fix.
func TestRelaunchSourceWorkspaceRefusalNamesTheMissingPin(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	const key = "726e724cdeadbeef"

	cases := []struct {
		name string
		env  func(t *testing.T) map[string]string
		want []string
	}{
		{
			name: "no pin at all",
			env: func(*testing.T) map[string]string {
				return map[string]string{FromSourceEnv: ws}
			},
			want: []string{"no from-source build is pinned (" + FromSourceKeyEnv + " is unset)"},
		},
		{
			name: "the pinned blob is another file",
			env: func(t *testing.T) map[string]string {
				home, _ := sourceStoreBlob(t, key)
				return map[string]string{
					FromSourceEnv: ws, FromSourceKeyEnv: key, putnamiHomeEnv: home,
				}
			},
			want: []string{FromSourceKeyEnv + " pins " + key, "not this binary"},
		},
		{
			name: "the pinned blob was reaped",
			env: func(t *testing.T) map[string]string {
				return map[string]string{
					FromSourceEnv: ws, FromSourceKeyEnv: key, putnamiHomeEnv: t.TempDir(),
				}
			},
			want: []string{FromSourceKeyEnv + " pins " + key, "is gone"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			c := baseConfig(&cap, "/usr/local/bin/putnami")
			c.getenv = envFunc(tc.env(t))
			err := mustFailClosed(t, ws, c, &cap, protocolcli.ExitUsage, "./putnamiw build --all")
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q must state %q", err, want)
				}
			}
		})
	}
}

// Restart sets the entries its caller names, replacing an inherited value of
// the same variable, and keeps the launcher opt-out last.
func TestRestartEnvSetsTheGivenEntries(t *testing.T) {
	env := restartEnv([]string{"PATH=/usr/bin", "PUTNAMI_PROVIDERS=stale"}, "PUTNAMI_PROVIDERS=install,publish")
	want := []string{"PATH=/usr/bin", "PUTNAMI_PROVIDERS=install,publish", NoRelaunchEnv + "=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("restart env = %v, want %v", env, want)
	}
}

// Restart hands control to a CLI this process just INSTALLED, which by
// definition did not come from the workspace tree. The marker must not survive
// into it, or a released binary would present itself as this tree's own engine.
func TestRestartEnvDropsTheFromSourceMarker(t *testing.T) {
	env := restartEnv([]string{
		"PATH=/usr/bin",
		FromSourceEnv + "=/some/workspace",
		FromSourceKeyEnv + "=726e724cdeadbeef",
		LaunchedEnv + "=" + testSHA,
		NoRelaunchEnv + "=1",
	})
	// The engine pin must go with the marker. A freshly installed CLI is not the
	// blob the pin names, so keeping it would hand that CLI a pin it cannot
	// satisfy — and keeping the pin without the marker is just as wrong.
	for _, dropped := range []string{FromSourceEnv, FromSourceKeyEnv} {
		for _, entry := range env {
			if strings.HasPrefix(entry, dropped+"=") {
				t.Fatalf("restart env still carries %s: %v", dropped, env)
			}
		}
	}
	if !slices.Contains(env, NoRelaunchEnv+"=1") {
		t.Errorf("restart env must still opt out of the launcher: %v", env)
	}
}

// A Go test binary came out of `go test` on this very tree, so it already
// satisfies the invariant. Refusing it would break every suite that runs inside
// the repository for no safety gain.
func TestRelaunchSourceWorkspaceAcceptsATestBinary(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	for _, self := range []string{
		"/tmp/go-build123/b001/cli.test",
		`C:\tmp\go-build123\b001\cli.test.exe`,
	} {
		var cap capture
		c := baseConfig(&cap, self)
		mustRun(t, ws, c, "a Go test binary in a source workspace")
		if cap.calls != 0 {
			t.Error("a test binary must never be replaced")
		}
	}
}

// The loop guard still wins: the child of a relaunch must not re-decide.
func TestRelaunchSourceWorkspaceRespectsTheLaunchedGuard(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	var cap capture
	c := baseConfig(&cap, "/usr/local/bin/putnami")
	c.getenv = envFunc(map[string]string{LaunchedEnv: testSHA})
	mustRun(t, ws, c, LaunchedEnv+" in a source workspace")
}

// A published pin is untouched by any of the above: it still resolves, still
// relaunches, and still honors the opt-out.
func TestRelaunchPublishedPinUnaffectedByTheSentinelBranch(t *testing.T) {
	ws := t.TempDir()
	writeCLILock(t, ws, "linux", "amd64", testSHA)
	self := touch(t, filepath.Join(ws, "self"))
	resolved := touch(t, filepath.Join(ws, "resolved"))

	var cap capture
	c := baseConfig(&cap, self)
	// A stray FromSourceEnv must not suppress a published pin: the marker is
	// evidence for a SOURCE workspace only.
	c.getenv = envFunc(map[string]string{FromSourceEnv: ws})
	c.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		return resolved, nil
	}
	launched, err := relaunch(context.Background(), ws, c)
	if err != nil || !launched || cap.calls != 1 {
		t.Fatalf("published pin: launched=%v err=%v calls=%d; want a relaunch", launched, err, cap.calls)
	}

	var optOut capture
	c2 := baseConfig(&optOut, self)
	c2.getenv = envFunc(map[string]string{NoRelaunchEnv: "1"})
	c2.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
		t.Fatalf("resolve called despite %s", NoRelaunchEnv)
		return "", nil
	}
	mustRun(t, ws, c2, NoRelaunchEnv+" against a published pin")
}

func TestPutnamiwCommandQuotesArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "./putnamiw"},
		{[]string{"build", "--all"}, "./putnamiw build --all"},
		{[]string{"run", "--msg", "hello world"}, `./putnamiw run --msg 'hello world'`},
	} {
		if got := putnamiwCommand(tc.args); got != tc.want {
			t.Errorf("putnamiwCommand(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
