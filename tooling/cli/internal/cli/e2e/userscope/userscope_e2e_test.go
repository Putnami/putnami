// Package userscope drives the user scope through the real CLI: `extensions
// install|list|remove --user` against a registry double, and the dispatch of
// a user-scope extension from a directory that is not a workspace. The tests
// set HOME, change the working directory and swap the process streams, so they
// run one after another in their own test binary.
package userscope

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

const (
	userScopeExtensionName = "@acme/audit"
	noWorkspaceMessage     = "putnami: no workspace found (looking for putnami.workspace.json)\n"
)

// userScopeExtensionScript is the task every fixture extension runs. It prints
// where it ran, what PUTNAMI_CALLER_DIR said and the job context it was handed,
// so a test reads what crossed the process boundary.
func userScopeExtensionScript(marker string) string {
	return `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
echo "marker=` + marker + `"
echo "pwd=$(pwd -P)"
echo "caller=$PUTNAMI_CALLER_DIR"
echo "context-begin"
cat "$ctx"
echo
echo "context-end"
`
}

// userScopeExtensionManifest declares the `audit` group: `run` is interactive
// and workspace-optional and is the group default; `check` is interactive but
// needs a workspace; `scan` is not interactive. `audit-run` is also a flat
// command. `crash` and `interrupt` are interactive and workspace-optional:
// `crash` kills itself with SIGKILL, and `interrupt` with SIGINT.
func userScopeExtensionManifest(name, version string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "commands": {
    "audit-run": {"description": "Audit the current directory.", "run": [{"id": "run", "task": "audit-exec"}]},
    "audit-crash": {"description": "Die from a signal.", "run": [{"id": "crash", "task": "audit-crash-exec"}]},
    "audit-interrupt": {"description": "Die from SIGINT.", "run": [{"id": "interrupt", "task": "audit-interrupt-exec"}]}
  },
  "commandGroups": {
    "audit": {
      "description": "Audit commands.",
      "default": "run",
      "subcommands": {
        "run":   {"command": "audit-run", "interactive": true, "workspace": "optional", "description": "Audit the current directory."},
        "check": {"command": "audit-run", "interactive": true, "description": "Audit a workspace."},
        "scan":  {"command": "audit-run", "description": "Scan a workspace."},
        "crash": {"command": "audit-crash", "interactive": true, "workspace": "optional", "description": "Die from a signal."},
        "interrupt": {"command": "audit-interrupt", "interactive": true, "workspace": "optional", "description": "Die from SIGINT."}
      }
    }
  },
  "tasks": {
    "audit-exec": {"kind": "command", "command": "{extensionRoot}/run.sh", "cache": false},
    "audit-crash-exec": {"kind": "command", "command": "{extensionRoot}/crash.sh", "cache": false, "timeoutMs": -1},
    "audit-interrupt-exec": {"kind": "command", "command": "{extensionRoot}/interrupt.sh", "cache": false, "timeoutMs": -1}
  }
}`, name, version, protocolcli.CurrentContract)
}

// userScopeDeadlineManifest declares the `audit hang` subcommand, interactive
// and workspace-optional, whose task passes its 300 ms deadline: the deadline is
// the subject of the test that uses it, so the fixture guard lists this
// declaration. The task sleeps for 30 s, so a loaded host only reaches the
// deadline sooner and the verdict does not change.
func userScopeDeadlineManifest(name, version string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "commands": {
    "audit-hang": {"description": "Outlive the task deadline.", "run": [{"id": "hang", "task": "audit-hang-exec"}]}
  },
  "commandGroups": {
    "audit": {
      "description": "Audit commands.",
      "subcommands": {
        "hang": {"command": "audit-hang", "interactive": true, "workspace": "optional", "description": "Outlive the task deadline."}
      }
    }
  },
  "tasks": {
    "audit-hang-exec": {"kind": "command", "command": "{extensionRoot}/hang.sh", "cache": false, "timeoutMs": 300}
  }
}`, name, version, protocolcli.CurrentContract)
}

// userScopeTestEnv isolates one test: HOME, and so the user scope, is a fresh
// directory, the repair marker is unset, the registry credential seam never
// spawns a CLI, and the returned caller directory holds a file and a
// subdirectory and is the working directory.
func userScopeTestEnv(t *testing.T) (userRoot, callerDir string) {
	t.Helper()
	clitest.RequireShell(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(extension.UserScopeRepairEnv, "")
	t.Setenv(extension.UnsafeInstallEnv, "")
	t.Setenv(extension.PrivatePutRegistryURLEnv, "")
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(home, "artifacts"))
	original := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryToken = original })

	callerDir = t.TempDir()
	writeTestFile(t, filepath.Join(callerDir, "notes.txt"), "caller content\n", 0o644)
	writeTestFile(t, filepath.Join(callerDir, "src", "main.txt"), "nested\n", 0o644)
	t.Chdir(callerDir)
	return filepath.Join(home, ".putnami", "user"), callerDir
}

// pinUserScopeExtension installs the fixture extension straight into the user
// scope: the stable link directory and the lock entry.
func pinUserScopeExtension(t *testing.T, userRoot, marker string) {
	t.Helper()
	dir := layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName)
	writeTestFile(t, filepath.Join(dir, "putnami.extension.json"), userScopeExtensionManifest(userScopeExtensionName, "1.0.0"), 0o644)
	writeTestFile(t, filepath.Join(dir, "run.sh"), userScopeExtensionScript(marker), 0o755)
	writeTestFile(t, filepath.Join(dir, "crash.sh"), "#!/bin/sh\nkill -KILL $$\n", 0o755)
	writeTestFile(t, filepath.Join(dir, "interrupt.sh"), "#!/bin/sh\nkill -INT $$\n", 0o755)
	writeUserScopeLockEntry(t, userRoot, lockfile.LockEntry{Version: "1.0.0"})
}

// pinUserScopeDeadlineExtension installs the extension of
// userScopeDeadlineManifest into the user scope.
func pinUserScopeDeadlineExtension(t *testing.T, userRoot string) {
	t.Helper()
	dir := layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName)
	writeTestFile(t, filepath.Join(dir, "putnami.extension.json"), userScopeDeadlineManifest(userScopeExtensionName, "1.0.0"), 0o644)
	// exec replaces the shell, so SIGTERM reaches the sleep itself, which does
	// not trap it.
	writeTestFile(t, filepath.Join(dir, "hang.sh"), "#!/bin/sh\nexec sleep 30\n", 0o755)
	writeUserScopeLockEntry(t, userRoot, lockfile.LockEntry{Version: "1.0.0"})
}

func writeUserScopeLockEntry(t *testing.T, userRoot string, entry lockfile.LockEntry) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.SetExtension(userScopeExtensionName, entry)
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.WriteLockFile(userRoot, lf); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// runApp runs one CLI invocation in the current directory and returns its exit
// code and combined output.
func runApp(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	var code int
	output := clitest.CaptureStdoutStderr(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		code = app.Run(context.Background(), argv)
	})
	return code, output
}

// snapshotTree records every entry under root: its kind, mode, modification
// time and content digest. Two equal snapshots mean nothing was created,
// removed, rewritten or touched, including a file created and deleted again.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stamp := fmt.Sprintf("%v %d", info.Mode(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[rel] = "link " + stamp + " " + target
		case info.IsDir():
			snapshot[rel] = "dir " + stamp
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			snapshot[rel] = "file " + stamp + " " + hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func assertSameTree(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for path, want := range before {
		if got, ok := after[path]; !ok {
			diffs = append(diffs, "removed "+path)
		} else if got != want {
			diffs = append(diffs, "changed "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			diffs = append(diffs, "created "+path)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("%s is not byte-identical after the run:\n  %s", label, strings.Join(diffs, "\n  "))
	}
}

// userScopeRun is what the fixture task reported about its own process.
type userScopeRun struct {
	marker  string
	pwd     string
	caller  string
	context []byte
}

func parseUserScopeRun(t *testing.T, output string) userScopeRun {
	t.Helper()
	var run userScopeRun
	for _, line := range strings.Split(output, "\n") {
		if v, ok := strings.CutPrefix(line, "marker="); ok {
			run.marker = v
		}
		if v, ok := strings.CutPrefix(line, "pwd="); ok {
			run.pwd = v
		}
		if v, ok := strings.CutPrefix(line, "caller="); ok {
			run.caller = v
		}
	}
	if _, rest, ok := strings.Cut(output, "context-begin\n"); ok {
		if doc, _, ok := strings.Cut(rest, "\ncontext-end"); ok {
			run.context = []byte(doc)
		}
	}
	if run.marker == "" || run.context == nil {
		t.Fatalf("the fixture task did not run:\n%s", output)
	}
	return run
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// TestAppRun_UserScope_OptionalSubcommandRunsInCallerDir pins the no-workspace
// dispatch end to end: a workspace-optional interactive subcommand pinned in
// the user scope runs in the caller's directory, learns that directory from
// its context and from PUTNAMI_CALLER_DIR, gets every workspace path rooted at
// the user scope, and leaves the caller's directory byte-identical.
func TestAppRun_UserScope_OptionalSubcommandRunsInCallerDir(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-launch-names-the-directory-it-was-typed-in", "outside-a-workspace-it-equals-the-caller-directory")
	userRoot, callerDir := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")
	before := snapshotTree(t, callerDir)

	code, output := runApp(t, "audit", "run")
	if code != cli.ExitSuccess {
		t.Fatalf("audit run exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	run := parseUserScopeRun(t, output)
	if run.marker != "user-scope" {
		t.Fatalf("marker = %q, want the user-scope extension", run.marker)
	}
	if want := realPath(t, callerDir); run.pwd != want {
		t.Fatalf("the job ran in %q, want the caller directory %q", run.pwd, want)
	}
	if realPath(t, run.caller) != realPath(t, callerDir) {
		t.Fatalf("PUTNAMI_CALLER_DIR = %q, want the caller directory %q", run.caller, callerDir)
	}

	parsed, diags := protocoljob.ParseAndValidate(run.context)
	if diag.HasErrors(diags) {
		t.Fatalf("the job context violates the protocol: %v\n%s", diags, run.context)
	}
	if parsed.UserScope == nil || realPath(t, parsed.UserScope.CallerDir) != realPath(t, callerDir) {
		t.Fatalf("userScope = %+v, want callerDir %q", parsed.UserScope, callerDir)
	}
	if realPath(t, parsed.WorkspaceRoot) != realPath(t, userRoot) {
		t.Fatalf("workspaceRoot = %q, want the user scope %q", parsed.WorkspaceRoot, userRoot)
	}
	// Every interactive launch names the directory it was typed in; outside a
	// workspace that is the caller directory too.
	if parsed.InvocationDir == "" || realPath(t, parsed.InvocationDir) != realPath(t, callerDir) {
		t.Fatalf("invocationDir = %q, want the caller directory %q", parsed.InvocationDir, callerDir)
	}

	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
	if _, err := os.Stat(filepath.Join(callerDir, ".putnami")); !os.IsNotExist(err) {
		t.Fatalf("the run created .putnami in the caller directory (stat err %v)", err)
	}
}

// TestAppRun_UserScope_InteractiveDeadlineExitsOneAndSaysSo pins a run that
// passes its deadline: the child never traps SIGTERM, so it dies from the signal
// and reports no exit code of its own. putnami names the command and the
// deadline, and exits 1.
func TestAppRun_UserScope_InteractiveDeadlineExitsOneAndSaysSo(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "an-interactive-run-that-passes-its-deadline-exits-one-and-names-it")
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeDeadlineExtension(t, userRoot)

	code, output := runApp(t, "audit", "hang")

	if code != cli.ExitError {
		t.Fatalf("audit hang exit = %d, want %d:\n%s", code, cli.ExitError, output)
	}
	want := "putnami: `audit hang` stopped: it passed its 300ms deadline\n"
	if !strings.Contains(output, want) {
		t.Fatalf("output lacks %q:\n%s", want, output)
	}
}

// TestAppRun_UserScope_InteractiveSignalDeathExitsOneAndSaysWhy pins a child
// that dies from a signal with no deadline involved: exit 1, and the error
// names the signal.
func TestAppRun_UserScope_InteractiveSignalDeathExitsOneAndSaysWhy(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "an-interactive-child-that-ends-from-a-signal-exits-one-and-names-it")
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")

	code, output := runApp(t, "audit", "crash")
	if code != cli.ExitError {
		t.Fatalf("audit crash exit = %d, want %d:\n%s", code, cli.ExitError, output)
	}
	if want := "putnami: signal: killed\n"; !strings.Contains(output, want) {
		t.Fatalf("output lacks %q:\n%s", want, output)
	}
	if strings.Contains(output, "deadline") {
		t.Fatalf("a crash outside a deadline mentions one:\n%s", output)
	}
}

// TestAppRun_UserScope_InteractiveInterruptExitsSignalReceivedSilently pins a
// terminal Ctrl+C that reaches the child before putnami's own cancel does: the
// child ends from SIGINT, and the run is an interrupted one, exit 130, with no
// line from putnami.
func TestAppRun_UserScope_InteractiveInterruptExitsSignalReceivedSilently(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "an-interactive-child-that-ends-from-sigint-is-an-interrupted-run")
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")

	code, output := runApp(t, "audit", "interrupt")
	if code != cli.ExitSignalReceived {
		t.Fatalf("audit interrupt exit = %d, want %d:\n%s", code, cli.ExitSignalReceived, output)
	}
	if strings.Contains(output, "putnami:") {
		t.Fatalf("an interrupted run prints a putnami line:\n%s", output)
	}
}

// TestAppRun_UserScope_LeavesAGitCallerDirClean pins the repository case: run
// from a git working tree, the command leaves `git status --porcelain` empty.
func TestAppRun_UserScope_LeavesAGitCallerDirClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	userRoot, callerDir := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")
	repo := filepath.Join(callerDir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	porcelain := func() string {
		cmd := exec.Command(git.Executable(), "status", "--porcelain", "--untracked-files=all", "--ignored")
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git status: %v\n%s", err, out)
		}
		return string(out)
	}
	gitInit := exec.Command(git.Executable(), "init", "--quiet")
	gitInit.Dir = repo
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if got := porcelain(); got != "" {
		t.Fatalf("a fresh repository is not clean: %q", got)
	}
	t.Chdir(repo)

	if code, output := runApp(t, "audit", "run"); code != cli.ExitSuccess {
		t.Fatalf("audit run exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	if got := porcelain(); got != "" {
		t.Fatalf("git status --porcelain after the run = %q, want empty", got)
	}
}

// TestAppRun_UserScope_WorkspaceRequiredCommandsFail pins the refusal: outside
// a workspace, everything but a workspace-optional interactive subcommand
// prints exactly the no-workspace message and exits 1, and the extension never
// runs.
func TestAppRun_UserScope_WorkspaceRequiredCommandsFail(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")
	before := snapshotTree(t, callerDir)

	for _, argv := range [][]string{
		{"audit", "check"}, // interactive, workspace required
		{"audit", "scan"},  // not interactive
		{"audit", "nope"},  // unknown subcommand
		{"audit-run"},      // the flat command behind them
		{"build"},          // a job command
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			code, output := runApp(t, argv...)
			if code != cli.ExitError {
				t.Fatalf("%v exit = %d, want %d:\n%s", argv, code, cli.ExitError, output)
			}
			if output != noWorkspaceMessage {
				t.Fatalf("%v output = %q, want exactly %q", argv, output, noWorkspaceMessage)
			}
		})
	}
	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
}

// TestAppRun_UserScope_RejectsSelectionFlags pins that project selection is a
// usage error outside a workspace instead of being dropped.
func TestAppRun_UserScope_RejectsSelectionFlags(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")

	for _, argv := range [][]string{
		{"audit", "run", "--projects", "api"},
		{"audit", "--all"},
		{"audit", "run", "--impacted"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			code, output := runApp(t, argv...)
			if code != cli.ExitUsage {
				t.Fatalf("%v exit = %d, want %d:\n%s", argv, code, cli.ExitUsage, output)
			}
			if !strings.Contains(output, "selects workspace projects") {
				t.Fatalf("%v output does not explain the refusal:\n%s", argv, output)
			}
			mustNotContain(t, output, "marker=")
		})
	}
}

// TestAppRun_UserScope_RefusesAnUnhonoredPreview pins that a user-scope
// subcommand, interactive by construction, refuses --plan and a --dry-run it
// does not declare, rather than running for real.
func TestAppRun_UserScope_RefusesAnUnhonoredPreview(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-subcommand-refuses-a-preview-it-cannot-honor", "an-undeclared-interactive-preview-is-refused")
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")

	for _, c := range []struct {
		flag, want string
	}{
		{"--dry-run", "putnami: --dry-run is refused on `putnami audit run`: the interactive subcommand does not declare it, so it would run for real\n"},
		{"--plan", "putnami: --plan is refused on `putnami audit run`: an interactive subcommand has no plan to preview\n"},
	} {
		t.Run(c.flag, func(t *testing.T) {
			code, output := runApp(t, "audit", "run", c.flag)
			if code != cli.ExitUsage {
				t.Fatalf("audit run %s exit = %d, want %d:\n%s", c.flag, code, cli.ExitUsage, output)
			}
			if !strings.Contains(output, c.want) {
				t.Fatalf("audit run %s output does not explain the refusal, want %q:\n%s", c.flag, c.want, output)
			}
			mustNotContain(t, output, "marker=")
		})
	}
}

// TestAppRun_UserScope_GroupDefaultSubcommand pins the group default: a bare
// `putnami audit` runs the declared default, and `putnami audit --help` still
// prints the group help without running anything.
func TestAppRun_UserScope_GroupDefaultSubcommand(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")

	code, output := runApp(t, "audit")
	if code != cli.ExitSuccess {
		t.Fatalf("audit exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	if run := parseUserScopeRun(t, output); run.pwd != realPath(t, callerDir) {
		t.Fatalf("the default subcommand ran in %q, want %q", run.pwd, callerDir)
	}

	code, output = runApp(t, "audit", "--help")
	if code != cli.ExitSuccess {
		t.Fatalf("audit --help exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	for _, want := range []string{"run", "check", "scan"} {
		if !strings.Contains(output, want) {
			t.Fatalf("audit --help does not list %q:\n%s", want, output)
		}
	}
	mustNotContain(t, output, "marker=")
}

// TestAppRun_UserScope_WorkspacePinWins pins the precedence: inside a
// workspace the workspace's own extension answers the group, and the user
// scope, which pins another extension for the same group, is never read.
func TestAppRun_UserScope_WorkspacePinWins(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope-B")

	wsRoot := t.TempDir()
	writeTestFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{"name":"pin-ws","includes":["audit-extension"]}`, 0o644)
	extDir := filepath.Join(wsRoot, "audit-extension")
	writeTestFile(t, filepath.Join(extDir, "putnami.json"), `{"name":"@acme/workspace-audit"}`, 0o644)
	writeTestFile(t, filepath.Join(extDir, "putnami.extension.json"), userScopeExtensionManifest("@acme/workspace-audit", "0.1.0"), 0o644)
	writeTestFile(t, filepath.Join(extDir, "run.sh"), userScopeExtensionScript("workspace-A"), 0o755)
	t.Chdir(wsRoot)

	for _, argv := range [][]string{{"audit", "run"}, {"audit"}, {"audit", "check"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			code, output := runApp(t, argv...)
			if code != cli.ExitSuccess {
				t.Fatalf("%v exit = %d, want %d:\n%s", argv, code, cli.ExitSuccess, output)
			}
			run := parseUserScopeRun(t, output)
			if run.marker != "workspace-A" {
				t.Fatalf("%v ran %q, want the workspace pin", argv, run.marker)
			}
			if run.caller != "" || bytes.Contains(run.context, []byte(`"userScope"`)) {
				t.Fatalf("a workspace run reads as a user-scope run: caller %q\n%s", run.caller, run.context)
			}
			mustNotContain(t, output, "user-scope")
		})
	}
}

// TestAppRun_UserScope_BuiltinsIgnoreABrokenUserScope pins that a built-in
// never depends on the user scope, and that a group lookup reports an
// unreadable user-scope lock as a warning before the no-workspace refusal.
func TestAppRun_UserScope_BuiltinsIgnoreABrokenUserScope(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	writeTestFile(t, filepath.Join(userRoot, lockfile.LockFilename), "{not json", 0o644)

	code, output := runApp(t, "--version")
	if code != cli.ExitSuccess || !strings.HasPrefix(output, "putnami ") {
		t.Fatalf("--version exit = %d, output %q", code, output)
	}
	mustNotContain(t, output, "warning")

	code, output = runApp(t, "help")
	if code != cli.ExitSuccess {
		t.Fatalf("help exit = %d:\n%s", code, output)
	}
	mustNotContain(t, output, "warning")

	code, output = runApp(t, "audit", "run")
	if code != cli.ExitError {
		t.Fatalf("audit run exit = %d, want %d:\n%s", code, cli.ExitError, output)
	}
	if !strings.Contains(output, "putnami: warning: read the user-scope lock") || !strings.HasSuffix(output, noWorkspaceMessage) {
		t.Fatalf("audit run output = %q, want a lock warning then %q", output, noWorkspaceMessage)
	}
}

// TestAppRun_UserScope_UnrepairablePinNamesInstallCommand pins the repair
// remediation at the CLI surface: a pinned extension whose link is gone and
// whose archive cannot be downloaded names the command that reinstalls it.
func TestAppRun_UserScope_UnrepairablePinNamesInstallCommand(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	t.Setenv(extension.PutRegistryURLEnv, srv.URL)
	writeUserScopeLockEntry(t, userRoot, lockfile.LockEntry{
		Version:     "1.0.0",
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): strings.Repeat("a", 64)},
	})
	before := snapshotTree(t, callerDir)

	code, output := runApp(t, "audit", "run")
	if code != cli.ExitError {
		t.Fatalf("audit run exit = %d, want %d:\n%s", code, cli.ExitError, output)
	}
	if !strings.Contains(output, "putnami extensions install --user "+userScopeExtensionName) {
		t.Fatalf("the warning does not name the install command:\n%s", output)
	}
	if !strings.Contains(output, "putnami extensions install --user --latest "+userScopeExtensionName) {
		t.Fatalf("the warning does not name the command that moves the pin:\n%s", output)
	}
	if !strings.HasSuffix(output, noWorkspaceMessage) {
		t.Fatalf("output = %q, want it to end with %q", output, noWorkspaceMessage)
	}
	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
}

// userScopeRegistry serves one extension archive for every request, advertising
// integrity as told, and counts the requests.
type userScopeRegistry struct {
	server   *httptest.Server
	archive  []byte
	digest   string
	requests atomic.Int64
}

func newUserScopeRegistry(t *testing.T, advertise func(digest string) string) *userScopeRegistry {
	t.Helper()
	reg := &userScopeRegistry{archive: userScopeArchive(t, "user-scope")}
	sum := sha256.Sum256(reg.archive)
	reg.digest = hex.EncodeToString(sum[:])
	reg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reg.requests.Add(1)
		w.Header().Set("X-Resolved-Version", "1.0.0")
		if integrity := advertise(reg.digest); integrity != "" {
			w.Header().Set("X-Integrity", integrity)
		}
		_, _ = w.Write(reg.archive)
	}))
	t.Cleanup(reg.server.Close)
	t.Setenv(extension.PutRegistryURLEnv, reg.server.URL)
	return reg
}

// userScopeArchive is the registry archive of the fixture extension.
func userScopeArchive(t *testing.T, marker string) []byte {
	t.Helper()
	return userScopeArchiveAt(t, marker, "1.0.0")
}

// userScopeArchiveAt is the registry archive of one version of the fixture
// extension.
func userScopeArchiveAt(t *testing.T, marker, version string) []byte {
	t.Helper()
	return userScopeArchiveOf(t, marker, userScopeExtensionManifest(userScopeExtensionName, version))
}

// userScopeArchiveOf is the registry archive of the fixture extension with
// manifest as its putnami.extension.json.
func userScopeArchiveOf(t *testing.T, marker, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range []struct {
		name, content string
		mode          int64
	}{
		{"putnami.extension.json", manifest, 0o644},
		{"run.sh", userScopeExtensionScript(marker), 0o755},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readUserScopeLock(t *testing.T, userRoot string) *lockfile.LockFile {
	t.Helper()
	lf, err := lockfile.ReadLockFile(userRoot)
	if err != nil {
		t.Fatalf("read the user-scope lock: %v", err)
	}
	return lf
}

// TestAppRun_ExtensionsInstallUser_LatestMovesThePin pins how a pin moves: a
// re-run keeps it while the registry has a newer release, and --latest moves
// it to that release, which the next run of the command gets.
func TestAppRun_ExtensionsInstallUser_LatestMovesThePin(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	archives := map[string][]byte{
		"1.0.0": userScopeArchiveAt(t, "user-scope-1.0.0", "1.0.0"),
		"1.1.0": userScopeArchiveAt(t, "user-scope-1.1.0", "1.1.0"),
	}
	var latest atomic.Value
	latest.Store("1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		version := latest.Load().(string)
		archive := archives[version]
		sum := sha256.Sum256(archive)
		w.Header().Set("X-Resolved-Version", version)
		w.Header().Set("X-Integrity", "sha256:"+hex.EncodeToString(sum[:]))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(extension.PutRegistryURLEnv, srv.URL)
	before := snapshotTree(t, callerDir)

	install := func(args ...string) {
		t.Helper()
		argv := append([]string{"extensions", "install", "--user"}, args...)
		if code, output := runApp(t, argv...); code != cli.ExitSuccess {
			t.Fatalf("%v exit = %d:\n%s", argv, code, output)
		}
	}
	ran := func() string {
		t.Helper()
		code, output := runApp(t, "audit", "run")
		if code != cli.ExitSuccess {
			t.Fatalf("audit run exit = %d:\n%s", code, output)
		}
		return parseUserScopeRun(t, output).marker
	}

	install(userScopeExtensionName)
	latest.Store("1.1.0")
	install(userScopeExtensionName)
	if got := ran(); got != "user-scope-1.0.0" {
		t.Fatalf("after a re-run the command ran %q, want the kept pin user-scope-1.0.0", got)
	}
	install("--latest", userScopeExtensionName)
	if got := ran(); got != "user-scope-1.1.0" {
		t.Fatalf("after --latest the command ran %q, want user-scope-1.1.0", got)
	}
	if entry, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName); !ok || entry.Version != "1.1.0" {
		t.Fatalf("user-scope lock entry = %+v (present %v), want version 1.1.0", entry, ok)
	}
	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
}

// TestAppRun_ExtensionsInstallUser_LatestKeepsANewerPin pins that --latest
// never moves a user-scope pin to an older build: when latest serves 1.0.0
// below the pinned 1.1.0, the install keeps 1.1.0, names the channel and the
// build it serves, never downloads 1.0.0, and the command runs 1.1.0.
func TestAppRun_ExtensionsInstallUser_LatestKeepsANewerPin(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	archives := map[string][]byte{
		"1.0.0": userScopeArchiveAt(t, "user-scope-1.0.0", "1.0.0"),
		"1.1.0": userScopeArchiveAt(t, "user-scope-1.1.0", "1.1.0"),
	}
	var latest atomic.Value
	latest.Store("1.1.0")
	var olderDownloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := latest.Load().(string)
		if _, ok := archives[r.URL.Query().Get("channel")]; ok {
			version = r.URL.Query().Get("channel")
		}
		if r.Method == http.MethodGet && version == "1.0.0" {
			olderDownloads.Add(1)
		}
		archive := archives[version]
		sum := sha256.Sum256(archive)
		w.Header().Set("X-Resolved-Version", version)
		w.Header().Set("X-Integrity", "sha256:"+hex.EncodeToString(sum[:]))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(extension.PutRegistryURLEnv, srv.URL)

	if code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName); code != cli.ExitSuccess {
		t.Fatalf("install exit = %d:\n%s", code, output)
	}
	latest.Store("1.0.0")
	code, output := runApp(t, "extensions", "install", "--user", "--latest", userScopeExtensionName)
	if want := "(kept: channel latest serves the older build 1.0.0)"; code != cli.ExitSuccess || !strings.Contains(output, want) {
		t.Fatalf("install --latest exit = %d, want success with %q:\n%s", code, want, output)
	}
	if n := olderDownloads.Load(); n != 0 {
		t.Errorf("install --latest downloaded the older build %d time(s)", n)
	}
	if entry, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName); !ok || entry.Version != "1.1.0" {
		t.Fatalf("user-scope lock entry = %+v (present %v), want version 1.1.0", entry, ok)
	}
	code, output = runApp(t, "audit", "run")
	if code != cli.ExitSuccess {
		t.Fatalf("audit run exit = %d:\n%s", code, output)
	}
	if got := parseUserScopeRun(t, output).marker; got != "user-scope-1.1.0" {
		t.Fatalf("after install --latest the command ran %q, want the kept pin user-scope-1.1.0", got)
	}
}

// TestAppRun_ExtensionsInstallUser_ARefusalKeepsThePriorPin pins that a
// refused install leaves the build that worked before it. 2.0.0 names an
// extension dependency, which the user scope refuses: the stable link still
// resolves 1.0.0, the lock still pins it, and the command runs it.
func TestAppRun_ExtensionsInstallUser_ARefusalKeepsThePriorPin(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	current := fmt.Sprintf(`"cliContract": %d,`, protocolcli.CurrentContract)
	dependent := userScopeExtensionManifest(userScopeExtensionName, "2.0.0")
	if !strings.Contains(dependent, current) {
		t.Fatalf("the fixture manifest has no %s", current)
	}
	dependent = strings.Replace(dependent, current,
		fmt.Sprintf(`"cliContract": %d, "extensionDependencies": {"@acme/dev": "^1.0.0"},`, protocolcli.ExtensionDependenciesContract), 1)
	archives := map[string][]byte{
		"1.0.0": userScopeArchiveAt(t, "user-scope-1.0.0", "1.0.0"),
		"2.0.0": userScopeArchiveOf(t, "user-scope-2.0.0", dependent),
	}
	var latest atomic.Value
	latest.Store("1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := latest.Load().(string)
		if _, ok := archives[r.URL.Query().Get("channel")]; ok {
			version = r.URL.Query().Get("channel")
		}
		archive := archives[version]
		sum := sha256.Sum256(archive)
		w.Header().Set("X-Resolved-Version", version)
		w.Header().Set("X-Integrity", "sha256:"+hex.EncodeToString(sum[:]))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(extension.PutRegistryURLEnv, srv.URL)

	if code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName); code != cli.ExitSuccess {
		t.Fatalf("install exit = %d:\n%s", code, output)
	}
	latest.Store("2.0.0")
	code, output := runApp(t, "extensions", "install", "--user", "--latest", userScopeExtensionName)
	if code == cli.ExitSuccess || !strings.Contains(output, userScopeExtensionName+"@2.0.0 names extension dependencies (@acme/dev)") {
		t.Fatalf("install --latest exit = %d, want the refusal:\n%s", code, output)
	}

	stable := layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName)
	data, err := os.ReadFile(filepath.Join(stable, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("after the refusal the stable link does not resolve: %v", err)
	}
	var linked struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &linked); err != nil || linked.Version != "1.0.0" {
		t.Fatalf("after the refusal the stable link resolves version %q (%v), want the pinned 1.0.0", linked.Version, err)
	}
	if entry, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName); !ok || entry.Version != "1.0.0" {
		t.Fatalf("user-scope lock entry = %+v (present %v), want version 1.0.0", entry, ok)
	}
	code, output = runApp(t, "audit", "run")
	if code != cli.ExitSuccess {
		t.Fatalf("audit run exit = %d:\n%s", code, output)
	}
	if got := parseUserScopeRun(t, output).marker; got != "user-scope-1.0.0" {
		t.Fatalf("after the refusal the command ran %q, want user-scope-1.0.0", got)
	}
}

// TestAppRun_UserScope_RunsInAJavaScriptMonorepo pins that a root package.json
// declaring "workspaces" is not a workspace root: from the monorepo root and
// from a package, a command group the user scope pins runs from the user scope
// in that directory, a subcommand that needs a workspace is refused as outside
// one, and nothing is bootstrapped or written in the monorepo.
func TestAppRun_UserScope_RunsInAJavaScriptMonorepo(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	pinUserScopeExtension(t, userRoot, "user-scope")
	writeMonorepo(t, callerDir)
	packageDir := filepath.Join(callerDir, "packages", "app")
	before := snapshotTree(t, callerDir)

	for _, dir := range []string{callerDir, packageDir} {
		t.Chdir(dir)
		code, output := runApp(t, "audit", "run")
		if code != cli.ExitSuccess {
			t.Fatalf("audit run in %s exit = %d, want %d:\n%s", dir, code, cli.ExitSuccess, output)
		}
		run := parseUserScopeRun(t, output)
		if run.marker != "user-scope" {
			t.Fatalf("audit run in %s ran %q, want the user-scope extension", dir, run.marker)
		}
		if want := realPath(t, dir); run.pwd != want {
			t.Fatalf("the job ran in %q, want %q", run.pwd, want)
		}
	}

	code, output := runApp(t, "audit", "check")
	if code != cli.ExitError || !strings.HasSuffix(output, noWorkspaceMessage) {
		t.Fatalf("audit check exit = %d, output %q; want %d and %q", code, output, cli.ExitError, noWorkspaceMessage)
	}
	assertSameTree(t, "the monorepo", before, snapshotTree(t, callerDir))
}

// writeMonorepo writes an npm monorepo root at dir: a package.json declaring
// "workspaces" and one package.
func writeMonorepo(t *testing.T, dir string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, "package.json"), `{"name":"mono","private":true,"workspaces":["packages/*"]}`, 0o644)
	writeTestFile(t, filepath.Join(dir, "packages", "app", "package.json"), `{"name":"app"}`, 0o644)
}

// TestAppRun_UserScope_RepairsThePinAndResolvesAliases pins that a pin whose
// link and artifact tree are gone is downloaded again and runs, that an alias
// from ~/.putnami/config.json reaches the group, and that the caller directory
// stays untouched.
func TestAppRun_UserScope_RepairsThePinAndResolvesAliases(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	reg := newUserScopeRegistry(t, func(digest string) string { return "sha256:" + digest })
	if code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName); code != cli.ExitSuccess {
		t.Fatalf("extensions install --user exit = %d:\n%s", code, output)
	}
	writeTestFile(t, filepath.Join(os.Getenv("HOME"), ".putnami", "config.json"), `{"aliases":{"au":"audit"}}`, 0o644)
	for _, dir := range []string{layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName), os.Getenv("PUTNAMI_ARTIFACT_DIR")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	// The next invocation is a new process, which owns the repair.
	t.Setenv(extension.UserScopeRepairEnv, "")
	downloads := reg.requests.Load()
	before := snapshotTree(t, callerDir)

	for _, argv := range [][]string{{"audit", "run"}, {"au", "run"}} {
		code, output := runApp(t, argv...)
		if code != cli.ExitSuccess {
			t.Fatalf("%v exit = %d, want %d:\n%s", argv, code, cli.ExitSuccess, output)
		}
		if run := parseUserScopeRun(t, output); run.marker != "user-scope" {
			t.Fatalf("%v ran %q, want the user-scope extension", argv, run.marker)
		}
	}
	if reg.requests.Load() == downloads {
		t.Fatal("the pin whose tree was gone was not downloaded again")
	}
	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
}

// TestAppRun_ExtensionsInstallUser_PinsVerifiesAndRuns pins the whole user
// scope lifecycle from a directory that is not a workspace: install verifies
// the archive and writes the lock entry and link, a re-run changes nothing and
// downloads nothing, the pinned command then runs, list reports the pin, and
// remove drops it. The caller's directory is never touched.
func TestAppRun_ExtensionsInstallUser_PinsVerifiesAndRuns(t *testing.T) {
	userRoot, callerDir := userScopeTestEnv(t)
	reg := newUserScopeRegistry(t, func(digest string) string { return "sha256:" + digest })
	before := snapshotTree(t, callerDir)

	code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName)
	if code != cli.ExitSuccess {
		t.Fatalf("extensions install --user exit = %d:\n%s", code, output)
	}
	lockPath := filepath.Join(userRoot, lockfile.LockFilename)
	firstLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("the user-scope lock was not written: %v", err)
	}
	entry, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName)
	if !ok || entry.Version != "1.0.0" || entry.IntegrityFor(runtime.GOOS, runtime.GOARCH) != reg.digest {
		t.Fatalf("user-scope lock entry = %+v (present %v), want version 1.0.0 with digest %s", entry, ok, reg.digest)
	}
	if _, err := os.Stat(filepath.Join(layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName), "putnami.extension.json")); err != nil {
		t.Fatalf("the user-scope link does not resolve: %v", err)
	}

	downloads := reg.requests.Load()
	code, output = runApp(t, "extensions", "install", "--user", userScopeExtensionName, "--output=json")
	if code != cli.ExitSuccess {
		t.Fatalf("second extensions install --user exit = %d:\n%s", code, output)
	}
	if !json.Valid([]byte(output)) || !strings.Contains(output, "cached") {
		t.Fatalf("extensions install --user --output=json is not one JSON result reporting the cached pin:\n%s", output)
	}
	if secondLock, _ := os.ReadFile(lockPath); !bytes.Equal(firstLock, secondLock) {
		t.Fatalf("a re-run rewrote the user-scope lock:\n%s\n---\n%s", firstLock, secondLock)
	}
	if got := reg.requests.Load(); got != downloads {
		t.Fatalf("a re-run made %d registry requests, want none", got-downloads)
	}

	code, output = runApp(t, "audit")
	if code != cli.ExitSuccess {
		t.Fatalf("audit after install exit = %d:\n%s", code, output)
	}
	if run := parseUserScopeRun(t, output); run.pwd != realPath(t, callerDir) {
		t.Fatalf("the installed command ran in %q, want %q", run.pwd, callerDir)
	}

	code, output = runApp(t, "extensions", "list", "--user", "--output=jsonl")
	if code != cli.ExitSuccess {
		t.Fatalf("extensions list --user exit = %d:\n%s", code, output)
	}
	for _, want := range []string{`"name":"` + userScopeExtensionName + `"`, `"installed":"1.0.0"`, `"source":"user"`} {
		if !strings.Contains(output, want) {
			t.Fatalf("extensions list --user --output=jsonl lacks %s:\n%s", want, output)
		}
	}
	if line := strings.TrimSpace(output); strings.Contains(line, "\n") || !json.Valid([]byte(line)) {
		t.Fatalf("extensions list --user --output=jsonl is not one JSON line:\n%s", output)
	}

	code, output = runApp(t, "extensions", "remove", "--user", userScopeExtensionName)
	if code != cli.ExitSuccess {
		t.Fatalf("extensions remove --user exit = %d:\n%s", code, output)
	}
	if _, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName); ok {
		t.Fatal("extensions remove --user kept the lock entry")
	}
	if _, err := os.Lstat(layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName)); !os.IsNotExist(err) {
		t.Fatalf("extensions remove --user kept the link (lstat err %v)", err)
	}
	if code, output := runApp(t, "audit", "run"); code != cli.ExitError || output != noWorkspaceMessage {
		t.Fatalf("audit run after remove: exit %d, output %q", code, output)
	}

	assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
}

// TestAppRun_ExtensionsInstallUser_RefusesUnverifiedArchives pins that the user
// scope verifies archives exactly like a workspace: a digest mismatch is
// refused, an archive with no digest is refused unless PUTNAMI_UNSAFE_INSTALL
// is set, and a refusal writes neither a lock entry nor a link.
func TestAppRun_ExtensionsInstallUser_RefusesUnverifiedArchives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		advertise func(string) string
		want      string
	}{
		{"digest mismatch", func(string) string { return "sha256:" + strings.Repeat("0", 64) }, "integrity"},
		{"no digest", func(string) string { return "" }, "refusing to install unverified bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userRoot, callerDir := userScopeTestEnv(t)
			newUserScopeRegistry(t, tc.advertise)
			before := snapshotTree(t, callerDir)

			code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName)
			if code == cli.ExitSuccess {
				t.Fatalf("extensions install --user accepted an unverified archive:\n%s", output)
			}
			if !strings.Contains(output, tc.want) {
				t.Fatalf("output does not report %q:\n%s", tc.want, output)
			}
			if lf := readUserScopeLock(t, userRoot); lf != nil {
				if _, ok := lf.GetExtension(userScopeExtensionName); ok {
					t.Fatal("a refused install wrote a lock entry")
				}
			}
			if _, err := os.Lstat(layout.StableDir(userRoot, layout.Extensions, userScopeExtensionName)); !os.IsNotExist(err) {
				t.Fatalf("a refused install left a link (lstat err %v)", err)
			}
			assertSameTree(t, "the caller directory", before, snapshotTree(t, callerDir))
		})
	}
}

// TestAppRun_ExtensionsInstallUser_LeavesTheWorkspaceUntouched pins that the
// user scope is managed the same way from inside a workspace: the workspace's
// manifest, lock and state are neither read for the pin nor written.
func TestAppRun_ExtensionsInstallUser_LeavesTheWorkspaceUntouched(t *testing.T) {
	userRoot, _ := userScopeTestEnv(t)
	newUserScopeRegistry(t, func(digest string) string { return "sha256:" + digest })
	wsRoot := t.TempDir()
	writeTestFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{"name":"untouched-ws"}`, 0o644)
	writeTestFile(t, filepath.Join(wsRoot, lockfile.LockFilename), `{"version":1}`, 0o644)
	t.Chdir(wsRoot)
	before := snapshotTree(t, wsRoot)

	code, output := runApp(t, "extensions", "install", "--user", userScopeExtensionName)
	if code != cli.ExitSuccess {
		t.Fatalf("extensions install --user inside a workspace exit = %d:\n%s", code, output)
	}
	if _, ok := readUserScopeLock(t, userRoot).GetExtension(userScopeExtensionName); !ok {
		t.Fatal("the pin did not reach the user-scope lock")
	}
	assertSameTree(t, "the workspace", before, snapshotTree(t, wsRoot))
}

// TestAppRun_ExtensionsUserScope_UsageErrors pins the argument contract of the
// user-scope subcommands.
func TestAppRun_ExtensionsUserScope_UsageErrors(t *testing.T) {
	userScopeTestEnv(t)
	for _, argv := range [][]string{
		{"extensions", "install", "--user"},
		{"extensions", "install", "--user", "./local-extension"},
		{"extensions", "install", "--user", "@acme/a", "@acme/b"},
		{"extensions", "install", "--user", "--platform", "linux-x64", userScopeExtensionName},
		{"extensions", "update", "--user"},
		{"extensions", "list", "--user", userScopeExtensionName},
		{"extensions", "remove", "--user"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			code, output := runApp(t, argv...)
			if code != cli.ExitUsage {
				t.Fatalf("%v exit = %d, want %d:\n%s", argv, code, cli.ExitUsage, output)
			}
		})
	}
	if code, output := runApp(t, "extensions", "remove", "--user", userScopeExtensionName); code != cli.ExitError ||
		!strings.Contains(output, "not installed in the user scope") {
		t.Fatalf("removing an absent pin: exit %d, output %q", code, output)
	}
}

func mustNotContain(t *testing.T, output, needle string) {
	t.Helper()
	if strings.Contains(output, needle) {
		t.Errorf("output unexpectedly contained %q\nfull output:\n%s", needle, output)
	}
}
