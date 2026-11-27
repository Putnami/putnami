package planner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/repotest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// These tests read the REAL Go and TypeScript manifests: a plan holds only the
// steps a project's context gives work to (CLI ADR 0132), so a gate that stops
// matching what its task reads would drop real work while its own unit tests
// still pass.

func loadRepoExtension(t *testing.T, dir, name string) *extension.ExtensionDescription {
	t.Helper()
	path := filepath.Join(findJobsRepoRoot(t), filepath.FromSlash(dir), "putnami.extension.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s manifest unavailable: %v", name, err)
	}
	manifest, err := extension.LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest %s: %v", path, err)
	}
	description := extension.Resolve(manifest, filepath.Dir(path))
	description.Name = name
	return description
}

func goModule(module string, requires ...string) string {
	content := "module " + module + "\n\ngo 1.26\n"
	for _, r := range requires {
		content += "\nrequire " + r + " v0.0.0\n"
	}
	return content
}

// contextWorkspace is a Go and TypeScript workspace whose projects differ only
// in what gives the idle-prone steps work.
func contextWorkspace(t *testing.T) (string, []*workspace.Project) {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, root, "plain", "go.mod", goModule("example.com/plain"))
	writeProjectFile(t, root, "plain", "plain.go", "package plain\n\nfunc V() int { return 1 }\n")
	writeProjectFile(t, root, "plain", "plain_test.go", "package plain\n")

	writeProjectFile(t, root, "server", "go.mod", goModule("example.com/server", "go.putnami.dev/app"))
	writeProjectFile(t, root, "server", "main.go", "package main\n\nimport _ \"go.putnami.dev/app\"\n\nfunc main() {}\n")
	writeProjectFile(t, root, "server", "main_test.go", "package main\n")

	writeProjectFile(t, root, "routes", "go.mod", goModule("example.com/routes", "go.putnami.dev/http"))
	writeProjectFile(t, root, "routes", "routes.go", "package routes\n\nimport _ \"go.putnami.dev/http\"\n")
	writeProjectFile(t, root, "routes", "routes_test.go", "package routes\n")

	writeProjectFile(t, root, "settings", "go.mod", goModule("example.com/settings"))
	writeProjectFile(t, root, "settings", "settings.go", "package settings\n")
	writeProjectFile(t, root, "settings", "settings_test.go", "package settings\n")
	writeProjectFile(t, root, "settings", "conf/.env.yaml", "a: 1\n")

	writeProjectFile(t, root, "consumer", "go.mod", goModule("example.com/consumer", "example.com/settings"))
	writeProjectFile(t, root, "consumer", "consumer.go", "package consumer\n")
	writeProjectFile(t, root, "consumer", "consumer_test.go", "package consumer\n")

	writeProjectFile(t, root, "tslib", "package.json", `{"name":"tslib","main":"src/index.ts"}`)
	writeProjectFile(t, root, "tslib", "src/index.ts", "export const v = 1;\n")
	writeProjectFile(t, root, "tslib", "test/index.test.ts", "import \"../src/index\";\n")
	writeProjectFile(t, root, "tsapp", "package.json", `{"name":"tsapp","main":"src/main.ts"}`)
	writeProjectFile(t, root, "tsapp", "src/main.ts", "console.log(1);\n")

	goExt := []string{"@putnami/go"}
	tsExt := []string{"@putnami/typescript"}
	return root, []*workspace.Project{
		{ID: "/plain", Name: "plain", Path: "plain", Type: "library", Extensions: goExt},
		{ID: "/server", Name: "server", Path: "server", Type: "application", Extensions: goExt, Dependencies: []string{"plain"}},
		{ID: "/routes", Name: "routes", Path: "routes", Type: "library", Extensions: goExt},
		{ID: "/settings", Name: "settings", Path: "settings", Type: "library", Extensions: goExt},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Type: "library", Extensions: goExt, Dependencies: []string{"settings"}},
		{ID: "/tslib", Name: "tslib", Path: "tslib", Type: "library", Extensions: tsExt},
		{ID: "/tsapp", Name: "tsapp", Path: "tsapp", Type: "application", Extensions: tsExt},
	}
}

func planContext(t *testing.T, root string, projects []*workspace.Project) map[string]bool {
	t.Helper()
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"
	extensions := []*extension.ExtensionDescription{
		loadRepoExtension(t, "go/extension", "@putnami/go"),
		loadRepoExtension(t, "typescript/extension", "@putnami/typescript"),
	}
	planned, err := Plan(ws, []string{"build", "test", "lint"}, projects, extensions, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planKeySet(planned)
}

func TestAPlanHoldsOnlyTheStepsTheProjectContextGivesWork(t *testing.T) {
	repotest.Require(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-plan-holds-only-the-steps-a-project-context-gives-work-to", "an-idle-step-is-not-planned")
	t.Parallel()
	root, projects := contextWorkspace(t)
	keys := planContext(t, root, projects)

	want := map[string]bool{
		// Nothing gives the plain library config or infrastructure.
		"/plain:build~config-merge": false,
		"/plain:test~config-merge":  false,
		"/plain:build~infra":        false,
		// generate carries a library's sources into its dependents' keys
		// through ^generate: kept.
		"/plain:build~generate": true,
		"/plain:test~generate":  true,
		"/plain:test~test":      true,
		// The application describes itself through generate and is a workload.
		"/server:build~generate": true,
		"/server:test~generate":  true,
		"/server:build~infra":    true,
		"/routes:build~infra":    false,
		// Its own config and a dependency's config give config-merge work. The
		// build pipeline's config-merge feeds tidy alone, which CI replaces by
		// its check, so the test pipeline's is the one every environment plans.
		"/settings:test~config-merge": true,
		"/consumer:test~config-merge": true,
		// TypeScript: infra for a workload only; generate stays, since it stamps
		// the content hash every build reads.
		"/tslib:build~infra":    false,
		"/tsapp:build~infra":    true,
		"/tslib:build~generate": true,
		"/tslib:test~generate":  true,
	}
	for key, planned := range want {
		if keys[key] != planned {
			t.Errorf("%s planned = %v, want %v", key, keys[key], planned)
		}
	}
}

// TestAnIdleLibraryStillCarriesItsSourcesToItsDependents keeps the key chain
// a dependent's identity follows: its generate waits for the generate of each
// dependency, which reads that dependency's sources, so a source edit in a
// library that plans nothing else still moves every dependent key.
func TestAnIdleLibraryStillCarriesItsSourcesToItsDependents(t *testing.T) {
	repotest.Require(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-plan-holds-only-the-steps-a-project-context-gives-work-to", "an-idle-library-still-carries-its-sources-to-its-dependents")
	t.Parallel()
	root, projects := contextWorkspace(t)
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"
	planned, err := Plan(ws, []string{"build", "test", "lint"}, projects, []*extension.ExtensionDescription{
		loadRepoExtension(t, "go/extension", "@putnami/go"),
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, key := range []string{"/server:build~generate", "/server:test~generate"} {
		job := findScheduled(planned, key)
		if job == nil {
			t.Fatalf("%s missing", key)
		}
		if !reaches(planned, job, "/plain:build~generate") && !reaches(planned, job, "/plain:test~generate") {
			t.Errorf("%s no longer waits for the idle library's generate: %v", key, job.DependsOn)
		}
	}
}

// settle waits past the file index's margin, so the next plan records its
// activation answers with their witnesses.
func settle() { time.Sleep(fileindex.Margin + 100*time.Millisecond) }

// TestAStepThatGainsWorkIsPlannedInTheNextRun is the guard of CLI ADR 0132: an
// answer kept with its witnesses never outlives the file that changes it.
func TestAStepThatGainsWorkIsPlannedInTheNextRun(t *testing.T) {
	repotest.Require(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-plan-holds-only-the-steps-a-project-context-gives-work-to", "a-step-that-gains-work-is-planned-in-the-next-run")
	t.Parallel()
	cases := []struct {
		name string
		key  string
		edit func(t *testing.T, root string)
	}{
		{"own config gives config-merge work", "/plain:test~config-merge", func(t *testing.T, root string) {
			writeProjectFile(t, root, "plain", "conf/.env.yaml", "b: 2\n")
		}},
		{"an environment config gives config-merge work", "/plain:test~config-merge", func(t *testing.T, root string) {
			writeProjectFile(t, root, "plain", "conf/.env.local.yaml", "b: 2\n")
		}},
		{"a dependency's config gives config-merge work", "/server:test~config-merge", func(t *testing.T, root string) {
			writeProjectFile(t, root, "plain", "conf/.env.yaml", "b: 2\n")
		}},
		{"an infrastructure output left behind gives infra work", "/plain:build~infra", func(t *testing.T, root string) {
			writeProjectFile(t, root, "plain", ".gen/requirements.json", "{}\n")
		}},
		{"a TypeScript infrastructure output left behind gives infra work", "/tslib:build~infra", func(t *testing.T, root string) {
			writeProjectFile(t, root, "tslib", ".gen/infra/runtime.json", "{}\n")
		}},
		{"a document gives docs lint work", "/plain:lint~docs", func(t *testing.T, root string) {
			writeProjectFile(t, root, "plain", "doc/guide.md", "# Guide\n")
		}},
	}
	// The subtests run one after another: each reads the process's count of
	// answers held by their witnesses.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, projects := contextWorkspace(t)
			settle()
			if keys := planContext(t, root, projects); keys[tc.key] {
				t.Fatalf("%s planned before the edit", tc.key)
			}
			// The answers are kept: a second plan of the unchanged tree reads
			// them from their witnesses and still drops the step.
			held := workspace.ActivationHolds()
			if keys := planContext(t, root, projects); keys[tc.key] {
				t.Fatalf("%s planned on an unchanged tree", tc.key)
			}
			if workspace.ActivationHolds() == held {
				t.Fatal("the unchanged plan read no activation answer from its witnesses")
			}
			tc.edit(t, root)
			if keys := planContext(t, root, projects); !keys[tc.key] {
				t.Errorf("%s not planned after the edit", tc.key)
			}
		})
	}
}

// TestADroppedStepSplicesItsDependents keeps the ordering through a dropped
// step: with describe spliced out, compile still waits for generate, and
// generate for the dependency's generate.
func TestADroppedStepSplicesItsDependents(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	root, projects := contextWorkspace(t)
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"
	planned, err := Plan(ws, []string{"build"}, projects, []*extension.ExtensionDescription{
		loadRepoExtension(t, "go/extension", "@putnami/go"),
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	compile := findScheduled(planned, "/plain:build~compile")
	if compile == nil {
		t.Fatal("plain compile missing")
	}
	if !reaches(planned, compile, "/plain:build~generate") {
		t.Errorf("plain compile no longer orders after generate: %v", compile.DependsOn)
	}
	server := findScheduled(planned, "/server:build~compile")
	if server == nil || !reaches(planned, server, "/plain:build~generate") {
		t.Errorf("server compile no longer orders after its dependency's generate: %+v", server)
	}
}

func reaches(planned []*jobmodel.ScheduledJob, from *jobmodel.ScheduledJob, key string) bool {
	seen := map[string]bool{}
	stack := append([]string(nil), from.DependsOn...)
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next == key {
			return true
		}
		if seen[next] {
			continue
		}
		seen[next] = true
		if job := findScheduled(planned, next); job != nil {
			stack = append(stack, job.DependsOn...)
		}
	}
	return false
}
