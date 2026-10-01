package specgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The synthetic fixture the whole matrix runs over: one spec-owning project
// (billing) whose projection declares one executable requirement (issue, one
// acceptance check) and one unexecutable requirement (record), plus one
// unmapped textual requirement (ghost).
const fixtureProjection = `{
  "protocolVersion": 1,
  "groups": [
    {
      "feature": "billing/invoicing",
      "spec": "billing/specs/invoicing.json",
      "specRequirements": ["ghost", "issue", "record"],
      "requirements": [
        {
          "id": "issue",
          "stage": "coded",
          "evidenceKinds": ["attestation"],
          "verification": {"kind": "acceptance", "checks": ["issue-check"]}
        },
        {"id": "record", "stage": "coded", "evidenceKinds": ["attestation"]}
      ]
    }
  ]
}`

func fixtureReport(status string) string {
	return `{
  "protocolVersion": 1,
  "observations": [
    {
      "feature": "billing/invoicing",
      "requirement": "issue",
      "check": "issue-check",
      "status": "` + status + `",
      "provenance": {"path": "invoice_test.go", "symbol": "TestInvoiceIssued"}
    }
  ]
}`
}

type gateFixture struct {
	ws      *workspace.Workspace
	planned []*jobs.ScheduledJob
	results map[string]*jobs.JobResult
}

// newGateFixture lays the session output tree out on disk exactly as the
// scheduler leaves it: the projection under the validate command's output
// directory, the report under test's, and the provenance file inside the
// project root.
func newGateFixture(t *testing.T, projection, report string) *gateFixture {
	t.Helper()
	root := t.TempDir()
	project := &workspace.Project{ID: "/billing", Name: "@acme/billing", Path: "billing",
		Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"}, []*workspace.Project{project})

	writeFile(t, filepath.Join(root, "billing", "invoice_test.go"), "package billing\n")
	fixture := &gateFixture{ws: ws, results: map[string]*jobs.JobResult{}}
	if projection != "" {
		writeFile(t, filepath.Join(root, ".putnami", "out", "billing", "validate", "spec-criteria.json"), projection)
		job := fixtureJob(project, "validate~specs", "validate")
		fixture.planned = append(fixture.planned, job)
		fixture.results[job.Key()] = resultWithArtifact("success",
			features.SpecCriteriaProjectionArtifactID, "spec-criteria.json")
	}
	if report != "" {
		writeFile(t, filepath.Join(root, ".putnami", "out", "billing", "test", "putnami-feature-verification.json"), report)
		job := fixtureJob(project, "test", "test")
		fixture.planned = append(fixture.planned, job)
		fixture.results[job.Key()] = resultWithArtifact("success",
			features.VerificationReportArtifactID, "putnami-feature-verification.json")
	}
	return fixture
}

func (f *gateFixture) project() *workspace.Project { return f.ws.Projects[0] }

func (f *gateFixture) setPolicy(scope string, mode string) {
	options := map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": mode}}}
	if scope == "workspace" {
		f.ws.Config.Options = options
		return
	}
	f.project().Config.Options = options
}

func fixtureJob(project *workspace.Project, name, command string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test-fixture"},
		JobDef:    &extension.JobDefinition{Name: name, CommandName: command},
	}
}

func resultWithArtifact(status, id, path string) *jobs.JobResult {
	return &jobs.JobResult{Status: status, Events: []jobs.RawJobEvent{{
		Type: jobs.EventTypeArtifact,
		Data: map[string]any{"id": id, "name": id, "kind": "report", "path": path},
	}}}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func collect(t *testing.T, fixture *gateFixture) *features.SpecVerificationRecord {
	t.Helper()
	record, err := Collect(fixture.ws, fixture.planned, fixture.results,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return record
}

func soleGroup(t *testing.T, record *features.SpecVerificationRecord) features.SpecVerificationGroup {
	t.Helper()
	if len(record.Groups) != 1 {
		t.Fatalf("groups = %+v, want exactly one", record.Groups)
	}
	return record.Groups[0]
}

func requirementState(t *testing.T, group features.SpecVerificationGroup, id string) features.RequirementVerificationState {
	t.Helper()
	for _, verdict := range group.Requirements {
		if verdict.Requirement == id {
			return verdict.State
		}
	}
	t.Fatalf("requirement %s is missing from %+v", id, group.Requirements)
	return ""
}

func TestCollectJoinsProjectionAndObservations(t *testing.T) {
	fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
	fixture.setPolicy("project", "enforce")
	record := collect(t, fixture)

	group := soleGroup(t, record)
	if group.Project != "/billing" || group.Feature != "billing/invoicing" {
		t.Fatalf("group identity = %+v", group)
	}
	if group.Mode != features.VerificationModeEnforce || group.ModeSource != features.VerificationModeSourceProject {
		t.Errorf("mode = (%s, %s), want the project's enforce", group.Mode, group.ModeSource)
	}
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Errorf("issue = %s, want verified", got)
	}
	if got := requirementState(t, group, "record"); got != features.RequirementUnexecutable {
		t.Errorf("record = %s, want unexecutable", got)
	}
	if got := requirementState(t, group, "ghost"); got != features.RequirementUnmapped {
		t.Errorf("ghost = %s, want unmapped", got)
	}
	// The unresolved ghost/record requirements block enforce even though the
	// executable one is verified: one passing check never hides a gap.
	if !group.Blocked {
		t.Error("enforce did not block over the unmapped and unexecutable requirements")
	}
	if len(group.Reports) != 1 || !strings.HasPrefix(group.Reports[0].Digest, "sha256:") ||
		group.Reports[0].Path != ".putnami/out/billing/test/putnami-feature-verification.json" {
		t.Errorf("report refs = %+v", group.Reports)
	}
	if group.Counts.SpecRequirements != 3 || group.Counts.Verified != 1 ||
		group.Counts.Unmapped != 1 || group.Counts.Unexecutable != 1 {
		t.Errorf("counts = %+v", group.Counts)
	}
}

func TestCollectStatesFollowTheObservations(t *testing.T) {
	cases := map[string]struct {
		report string
		want   features.RequirementVerificationState
	}{
		"passing check verifies":     {fixtureReport("passed"), features.RequirementVerified},
		"failing check contradicts":  {fixtureReport("failed"), features.RequirementContradicted},
		"skipped check goes missing": {fixtureReport("skipped"), features.RequirementMissing},
		"no report at all":           {"", features.RequirementMissing},
	}
	for name, tc := range cases {
		fixture := newGateFixture(t, fixtureProjection, tc.report)
		fixture.setPolicy("workspace", "report")
		group := soleGroup(t, collect(t, fixture))
		if got := requirementState(t, group, "issue"); got != tc.want {
			t.Errorf("%s: issue = %s, want %s", name, got, tc.want)
		}
		if group.Blocked {
			t.Errorf("%s: report mode blocked", name)
		}
	}
}

func TestCollectUnderOffListsWithoutEvaluating(t *testing.T) {
	fixture := newGateFixture(t, fixtureProjection, fixtureReport("failed"))
	fixture.setPolicy("project", "off")
	group := soleGroup(t, collect(t, fixture))
	if group.AutomaticEvaluation {
		t.Error("off evaluated automatically")
	}
	if group.Blocked || len(group.Requirements) != 0 {
		t.Errorf("off produced verdicts: %+v", group)
	}
}

// TestCollectRejectsUntrustedInputsDeterministically covers the contained
// artifact path: a symlinked report, a provenance that escapes or names no
// real file, and an observation for an undeclared check all fail closed.
func TestCollectRejectsUntrustedInputsDeterministically(t *testing.T) {
	t.Run("symlinked report", func(t *testing.T) {
		fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
		fixture.setPolicy("project", "enforce")
		reportPath := filepath.Join(fixture.ws.Root, ".putnami", "out", "billing", "test", "putnami-feature-verification.json")
		outside := filepath.Join(fixture.ws.Root, "outside.json")
		writeFile(t, outside, fixtureReport("passed"))
		if err := os.Remove(reportPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, reportPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		record := collect(t, fixture)
		group := soleGroup(t, record)
		if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
			t.Errorf("issue = %s, want missing after the symlink refusal", got)
		}
		if !hasDiagnostic(record.Diagnostics, features.ErrorCodeSymlinkEscape) {
			t.Errorf("no symlink refusal was reported: %+v", record.Diagnostics)
		}
	})

	t.Run("provenance outside the reporting project", func(t *testing.T) {
		report := strings.Replace(fixtureReport("passed"), "invoice_test.go", "missing_test.go", 1)
		fixture := newGateFixture(t, fixtureProjection, report)
		fixture.setPolicy("project", "enforce")
		record := collect(t, fixture)
		group := soleGroup(t, record)
		if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
			t.Errorf("issue = %s, want missing after the provenance drop", got)
		}
		if !hasDiagnostic(record.Diagnostics, features.ErrorCodeInvalidPath) {
			t.Errorf("no provenance drop was reported: %+v", record.Diagnostics)
		}
	})

	t.Run("undeclared check grants nothing", func(t *testing.T) {
		report := strings.Replace(fixtureReport("passed"), `"check": "issue-check"`, `"check": "surprise-check"`, 1)
		fixture := newGateFixture(t, fixtureProjection, report)
		fixture.setPolicy("project", "report")
		group := soleGroup(t, collect(t, fixture))
		if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
			t.Errorf("issue = %s, want missing", got)
		}
		if !hasDiagnostic(group.Diagnostics, features.ErrorCodeInvalidObservation) {
			t.Errorf("the undeclared observation was not reported: %+v", group.Diagnostics)
		}
	})

	t.Run("unreadable projection fails closed", func(t *testing.T) {
		fixture := newGateFixture(t, `{"protocolVersion": 1, "groups": [{"feature": "not valid"}]}`, "")
		fixture.setPolicy("project", "enforce")
		group := soleGroup(t, collect(t, fixture))
		if !group.Blocked {
			t.Error("an unreadable projection did not block enforce")
		}
		if !diag.HasErrors(group.Diagnostics) {
			t.Errorf("no error finding on the group: %+v", group.Diagnostics)
		}
	})
}

// TestCollectAcceptsBatchNormalizedArtifactPaths pins the second path
// spelling a report arrives under: a batch member's artifact path is already
// workspace-relative, and the same bytes must resolve either way.
func TestCollectAcceptsBatchNormalizedArtifactPaths(t *testing.T) {
	fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
	fixture.setPolicy("project", "enforce")
	testJob := fixture.planned[1]
	fixture.results[testJob.Key()] = resultWithArtifact("success",
		features.VerificationReportArtifactID, ".putnami/out/billing/test/putnami-feature-verification.json")

	group := soleGroup(t, collect(t, fixture))
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Errorf("issue = %s, want verified through the batch path spelling", got)
	}
}

// TestCollectReadsReusedAndBatchProjectedResults pins the three arrival
// shapes a session's artifacts take besides a fresh solo run: a local cache
// hit and a remote-restored hit (replayed events over materialized files),
// and a batch member (typed Canonical records, no re-parsed event stream). A
// hit that lost the report would silently disarm the gate, which is the
// defect ADR 0002 names.
func TestCollectReadsReusedAndBatchProjectedResults(t *testing.T) {
	for _, reuse := range []jobs.ReuseKind{jobs.ReuseLocalCache, jobs.ReuseRemoteCache} {
		fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
		fixture.setPolicy("project", "enforce")
		for _, result := range fixture.results {
			result.MarkReuse(reuse)
		}
		group := soleGroup(t, collect(t, fixture))
		if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
			t.Errorf("%s: issue = %s, want verified from replayed events", reuse, got)
		}
	}

	fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
	fixture.setPolicy("project", "enforce")
	testJob := fixture.planned[1]
	fixture.results[testJob.Key()] = &jobs.JobResult{Status: "success", Canonical: &jobs.TaskResult{
		Artifacts: []jobs.TaskArtifact{{
			ID: features.VerificationReportArtifactID, Name: "report", Kind: "report",
			Path: ".putnami/out/billing/test/putnami-feature-verification.json",
		}},
	}}
	group := soleGroup(t, collect(t, fixture))
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Errorf("batch member: issue = %s, want verified from canonical records", got)
	}
}

func TestCollectIsDeterministicAcrossRuns(t *testing.T) {
	fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
	fixture.setPolicy("workspace", "enforce")
	first, err := features.MarshalSpecVerificationRecord(collect(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	second, err := features.MarshalSpecVerificationRecord(collect(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("two collections over one session differ:\n%s\n---\n%s", first, second)
	}
}

func hasDiagnostic(diagnostics []diag.Diagnostic, code string) bool {
	for _, finding := range diagnostics {
		if finding.Code == code {
			return true
		}
	}
	return false
}

// TestCollectSeparatesNotAskedFromFoundNothing pins the one distinction the
// persisted record cannot blur. A session that ran `test` but no `validate`
// carries no criteria projection, so the gate evaluated nothing; a record
// stating that as an empty evaluation is what `specs verify` later replays,
// and it makes every enforced feature in the workspace read as unevaluated.
// Absence has to stay absence all the way to the persistence seam, whose own
// contract is "a nil record means the gate did not run".
func TestCollectSeparatesNotAskedFromFoundNothing(t *testing.T) {
	t.Run("no projection at all records nothing", func(t *testing.T) {
		fixture := newGateFixture(t, "", fixtureReport("passed"))
		fixture.setPolicy("project", "enforce")
		record, err := Collect(fixture.ws, fixture.planned, fixture.results,
			time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), nil)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if record != nil {
			t.Fatalf("a session that projected no criteria recorded an evaluation: %+v", record)
		}
	})

	// The positive control against vacuity: the same fixture WITH a projection
	// still records, so the case above is proving absence of the projection and
	// not an unrelated collapse of the whole fixture.
	t.Run("a projection still records", func(t *testing.T) {
		fixture := newGateFixture(t, fixtureProjection, fixtureReport("passed"))
		fixture.setPolicy("project", "enforce")
		if record := collect(t, fixture); record == nil || len(record.Groups) != 1 {
			t.Fatalf("a projected session recorded %+v, want one group", record)
		}
	})

	// A projection that was declared and could not be read is the opposite
	// fact: the gate WAS asked, and the failure has to survive as a finding
	// that fails closed rather than vanish with the record.
	t.Run("an unreadable projection still records its finding", func(t *testing.T) {
		fixture := newGateFixture(t, "{ not json", fixtureReport("passed"))
		fixture.setPolicy("project", "enforce")
		record := collect(t, fixture)
		if record == nil {
			t.Fatal("an unreadable projection dropped the record that carries its finding")
		}
		if !diag.HasErrors(record.Diagnostics) && !soleGroup(t, record).Blocked {
			t.Errorf("an unreadable projection neither diagnosed nor blocked: %+v", record)
		}
	})
}
