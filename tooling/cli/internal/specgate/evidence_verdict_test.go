package specgate

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// evidenceFixture is a workspace of two projects, /billing and /ledger, with
// one workspace task at the root whose output holds finding fragments about
// both and about a root file no project owns.
type evidenceFixture struct {
	ws      *workspace.Workspace
	planned []*jobs.ScheduledJob
	results map[string]*jobs.JobResult
}

func finding(path, severity, rule string) string {
	return `{"protocolVersion":1,"kind":"finding","source":{"format":"putnami-sdd"},"subject":{"path":"` + path +
		`","line":3},"rule":"` + rule + `","severity":"` + severity + `","message":"` + rule + ` at ` + path + `"}`
}

func newEvidenceFixture(t *testing.T, lines ...string) *evidenceFixture {
	t.Helper()
	root := t.TempDir()
	billing := &workspace.Project{ID: "/billing", Name: "billing", Path: "billing", Config: &wsproto.ProjectConfig{}}
	ledger := &workspace.Project{ID: "/ledger", Name: "ledger", Path: "ledger", Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "evidence"}, []*workspace.Project{billing, ledger})
	task := &workspace.Project{ID: ".", Name: "workspace", Path: "."}
	writeFile(t, filepath.Join(root, ".putnami", "out", "~validate", evidence.FragmentFilename), strings.Join(lines, "\n")+"\n")
	job := evidenceJob(task, false)
	return &evidenceFixture{
		ws:      ws,
		planned: []*jobs.ScheduledJob{job},
		results: map[string]*jobs.JobResult{job.Key(): {Status: "success"}},
	}
}

// evidenceJob is a validate task of project that declares the reserved
// fragment file in its command output, optional when optional is set.
func evidenceJob(project *workspace.Project, optional bool) *jobs.ScheduledJob {
	declaration := &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"evidence": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: evidence.FragmentFilename, OptionalEmpty: optional},
	}}
	return &jobs.ScheduledJob{
		Project: project,
		Extension: &extension.ExtensionDescription{
			Name:  "@putnami/test-fixture",
			Tasks: map[string]extension.TaskDefinition{"features-validate": {Declares: declaration}},
		},
		JobDef: &extension.JobDefinition{Name: "validate~features", CommandName: "validate"},
		Step:   &extension.PipelineStep{Task: "features-validate"},
	}
}

func (f *evidenceFixture) verdict(t *testing.T, selection *protocoljob.Selection) (map[string]EvidenceSanction, []string) {
	t.Helper()
	findings, refusals := CollectFindings(f.ws, f.planned, f.results)
	return EvidenceVerdict(findings, refusals, selection)
}

func selected(ids ...string) *protocoljob.Selection {
	return &protocoljob.Selection{Mode: protocoljob.SelectionModeProjects, Scoped: true, ProjectIDs: ids}
}

func TestEvidenceVerdictFailsOnTheSelectedProjectsErrors(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings", "an-error-finding-of-a-selected-project-fails-the-run")
	fixture := newEvidenceFixture(t,
		finding("billing/putnami.features.json", "error", "features.duplicate_identity"),
		finding("ledger/putnami.features.json", "error", "features.parse_error"),
		finding("ledger/putnami.features.json", "warning", "features.missing_evidence"),
	)
	sanctions, warnings := fixture.verdict(t, nil)
	if len(sanctions) != 2 || sanctions["/billing"].Message == "" || sanctions["/ledger"].Message == "" {
		t.Fatalf("unscoped sanctions = %+v, want both projects", sanctions)
	}
	diagnostic := sanctions["/billing"].Diagnostics[0]
	if diagnostic.File != "billing/putnami.features.json" || diagnostic.Line != 3 || diagnostic.Code != "features.duplicate_identity" || diagnostic.Severity != "error" {
		t.Fatalf("diagnostic = %+v, want the finding located in the workspace", diagnostic)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "ledger/putnami.features.json:3:") {
		t.Fatalf("warnings = %v", warnings)
	}
	sanctions, _ = fixture.verdict(t, selected("/billing"))
	if len(sanctions) != 1 || !strings.Contains(sanctions["/billing"].Message, "features.duplicate_identity") {
		t.Fatalf("sanctions under /billing = %+v", sanctions)
	}
}

func TestEvidenceVerdictLeavesAnUnselectedProjectAlone(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings", "a-finding-of-an-unselected-project-does-not-fail-the-run")
	fixture := newEvidenceFixture(t,
		finding("ledger/putnami.features.json", "error", "features.parse_error"),
		finding("ledger/putnami.features.json", "warning", "features.missing_evidence"),
	)
	sanctions, warnings := fixture.verdict(t, selected("/billing"))
	if len(sanctions) != 0 || len(warnings) != 0 {
		t.Fatalf("sanctions = %+v, warnings = %v, want nothing for a run that selected /billing", sanctions, warnings)
	}
	// An unscoped selection counts every project.
	if sanctions, _ := fixture.verdict(t, &protocoljob.Selection{Mode: protocoljob.SelectionModeAll}); len(sanctions) != 1 {
		t.Fatalf("unscoped sanctions = %+v", sanctions)
	}
}

func TestEvidenceVerdictCountsUnownedFindingsUnderEverySelection(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings", "a-finding-no-project-owns-fails-every-selection")
	fixture := newEvidenceFixture(t, finding("README.md", "error", "docs-links"))
	sanctions, _ := fixture.verdict(t, selected("/billing"))
	if len(sanctions) != 1 || sanctions[""].Diagnostics[0].File != "README.md" {
		t.Fatalf("sanctions = %+v, want the unowned finding counted", sanctions)
	}

	// A file the reader refuses fails whatever the selection, naming the task.
	refused := newEvidenceFixture(t, `{"protocolVersion":1,"kind":"finding","project":"/billing"}`)
	sanctions, _ = refused.verdict(t, selected("/ledger"))
	if len(sanctions) != 1 || !strings.Contains(sanctions["."].Message, "was refused") {
		t.Fatalf("sanctions = %+v, want the refused file to fail the run", sanctions)
	}
}

// TestEvidenceVerdictReadsWhatTheTaskDeclares pins the producer contract: the
// declaration, not a reported artifact, decides what is read.
func TestEvidenceVerdictReadsWhatTheTaskDeclares(t *testing.T) {
	fixture := newEvidenceFixture(t, finding("billing/a.md", "error", "docs-links"))
	job := fixture.planned[0]

	// A task that did not succeed is not read: it fails the run on its own.
	fixture.results[job.Key()] = &jobs.JobResult{Status: "failed"}
	if findings, refusals := CollectFindings(fixture.ws, fixture.planned, fixture.results); len(findings) != 0 || len(refusals) != 0 {
		t.Fatalf("a failed task: findings = %+v, refusals = %+v", findings, refusals)
	}

	// A task that reports the artifact without declaring the file is refused.
	undeclared := &jobs.ScheduledJob{Project: job.Project, Extension: &extension.ExtensionDescription{Name: "@putnami/test-fixture"},
		JobDef: &extension.JobDefinition{Name: "validate~other", CommandName: "validate"}}
	results := map[string]*jobs.JobResult{undeclared.Key(): resultWithArtifact("success", evidence.FragmentArtifactID, evidence.FragmentFilename)}
	if _, refusals := CollectFindings(fixture.ws, []*jobs.ScheduledJob{undeclared}, results); len(refusals) != 1 ||
		!strings.Contains(refusals[0].Diagnostics[0].Message, "does not declare") {
		t.Fatalf("an undeclared artifact: refusals = %+v", refusals)
	}

	// A task that declares the file optional is refused: an absent file
	// could be an earlier run's silence.
	optional := evidenceJob(job.Project, true)
	results = map[string]*jobs.JobResult{optional.Key(): {Status: "success"}}
	if _, refusals := CollectFindings(fixture.ws, []*jobs.ScheduledJob{optional}, results); len(refusals) != 1 ||
		!strings.Contains(refusals[0].Diagnostics[0].Message, "optional") {
		t.Fatalf("an optional declaration: refusals = %+v", refusals)
	}

	// A declared file that is missing is refused, naming the file.
	if err := os.Remove(filepath.Join(fixture.ws.Root, ".putnami", "out", "~validate", evidence.FragmentFilename)); err != nil {
		t.Fatal(err)
	}
	fixture.results[job.Key()] = &jobs.JobResult{Status: "success"}
	_, refusals := CollectFindings(fixture.ws, fixture.planned, fixture.results)
	if len(refusals) != 1 || refusals[0].File != ".putnami/out/~validate/"+evidence.FragmentFilename {
		t.Fatalf("a missing file: refusals = %+v", refusals)
	}
}

// TestEvidenceVerdictReadsACachedTaskAsOneThatRan pins that a task served
// from the cache contributes the findings it contributes when it runs: the
// restore puts the same file back, and the reserved artifact it adds changes
// nothing the verdict reads.
func TestEvidenceVerdictReadsACachedTaskAsOneThatRan(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings", "a-cached-task-contributes-its-findings")
	fixture := newEvidenceFixture(t, finding("billing/a.md", "error", "docs-links"), finding("ledger/b.md", "warning", "docs-links"))
	job := fixture.planned[0]
	ran, ranRefusals := CollectFindings(fixture.ws, fixture.planned, fixture.results)

	cached := &jobs.JobResult{Status: "success"}
	cachekey.RestoreReservedDeclaredArtifactEvents(job, &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
		ID: "evidence", Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
		Path: evidence.FragmentFilename, State: store.TaskOutputPresent,
	}}}, cached)
	if len(jobs.TaskArtifactsOf(cached)) != 1 {
		t.Fatalf("the restore added no artifact: %+v", jobs.TaskArtifactsOf(cached))
	}
	fixture.results[job.Key()] = cached
	restored, restoredRefusals := CollectFindings(fixture.ws, fixture.planned, fixture.results)
	if len(ran) != 2 || len(ranRefusals) != 0 || len(restoredRefusals) != 0 || !reflect.DeepEqual(ran, restored) {
		t.Fatalf("ran = %+v (%+v), restored = %+v (%+v), want the same two findings", ran, ranRefusals, restored, restoredRefusals)
	}
}

// TestEvidenceVerdictFinalizerFailsTheRun pins the finalizer's rows: the
// failed <project>:evidence~verdict result, its diagnostics, the warnings on
// warn, and a run that the shared reduction then reports as failed.
func TestEvidenceVerdictFinalizerFailsTheRun(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings", "an-error-finding-of-a-selected-project-fails-the-run")
	fixture := newEvidenceFixture(t,
		finding("billing/a.md", "error", "docs-links"),
		finding("billing/b.md", "warning", "docs-links"),
		finding("ledger/c.md", "error", "docs-links"),
	)
	var warn strings.Builder
	EvidenceVerdictFinalizer(fixture.ws, fixture.planned, selected("/billing"), &warn)(fixture.results)
	row := fixture.results["/billing:"+EvidenceVerdictTask]
	if row == nil || row.Status != "failed" || row.Error == nil || !strings.Contains(row.Error.Message, "billing/a.md:3") {
		t.Fatalf("row = %+v, want a failed /billing verdict naming the finding", row)
	}
	if got := row.Canonical.Diagnostics; len(got) != 1 || got[0].Job != fixture.planned[0].Key() || got[0].File != "billing/a.md" {
		t.Fatalf("diagnostics = %+v", got)
	}
	if _, ok := fixture.results["/ledger:"+EvidenceVerdictTask]; ok {
		t.Fatal("an unselected project's finding failed the run")
	}
	if warn.String() != "putnami: evidence: billing/b.md:3: docs-links at billing/b.md (docs-links)\n" {
		t.Fatalf("warn = %q", warn.String())
	}
	session := jobs.ReduceSchedulerSession(fixture.planned, fixture.results, jobs.SessionOutcome{}, 0, nil)
	if session.Success() {
		t.Fatal("the run succeeds with a failed evidence verdict")
	}
	clean := newEvidenceFixture(t, finding("ledger/c.md", "error", "docs-links"))
	EvidenceVerdictFinalizer(clean.ws, clean.planned, selected("/billing"), io.Discard)(clean.results)
	if session := jobs.ReduceSchedulerSession(clean.planned, clean.results, jobs.SessionOutcome{}, 0, nil); !session.Success() {
		t.Fatal("a run whose selected projects have no error finding fails")
	}
}

// TestEvidenceVerdictMergesTheWorkspaceRow pins that a refused file of a
// workspace task named "workspace" and a finding no project owns, which share
// the workspace:evidence~verdict row, both reach it.
func TestEvidenceVerdictMergesTheWorkspaceRow(t *testing.T) {
	fixture := newEvidenceFixture(t, finding("README.md", "error", "docs-links"))
	refusing := evidenceJob(&workspace.Project{ID: "workspace", Name: "workspace", Path: "other"}, false)
	writeFile(t, filepath.Join(fixture.ws.Root, ".putnami", "out", "other", "~validate", evidence.FragmentFilename), "not json\n")
	fixture.planned = append(fixture.planned, refusing)
	fixture.results[refusing.Key()] = &jobs.JobResult{Status: "success"}
	for range 20 {
		results := map[string]*jobs.JobResult{}
		for key, value := range fixture.results {
			results[key] = value
		}
		EvidenceVerdictFinalizer(fixture.ws, fixture.planned, nil, io.Discard)(results)
		row := results["workspace:"+EvidenceVerdictTask]
		if row == nil || len(row.Canonical.Diagnostics) != 2 || !strings.Contains(row.Error.Message, "README.md") || !strings.Contains(row.Error.Message, "was refused") {
			t.Fatalf("row = %+v, want both failures", row)
		}
	}
}

// TestEvidenceVerdictTheRootOwnsWhatNoOtherProjectDoes pins that a project
// whose directory is one character long wins over the root.
func TestEvidenceVerdictTheRootOwnsWhatNoOtherProjectDoes(t *testing.T) {
	root := &workspace.Project{ID: "/", Path: "."}
	short := &workspace.Project{ID: "/a", Path: "a"}
	for _, order := range [][]*workspace.Project{{root, short}, {short, root}} {
		owners := newProjectOwners(workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "w"}, order))
		if got := owners.owner("a/x.md"); got != "/a" {
			t.Fatalf("owner(a/x.md) = %q, want /a", got)
		}
		if got := owners.owner("b/x.md"); got != "/" {
			t.Fatalf("owner(b/x.md) = %q, want the root", got)
		}
	}
}

// TestTheJobsPackagesNameTheReservedEvidenceFile pins that the jobs packages,
// which keep the reserved names without importing protocols/evidence, name
// the fragment file and artifact the protocol reserves.
func TestTheJobsPackagesNameTheReservedEvidenceFile(t *testing.T) {
	if cachekey.EvidenceFragmentFilename != evidence.FragmentFilename {
		t.Fatalf("cachekey names %q, the protocol reserves %q", cachekey.EvidenceFragmentFilename, evidence.FragmentFilename)
	}
	if cachekey.EvidenceFragmentArtifactID != evidence.FragmentArtifactID {
		t.Fatalf("cachekey names %q, the protocol reserves %q", cachekey.EvidenceFragmentArtifactID, evidence.FragmentArtifactID)
	}
}
