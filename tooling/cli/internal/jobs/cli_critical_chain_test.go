package jobs

import (
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestCLIValidationLintStaysOffTheTestCriticalChain plans the real CLI project
// against the real Go extension manifest. The CLI suite is the clean build's
// long pole, so its default validation cadence deliberately selects the
// read-only golangci task: a source writer would add test~test as a serialize
// predecessor and put the full lint tail behind the suite again.
//
// An explicit --fix remains authoritative. That second arm is load-bearing:
// the project default is a cadence choice, not removal of the supported source
// mutation path.
func TestCLIValidationLintStaysOffTheTestCriticalChain(t *testing.T) {
	t.Parallel()
	defaultPlan := planCLIValidationCadence(t, nil)
	testJob := requireCLIPlanJob(t, defaultPlan, "test~test")
	checkJob := requireCLIPlanJob(t, defaultPlan, "lint~golangci-lint-check-only")
	staticcheckJob := requireCLIPlanJob(t, defaultPlan, "lint~staticcheck-check-only")
	assertCLIValidationPolicy(t)

	if findCLIPlanJob(defaultPlan, "lint~golangci-lint") != nil {
		t.Fatal("default CLI validation plan selected the mutating golangci task; want the read-only task")
	}
	if hasResource(checkJob.JobDef.Writes, extension.ResourceIDSources) {
		t.Fatalf("default CLI lint writes project sources: %v", checkJob.JobDef.Writes)
	}
	assertNoOrderingEdge(t, testJob, checkJob)
	assertNoOrderingEdge(t, testJob, staticcheckJob)
	assertNoOrderingEdge(t, checkJob, staticcheckJob)

	fixPlan := planCLIValidationCadence(t, extension.ParamMap{"fix": true})
	fixTestJob := requireCLIPlanJob(t, fixPlan, "test~test")
	fixJob := requireCLIPlanJob(t, fixPlan, "lint~golangci-lint")

	if findCLIPlanJob(fixPlan, "lint~golangci-lint-check-only") != nil {
		t.Fatal("explicit --fix plan retained the read-only golangci task; want the mutating task")
	}
	if !hasResource(fixJob.JobDef.Writes, extension.ResourceIDSources) {
		t.Fatalf("explicit --fix task does not declare its project source write: %v", fixJob.JobDef.Writes)
	}
	if !containsStr(fixJob.SerializeAfter, fixTestJob.Key()) {
		t.Fatalf("explicit --fix task serializeAfter = %v, want test reader %s", fixJob.SerializeAfter, fixTestJob.Key())
	}
}

func assertCLIValidationPolicy(t *testing.T) {
	t.Helper()
	projectConfig := wsproto.LoadProjectConfig(filepath.Join(findJobsRepoRoot(t), "tooling", "cli"))
	if projectConfig == nil {
		t.Fatal("tooling/cli/putnami.json did not load")
	}
	if got := projectConfig.Options["@putnami/go:test"]["race"]; got != true {
		t.Fatalf("default CLI test race = %#v, want true", got)
	}
	if got := projectConfig.Options["test"]["coverage-threshold"]; got != float64(80) {
		t.Fatalf("default CLI coverage threshold = %#v, want 80", got)
	}
	for _, scope := range []string{"test", "@putnami/go:test"} {
		if _, configured := projectConfig.Options[scope]["package-parallel"]; configured {
			t.Fatalf("CLI validation pins package-parallel in %q; feasible-limit measurements retained Go's native default", scope)
		}
	}
}

func planCLIValidationCadence(t *testing.T, commandParams extension.ParamMap) []*ScheduledJob {
	t.Helper()
	repoRoot := findJobsRepoRoot(t)
	projectRoot := filepath.Join(repoRoot, "tooling", "cli")
	projectConfig := wsproto.LoadProjectConfig(projectRoot)
	if projectConfig == nil {
		t.Fatal("tooling/cli/putnami.json did not load")
	}
	// The suite reads every first-party extension manifest, the Go one included.
	const manifestPattern = "../../**/putnami.extension.json"
	if !containsStr(paramStrings(projectConfig.Options["test"], "filePatterns"), manifestPattern) {
		t.Fatalf("CLI test cache key does not include %q", manifestPattern)
	}

	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("go/extension/putnami.extension.json did not load")
	}
	goExtension.RelPath = "go/extension"
	for _, job := range goExtension.Jobs {
		job.ExtensionPath = goExtension.RelPath
	}

	cliProject := &workspace.Project{
		ID:           "/tooling/cli",
		Name:         projectConfig.Name,
		Type:         projectConfig.Type,
		Path:         "tooling/cli",
		Tags:         append([]string(nil), projectConfig.Tags...),
		Dependencies: append([]string(nil), projectConfig.Dependencies...),
		Publish:      append([]string(nil), projectConfig.Publish...),
		Extensions:   append([]string(nil), projectConfig.Extensions...),
		Config:       projectConfig,
	}
	ws := workspace.NewWorkspace(repoRoot, &wsproto.Config{}, []*workspace.Project{cliProject})
	planned, err := Plan(
		ws,
		[]string{"lint", "test", "build"},
		[]*workspace.Project{cliProject},
		[]*extension.ExtensionDescription{goExtension},
		commandParams,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("plan CLI validation cadence: %v", err)
	}
	return planned
}

func requireCLIPlanJob(t *testing.T, planned []*ScheduledJob, name string) *ScheduledJob {
	t.Helper()
	job := findCLIPlanJob(planned, name)
	if job == nil {
		t.Fatalf("CLI plan does not contain %q", name)
	}
	return job
}

func findCLIPlanJob(planned []*ScheduledJob, name string) *ScheduledJob {
	for _, job := range planned {
		if job.Project.ID == "/tooling/cli" && job.JobDef.Name == name {
			return job
		}
	}
	return nil
}

func hasResource(refs []extension.ResourceRef, id string) bool {
	for _, ref := range refs {
		if ref.ID == id {
			return true
		}
	}
	return false
}

func assertNoOrderingEdge(t *testing.T, a, b *ScheduledJob) {
	t.Helper()
	for _, edge := range append(append([]string(nil), a.DependsOn...), a.SerializeAfter...) {
		if edge == b.Key() {
			t.Fatalf("%s is ordered after %s", a.Key(), b.Key())
		}
	}
	for _, edge := range append(append([]string(nil), b.DependsOn...), b.SerializeAfter...) {
		if edge == a.Key() {
			t.Fatalf("%s is ordered after %s", b.Key(), a.Key())
		}
	}
}
