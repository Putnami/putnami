package clitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// The portable-execution fixture the runner conformance packages share
// (e2e/portable, e2e/lifecycle, e2e/admission): a placement workspace
// that is also a Git repository, whose extension declares the reserved runner
// provider command launching this binary in its provider role, and the readers
// of what one remote run left behind.

// PortableFixture is WhereFixture with the two things portable execution
// needs: a Git repository so the source can be captured, and an extension
// declaring the reserved runner provider command that launches the fixture
// provider. The gate task records the bytes it ran against so a test can prove
// which tree executed.
func PortableFixture(t *testing.T, fail, provider bool) (root, providerRoot string) {
	t.Helper()
	root = WhereFixture(t, fail, false)
	providerRoot = t.TempDir()
	WriteFile(t, filepath.Join(root, "app", "marker"), "committed\n")
	WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n")
	// Inside the fixture's remote execution (this binary in its CLI role) the
	// task re-enters the CLI through the executable the runner exported, as a
	// nested session or a hook would. It must run as an ordinary invocation:
	// a leaked bound-request channel would hijack it.
	WriteFile(t, filepath.Join(root, "extension", "gate.sh"), `echo "ran $(cat "$PUTNAMI_PROJECT_ROOT/marker")" >> "$PUTNAMI_PROJECT_ROOT/calls"
if [ "${PUTNAMI_RUNNER_FIXTURE:-}" = cli ]; then
  if nested="$("$PUTNAMI_CLI_EXECUTABLE" --version 2>&1)"; then
    echo "nested $nested" >> "$PUTNAMI_PROJECT_ROOT/calls"
  else
    echo "nested CLI invocation hijacked: $nested" >&2
    exit 9
  fi
fi
if [ -f "$PUTNAMI_PROJECT_ROOT/fail" ]; then
  echo "intentional placement fixture failure" >&2
  exit 7
fi
`)
	if provider {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		disabled := false
		manifest := extensionproto.Manifest{
			Name: "@fixture/runner", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
			Commands: map[string]extensionproto.CommandDefinition{
				runner.ProviderCommandName: {Description: "Local isolated conformance provider", Run: []extensionproto.PipelineStep{{ID: "provider", Task: "provider"}}},
			},
			Tasks: map[string]extensionproto.TaskDefinition{"provider": {
				Kind: "command", Command: executable, Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: 60000,
				Env: map[string]string{RunnerFixtureRoleEnv: "provider", RunnerFixtureRootEnv: providerRoot},
			}},
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		WriteFile(t, filepath.Join(root, "runner", "putnami.json"), `{"name":"@fixture/runner"}`)
		WriteFile(t, filepath.Join(root, "runner", "putnami.extension.json"), string(data))
		WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"placement-fixture","includes":["extension","runner","app"]}`)
	}
	InitGitRepo(t, root)
	return root, providerRoot
}

func ReadPortableSession(t *testing.T, root, id string) (protocolcli.SessionFile, protocolcli.SessionPlanFile) {
	t.Helper()
	dir := filepath.Join(root, ".putnami", "sessions", id)
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("invalid canonical session: %v", violations)
	}
	var session protocolcli.SessionFile
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan protocolcli.SessionPlanFile
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	return session, plan
}

func LatestSessionID(t *testing.T, root string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, ".putnami", "sessions", "latest"))
	if err != nil {
		t.Fatalf("no latest session: %v", err)
	}
	return filepath.Base(target)
}

func ExecutedCalls(t *testing.T, providerRoot string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(providerRoot, "attempts", "*", "putnami-source-*", "app", "calls"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one executed snapshot, found %v (%v)", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func MustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// BoundFixtureRequest writes a minimal bound request for the placement fixture
// whose expected plan carries the given task keys.
func BoundFixtureRequest(t *testing.T, root string, keys ...string) []byte {
	t.Helper()
	head := strings.TrimSpace(GitOutput(t, root, "rev-parse", "HEAD"))
	tasks := []runner.PlannedTask{}
	for _, key := range keys {
		name := strings.SplitN(key, ":", 2)[1]
		tasks = append(tasks, runner.PlannedTask{
			Identity: protocolcli.TaskIdentity{Key: key, Scope: protocolcli.TaskScopeProject,
				Project:  protocolcli.ProjectIdentity{ID: "/app", Name: "app"},
				Task:     protocolcli.TaskRef{Name: name, Command: "build", Step: "check", Kind: "check"},
				Provider: protocolcli.ProviderIdentity{Extension: "@fixture/gate", Version: "1.0.0"}},
			DependsOn: []string{}, SerializeAfter: []string{}, DeadlineMs: 60000,
			Resources: runner.TaskResources{CPUWeight: 1, Reads: []runner.TaskResource{}, Writes: []runner.TaskResource{}},
		})
	}
	request := runner.ExecutionRequest{
		Version:  runner.ExecutionRequestVersion,
		Protocol: runner.ProtocolBlock{Version: runner.ProviderProtocolVersion, Capabilities: []string{}},
		Source: runner.SourceBlock{Digest: runner.BlobDigest(nil), IndexDigest: strings.Repeat("0", 64),
			Git: runner.GitContext{Head: head, Branch: "main"}, Versions: []runner.LineVersion{}},
		Invocation: runner.InvocationBlock{Commands: []string{"build"}, Params: map[string]runner.ParamValue{},
			Flags: runner.ExecutionFlags{NoCache: true, ResourceBudgets: map[string]int{}}, Cwd: "."},
		Selection: runner.SelectionBlock{RequestedMode: runner.SelectionModeProjects, Mode: runner.SelectionModeProjects, Scoped: true,
			Projects: []string{"/app"}, ChangedPaths: []string{}, Diagnostics: []string{}, NoCacheProjects: []string{}},
		Plan:        runner.PlanBlock{Tasks: tasks},
		Environment: runner.EnvironmentBlock{CLI: runner.PinnedCLI{Source: runner.CLISourceUnpinned}, Extensions: []runner.PinnedComponent{}, Toolchains: []runner.PinnedComponent{}, Platform: runner.Platform{OS: "linux", Arch: "arm64"}},
		Control:     runner.ControlBlock{Caller: runner.CallerCLI, IdempotencyKey: strings.Repeat("a", 32), Deadline: "2030-01-01T00:00:00Z"},
	}
	data, err := runner.CanonicalExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func AttemptRecords(t *testing.T, root string) []*runnerprovider.AttemptRecord {
	t.Helper()
	records, err := runnerprovider.NewAttemptStore(root).List()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func OnlyAttemptRecord(t *testing.T, root string) *runnerprovider.AttemptRecord {
	t.Helper()
	records := AttemptRecords(t, root)
	if len(records) != 1 {
		t.Fatalf("expected one attempt record, found %d", len(records))
	}
	return records[0]
}

func CountLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}
