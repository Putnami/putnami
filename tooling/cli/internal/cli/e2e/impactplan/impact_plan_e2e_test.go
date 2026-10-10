// Package impactplan drives `putnami impact-plan` and `putnami change-plan`
// through the real CLI: the argument parser, the structured dispatcher,
// discovery and the engine's impacted planner, in a fixture workspace that is
// a Git repository. These tests swap the process streams and the working
// directory, so they run in their own test binary.
package impactplan

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// impactPlanFixture is the placement fixture as a two-commit repository whose
// head changes one file of /app, the project every gate command plans a task
// in. Its validate command also runs validate-workspace, as an extension may
// declare. It returns the root and the base commit.
func impactPlanFixture(t *testing.T) (root, baseSHA string) {
	t.Helper()
	root = clitest.WhereFixture(t, false, false)
	manifestPath := filepath.Join(root, "extension", "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	validate := manifest.Commands["validate"]
	validate.AlsoRuns = []string{"validate-workspace"}
	manifest.Commands["validate"] = validate
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, manifestPath, string(data))
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n")
	clitest.InitGitRepo(t, root)
	baseSHA = strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
	clitest.WriteFile(t, filepath.Join(root, "app", "source.txt"), "head\n")
	clitest.RunGit(t, root, "add", "app/source.txt")
	clitest.RunGit(t, root, "commit", "-m", "head")
	return root, baseSHA
}

// runStructured runs one invocation of the real CLI in root and decodes its
// stdout as exactly one result envelope whose data is a T.
func runStructured[T any](t *testing.T, root, command string, args ...string) T {
	t.Helper()
	var code int
	stdout, stderr := clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(root)
		code = app.Run(context.Background(), append([]string{command}, args...))
	})
	if code != 0 {
		t.Fatalf("%s exit = %d\nstdout:\n%s\nstderr:\n%s", command, code, stdout, stderr)
	}
	var envelope struct {
		Command string `json:"command"`
		Data    T      `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode %s: %v\n%s", command, err, stdout)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("%s stdout holds more than one envelope:\n%s", command, stdout)
	}
	if envelope.Command != command {
		t.Fatalf("envelope command = %q, want %s", envelope.Command, command)
	}
	return envelope.Data
}

// TestImpactPlanPlansTheNamedCommandsAndRunsNothing holds the command to its
// seam: one envelope whose data the protocol package validates alone, the
// tasks of exactly the named commands, and no task executed.
func TestImpactPlanPlansTheNamedCommandsAndRunsNothing(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-structured-result", "the-command-writes-one-envelope")
	root, baseSHA := impactPlanFixture(t)
	headSHA := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	plan := runStructured[ciproto.ImpactPlan](t, root, "impact-plan", "l,test", "--base", baseSHA, "--no-cache", "--output=json")
	if err := ciproto.ValidateImpactPlan(plan); err != nil {
		t.Fatalf("data does not validate: %v", err)
	}
	if plan.BaseSHA != baseSHA || plan.HeadSHA != headSHA || strings.Join(plan.Commands, ",") != "lint,test" {
		t.Fatalf("plan = %s..%s %v, want %s..%s [lint test]", plan.BaseSHA, plan.HeadSHA, plan.Commands, baseSHA, headSHA)
	}
	if strings.Join(plan.ChangedFiles, ",") != "app/source.txt" || plan.Cache.Status != "disabled" {
		t.Fatalf("changed files = %v, cache = %+v", plan.ChangedFiles, plan.Cache)
	}
	planned := map[string]bool{}
	for _, task := range plan.Tasks {
		planned[task.Identity.Task.Command] = true
		if task.Identity.Project.ID != "/app" {
			t.Errorf("task %s is outside the impacted project /app", task.Identity.Key)
		}
	}
	if len(planned) != 2 || !planned["lint"] || !planned["test"] {
		t.Fatalf("planned commands = %v, want lint and test", planned)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a planned task ran: %v", err)
	}
}

// TestChangePlanIsTheProjectionOfImpactPlan holds the one projection through
// the real engine: change-plan's ChangePlan is ChangePlanFromImpactPlan of the
// ImpactPlan impact-plan emits for the same gate and range.
func TestChangePlanIsTheProjectionOfImpactPlan(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "change-plan-is-the-projection-of-impact-plan")
	root, baseSHA := impactPlanFixture(t)
	clitest.RunGit(t, root, "remote", "add", "origin", "https://example.test/org/repo.git")

	impact := runStructured[ciproto.ImpactPlan](t, root, "impact-plan", "lint,test,build,validate", "--base", baseSHA, "--no-cache", "--output=json")
	change := runStructured[ciproto.ChangePlan](t, root, "change-plan", "--base", baseSHA, "--no-cache", "--output=json")
	if err := ciproto.ValidateChangePlan(change); err != nil {
		t.Fatalf("change plan does not validate: %v", err)
	}
	projected, err := ciproto.ChangePlanFromImpactPlan(impact, change.Repository)
	if err != nil {
		t.Fatalf("ChangePlanFromImpactPlan: %v", err)
	}
	changeBytes, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	projectedBytes, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if string(changeBytes) != string(projectedBytes) {
		t.Fatalf("change-plan differs from the projection of impact-plan:\n%s\n%s", changeBytes, projectedBytes)
	}
	// The plan names the commands requested; its tasks also hold the
	// validate-workspace task validate plans through alsoRuns.
	planned := map[string]bool{}
	for _, task := range impact.Tasks {
		planned[task.Identity.Task.Command] = true
	}
	for _, command := range []string{"lint", "test", "build", "validate", "validate-workspace"} {
		if !planned[command] {
			t.Errorf("no %s task in the plan: %+v", command, impact.Tasks)
		}
	}
	if strings.Join(impact.Commands, ",") != "lint,test,build,validate" {
		t.Errorf("commands = %v, want the requested list", impact.Commands)
	}
}
