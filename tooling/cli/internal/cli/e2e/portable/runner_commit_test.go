package portable

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// The executing side of a version 2 request: the bound-request channel of the
// ordinary terminal adapter, on a fixture checkout, through the real engine
// and session writer. A runner provider would check the commit out and launch
// the CLI the same way.

// runBoundRequest runs the CLI with no argument and the bound-request channel
// naming request, as a runner provider launches it.
func runBoundRequest(t *testing.T, root string, request []byte) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, request, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runnerprovider.BoundRequestEnv, path)
	return clitest.RunGateArgs(t, root)
}

func headCommit(t *testing.T, root string) string {
	t.Helper()
	return strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
}

func TestPortableExecutingEnginePlansAndRunsACommitRequest(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "commit-request", "commit-request-plans-and-runs-the-checkout")
	clitest.RequireRunnerFixture(t)
	clitest.RequireShell(t)
	root, _ := clitest.PortableFixture(t, false, true)
	first := headCommit(t, root)

	// A snapshot request over the app project, through the conformance
	// provider: the session a version 2 request over the same selection must
	// match.
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 {
		t.Fatalf("snapshot request: exit %d: %s", code, output)
	}
	snapshotID := clitest.LatestSessionID(t, root)
	snapshot, snapshotPlan := clitest.ReadPortableSession(t, root, snapshotID)
	if snapshot.Placement == nil || snapshot.Placement.Provenance == nil {
		t.Fatalf("snapshot session placement = %+v", snapshot.Placement)
	}

	// The version 2 request runs the checkout itself, and records the same
	// session but for the checkout's own tree and no provenance, which names
	// a source snapshot the request does not have.
	code, output = runBoundRequest(t, root, clitest.CommitFixtureRequest(t, first, "", runner.RequestedSelection{Mode: runner.SelectionModeProjects, Projects: []string{"app"}}))
	if code != 0 {
		t.Fatalf("commit request: exit %d: %s", code, output)
	}
	commitID := clitest.LatestSessionID(t, root)
	commit, commitPlan := clitest.ReadPortableSession(t, root, commitID)
	if commitID == snapshotID {
		t.Fatal("the commit request recorded no session")
	}
	if commit.Placement == nil || commit.Placement.Requested != "remote" || commit.Placement.Actual != "remote" || commit.Placement.Provenance != nil {
		t.Fatalf("placement = %+v; want remote, remote and no provenance", commit.Placement)
	}
	if commit.Tree == nil || commit.Tree.HeadSHA != first {
		t.Fatalf("the session does not record the checkout's tree: %+v", commit.Tree)
	}
	if !reflect.DeepEqual(commit.Selection, snapshot.Selection) || commit.Selection == nil || strings.Join(commit.Selection.Projects, ",") != "/app" {
		t.Fatalf("selection = %+v; want the snapshot request's %+v", commit.Selection, snapshot.Selection)
	}
	if !reflect.DeepEqual(commitPlan.Tasks, snapshotPlan.Tasks) || len(commit.Tasks) != len(snapshot.Tasks) || commit.Run.ExitCode != snapshot.Run.ExitCode {
		t.Fatalf("the commit request ran another plan than the snapshot request:\n%+v\n%+v", commitPlan.Tasks, snapshotPlan.Tasks)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || string(calls) != "ran committed\n" {
		t.Fatalf("calls = %q (%v); want the checkout executed once", calls, err)
	}

	// An impacted request measures the change against source.base: here the
	// second commit's edit to the app project.
	if err := os.Remove(filepath.Join(root, "app", "calls")); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "changed\n")
	clitest.RunGit(t, root, "commit", "-am", "change the app")
	second := headCommit(t, root)
	code, output = runBoundRequest(t, root, clitest.CommitFixtureRequest(t, second, first, runner.RequestedSelection{Mode: runner.SelectionModeImpacted}))
	if code != 0 {
		t.Fatalf("impacted commit request: exit %d: %s", code, output)
	}
	impacted, _ := clitest.ReadPortableSession(t, root, clitest.LatestSessionID(t, root))
	if impacted.Selection == nil || impacted.Selection.Mode != "impacted" || strings.Join(impacted.Selection.Projects, ",") != "/app" {
		t.Fatalf("selection = %+v; want the app impacted against source.base", impacted.Selection)
	}
	if impacted.Git == nil || impacted.Git.Baseline != first || impacted.Tree == nil || impacted.Tree.HeadSHA != second {
		t.Fatalf("git = %+v, tree = %+v; want baseline %s on %s", impacted.Git, impacted.Tree, first, second)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || string(calls) != "ran changed\n" {
		t.Fatalf("calls = %q (%v); want the changed bytes executed once", calls, err)
	}

	// Measured against itself, a clean checkout changes nothing: the empty
	// selection is the ordinary no-op. The runs above left untracked files in
	// the project, which an impacted selection counts as changes.
	clitest.RunGit(t, root, "clean", "-fdq", "app")
	code, output = runBoundRequest(t, root, clitest.CommitFixtureRequest(t, second, second, runner.RequestedSelection{Mode: runner.SelectionModeImpacted}))
	if code != 0 || !strings.Contains(output, "No impacted projects found against "+second) {
		t.Fatalf("an empty impacted selection must be a no-op: exit %d: %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("an empty impacted selection ran a task: %v", err)
	}
}

func TestPortableExecutingEngineRefusesACommitRequestOnAnotherCheckout(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "commit-request", "commit-request-refuses-another-checkout")
	root, _ := clitest.PortableFixture(t, false, false)
	first := headCommit(t, root)
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "second\n")
	clitest.RunGit(t, root, "commit", "-am", "second")
	second := headCommit(t, root)
	all := runner.RequestedSelection{Mode: runner.SelectionModeAll}

	refused := func(name string, request []byte, want string) {
		t.Helper()
		code, output := runBoundRequest(t, root, request)
		if code != cli.ExitError || !strings.Contains(output, "bound execution request refused: "+want) {
			t.Fatalf("%s: exit %d: %s", name, code, output)
		}
		if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
			t.Fatalf("%s: a task ran: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
			t.Fatalf("%s: a session was recorded: %v", name, err)
		}
	}
	refused("another commit", clitest.CommitFixtureRequest(t, first, "", all),
		"the checkout's HEAD is "+second+", but the request names source.commit "+first)
	clitest.WriteFile(t, filepath.Join(root, "app", "marker"), "uncommitted\n")
	refused("a modified tracked file", clitest.CommitFixtureRequest(t, second, "", all),
		"the checkout of source.commit "+second+" modifies 1 tracked file(s): app/marker")

	// A request that mixes the versions is malformed, refused before any
	// check of the checkout.
	clitest.RunGit(t, root, "checkout", "--", "app/marker")
	mixed := strings.Replace(string(clitest.CommitFixtureRequest(t, second, "", all)), `"selection":`, `"plan":{"tasks":[]},"selection":`, 1)
	code, output := runBoundRequest(t, root, []byte(mixed))
	if code != cli.ExitUsage || !strings.Contains(output, "carries no plan") {
		t.Fatalf("a version 2 request with a plan: exit %d: %s", code, output)
	}
}
