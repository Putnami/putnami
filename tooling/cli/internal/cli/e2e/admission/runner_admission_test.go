package admission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// The T2 admission vertical (ADR 0037): the same real engine, real
// out-of-process fixture provider, real materialized snapshot and real child
// engine as the T3 scenarios, on a workspace whose tasks declare inputs the
// plain snapshot does not carry — a git-ignored configuration file and an
// environment variable.

// portableInputFixture is clitest.PortableFixture with two more tasks on the gate
// extension: `test` runs readconf, which declares the project's conf/*.txt
// (and a generated .gen/*.txt) as file inputs and reads the ignored
// app/conf/local.txt; `validate` runs envcheck, a cacheable task keyed on an
// environment variable. The ignored inputs exist locally and are committed to
// nothing.
func portableInputFixture(t *testing.T, provider bool) (root, providerRoot string) {
	t.Helper()
	root, providerRoot = clitest.PortableFixture(t, false, provider)
	manifestPath := filepath.Join(root, "extension", "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	disabled := false
	check := manifest.Tasks["check"]
	manifest.Tasks["readconf"] = extensionproto.TaskDefinition{
		Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/readconf.sh"}, TimeoutMs: 60000,
		Cache:  &extensionproto.TaskCachePolicy{Enabled: &disabled},
		Inputs: map[string]extensionproto.TaskInputPort{"conf": {From: extensionproto.TaskInputFromProject, Files: []string{"conf/*.txt", ".gen/*.txt"}}},
		Reads:  check.Reads, Writes: check.Writes,
	}
	manifest.Tasks["envcheck"] = extensionproto.TaskDefinition{
		Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/gate.sh"}, TimeoutMs: 60000,
		Inputs:   map[string]extensionproto.TaskInputPort{"PUTNAMI_FIXTURE_TARGET": {From: extensionproto.TaskInputFromEnv}},
		Declares: &extensionproto.TaskDeclaration{},
		Reads:    check.Reads, Writes: check.Writes,
	}
	// A CACHEABLE task with no declared file input at all: its key falls back
	// to the whole non-hidden project tree, which is what must not be bound.
	manifest.Tasks["nodecl"] = extensionproto.TaskDefinition{
		Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/gate.sh"}, TimeoutMs: 60000,
		Declares: &extensionproto.TaskDeclaration{},
		Reads:    check.Reads, Writes: check.Writes,
	}
	manifest.Commands["test"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: []extensionproto.PipelineStep{{ID: "read", Task: "readconf"}}}
	manifest.Commands["validate"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: []extensionproto.PipelineStep{{ID: "env", Task: "envcheck"}}}
	manifest.Commands["lint"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: []extensionproto.PipelineStep{{ID: "whole", Task: "nodecl"}}}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, manifestPath, string(data))
	// The task's output is the bytes it read, written beside the snapshot's
	// other evidence: a successful task's stdout is not forwarded, so the file
	// the snapshot executed against is what proves which bytes it saw.
	clitest.WriteFile(t, filepath.Join(root, "extension", "readconf.sh"), `content="$(cat "$PUTNAMI_PROJECT_ROOT/conf/local.txt")" || exit 3
echo "read $content" >> "$PUTNAMI_PROJECT_ROOT/calls"
`)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\napp/conf/local.txt\napp/build-artifact.bin\napp/tmp/\n")
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", "declare inputs")
	clitest.WriteFile(t, filepath.Join(root, "app", "conf", "local.txt"), "local secret-free config\n")
	clitest.WriteFile(t, filepath.Join(root, "app", ".gen", "generated.txt"), "recreated by the lifecycle\n")
	return root, providerRoot
}

// providerAttemptDocuments reads the request and manifest the fixture
// provider persisted for one attempt.
func providerAttemptDocuments(t *testing.T, providerRoot, attempt string) (runner.ExecutionRequest, runner.SourceManifest) {
	t.Helper()
	dir := filepath.Join(providerRoot, "attempts", attempt)
	data, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := runner.ParseExecutionRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, clitest.FixtureManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := runner.ParseSourceManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return request, manifest
}

func boundManifestPaths(manifest runner.SourceManifest) []string {
	paths := []string{}
	for _, entry := range manifest.Entries {
		if entry.Bound {
			paths = append(paths, entry.Path)
		}
	}
	return paths
}

func manifestHasPath(manifest runner.SourceManifest, name string) bool {
	for _, entry := range manifest.Entries {
		if entry.Path == name {
			return true
		}
	}
	return false
}

func TestPortableRemoteBindsARequiredIgnoredInput(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "required-ignored-input-is-bound")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := portableInputFixture(t, true)
	gate := []string{"test", "--projects", "app", "--no-cache"}

	// The local run reads the file from the worktree; binding changes nothing
	// about it.
	code, output := clitest.RunGateArgs(t, root, append(gate, "--where", "local")...)
	if code != 0 {
		t.Fatalf("local run: exit %d: %s", code, output)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || string(calls) != "read local secret-free config\n" {
		t.Fatalf("local run did not read the ignored input: %q (%v)", calls, err)
	}
	if err := os.Remove(filepath.Join(root, "app", "calls")); err != nil {
		t.Fatal(err)
	}

	code, output = clitest.RunGateArgs(t, root, append(gate, "--where", "remote")...)
	if code != 0 {
		t.Fatalf("remote run: exit %d: %s", code, output)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); got != "read local secret-free config\n" {
		t.Fatalf("the snapshot did not carry the ignored input's bytes: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task executed in the submitting worktree: %v", err)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	request, manifest := providerAttemptDocuments(t, providerRoot, record.Attempt)
	if strings.Join(request.Source.Bound, ",") != "app/conf/local.txt" {
		t.Fatalf("the request did not admit exactly the ignored input: %v", request.Source.Bound)
	}
	if strings.Join(boundManifestPaths(manifest), ",") != "app/conf/local.txt" {
		t.Fatalf("the manifest did not flag exactly the bound entry: %+v", manifest.Entries)
	}
	if manifestHasPath(manifest, "app/.gen/generated.txt") || manifestHasPath(manifest, ".putnami/sessions/latest") {
		t.Fatalf("a lifecycle-recreated ignored path was bound: %+v", manifest.Entries)
	}
	if digest, err := runner.SourceDigest(manifest); err != nil || digest != record.SourceDigest || digest != request.Source.Digest {
		t.Fatalf("the attempt record, the request and the manifest disagree about the source: %s / %s / %s (%v)", record.SourceDigest, request.Source.Digest, digest, err)
	}
	session, _ := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if session.Placement == nil || session.Placement.Actual != "remote" || session.Run.ExitCode != 0 || len(session.Tasks) != 1 {
		t.Fatalf("imported session: placement=%+v run=%+v", session.Placement, session.Run)
	}
}

func TestPortableRemoteWithoutTheIgnoredInputFailsLikeAFreshClone(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "required-ignored-input-is-bound")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := portableInputFixture(t, true)
	if err := os.Remove(filepath.Join(root, "app", "conf", "local.txt")); err != nil {
		t.Fatal(err)
	}
	code, output := clitest.RunGateArgs(t, root, "test", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 1 {
		t.Fatalf("remote run without the input: exit %d: %s", code, output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	request, manifest := providerAttemptDocuments(t, providerRoot, record.Attempt)
	if len(request.Source.Bound) != 0 || len(boundManifestPaths(manifest)) != 0 || manifestHasPath(manifest, "app/conf/local.txt") {
		t.Fatalf("an absent input was bound: request=%v manifest=%+v", request.Source.Bound, manifest.Entries)
	}
	session, _ := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if session.Run.ExitCode != 1 || session.Run.Counts.Failed != 1 {
		t.Fatalf("the task did not fail in the snapshot as it would in a fresh clone: %+v", session.Run)
	}
}

func TestPortableRemoteRefusesAnEnvKeyedTaskBeforeSubmission(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "env-keyed-task-refused-before-submission")
	clitest.RequireShell(t)
	root, providerRoot := portableInputFixture(t, true)
	code, output := clitest.RunGateArgs(t, root, "validate", "--projects", "app", "--no-cache", "--where", "remote")
	if code != cli.ExitUsage || !strings.Contains(output, "/app:validate~env") || !strings.Contains(output, "PUTNAMI_FIXTURE_TARGET") || !strings.Contains(output, "never travel") {
		t.Fatalf("env-keyed task: exit %d: %s", code, output)
	}
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 0 {
		t.Fatalf("a refused request reached the provider: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(providerRoot, "spawns")); !os.IsNotExist(err) {
		t.Fatalf("a refused request started a provider process: %v", err)
	}
	if records := clitest.AttemptRecords(t, root); len(records) != 0 {
		t.Fatalf("a refused request recorded an attempt: %+v", records)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a refused request recorded a session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a refused request ran the task locally: %v", err)
	}
	// The same task runs locally, with the provider installed or not.
	code, output = clitest.RunGateArgs(t, root, "validate", "--projects", "app", "--no-cache", "--where", "local")
	if code != 0 {
		t.Fatalf("local run of the env-keyed task: exit %d: %s", code, output)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || strings.Count(string(calls), "ran committed\n") != 1 {
		t.Fatalf("expected one local execution: %q (%v)", calls, err)
	}
	unprovided, _ := portableInputFixture(t, false)
	code, output = clitest.RunGateArgs(t, unprovided, "validate", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 || strings.Contains(output, "never travel") {
		t.Fatalf("absent provider must still run the env-keyed task locally: exit %d: %s", code, output)
	}
	if calls, err := os.ReadFile(filepath.Join(unprovided, "app", "calls")); err != nil || strings.Count(string(calls), "ran committed\n") != 1 {
		t.Fatalf("expected one local fallback execution: %q (%v)", calls, err)
	}
}

func TestPortableExecutingEngineRefusesADivergentBoundSet(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "executing-engine-refuses-divergent-bound-set")
	root, _ := clitest.PortableFixture(t, false, false)
	// The plan is the one the snapshot produces, but the request claims a
	// bound input no planned task selects: the executing engine must refuse
	// before scheduling anything, exactly as it refuses a divergent plan.
	request := boundFixtureRequestWith(t, root, []string{"app/conf/local.txt"}, "/app:build~check")
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, request, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runnerprovider.BoundRequestEnv, path)
	code, output := clitest.RunGateArgs(t, root)
	if code != cli.ExitError || !strings.Contains(output, "admitted inputs differ") || !strings.Contains(output, "app/conf/local.txt") {
		t.Fatalf("divergent bound set: exit %d: %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task ran despite the refused bound set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a refused bound set recorded a session: %v", err)
	}
}

func TestPortableSecondSnapshotTransfersOnlyTheChangedBlob(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "second-snapshot-transfers-only-changed-blobs")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := portableInputFixture(t, true)
	gate := []string{"test", "--projects", "app", "--no-cache", "--where", "remote"}
	code, output := clitest.RunGateArgs(t, root, gate...)
	if code != 0 {
		t.Fatalf("first remote run: exit %d: %s", code, output)
	}
	first := clitest.CountLines(t, filepath.Join(providerRoot, "ingested"))
	if first < 2 {
		t.Fatalf("the first snapshot transferred %d blobs; expected the whole tree", first)
	}
	// One edit, to the bound input itself: the next snapshot has a new source
	// identity and exactly one blob the provider lacks.
	clitest.WriteFile(t, filepath.Join(root, "app", "conf", "local.txt"), "local secret-free config, edited\n")
	code, output = clitest.RunGateArgs(t, root, gate...)
	if code != 0 {
		t.Fatalf("second remote run: exit %d: %s", code, output)
	}
	if second := clitest.CountLines(t, filepath.Join(providerRoot, "ingested")); second != first+1 {
		t.Fatalf("the second snapshot transferred %d blob(s); expected exactly the one that changed", second-first)
	}
	records := clitest.AttemptRecords(t, root)
	if len(records) != 2 || records[0].SourceDigest == records[1].SourceDigest || records[0].InputDigest == records[1].InputDigest {
		t.Fatalf("two snapshots with different bytes must have different identities: %+v", records)
	}
	_, newest := providerAttemptDocuments(t, providerRoot, records[0].Attempt)
	for _, entry := range newest.Entries {
		if entry.Path == "app/conf/local.txt" && (!entry.Bound || entry.Digest != runner.BlobDigest([]byte("local secret-free config, edited\n"))) {
			t.Fatalf("the newest snapshot did not carry the edited bound input: %+v", entry)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(providerRoot, "attempts", "*", "putnami-source-*", "app", "calls"))
	if len(matches) != 2 {
		t.Fatalf("expected two executed snapshots, found %v", matches)
	}
}

// boundFixtureRequestWith is clitest.BoundFixtureRequest with an admitted bound set
// and an expected plan that MATCHES the fixture's real graph — the contract
// digest and declared resources of its `check` task — so the executing
// engine's refusal can only come from the bound set.
func boundFixtureRequestWith(t *testing.T, root string, bound []string, keys ...string) []byte {
	t.Helper()
	request, err := runner.ParseExecutionRequest(clitest.BoundFixtureRequest(t, root, keys...))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "extension", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	check := manifest.Tasks["check"]
	for index := range request.Plan.Tasks {
		request.Plan.Tasks[index].ContractDigest = extensionproto.TaskContractDigest(check)
		request.Plan.Tasks[index].Resources = runner.TaskResources{CPUWeight: 1,
			Reads:  []runner.TaskResource{{ID: extensionproto.ResourceIDSources, Scope: extensionproto.ResourceScopeProject}},
			Writes: []runner.TaskResource{{ID: "calls", Scope: extensionproto.ResourceScopeWorkspace}}}
	}
	request.Source.Bound = bound
	data, err = runner.CanonicalExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A keyed task that declares NO file input keys on the whole project tree.
// Whatever that tree happens to hold — a stale build artifact, a directory of
// scratch files, a binary past the protocol's size limit — is not a statement
// of what the task reads: none of it is bound, the request is not refused
// over it, and the run says out loud which ignored files did not travel.
func TestPortableRemoteReportsFallbackInputsWithoutBindingThem(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "undeclared-fallback-inputs-are-reported-not-bound")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := portableInputFixture(t, true)
	oversize, err := os.Create(filepath.Join(root, "app", "build-artifact.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the size the admission would have refused on, none of the bytes.
	if err := oversize.Truncate(runner.MaxSourceFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := oversize.Close(); err != nil {
		t.Fatal(err)
	}
	for index := range 6 {
		clitest.WriteFile(t, filepath.Join(root, "app", "tmp", fmt.Sprintf("scratch-%d.out", index)), "scratch\n")
	}

	code, output := clitest.RunGateArgs(t, root, "lint", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 {
		t.Fatalf("remote run with an undeclared fallback: exit %d: %s", code, output)
	}
	if !strings.Contains(output, "/app:lint~whole") || !strings.Contains(output, "declares no file inputs") ||
		!strings.Contains(output, "app/build-artifact.bin") || !strings.Contains(output, "app/tmp/scratch-0.out") ||
		!strings.Contains(output, "8 git-ignored file(s)") || !strings.Contains(output, "and 3 more") || !strings.Contains(output, "filePatterns") {
		t.Fatalf("the unbound ignored inputs were not reported: %s", output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	request, manifest := providerAttemptDocuments(t, providerRoot, record.Attempt)
	if len(request.Source.Bound) != 0 || len(boundManifestPaths(manifest)) != 0 {
		t.Fatalf("a fallback path was bound: request=%v manifest=%v", request.Source.Bound, boundManifestPaths(manifest))
	}
	for _, name := range []string{"app/build-artifact.bin", "app/tmp/scratch-0.out", "app/conf/local.txt", "app/.gen/generated.txt"} {
		if manifestHasPath(manifest, name) {
			t.Fatalf("an ignored path traveled in the snapshot: %s", name)
		}
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 1 {
		t.Fatalf("the task did not execute once in the snapshot: %q", got)
	}
	// The same worktree, with a task that DOES declare the file it reads:
	// the declaration is what binds, and the oversize artifact beside it
	// changes nothing.
	code, output = clitest.RunGateArgs(t, root, "test", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 {
		t.Fatalf("remote run of the declaring task: exit %d: %s", code, output)
	}
	if strings.Contains(output, "declares no file inputs") {
		t.Fatalf("a declaring task was reported as declaring nothing: %s", output)
	}
	records := clitest.AttemptRecords(t, root)
	if len(records) != 2 {
		t.Fatalf("expected two attempts, found %d", len(records))
	}
	request, manifest = providerAttemptDocuments(t, providerRoot, records[0].Attempt)
	if strings.Join(request.Source.Bound, ",") != "app/conf/local.txt" || strings.Join(boundManifestPaths(manifest), ",") != "app/conf/local.txt" {
		t.Fatalf("the declared ignored input was not bound: request=%v manifest=%v", request.Source.Bound, boundManifestPaths(manifest))
	}
	if manifestHasPath(manifest, "app/build-artifact.bin") {
		t.Fatal("the oversize artifact traveled beside the declared input")
	}
}
