//go:build unix

// Package servedependency starts two serve runs of the built CLI side by side
// in one on-disk workspace, as two terminals or a composition do: the first
// serves a provider, the second serves a consumer that depends on the
// provider, so the second run plans the provider's finite serve step while the
// provider serves. Each run is its own CLI process, so this package runs the
// CLI fixtureproc builds rather than cli.NewApp in the test process.
package servedependency

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// hangDetector bounds a wait for an answer that must arrive. It detects a
// hang; it is no latency budget.
const hangDetector = 120 * time.Second

func TestMain(m *testing.M) {
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-servedependency-test-")
	if err == nil {
		_ = os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		_ = os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := clitest.Main(m)
	fixtureproc.Remove()
	if dir != nil {
		_ = dir.Remove()
	}
	os.Exit(code)
}

// dependencyWorkspace writes a workspace of two projects served by one
// extension, and returns its root. Its serve command has the shape of a
// language extension's: a cached generate step, which also runs the generate
// step of every project the served one depends on, writes the serve command
// output; the serve step after it claims readiness and serves until its
// project's stop file exists. The consumer depends on the provider.
func dependencyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"serve-dependency","includes":["extension","provider","consumer"]}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/serve"}`)
	clitest.WriteFile(t, filepath.Join(root, "provider", "putnami.json"),
		`{"name":"provider","extensions":["@fixture/serve"]}`)
	clitest.WriteFile(t, filepath.Join(root, "consumer", "putnami.json"),
		`{"name":"consumer","extensions":["@fixture/serve"],"dependencies":["provider"]}`)
	for _, project := range []string{"provider", "consumer"} {
		clitest.WriteFile(t, filepath.Join(root, project, "serve.project"), project+"\n")
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "generate.sh"),
		`printf 'generated %s\n' "$PUTNAMI_PROJECT_NAME" > "$PUTNAMI_OUTPUT_PATH/generate.txt"`+"\n")
	serve := fmt.Sprintf(`printf '{"v":2,"type":"ready","target":"server","name":"%%s","endpoints":[{"scheme":"http","host":"127.0.0.1","port":43120}]}\n' "$PUTNAMI_PROJECT_NAME"
while [ ! -f "$PUTNAMI_PROJECT_ROOT/stop" ]; do
  if ! kill -0 %d 2>/dev/null; then exit 125; fi
  sleep 0.05
done
`, os.Getpid())
	clitest.WriteFile(t, filepath.Join(root, "extension", "serve.sh"), serve)
	enabled, disabled := true, false
	manifest := extensionproto.Manifest{
		Name: "@fixture/serve", Version: "1.0.0", CLIContract: protocolcli.CommandKindContract,
		Commands: map[string]extensionproto.CommandDefinition{
			"serve": {
				ActivationFiles: []string{"serve.project"},
				Traits:          &extensionproto.CommandTraits{Workload: extensionproto.WorkloadServe},
				Flags: map[string]extensionproto.FlagDefinition{
					"port":  {Type: "number"},
					"watch": {Type: "boolean", Default: true},
				},
				Run: []extensionproto.PipelineStep{
					{ID: "generate", Task: "serve-generate", DependsOn: []string{"^generate"}},
					{ID: "serve", Task: "serve-run", DependsOn: []string{"generate"}},
				},
			},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"serve-generate": {
				Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/generate.sh"},
				Cache:     &extensionproto.TaskCachePolicy{Enabled: &enabled},
				TimeoutMs: -1,
				Declares: &extensionproto.TaskDeclaration{Outputs: map[string]extensionproto.DeclaredOutput{
					"stamp": {Kind: extensionproto.OutputKindFile, Root: extensionproto.OutputRootCommandOutput, Path: "generate.txt"},
				}},
			},
			"serve-run": {
				Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/serve.sh"},
				Cache:     &extensionproto.TaskCachePolicy{Enabled: &disabled},
				TimeoutMs: -1,
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	return root
}

// serveRun is one `putnami serve` process a test started.
type serveRun struct {
	project string
	cmd     *exec.Cmd
	records chan protocolcli.SessionStreamRecord
	stderr  *syncBuffer
	done    chan struct{}
	err     error
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServe starts `putnami serve` of project in root, unwatched, with the
// session stream on stdout. Its records arrive on run.records, which closes
// with stdout.
func startServe(t *testing.T, root, project string) *serveRun {
	t.Helper()
	cmd := exec.Command(fixtureproc.CLI(t), "serve", "--projects", project, "--port", "0", "--no-watch", "--output=jsonl")
	cmd.Dir = root
	cmd.Env = fixtureproc.CLIEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	run := &serveRun{project: project, cmd: cmd, records: make(chan protocolcli.SessionStreamRecord, 64), stderr: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stderr = run.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start putnami serve %s: %v", project, err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var record protocolcli.SessionStreamRecord
			if json.Unmarshal(scanner.Bytes(), &record) == nil {
				run.records <- record
			}
		}
		close(run.records)
		run.err = cmd.Wait()
		close(run.done)
	}()
	t.Cleanup(func() {
		stop(t, root, project)
		select {
		case <-run.done:
		case <-time.After(hangDetector):
			_ = cmd.Process.Kill()
			<-run.done
		}
	})
	return run
}

// stop creates project's stop file, which ends its serve step.
func stop(t *testing.T, root, project string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, project, "stop"), nil, 0o644); err != nil {
		t.Error(err)
	}
}

// awaitReady returns the run's records up to its serve step's ready claim. It
// fails when the run ends first, when it prints a task-output lock wait line,
// or when no claim arrives in time.
func (r *serveRun) awaitReady(t *testing.T) []protocolcli.SessionStreamRecord {
	t.Helper()
	var seen []protocolcli.SessionStreamRecord
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(hangDetector)
	for {
		select {
		case record, ok := <-r.records:
			if !ok {
				t.Fatalf("putnami serve %s ended before its serve step was ready\nstderr:\n%s", r.project, r.stderr.String())
			}
			seen = append(seen, record)
			if record.Record == protocolcli.RecordTaskEvent && record.Identity != nil &&
				record.Identity.Key == "/"+r.project+":serve~serve" && record.Event["type"] == "ready" {
				return seen
			}
		case <-poll.C:
			if line := lockWaitLine(r.stderr.String()); line != "" {
				t.Fatalf("putnami serve %s waits for a task-output lock instead of coming up: %s", r.project, line)
			}
		case <-deadline:
			t.Fatalf("putnami serve %s was not ready within %s\nstderr:\n%s", r.project, hangDetector, r.stderr.String())
		}
	}
}

// lockWaitLine returns the first task-output lock wait line in stderr, or "".
func lockWaitLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "putnami: ") && strings.Contains(line, " waits for ") {
			return line
		}
	}
	return ""
}

// exitCode waits for the run to end and returns its exit code.
func (r *serveRun) exitCode(t *testing.T) int {
	t.Helper()
	for {
		select {
		case _, ok := <-r.records:
			if ok {
				continue
			}
			select {
			case <-r.done:
			case <-time.After(hangDetector):
				t.Fatalf("putnami serve %s did not exit within %s\nstderr:\n%s", r.project, hangDetector, r.stderr.String())
			}
			var exitErr *exec.ExitError
			switch {
			case r.err == nil:
				return 0
			case errors.As(r.err, &exitErr):
				return exitErr.ExitCode()
			}
			t.Fatalf("wait putnami serve %s: %v", r.project, r.err)
		case <-time.After(hangDetector):
			t.Fatalf("putnami serve %s did not close stdout within %s\nstderr:\n%s", r.project, hangDetector, r.stderr.String())
		}
	}
}

// ended returns the status and reuse of the task:end record of key in records.
func ended(records []protocolcli.SessionStreamRecord, key string) (status, reuse string, found bool) {
	for _, record := range records {
		if record.Record == protocolcli.RecordTaskEnd && record.Task != nil && record.Task.Identity.Key == key {
			return record.Task.Status, record.Task.Reuse, true
		}
	}
	return "", "", false
}

// TestADependentsServeRunComesUpBesideItsDependencysServeRun serves the
// provider, then the consumer that depends on it, each in its own CLI process.
// The consumer's run plans the provider's generate step, which writes the
// provider's serve command output while the provider serves; both runs come
// up, and each ends with its serve step.
func TestADependentsServeRunComesUpBesideItsDependencysServeRun(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-command-output-once-it-started",
		"a-dependents-serve-run-comes-up-beside-its-dependencys-serve-run")
	root := dependencyWorkspace(t)

	provider := startServe(t, root, "provider")
	provider.awaitReady(t)

	consumer := startServe(t, root, "consumer")
	records := consumer.awaitReady(t)
	status, reuse, found := ended(records, "/provider:serve~generate")
	if !found || status != "success" {
		t.Fatalf("the consumer's run did not run the provider's generate step before serving (found %v, status %q)\nstderr:\n%s",
			found, status, consumer.stderr.String())
	}
	t.Logf("the consumer's run ran the provider's generate step with reuse %q", reuse)
	if line := lockWaitLine(provider.stderr.String() + consumer.stderr.String()); line != "" {
		t.Errorf("a serve run waited for a task-output lock: %s", line)
	}

	stop(t, root, "consumer")
	if code := consumer.exitCode(t); code != 0 {
		t.Errorf("putnami serve consumer exited %d\nstderr:\n%s", code, consumer.stderr.String())
	}
	stop(t, root, "provider")
	if code := provider.exitCode(t); code != 0 {
		t.Errorf("putnami serve provider exited %d\nstderr:\n%s", code, provider.stderr.String())
	}
}
