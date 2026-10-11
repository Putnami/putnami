package cachecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestAnalyzeRejectsVolatileCapturedBytes(t *testing.T) {
	fixture := newVerificationFixture(t, "stable-key", "stable-key", "first", "second")
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	assertFinding(t, report, "double-run-artifact", "error")
}

func TestAnalyzeIgnoresEphemeralBytesInCapturedDirectory(t *testing.T) {
	fixture := newGeneratedDirectoryVerificationFixture(t)
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	if report.Status != "passed" || report.Summary.Blocking != 0 {
		t.Fatalf("report = %+v, want the volatile version stamp to be ignored", report)
	}
}

func TestAnalyzeRejectsCacheHitTreeDrift(t *testing.T) {
	fixture := newVerificationFixture(t, "stable-key", "stable-key", "same", "same")
	fixture.hit.Trees[fixture.project.ID][".gen/out.txt"] = "restore-poison"
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	assertFinding(t, report, "hit-live", "error")
}

func TestAnalyzeRejectsUndeclaredWriters(t *testing.T) {
	fixture := newVerificationFixture(t, "stable-key", "stable-key", "same", "same")
	fixture.first.Results[fixture.job.Key()].VerificationWrites = []string{"rogue.txt"}
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	assertFinding(t, report, "write-closure", "error")
}

func TestAnalyzeReportsAmbientKeyDriftWithoutBlocking(t *testing.T) {
	fixture := newVerificationFixture(t, "ambient-a", "ambient-b", "same", "same")
	fixture.job.JobDef.TaskCachePolicy.Key = &extension.TaskCacheKey{Env: []string{"CLOCK"}}
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	assertFinding(t, report, "double-run-key", "info")
	if report.Status != "passed" || report.Summary.Blocking != 0 {
		t.Fatalf("ambient report = %+v, want a non-blocking pass", report)
	}
}

func TestAnalyzeRejectsUnclassifiedGeneratedPaths(t *testing.T) {
	fixture := newVerificationFixture(t, "stable-key", "stable-key", "same", "same")
	fixture.first.Trees[fixture.project.ID][".gen/mystery.txt"] = "same"
	fixture.hit.Trees[fixture.project.ID][".gen/mystery.txt"] = "same"
	report := Analyze([]string{"build"}, fixture.first, fixture.second, fixture.hit)

	assertFinding(t, report, "gen-classification", "error")
}

func TestWriteAllowedResolvesProjectAndWorkspaceOutputRoots(t *testing.T) {
	job := &jobs.ScheduledJob{Project: &workspace.Project{ID: "/app", Path: "apps/app"}}
	contract := jobs.VerificationContract{Outputs: []jobs.VerificationOutput{
		{Root: extension.OutputRootProject, Path: ".gen/client/**"},
		{Root: extension.OutputRootWorkspace, Path: "shared/report.json"},
	}}
	tests := []struct {
		path string
		want bool
	}{
		{path: "apps/app/.gen/client/index.ts", want: true},
		{path: "shared/report.json", want: true},
		{path: "apps/other/.gen/client/index.ts", want: false},
	}
	for _, testCase := range tests {
		if got := writeAllowed(job, contract, []jobs.VerificationContract{contract}, testCase.path, overrides{}); got != testCase.want {
			t.Errorf("writeAllowed(%q) = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}

// TestWriteAllowedHonorsProjectLevelOwnership pins a write-ownership fix: write ownership
// is the PROJECT's declared output surface, matching the one-owner-per-output
// model the manifests declare (build-generate owns <project>/.gen whole while
// build-describe's staging and config-extract's fallback write inside it and
// declare nothing). Scoring a task against only its own outputs made this check
// fail by construction on every Go project.
func TestWriteAllowedHonorsProjectLevelOwnership(t *testing.T) {
	job := &jobs.ScheduledJob{Project: &workspace.Project{ID: "/app", Path: "apps/app"}}
	// The writer declares nothing, exactly like build~describe's staging write.
	writer := jobs.VerificationContract{}
	owner := jobs.VerificationContract{Outputs: []jobs.VerificationOutput{
		{Root: extension.OutputRootProject, Path: ".gen"},
	}}
	projectContracts := []jobs.VerificationContract{writer, owner}

	if !writeAllowed(job, writer, projectContracts, "apps/app/.gen/schema/capabilities.json", overrides{}) {
		t.Error("a write into the sibling-owned .gen subtree must be allowed")
	}
	// The check still has teeth: outside every declared output of the project.
	if writeAllowed(job, writer, projectContracts, "apps/app/rogue.txt", overrides{}) {
		t.Error("a write outside the project's declared surface must still fail")
	}
	// And ownership does not cross project boundaries.
	if writeAllowed(job, writer, projectContracts, "apps/other/.gen/x.json", overrides{}) {
		t.Error("another project's tree must never be covered by this project's owners")
	}
}

type verificationFixture struct {
	job     *jobs.ScheduledJob
	project *workspace.Project
	first   Run
	second  Run
	hit     Run
}

func newVerificationFixture(t *testing.T, firstKey, secondKey, firstBytes, secondBytes string) verificationFixture {
	t.Helper()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	job := &jobs.ScheduledJob{
		Project: project,
		Extension: &extension.ExtensionDescription{Name: "@test/cache-verify", Tasks: map[string]extension.TaskDefinition{
			"emit": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"generated": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: ".gen/out.txt"},
			}}},
		}},
		JobDef: &extension.JobDefinition{
			Name: "build~emit", Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true},
		},
		Step: &extension.PipelineStep{Task: "emit"},
	}
	ws := workspace.NewWorkspace(t.TempDir(), nil, []*workspace.Project{project})
	firstStore, secondStore := t.TempDir(), t.TempDir()
	ingestVerificationEntry(t, firstStore, firstKey, firstBytes)
	ingestVerificationEntry(t, secondStore, secondKey, secondBytes)

	firstResult := &jobs.JobResult{Status: "success", CacheKey: firstKey}
	secondResult := &jobs.JobResult{Status: "success", CacheKey: secondKey}
	hitResult := &jobs.JobResult{Status: "success", CacheKey: firstKey, CacheHit: true}
	tree := Tree{".gen/out.txt": "stable"}
	return verificationFixture{
		job: job, project: project,
		first: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): firstResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: firstStore},
		second: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): secondResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: secondStore},
		hit: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): hitResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: firstStore},
	}
}

func newGeneratedDirectoryVerificationFixture(t *testing.T) verificationFixture {
	t.Helper()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	job := &jobs.ScheduledJob{
		Project: project,
		Extension: &extension.ExtensionDescription{Name: "@test/cache-verify", Tasks: map[string]extension.TaskDefinition{
			"emit": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"generated": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"},
			}}},
		}},
		JobDef: &extension.JobDefinition{
			Name: "build~emit", Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true},
		},
		Step: &extension.PipelineStep{Task: "emit"},
	}
	ws := workspace.NewWorkspace(t.TempDir(), nil, []*workspace.Project{project})
	firstStore, secondStore := t.TempDir(), t.TempDir()
	ingestVerificationDirectoryEntry(t, firstStore, "stable-key", map[string]string{
		"out.txt": "stable", "version.json": "first build time",
	})
	ingestVerificationDirectoryEntry(t, secondStore, "stable-key", map[string]string{
		"out.txt": "stable", "version.json": "second build time",
	})

	firstResult := &jobs.JobResult{Status: "success", CacheKey: "stable-key"}
	secondResult := &jobs.JobResult{Status: "success", CacheKey: "stable-key"}
	hitResult := &jobs.JobResult{Status: "success", CacheKey: "stable-key", CacheHit: true}
	tree := Tree{".gen/out.txt": "stable", ".gen/version.json": "first"}
	return verificationFixture{
		job: job, project: project,
		first: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): firstResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: firstStore},
		second: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): secondResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: secondStore},
		hit: Run{Workspace: ws, Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{job.Key(): hitResult},
			Trees: map[string]Tree{project.ID: cloneTree(tree)}, StoreRoot: firstStore},
	}
}

func ingestVerificationEntry(t *testing.T, storeRoot, key, contents string) {
	t.Helper()
	staging := t.TempDir()
	output := store.DeclaredEntryOutput{
		ID: "generated", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: ".gen/out.txt",
	}
	filename := store.TaskStagingPath(staging, output)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.NewLocalStore(storeRoot).IngestTaskEntry(staging, store.TaskEntrySpec{
		Key: key, Result: &store.EntryResult{Status: "success"}, Outputs: []store.DeclaredEntryOutput{output},
	})
	if err != nil {
		t.Fatalf("ingest verification entry: %v", err)
	}
}

func ingestVerificationDirectoryEntry(t *testing.T, storeRoot, key string, files map[string]string) {
	t.Helper()
	staging := t.TempDir()
	output := store.DeclaredEntryOutput{
		ID: "generated", Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen",
	}
	for name, contents := range files {
		filename := filepath.Join(store.TaskStagingPath(staging, output), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.NewLocalStore(storeRoot).IngestTaskEntry(staging, store.TaskEntrySpec{
		Key: key, Result: &store.EntryResult{Status: "success"}, Outputs: []store.DeclaredEntryOutput{output},
	})
	if err != nil {
		t.Fatalf("ingest verification directory entry: %v", err)
	}
}

func assertFinding(t *testing.T, report Report, check, severity string) {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Check == check && finding.Severity == severity {
			return
		}
	}
	t.Fatalf("findings = %+v, want %s/%s", report.Findings, check, severity)
}

func cloneTree(tree Tree) Tree {
	result := make(Tree, len(tree))
	for item, digest := range tree {
		result[item] = digest
	}
	return result
}

// TestFailedTaskSuffixNamesFailingTasks pins a diagnostic fix: an aborted
// verification run must say which tasks failed and why, instead of leaving a
// bare exit code that reads like a determinism bug when the cause is an
// ordinary build failure in a throwaway worktree.
func TestFailedTaskSuffixNamesFailingTasks(t *testing.T) {
	if got := failedTaskSuffix(map[string]*jobs.JobResult{
		"/app:build~compile": {Status: "success"},
	}); got != "" {
		t.Errorf("suffix = %q, want empty when nothing failed", got)
	}

	got := failedTaskSuffix(map[string]*jobs.JobResult{
		"/app:build~compile": {Status: "failed", Error: &jobs.JobError{Message: "undefined: foo"}},
		"/app:test~unit":     {Status: "failed"},
		"/app:lint~check":    {Status: "success"},
	})
	for _, want := range []string{"/app:build~compile", "undefined: foo", "/app:test~unit"} {
		if !strings.Contains(got, want) {
			t.Errorf("suffix = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "lint~check") {
		t.Errorf("suffix = %q, want successful tasks omitted", got)
	}

	many := map[string]*jobs.JobResult{}
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		many["/app:"+key] = &jobs.JobResult{Status: "failed"}
	}
	if suffix := failedTaskSuffix(many); !strings.Contains(suffix, "+2 more failed task(s)") {
		t.Errorf("suffix = %q, want the list bounded with a remainder count", suffix)
	}
}
