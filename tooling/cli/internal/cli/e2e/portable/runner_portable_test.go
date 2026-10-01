package portable

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// The T3 conformance vertical: the ordinary terminal adapter, the
// real engine, a real out-of-process provider (this binary in its provider
// role), a real materialized snapshot, a real child engine (this binary in its
// CLI role, or the snapshot's own ./putnamiw) and real session files. Nothing
// in the planner, scheduler or session writer is mocked.

func TestPortableRemoteRunsInIsolatedProviderAndImportsTheSession(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "isolated-execution", "dirty-snapshot-executes-remotely-and-imports")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := clitest.PortableFixture(t, false, true)
	// Dirty, uncommitted bytes are what must execute: a tracked edit and an
	// untracked new file. Nothing is committed or pushed.
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "dirty\n")
	clitest.WriteFile(t, filepath.Join(root, "app", "untracked.txt"), "new\n")
	gate := []string{"lint,test,build,validate,validate-workspace", "--projects", "app", "--no-cache", "--continue-on-error", "--output=json"}

	code, output := clitest.RunGateArgs(t, root, append(gate, "--where", "local")...)
	if code != 0 {
		t.Fatalf("local gate: exit %d: %s", code, output)
	}
	localID := clitest.LatestSessionID(t, root)
	_, localPlan := clitest.ReadPortableSession(t, root, localID)
	if err := os.Remove(filepath.Join(root, "app", "calls")); err != nil {
		t.Fatal(err)
	}

	code, output = clitest.RunGateArgs(t, root, append(gate, "--where", "remote")...)
	if code != 0 {
		t.Fatalf("remote gate: exit %d: %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task executed in the submitting worktree: %v", err)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran dirty\n") != 5 || strings.Count(got, "nested putnami") != 5 {
		t.Fatalf("the isolated snapshot did not execute the dirty bytes five times with a nested CLI call each: %q", got)
	}
	remoteID := clitest.LatestSessionID(t, root)
	if remoteID == localID {
		t.Fatal("remote gate did not import a new session")
	}
	session, plan := clitest.ReadPortableSession(t, root, remoteID)
	if session.Placement == nil || session.Placement.Requested != "remote" || session.Placement.Actual != "remote" {
		t.Fatalf("placement = %+v", session.Placement)
	}
	if session.Tree == nil || !session.Tree.Dirty || session.Git == nil || session.Git.Branch != "main" {
		t.Fatalf("imported session lost its bound tree or git block: tree=%+v git=%+v", session.Tree, session.Git)
	}
	if len(session.Tasks) != 5 || session.Run.ExitCode != 0 {
		t.Fatalf("imported session: %+v", session.Run)
	}
	if !reflect.DeepEqual(plan.Tasks, localPlan.Tasks) {
		t.Fatalf("remote plan differs from the local plan:\n%+v\n%+v", plan.Tasks, localPlan.Tasks)
	}
	if !strings.Contains(output, "executing remotely through @fixture/runner") || !strings.Contains(output, "remote session "+remoteID+" imported") {
		t.Fatalf("placement notices missing: %s", output)
	}
	// The remote's canonical result document is what the local stdout carries.
	start := strings.Index(output, "{\n")
	if start < 0 {
		t.Fatalf("no result document forwarded: %s", output)
	}
	document := output[start:]
	if end := strings.LastIndex(document, "\n}"); end >= 0 {
		document = document[:end+2]
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentResultEnvelope, []byte(document)); len(violations) != 0 {
		t.Fatalf("forwarded result document invalid: %v\n%s", violations, document)
	}
	sessions, _ := filepath.Glob(filepath.Join(root, ".putnami", "sessions", "[0-9]*"))
	if len(sessions) != 2 {
		t.Fatalf("expected the local and the imported session only, found %v", sessions)
	}

	// Repeating the import of the same bundle is safe; a different record
	// under the same id is refused.
	exchange := t.TempDir()
	bundle := rebundle(t, root, exchange, remoteID)
	result, err := runnerprovider.ImportBundle(root, exchange, bundle, 20)
	if err != nil || len(result.Reused) != 1 || len(result.Imported) != 0 {
		t.Fatalf("repeat import = %+v, %v", result, err)
	}
	tampered := filepath.Join(root, ".putnami", "sessions", remoteID, "plan.json")
	if err := os.WriteFile(tampered, []byte(`{"protocolVersion":2,"sessionId":"`+remoteID+`","commands":["build"],"tasks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runnerprovider.ImportBundle(root, exchange, bundle, 20); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collision with different content was not refused: %v", err)
	}
}

// rebundle rebuilds the bundle of an imported session from the local store,
// through the same exchange layout the provider uses.
func rebundle(t *testing.T, root, exchange, id string) runner.SessionBundle {
	t.Helper()
	session := runner.BundleSession{ID: id}
	dir := filepath.Join(root, ".putnami", "sessions", id)
	for _, name := range []string{runner.BundleSessionFile, runner.BundlePlanFile, runner.BundleEventsFile, runner.BundleReportFile, runner.BundleSpecFile} {
		file, ok, err := clitest.ExportSessionFile(exchange, filepath.Join(dir, name), name)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			session.Files = append(session.Files, file)
		}
	}
	file, ok, err := clitest.ExportSessionFile(exchange, filepath.Join(root, ".putnami", "reports", id+".json"), runner.BundleRunReportFile)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		session.Files = append(session.Files, file)
	}
	return runner.SessionBundle{Attempt: "rebundle", SessionID: id, Sessions: []runner.BundleSession{session}}
}

func TestPortableRemoteFailureIsImportedWithItsMessage(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "isolated-execution", "remote-failure-keeps-exit-code-and-message")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := clitest.PortableFixture(t, true, true)
	code, output := clitest.RunGateArgs(t, root, "lint,test,build,validate,validate-workspace", "--projects", "app", "--no-cache", "--continue-on-error", "--where", "remote")
	if code != 1 {
		t.Fatalf("remote failing gate: exit %d: %s", code, output)
	}
	if !strings.Contains(output, "intentional placement fixture failure") {
		t.Fatalf("the remote task's failure message is not visible locally: %s", output)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 5 {
		t.Fatalf("expected five executions in the snapshot: %q", got)
	}
	session, _ := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if session.Run.ExitCode != 1 || session.Run.Counts.Failed != 5 {
		t.Fatalf("imported failing session: %+v", session.Run)
	}
	if !strings.Contains(string(clitest.MustJSON(t, session.Run.Failures)), "intentional placement fixture failure") {
		t.Fatalf("failure message missing from the imported record: %s", clitest.MustJSON(t, session.Run.Failures))
	}
}

func TestPortableRemoteImpactedSelectionIsFrozen(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "isolated-execution", "impacted-selection-frozen")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := clitest.PortableFixture(t, false, true)
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "impacted\n")
	code, output := clitest.RunGateArgs(t, root, "lint,test,build,validate,validate-workspace", "--impacted", "--baseline", "HEAD", "--no-cache", "--continue-on-error", "--where", "remote")
	if code != 0 {
		t.Fatalf("remote impacted gate: exit %d: %s", code, output)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran impacted\n") != 5 {
		t.Fatalf("expected five executions of the changed project: %q", got)
	}
	session, plan := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if session.Selection == nil || session.Selection.Mode != "impacted" || !session.Selection.Scoped || strings.Join(session.Selection.Projects, ",") != "/app" {
		t.Fatalf("frozen impacted selection was not recorded as impacted: %+v", session.Selection)
	}
	if session.Git == nil || len(session.Git.Baseline) != 40 {
		t.Fatalf("baseline was not frozen to a commit: %+v", session.Git)
	}
	for _, task := range plan.Tasks {
		if task.Identity.Project.ID != "/app" {
			t.Fatalf("the executing engine widened the frozen selection: %+v", task.Identity)
		}
	}
	// With the edit reverted nothing is impacted: the legitimate empty
	// selection stays an explicit local no-op and submits nothing.
	clitest.RunGit(t, root, "checkout", "--", "app/marker")
	code, output = clitest.RunGateArgs(t, root, "build", "--impacted", "--baseline", "HEAD", "--where", "remote")
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 1 {
		t.Fatalf("an empty impacted selection submitted an attempt: %v", entries)
	}
	if code != 0 || !strings.Contains(output, "No impacted projects found") {
		t.Fatalf("an empty impacted selection must stay an explicit local no-op: exit %d: %s", code, output)
	}
}

func TestPortableRemoteEditAfterSubmitHasNoEffect(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "isolated-execution", "edit-after-submit-has-no-effect")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, providerRoot := clitest.PortableFixture(t, false, true)
	hold := filepath.Join(t.TempDir(), "hold")
	t.Setenv(clitest.RunnerFixtureHoldEnv, hold)
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "before\n")
	done := make(chan struct{})
	var code int
	var output string
	go func() {
		defer close(done)
		code, output = clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	}()
	for {
		if _, err := os.Stat(hold + ".ready"); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("the run finished before the snapshot was materialized: exit %d: %s", code, output)
		default:
		}
	}
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "after\n")
	clitest.WriteFile(t, filepath.Join(root, "app", "fail"), "")
	clitest.WriteFile(t, hold, "")
	<-done
	if code != 0 {
		t.Fatalf("remote build after a later edit: exit %d: %s", code, output)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); !strings.HasPrefix(got, "ran before\nnested putnami") || strings.Contains(got, "after") {
		t.Fatalf("the edit after submission reached execution: %q", got)
	}
}

func TestPortableRemoteRefusesUnsupportedShapesBeforeSubmission(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "portable-request", "unsupported-shapes-rejected-before-submission")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	for _, args := range [][]string{
		{"build", "--projects", "app", "--watch", "--where", "remote"},
		{"format", "--projects", "app", "--where", "remote"},
	} {
		code, output := clitest.RunGateArgs(t, root, args...)
		if code != cli.ExitUsage || !strings.Contains(output, "not portable") {
			t.Errorf("%v: exit %d: %s", args, code, output)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 0 {
		t.Fatalf("a rejected shape reached the provider: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a rejected request recorded a session: %v", err)
	}
}

func TestPortableRemoteRefusesProvidersTheRunnerDidNotNegotiate(t *testing.T) {
	spectest.Proves(t, "cli/credential-provider", "providers-are-opt-in", "remote-providers-need-the-capability")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	// The fixture runner provider echoes execution-request-v1 and
	// session-bundle-v1 only, never invocation-providers-v1.
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--providers", "install", "--where", "remote")
	if code != cli.ExitUsage || !strings.Contains(output, runner.CapabilityInvocationProvidersV1) || !strings.Contains(output, "--providers") {
		t.Fatalf("providers without the capability: exit %d: %s", code, output)
	}
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 0 {
		t.Fatalf("a request the runner cannot carry was submitted: %v", entries)
	}
	if records := clitest.AttemptRecords(t, root); len(records) != 0 {
		t.Fatalf("a refused negotiation left an attempt record: %+v", records)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task ran locally after the refusal: %v", err)
	}
}

func TestPortableRemoteProviderThatCannotInitializeIsNotAFallback(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "portable-request", "unready-provider-is-an-error-not-a-fallback")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	for env, want := range map[string]string{clitest.RunnerFixtureNotReadyEnv: "cannot execute", clitest.RunnerFixtureProtocolEnv: "protocol version"} {
		t.Setenv(env, map[string]string{clitest.RunnerFixtureNotReadyEnv: "1", clitest.RunnerFixtureProtocolEnv: "7"}[env])
		code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--where", "remote")
		t.Setenv(env, "")
		if code != cli.ExitError || !strings.Contains(output, want) {
			t.Errorf("%s: exit %d: %s", env, code, output)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task ran locally after a provider failure: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 0 {
		t.Fatalf("a failed negotiation submitted an attempt: %v", entries)
	}
	if records := clitest.AttemptRecords(t, root); len(records) != 0 {
		t.Fatalf("a failed negotiation left an attempt record: %+v", records)
	}
}

func TestPortableExecutingEngineRefusesADivergentPlan(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "portable-request", "executing-engine-refuses-divergent-plan")
	root, _ := clitest.PortableFixture(t, false, false)
	// A bound request whose expected plan names a task the snapshot cannot
	// produce: the executing engine must refuse before scheduling anything.
	request := clitest.BoundFixtureRequest(t, root, "/app:build~check", "/app:build~missing")
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, request, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runnerprovider.BoundRequestEnv, path)
	code, output := clitest.RunGateArgs(t, root)
	if code != cli.ExitError || !strings.Contains(output, "differs from the expected plan") {
		t.Fatalf("divergent plan: exit %d: %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task ran despite the refused plan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a refused plan recorded a session: %v", err)
	}
	// The adapter consumed (unset) the channel for the process above, as it
	// must so its own tasks never inherit it; a fresh process would see it again.
	if os.Getenv(runnerprovider.BoundRequestEnv) != "" {
		t.Fatal("the bound-request channel survived the executing run and would reach its tasks")
	}
	t.Setenv(runnerprovider.BoundRequestEnv, path)
	code, output = clitest.RunGateArgs(t, root, "build")
	if code != cli.ExitUsage || !strings.Contains(output, "arguments are not accepted") {
		t.Fatalf("argv beside a bound request: exit %d: %s", code, output)
	}
}

func TestPortableRemoteWithoutProviderStillRunsLocallyOnce(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "optional-placement", "absent-provider-runs-once-locally")
	clitest.RequireShell(t)
	root, providerRoot := clitest.PortableFixture(t, false, false)
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 || strings.Contains(output, "executing remotely") {
		t.Fatalf("absent provider: exit %d: %s", code, output)
	}
	calls, err := os.ReadFile(filepath.Join(root, "app", "calls"))
	if err != nil || string(calls) != "ran committed\n" {
		t.Fatalf("expected exactly one local execution: %q (%v)", calls, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts")); len(entries) != 0 {
		t.Fatalf("no provider is installed, yet an attempt exists: %v", entries)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "..", "*", "runner-exchange")); len(matches) != 0 {
		t.Fatalf("absent provider created an exchange directory: %v", matches)
	}
	session, _ := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if session.Placement == nil || session.Placement.Requested != "remote" || session.Placement.Actual != "local" {
		t.Fatalf("placement = %+v", session.Placement)
	}
}

var _ = context.Background
