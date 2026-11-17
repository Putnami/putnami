package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	relayFeature           = "cli/agent-workflow-lifecycle"
	relayRequirement       = "host-hook-relay"
	passthroughRequirement = "host-hook-relay-passthrough"

	// helperEnv turns the test binary into the relay's child: it reports what
	// it received and exits with the code its last argument names.
	helperEnv = "HOSTRELAY_TEST_CHILD"
)

// childReport is what the helper child prints on standard output.
type hostHookChildReport struct {
	Dir   string   `json:"dir"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
	Env   []string `json:"env"`
}

// hostHookTestHome is the user home of the relay tests: it records every
// workspace newHostHookWorkspace makes.
var hostHookTestHome string

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHostHookChild())
	}
	home, err := os.MkdirTemp("", "host-hook-home-")
	if err != nil {
		panic(err)
	}
	hostHookTestHome = home
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func runHostHookChild() int {
	dir, _ := os.Getwd()
	stdin, _ := io.ReadAll(os.Stdin)
	var env []string
	for _, name := range []string{LaunchedEnv, NoRelaunchEnv, FromSourceEnv, FromSourceKeyEnv, "KEPT"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	args := os.Args[1:]
	_ = json.NewEncoder(os.Stdout).Encode(hostHookChildReport{Dir: dir, Args: args, Stdin: string(stdin), Env: env})
	_, _ = fmt.Fprintln(os.Stderr, "child stderr")
	code, _ := strconv.Atoi(args[len(args)-1])
	return code
}

// newWorkspace makes a workspace root with a sub directory, both resolved.
func newHostHookWorkspace(t *testing.T) (root, sub string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub = filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecordHostHookRepository(hostHookTestHome, root); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

type hostHookRun struct {
	code           int
	stdout, stderr string
}

func relayHostHook(t *testing.T, event string, ownDir string, environ []string, args ...string) hostHookRun {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunHostHook(context.Background(), args, HostHookEnv{
		Stdin:      strings.NewReader(event),
		Stdout:     &stdout,
		Stderr:     &stderr,
		Getwd:      func() (string, error) { return ownDir, nil },
		Executable: func() (string, error) { return self, nil },
		Environ:    func() []string { return append([]string{helperEnv + "=1"}, environ...) },
		HomeDir:    func() (string, error) { return hostHookTestHome, nil },
	})
	return hostHookRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r hostHookRun) report(t *testing.T) hostHookChildReport {
	t.Helper()
	var report hostHookChildReport
	if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
		t.Fatalf("child output %q: %v (stderr %q)", r.stdout, err, r.stderr)
	}
	return report
}

func hostHookEventIn(dir string) string {
	data, _ := json.Marshal(map[string]string{"hook_event_name": "Stop", "cwd": dir, "session_id": "s1"})
	return string(data)
}

func TestRelayRunsThePathInTheEventWorkspace(t *testing.T) {
	spectest.Proves(t, relayFeature, relayRequirement, "the-relay-runs-the-path-in-the-event-workspace")
	_, sub := newHostHookWorkspace(t)
	stale, _ := newHostHookWorkspace(t)
	event := hostHookEventIn(sub)

	run := relayHostHook(t, event, stale, nil, "agent", "hook", "--engine", "claude-code", "0")

	if run.code != 0 {
		t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
	}
	report := run.report(t)
	if report.Dir != sub {
		t.Fatalf("the child ran in %q, want the event's cwd %q", report.Dir, sub)
	}
	if got := strings.Join(report.Args, " "); got != "agent hook --engine claude-code 0" {
		t.Fatalf("the child got args %q", got)
	}
}

func TestRelayReplaysTheEventAndPassesTheExitCode(t *testing.T) {
	spectest.Proves(t, relayFeature, passthroughRequirement, "the-relay-replays-the-event-and-passes-the-exit-code")
	_, sub := newHostHookWorkspace(t)
	event := hostHookEventIn(sub) + "\n  \x00trailing bytes"

	run := relayHostHook(t, event, sub, nil, "agent", "hook", "--idle-wake", "75")

	if run.code != 75 {
		t.Fatalf("exit %d, want the child's 75", run.code)
	}
	if got := run.report(t).Stdin; got != event {
		t.Fatalf("the child read %q, want the event byte for byte", got)
	}
	if !strings.Contains(run.stderr, "child stderr") {
		t.Fatalf("the child's standard error did not pass through: %q", run.stderr)
	}
}

func TestRelayFallsBackToItsOwnDirectory(t *testing.T) {
	spectest.Proves(t, relayFeature, relayRequirement, "the-relay-falls-back-to-its-own-directory")
	_, own := newHostHookWorkspace(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]string{
		"no workspace":  hostHookEventIn(outside),
		"relative cwd":  hostHookEventIn("pkg"),
		"missing cwd":   hostHookEventIn(filepath.Join(outside, "gone")),
		"not json":      "not json",
		"no cwd member": `{"hook_event_name":"Stop"}`,
	} {
		t.Run(name, func(t *testing.T) {
			run := relayHostHook(t, event, own, nil, "agent", "hook", "0")
			if run.code != 0 {
				t.Fatalf("exit %d, stderr %q", run.code, run.stderr)
			}
			if got := run.report(t).Dir; got != own {
				t.Fatalf("the child ran in %q, want the relay's own directory %q", got, own)
			}
		})
	}
}

func TestRelayRunsASourceWorkspaceWrapper(t *testing.T) {
	spectest.Proves(t, relayFeature, relayRequirement, "the-relay-runs-a-source-workspace-wrapper")
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	root, sub := newHostHookWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "putnami.lock.json"), []byte(`{"version":5,"cli":{"source":"workspace"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "wrapper-ran")
	script := "#!/bin/sh\npwd > '" + marker + "'\necho \"$@\" >> '" + marker + "'\nexit 3\n"
	if err := os.WriteFile(filepath.Join(root, "putnamiw"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	run := relayHostHook(t, hostHookEventIn(sub), t.TempDir(), nil, "agent", "hook")

	if run.code != 3 {
		t.Fatalf("exit %d, want the wrapper's 3 (stderr %q)", run.code, run.stderr)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the source workspace's putnamiw did not run: %v", err)
	}
	if want := sub + "\nagent hook\n"; string(data) != want {
		t.Fatalf("the wrapper recorded %q, want %q", data, want)
	}
}

func TestRelayChildDropsTheLauncherGuards(t *testing.T) {
	spectest.Proves(t, relayFeature, passthroughRequirement, "the-child-drops-the-launcher-guards")
	_, sub := newHostHookWorkspace(t)
	environ := []string{
		LaunchedEnv + "=abc",
		NoRelaunchEnv + "=1",
		FromSourceEnv + "=/elsewhere",
		FromSourceKeyEnv + "=key",
		"KEPT=yes",
	}

	report := relayHostHook(t, hostHookEventIn(sub), sub, environ, "agent", "hook", "0").report(t)

	if got := strings.Join(report.Env, ","); got != "KEPT=yes" {
		t.Fatalf("the child inherited %q, want only KEPT=yes", got)
	}
}

func TestRelayReportsAChildThatCannotStart(t *testing.T) {
	spectest.Proves(t, relayFeature, passthroughRequirement, "a-child-that-cannot-start-exits-1")
	_, sub := newHostHookWorkspace(t)
	var stdout, stderr bytes.Buffer
	code := RunHostHook(context.Background(), []string{"agent", "hook"}, HostHookEnv{
		Stdin:      strings.NewReader(hostHookEventIn(sub)),
		Stdout:     &stdout,
		Stderr:     &stderr,
		Getwd:      func() (string, error) { return sub, nil },
		Executable: func() (string, error) { return filepath.Join(sub, "no-such-putnami"), nil },
		Environ:    os.Environ,
		HomeDir:    func() (string, error) { return hostHookTestHome, nil },
	})
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "putnami host-hook:") {
		t.Fatalf("exit %d, stdout %q, stderr %q: want 1, nothing on stdout, the reason on stderr", code, stdout.String(), stderr.String())
	}
}

func TestRelayRefusesNoPathAndAnOversizedEvent(t *testing.T) {
	_, sub := newHostHookWorkspace(t)
	for name, tc := range map[string]struct {
		args  []string
		stdin io.Reader
	}{
		"no path":   {nil, strings.NewReader(hostHookEventIn(sub))},
		"oversized": {[]string{"agent", "hook"}, io.LimitReader(spaceReader{}, HostHookEventMax+1)},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunHostHook(context.Background(), tc.args, HostHookEnv{
				Stdin: tc.stdin, Stdout: &stdout, Stderr: &stderr,
				Getwd:      func() (string, error) { return sub, nil },
				Executable: os.Executable,
				Environ:    os.Environ,
			})
			if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestHostHookIsExemptFromTheLauncher(t *testing.T) {
	if !IsExemptInvocation([]string{HostHookCommand, "agent", "hook"}) {
		t.Fatal("host-hook must not relaunch into the pin of the directory the host started it in")
	}
}

type spaceReader struct{}

func (spaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// A repository the person never installed is not served: a user hook runs in
// every session, and the relay would start that repository's code.
func TestRelayServesOnlyARecordedRepository(t *testing.T) {
	spectest.Proves(t, relayFeature, relayRequirement, "the-relay-serves-only-a-recorded-repository")
	_, sub := newHostHookWorkspace(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunHostHook(context.Background(), []string{"agent", "hook", "0"}, HostHookEnv{
		Stdin: strings.NewReader(hostHookEventIn(sub)), Stdout: &stdout, Stderr: &stderr,
		Getwd:      func() (string, error) { return sub, nil },
		Executable: func() (string, error) { return self, nil },
		Environ:    func() []string { return []string{helperEnv + "=1"} },
		HomeDir:    func() (string, error) { return t.TempDir(), nil },
	})
	if code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "run any putnami command there") {
		t.Fatalf("exit %d, stdout %q, stderr %q: want 0, no child, the reason", code, stdout.String(), stderr.String())
	}
}

// Every worktree of a clone names the clone's common Git directory, so the
// record of one checkout serves the others.
func TestHostHookRepositoryNamesTheCloneOfAWorktree(t *testing.T) {
	spectest.Proves(t, relayFeature, relayRequirement, "the-relay-serves-only-a-recorded-repository")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "main")
	common := filepath.Join(main, ".git")
	gitDir := filepath.Join(common, "worktrees", "topic")
	tree := filepath.Join(base, "topic")
	for _, dir := range []string{gitDir, tree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "gitdir"), []byte(filepath.Join(tree, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tree whose .git file names the clone without the clone pointing back
	// is no worktree of it.
	impostor := filepath.Join(base, "impostor")
	if err := os.MkdirAll(impostor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(impostor, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HostHookRepository(impostor); got != impostor {
		t.Fatalf("an impostor .git file names %q, want its own root", got)
	}
	if got := HostHookRepository(main); got != common {
		t.Fatalf("main checkout names %q, want %q", got, common)
	}
	if got := HostHookRepository(tree); got != common {
		t.Fatalf("worktree names %q, want %q", got, common)
	}
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := HostHookRepository(plain); got != plain {
		t.Fatalf("a directory outside Git names %q", got)
	}

	home := t.TempDir()
	if err := RecordHostHookRepository(home, main); err != nil {
		t.Fatal(err)
	}
	if !HostHookRepositoryRecorded(home, tree) || HostHookRepositoryRecorded(home, plain) {
		t.Fatal("the record does not follow the clone")
	}
	// A repeated record keeps one entry.
	if err := RecordHostHookRepository(home, tree); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(HostHookTrustPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), common) != 1 {
		t.Fatalf("record:\n%s", data)
	}
	// An unreadable record records nothing, and is not overwritten.
	if err := os.WriteFile(HostHookTrustPath(home), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if HostHookRepositoryRecorded(home, main) || RecordHostHookRepository(home, plain) == nil {
		t.Fatal("an unreadable record was trusted or overwritten")
	}
}
