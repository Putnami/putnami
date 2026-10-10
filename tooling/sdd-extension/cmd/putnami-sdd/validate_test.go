package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The three validation jobs the canonical CI gate runs — features-validate,
// specs-validate and architecture-validate — tested at the boundary the gate
// actually crosses: a job context in, a verdict plus a JSONL stream out.
//
// What these tests are for is stated once here, because it decides every
// assertion below. A validation job has TWO failure modes and only one of them
// is loud. The loud one is a job that fails when it should pass. The quiet one
// is a job that passes because it read nothing, and that shape happened
// twice before the wire carried enough to prevent it:
//
//	architecture, wire view:    observedEdges=0  findings=0  valid=true
//	specs, project-scoped wire: valid=true, 0 specs
//
// Both were green. So no test here asserts only that a healthy fixture passes:
// each passing case also asserts the NON-ZERO count that proves the run read the
// documents, and each of the three jobs has a seeded-violation twin that must
// fail with the gate's exit code. A guard that refuses a partial view is
// asserted together with the evidence that the engine underneath would have
// answered "clean" — an unnecessary guard and a load-bearing one look identical
// from the outside, and only the second is worth keeping.

// --- the harness ------------------------------------------------------------

// jobOutcome is one job body's whole observable result: what it returned to the
// SDK runner, and what it wrote on the JSONL wire the CLI parses.
type jobOutcome struct {
	status string
	data   map[string]any
	err    error
	stream string
	events []runtimeproto.Event
}

// exitCode is the process exit code the SDK's runner derives from this outcome
// (extension-sdk/cli.exitCodeFor). It is the half of the gate contract a job
// body owns: a failing verdict must carry a classified error, or a red run
// leaves the process at 0.
func (o jobOutcome) exitCode() int {
	if o.err != nil {
		return protocolcli.ExitCodeForError(o.err)
	}
	if o.status == "FAILED" {
		return protocolcli.ExitFailure
	}
	return protocolcli.ExitSuccess
}

// diagnostics returns the diagnostic events, in stream order.
func (o jobOutcome) diagnostics() []runtimeproto.Event {
	found := make([]runtimeproto.Event, 0, len(o.events))
	for _, event := range o.events {
		if event.Type == runtimeproto.EventDiagnostic {
			found = append(found, event)
		}
	}
	return found
}

func (o jobOutcome) summaryLine() string {
	for _, event := range o.events {
		if event.Type == runtimeproto.EventSummary {
			return event.Message
		}
	}
	return ""
}

// runJob drives one job body exactly as cli.Run does — same signature, same
// emitter — and decodes the JSONL it wrote.
func runJob(t *testing.T, job cli.JobFunc, ctx *pctx.Context) jobOutcome {
	t.Helper()
	outcome := jobOutcome{}
	outcome.stream = captureStdout(t, func() {
		outcome.status, outcome.data, outcome.err = job(ctx, jsonl.New(), nil)
	})
	for _, line := range strings.Split(strings.TrimSpace(outcome.stream), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event runtimeproto.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the job wrote a line the CLI cannot parse: %v\n%s", err, line)
		}
		outcome.events = append(outcome.events, event)
	}
	return outcome
}

// validationFixture writes the project-scoped fixture both `validate` steps run
// over: one project with a durable feature manifest and a spec that details it,
// plus a sibling that mints the parent identity the manifest points at.
//
// The relation across the project boundary is deliberate. It is what makes the
// membership on the wire load-bearing rather than decorative — a view that saw
// only the job's own project reports that real relation as dangling.
func validationFixture(t *testing.T, spec string) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"gate","includes":["billing","platform"]}`)
	writeFixture(t, filepath.Join(root, "billing", "putnami.json"),
		`{"name":"@acme/billing","type":"library"}`)
	writeFixture(t, filepath.Join(root, "platform", "putnami.json"),
		`{"name":"@acme/platform","type":"library"}`)
	writeFixture(t, filepath.Join(root, "platform", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "platform",
  "features": [{
    "id": "platform/commerce",
    "type": "feature",
    "name": "Commerce",
    "outcome": "A business sells something",
    "owner": "platform",
    "target": "coded",
    "requirements": [{"id": "sell", "stage": "coded", "evidenceKinds": ["artifact"]}]
  }]
}`)
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename), billingManifest(`{"kind": "parent", "target": "platform/commerce"}`))
	writeFixture(t, filepath.Join(root, "billing", "specs", "invoicing.json"), spec)
	return root
}

// billingManifest is the job's OWN durable manifest, parameterized by the one
// thing the two features-validate tests disagree about: the relation it declares.
func billingManifest(relation string) string {
	return `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoicing",
    "type": "feature",
    "name": "Invoicing",
    "outcome": "A customer receives an invoice they can pay",
    "owner": "billing",
    "target": "coded",
    "relations": [` + relation + `],
    "requirements": [{"id": "issue", "stage": "coded", "evidenceKinds": ["artifact"]}]
  }]
}`
}

const validInvoicingSpec = `{
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["A customer receives an invoice they can pay"],
  "requirements": [{"id": "issue", "text": "An invoice is issued for every completed order."}]
}`

// invalidInvoicingSpec details a feature no manifest in the workspace mints. It
// is the seeded contract violation specs-validate must fail on.
const invalidInvoicingSpec = `{
  "protocolVersion": 1,
  "feature": "billing/refunds",
  "outcomes": ["A customer is refunded"],
  "requirements": []
}`

// billingMember and platformMember are the resolved membership entries the
// orchestrator writes for the fixture above.
func billingMember(root string) pctx.ProjectRef {
	return pctx.ProjectRef{
		ID: "/billing", Name: "@acme/billing", SourceName: "@acme/billing",
		Path: "billing", FullPath: filepath.Join(root, "billing"),
	}
}

func platformMember(root string) pctx.ProjectRef {
	return pctx.ProjectRef{
		ID: "/platform", Name: "@acme/platform", SourceName: "@acme/platform",
		Path: "platform", FullPath: filepath.Join(root, "platform"),
	}
}

// projectJobContext is the context a PROJECT-SCOPED task receives: its own
// project with its authored options, the complete resolved membership, the run's
// selection, and no `selectedProjects` (the orchestrator attaches that only to
// jobs that run once for the workspace).
func projectJobContext(root string, members ...pctx.ProjectRef) *pctx.Context {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return &pctx.Context{
		ProtocolVersion: 2,
		WorkspaceRoot:   root,
		// OutputPath is where the orchestrator points every step of this
		// command; specs-validate writes its declared criteria projection there.
		OutputPath: filepath.Join(root, ".putnami", "out", "billing", "validate"),
		Workspace:  pctx.Workspace{Name: "gate"},
		Project: pctx.Project{
			Name:     "@acme/billing",
			Path:     "billing",
			FullPath: filepath.Join(root, "billing"),
			Type:     "library",
			Options:  map[string]json.RawMessage{"publish": json.RawMessage(`{"npm":true}`)},
		},
		WorkspaceProjects: members,
		Selection: &pctx.Selection{
			Mode: pctx.SelectionModeAll, ProjectIDs: ids,
		},
	}
}

// architectureFixture writes the two-domain repository architecture-validate is
// asked about. With binding=true the cross-domain project edge is declared and
// the workspace is clean; with binding=false the same edge is undeclared and the
// evaluation must report it.
func architectureFixture(t *testing.T, binding bool) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "producer", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "producer",
  "owner": "producer-team",
  "projects": ["/producer"],
  "exports": [{
    "id": "producer.reference.v1",
    "version": 1,
    "status": "active",
    "description": "Stable producer reference.",
    "facts": [{"name":"id","authority":"producer","classification":"internal","personalData":"none"}],
    "modes": ["reference"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": []
}`)
	bindings := ""
	if binding {
		bindings = `,
    "bindings": [{"kind":"project-dependency","consumerProject":"/consumer","producerProject":"/producer"}]`
	}
	writeFixture(t, filepath.Join(root, "consumer", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "consumer",
  "owner": "consumer-team",
  "projects": ["/consumer"],
  "exports": [],
  "imports": [{
    "id": "consumer.producer-reference.v1",
    "version": 1,
    "from": {"domain":"producer","export":"producer.reference.v1"},
    "as": "consumer.producer-reference",
    "mode": "reference",
    "status": "active",
    "facts": ["id"],
    "justification": "The consumer stores the authoritative producer identity only"`+bindings+`
  }]
}`)
	return root
}

// architectureMembers is the resolved membership for the fixture above: two
// projects and the RESOLVED DIRECT edge between them. The edge is the member
// that used to be missing, which is why the wire evaluation once reported zero
// observed edges over a repository that has one.
func architectureMembers(root string) []pctx.ProjectRef {
	return []pctx.ProjectRef{
		{
			ID: "/consumer", Name: "consumer", Path: "consumer",
			FullPath: filepath.Join(root, "consumer"), Dependencies: []string{"/producer"},
		},
		{ID: "/producer", Name: "producer", Path: "producer", FullPath: filepath.Join(root, "producer")},
	}
}

// workspaceJobContext is the context a WORKSPACE-ONCE task receives: the
// synthetic workspace-root project, the complete membership, and the selection.
func workspaceJobContext(root string, members []pctx.ProjectRef) *pctx.Context {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return &pctx.Context{
		ProtocolVersion: 2,
		WorkspaceRoot:   root,
		Workspace: pctx.Workspace{
			Name: "gate", Options: architectureWorkspaceOptions(featureproto.VerificationModeEnforce),
		},
		Project:           pctx.Project{Name: "workspace", Path: ".", FullPath: root},
		SelectedProjects:  members,
		WorkspaceProjects: members,
		Selection:         &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: ids},
	}
}

func architectureWorkspaceOptions(mode featureproto.VerificationMode) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"sdd": json.RawMessage(`{"verification":{"architecture":"` + string(mode) + `"}}`),
	}
}

// reportDiagnostics decodes the diagnostics the engine put in the payload.
func reportDiagnostics(t *testing.T, data map[string]any) []diag.Diagnostic {
	t.Helper()
	encoded, err := json.Marshal(data["diagnostics"])
	if err != nil {
		t.Fatalf("re-encode diagnostics: %v", err)
	}
	var findings []diag.Diagnostic
	if err := json.Unmarshal(encoded, &findings); err != nil {
		t.Fatalf("decode diagnostics %s: %v", encoded, err)
	}
	return findings
}

// assertReportDiagnosticsReachTheWire is the property publishDiagnostics owns
// inside every job: EVERY diagnostic the engine reported is on the JSONL stream,
// in order, with its code intact — not only the ones that failed the run. A
// warning the gate never printed is a warning nobody acts on.
func assertReportDiagnosticsReachTheWire(t *testing.T, outcome jobOutcome) {
	t.Helper()
	reported := reportDiagnostics(t, outcome.data)
	published := outcome.diagnostics()
	if len(published) < len(reported) {
		t.Fatalf("the engine reported %d diagnostic(s) and the wire carried %d:\n%s",
			len(reported), len(published), outcome.stream)
	}
	for i, finding := range reported {
		if published[i].Code != finding.Code {
			t.Errorf("wire diagnostic[%d].code = %q, want %q", i, published[i].Code, finding.Code)
		}
		if published[i].Message != finding.Message {
			t.Errorf("wire diagnostic[%d].message = %q, want %q", i, published[i].Message, finding.Message)
		}
	}
}

// summaryOf reads one nested count out of a job's result payload.
func summaryOf(t *testing.T, data map[string]any, member string) float64 {
	t.Helper()
	summary, ok := data["summary"].(map[string]any)
	if !ok {
		t.Fatalf("the payload carries no summary: %#v", data)
	}
	value, ok := summary[member].(float64)
	if !ok {
		t.Fatalf("summary.%s = %#v, want a number", member, summary[member])
	}
	return value
}

// --- features-validate ------------------------------------------------------

// TestFeaturesValidatePassesAndReportsWhatItRead is the healthy half of the
// first `validate` step.
//
// The counts are asserted to be NON-ZERO, not merely error-free. A job that
// resolved no membership, discovered no root, and read no manifest returns
// exactly the same status and exactly the same absence of diagnostics; the only
// thing that separates the two is that one of them says it saw a feature.
func TestFeaturesValidatePassesAndReportsWhatItRead(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	outcome := runJob(t, runFeaturesValidate,
		projectJobContext(root, billingMember(root), platformMember(root)))

	if outcome.err != nil {
		t.Fatalf("features-validate failed on a healthy project: %v", outcome.err)
	}
	if outcome.status != "OK" {
		t.Errorf("status = %q, want OK", outcome.status)
	}
	if outcome.exitCode() != protocolcli.ExitSuccess {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitSuccess)
	}
	if outcome.data["valid"] != true {
		t.Errorf("valid = %#v, want true", outcome.data["valid"])
	}
	if got := summaryOf(t, outcome.data, "features"); got != 1 {
		t.Errorf("summary.features = %v, want 1; a run that read nothing also reports no error", got)
	}
	if got := summaryOf(t, outcome.data, "requirements"); got != 1 {
		t.Errorf("summary.requirements = %v, want 1", got)
	}
	// A passing run still publishes what it found. The engine's warnings are not
	// blocking and they are not silent.
	assertReportDiagnosticsReachTheWire(t, outcome)
	for _, event := range outcome.diagnostics() {
		if event.Severity != nil && *event.Severity == runtimeproto.SeverityError {
			t.Errorf("a passing run published an error diagnostic: %+v", event)
		}
	}
	// The human line the CLI prints for the task carries the same counts, so a
	// reader of the run log sees the size of what was checked.
	if want := "features: 1 feature(s), 1 requirement(s)"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
}

// TestFeaturesValidateFailsOnASeededViolation is what makes the test above
// non-vacuous, and it is the gate's own contract: a broken durable manifest must
// stop the run with a classified error, because the SDK turns that
// classification into the process exit code.
func TestFeaturesValidateFailsOnASeededViolation(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	// The relation now points at an identity no manifest in the workspace mints.
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename),
		billingManifest(`{"kind": "parent", "target": "platform/nonexistent"}`))

	outcome := runJob(t, runFeaturesValidate,
		projectJobContext(root, billingMember(root), platformMember(root)))

	if outcome.err == nil {
		t.Fatal("features-validate accepted a manifest whose relation resolves to nothing")
	}
	if outcome.status != "" {
		t.Errorf("status = %q, want the empty status a failed job returns", outcome.status)
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d (contract violation)", outcome.exitCode(), protocolcli.ExitUsage)
	}
	// The report travels with the failure: the CLI's failure envelope carries it,
	// and a verdict with no data tells a reader only that something broke.
	if outcome.data["valid"] != false {
		t.Errorf("valid = %#v, want false in the failure payload", outcome.data["valid"])
	}
	assertReportDiagnosticsReachTheWire(t, outcome)
	if !hasDiagnosticEvent(outcome.diagnostics(), "features.dangling_relation") {
		t.Errorf("diagnostic events = %+v, want the dangling-relation report", outcome.diagnostics())
	}
}

// TestFeaturesValidateResolvesRelationsAcrossTheWholeMembership is the false
// FAILURE this job's membership tier exists to prevent, and it is the half a
// blind-validator test cannot state.
//
// A feature relation names a feature, not a file, and the target may be authored
// anywhere in the workspace — a `parent` relation to a shared scaffolding feature
// is the normal case. The first `--impacted` gate run after the extraction caught this
// exactly: go/templates/go-library declares a parent authored in
// tooling/scaffold, and a one-project identity tier reported that real relation
// as dangling.
//
// So narrowing projectValidationView's MEMBERSHIP to the job's own project — the
// obvious "optimization", since the projection is one project anyway — turns
// every cross-project relation in the repository into a red gate. Both sides are
// asserted, because only the pair distinguishes "resolves" from "never checked".
func TestFeaturesValidateResolvesRelationsAcrossTheWholeMembership(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)

	seeing := runJob(t, runFeaturesValidate,
		projectJobContext(root, billingMember(root), platformMember(root)))
	if seeing.err != nil {
		t.Fatalf("a real cross-project relation was reported as broken: %v", seeing.err)
	}
	// The projection stays the job's own project: seeing the sibling must not
	// make this task report the sibling's features under its own key.
	if got := summaryOf(t, seeing.data, "features"); got != 1 {
		t.Errorf("summary.features = %v, want only the job's own project's", got)
	}

	blind := runJob(t, runFeaturesValidate, projectJobContext(root, billingMember(root)))
	if blind.err == nil {
		t.Fatal("a one-project membership resolved a relation authored in another project; " +
			"this pair no longer measures the membership tier")
	}
	if !hasDiagnosticEvent(blind.diagnostics(), "features.dangling_relation") {
		t.Errorf("diagnostics = %+v, want the dangling-relation report the narrow view produces",
			blind.diagnostics())
	}
}

// --- specs-validate ---------------------------------------------------------

// TestSpecsValidatePassesAndReportsWhatItRead pins the second `validate` step on
// a healthy project, with the count that the measured regression produced as
// zero: `valid=true, 0 specs` was the shape of a step that was green because it
// read nothing.
func TestSpecsValidatePassesAndReportsWhatItRead(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	outcome := runJob(t, runSpecsValidate,
		projectJobContext(root, billingMember(root), platformMember(root)))

	if outcome.err != nil {
		t.Fatalf("specs-validate failed on a healthy project: %v", outcome.err)
	}
	if outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Errorf("status = %q exit = %d, want OK and 0", outcome.status, outcome.exitCode())
	}
	if got := summaryOf(t, outcome.data, "specs"); got != 1 {
		t.Errorf("summary.specs = %v, want 1; this is the exact count the blind step reported as 0", got)
	}
	if got := summaryOf(t, outcome.data, "authoredFeatures"); got != 1 {
		t.Errorf("summary.authoredFeatures = %v, want 1", got)
	}
	// `project.options.publish.npm` travels on the job's own project and nowhere
	// else, so this count is also the proof that the own-project facts were
	// adopted: without them a publishable project reads as unpublished and its
	// completeness gap is never assessed.
	if got := summaryOf(t, outcome.data, "publishableProjects"); got != 1 {
		t.Errorf("summary.publishableProjects = %v, want 1; options.publish did not travel", got)
	}
	assertReportDiagnosticsReachTheWire(t, outcome)
}

// TestSpecsValidateEmitsTheCriteriaProjection pins the artifact half of the
// D9 split: a healthy specs-validate run writes the executable-criteria
// projection at its declared path, announces it under the reserved artifact
// id, and produces the same bytes on a second run — the property the cache
// contract restores on a hit.
func TestSpecsValidateEmitsTheCriteriaProjection(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	context := projectJobContext(root, billingMember(root), platformMember(root))
	outcome := runJob(t, runSpecsValidate, context)
	if outcome.err != nil {
		t.Fatalf("specs-validate failed on a healthy project: %v", outcome.err)
	}

	var announced *runtimeproto.Event
	for i, event := range outcome.events {
		if event.Type == runtimeproto.EventArtifact && event.ID == featureproto.SpecCriteriaProjectionArtifactID {
			announced = &outcome.events[i]
		}
	}
	if announced == nil {
		t.Fatalf("no %s artifact event on the wire; events = %+v", featureproto.SpecCriteriaProjectionArtifactID, outcome.events)
	}
	if announced.Path != filepath.Join(context.OutputPath, featureproto.SpecCriteriaProjectionFilename) {
		t.Errorf("artifact path = %q, want the absolute destination the CLI normalizes", announced.Path)
	}

	written, err := os.ReadFile(filepath.Join(context.OutputPath, featureproto.SpecCriteriaProjectionFilename))
	if err != nil {
		t.Fatalf("read the declared projection: %v", err)
	}
	projection, diagnostics := featureproto.ParseAndValidateSpecCriteriaProjection(written)
	if projection == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("the emitted projection fails its own strict reader: %v", diagnostics)
	}
	if len(projection.Groups) != 1 || projection.Groups[0].Feature != "billing/invoicing" {
		t.Fatalf("projection groups = %+v, want exactly billing/invoicing", projection.Groups)
	}
	group := projection.Groups[0]
	if len(group.SpecRequirements) != 1 || group.SpecRequirements[0] != "issue" {
		t.Errorf("specRequirements = %v, want the spec's [issue]", group.SpecRequirements)
	}
	// The manifest's same-ID requirement carries no criterion at v1, and the
	// projection must say so rather than drop it: unexecutable and unmapped are
	// different verdicts downstream.
	if len(group.Requirements) != 1 || group.Requirements[0].ID != "issue" || group.Requirements[0].Verification != nil {
		t.Errorf("requirements = %+v, want the criterionless same-ID join", group.Requirements)
	}

	rerun := projectJobContext(root, billingMember(root), platformMember(root))
	rerun.OutputPath = filepath.Join(root, ".putnami", "out", "billing", "validate-rerun")
	if second := runJob(t, runSpecsValidate, rerun); second.err != nil {
		t.Fatalf("rerun failed: %v", second.err)
	}
	again, err := os.ReadFile(filepath.Join(rerun.OutputPath, featureproto.SpecCriteriaProjectionFilename))
	if err != nil {
		t.Fatalf("read the rerun projection: %v", err)
	}
	if string(written) != string(again) {
		t.Fatalf("projection bytes are not deterministic:\n%s\n---\n%s", written, again)
	}
}

// TestSpecsValidateWritesNoProjectionForABrokenRepository pins the refusal: a
// projection derived from documents the validator refused would project
// identities nobody agreed to, so a failing run must leave nothing behind for
// a collector to trust.
func TestSpecsValidateWritesNoProjectionForABrokenRepository(t *testing.T) {
	root := validationFixture(t, invalidInvoicingSpec)
	context := projectJobContext(root, billingMember(root), platformMember(root))
	outcome := runJob(t, runSpecsValidate, context)
	if outcome.err == nil {
		t.Fatal("specs-validate accepted the seeded violation")
	}
	if _, err := os.Stat(filepath.Join(context.OutputPath, featureproto.SpecCriteriaProjectionFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failing run left a projection behind (stat err = %v)", err)
	}
}

// TestSpecsValidateFailsOnASeededViolationAndNamesTheDocument is the failing
// twin, plus the diagnostic contract the gate reads: the finding must name WHICH
// document broke, in the "<path>" / "<path>#<member>" spelling every other
// Putnami validator uses, so the CLI renders a file position instead of a bare
// message.
func TestSpecsValidateFailsOnASeededViolationAndNamesTheDocument(t *testing.T) {
	root := validationFixture(t, invalidInvoicingSpec)
	outcome := runJob(t, runSpecsValidate,
		projectJobContext(root, billingMember(root), platformMember(root)))

	if outcome.err == nil {
		t.Fatal("specs-validate accepted a spec detailing a feature no manifest mints")
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitUsage)
	}
	if outcome.data["valid"] != false {
		t.Errorf("valid = %#v, want false in the failure payload", outcome.data["valid"])
	}

	assertReportDiagnosticsReachTheWire(t, outcome)
	unknown := findDiagnosticEvent(outcome.diagnostics(), "features.unknown_feature")
	if unknown == nil {
		t.Fatalf("diagnostic events = %+v, want the unauthored-feature violation", outcome.diagnostics())
	}
	if unknown.Severity == nil || *unknown.Severity != runtimeproto.SeverityError {
		t.Errorf("severity = %v, want error", unknown.Severity)
	}
	if unknown.Location == nil || !strings.Contains(unknown.Location.File, "billing/specs/invoicing.json") {
		t.Errorf("location = %+v, want the spec document that broke", unknown.Location)
	}
	// These documents are validated as parsed JSON, not as text, so there is no
	// line to report and inventing one would send a reader to the wrong place.
	if unknown.Location != nil && (unknown.Location.Line != 0 || unknown.Location.Column != 0) {
		t.Errorf("location = %+v, want no invented line/column", unknown.Location)
	}
}

// --- architecture-validate --------------------------------------------------

// TestArchitectureValidatePassesOverADeclaredEdge is the healthy half of
// `validate-workspace`, and the assertion that matters is observedEdges.
//
// Measured before the wire carried `workspaceProjects[].dependencies`, this job
// reported `observedEdges=0 findings=0 valid=true` over a repository with one
// real cross-domain edge. Asserting only that a clean workspace passes would
// have been satisfied by that blind run too.
func TestArchitectureValidatePassesOverADeclaredEdge(t *testing.T) {
	root := architectureFixture(t, true)
	outcome := runJob(t, runArchitectureValidate, workspaceJobContext(root, architectureMembers(root)))

	if outcome.err != nil {
		t.Fatalf("architecture-validate failed on a declared edge: %v", outcome.err)
	}
	if outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Errorf("status = %q exit = %d, want OK and 0", outcome.status, outcome.exitCode())
	}
	if got := summaryOf(t, outcome.data, "domains"); got != 2 {
		t.Errorf("summary.domains = %v, want 2", got)
	}
	if got := summaryOf(t, outcome.data, "observedEdges"); got != 1 {
		t.Errorf("summary.observedEdges = %v, want 1; this is the count the blind job reported as 0", got)
	}
	if got := summaryOf(t, outcome.data, "findings"); got != 0 {
		t.Errorf("summary.findings = %v, want 0", got)
	}
	if want := "1 observed edge(s), 0 finding(s)"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
}

// TestArchitectureValidateFailsOnAnUndeclaredEdge is the seeded violation for
// the workspace-once job, and it is the regression test for the blind evaluation
// in the strictest sense: the same fixture, evaluated without the resolved edges
// on the wire, passes. The failure below is only reachable because the
// membership reached the job.
func TestArchitectureValidateFailsOnAnUndeclaredEdge(t *testing.T) {
	root := architectureFixture(t, false)
	members := architectureMembers(root)
	outcome := runJob(t, runArchitectureValidate, workspaceJobContext(root, members))

	if outcome.err == nil {
		t.Fatal("architecture-validate accepted an undeclared cross-domain dependency")
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitUsage)
	}
	if outcome.data["valid"] != false {
		t.Errorf("valid = %#v, want false in the failure payload", outcome.data["valid"])
	}
	if got := summaryOf(t, outcome.data, "findings"); got != 1 {
		t.Fatalf("summary.findings = %v, want the undeclared dependency", got)
	}

	// Findings are published as diagnostics with NO file position: a finding is
	// the ratchet's verdict on the graph, not a complaint about one document, and
	// guessing which of the two manifests is "the" file would send half the
	// readers to the wrong one.
	undeclared := findDiagnosticEvent(outcome.diagnostics(),
		string(archproto.ErrorCodeUndeclaredProjectDependency))
	if undeclared == nil {
		t.Fatalf("diagnostic events = %+v, want the undeclared-dependency finding", outcome.diagnostics())
	}
	if undeclared.Location != nil {
		t.Errorf("location = %+v, want none: a finding names an edge, not a file", undeclared.Location)
	}

	// The measurement this job exists to defend: strip the resolved edges the
	// wire carries and the very same repository validates clean.
	for i := range members {
		members[i].Dependencies = nil
	}
	blind := runJob(t, runArchitectureValidate, workspaceJobContext(root, members))
	if blind.err != nil {
		t.Fatalf("the edgeless view failed for some other reason; this comparison no longer isolates the edges: %v", blind.err)
	}
	if got := summaryOf(t, blind.data, "observedEdges"); got != 0 {
		t.Fatalf("the edgeless view observed %v edge(s); the fixture no longer reproduces the blind run", got)
	}
}

func TestArchitectureValidateReportMakesCoherentFindingsAdvisory(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-adoption-policy",
		"automatic-architecture-policy-controls-admission")
	root := architectureFixture(t, false)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = architectureWorkspaceOptions(featureproto.VerificationModeReport)
	ctx.Project.Options = map[string]json.RawMessage{
		"sdd": json.RawMessage(`{"verification":{"architecture":"enforce"}}`),
	}

	outcome := runJob(t, runArchitectureValidate, ctx)
	if outcome.err != nil || outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Fatalf("report mode blocked a coherent architecture finding: status=%q err=%v", outcome.status, outcome.err)
	}
	if outcome.data["mode"] != string(featureproto.VerificationModeReport) ||
		outcome.data["modeSource"] != string(featureproto.VerificationModeSourceWorkspace) ||
		outcome.data["automaticEvaluation"] != true {
		t.Errorf("policy data = mode:%#v source:%#v automatic:%#v",
			outcome.data["mode"], outcome.data["modeSource"], outcome.data["automaticEvaluation"])
	}
	if outcome.data["valid"] != false || summaryOf(t, outcome.data, "findings") != 1 {
		t.Errorf("typed evaluation was weakened instead of preserved: %#v", outcome.data)
	}
	finding := findDiagnosticEvent(outcome.diagnostics(), string(archproto.ErrorCodeUndeclaredProjectDependency))
	if finding == nil || finding.Severity == nil || *finding.Severity != runtimeproto.SeverityWarning {
		t.Fatalf("report finding was not published as an advisory warning: %+v", outcome.diagnostics())
	}
	if !strings.Contains(outcome.summaryLine(), "mode=report source=workspace") {
		t.Errorf("summary = %q, want effective policy and provenance", outcome.summaryLine())
	}
}

func TestArchitectureValidateReportStillBlocksStructuralErrors(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-adoption-policy",
		"report-never-hides-structural-errors")
	root := architectureFixture(t, false)
	writeFixture(t, filepath.Join(root, "producer", archproto.ManifestFilename), `{`)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = architectureWorkspaceOptions(featureproto.VerificationModeReport)

	outcome := runJob(t, runArchitectureValidate, ctx)
	if outcome.err == nil || outcome.exitCode() != protocolcli.ExitUsage {
		t.Fatalf("report mode hid a structural parse error: status=%q data=%#v", outcome.status, outcome.data)
	}
	if outcome.data["mode"] != string(featureproto.VerificationModeReport) || outcome.data["automaticEvaluation"] != true {
		t.Errorf("failure payload lost policy provenance: %#v", outcome.data)
	}
	counts, ok := outcome.data["diagnosticCounts"].(map[string]any)
	if errors, present := counts["errors"].(float64); !ok || !present || errors == 0 {
		t.Errorf("failure payload has no structural error count: %#v", outcome.data["diagnosticCounts"])
	}
}

func TestArchitectureValidateOffSkipsAutomaticEvaluation(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-adoption-policy",
		"off-skips-automatic-architecture-evaluation")
	ctx := &pctx.Context{Workspace: pctx.Workspace{
		Name: "gate", Options: architectureWorkspaceOptions(featureproto.VerificationModeOff),
	}}
	outcome := runJob(t, runArchitectureValidate, ctx)
	if outcome.err != nil || outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Fatalf("off mode did not skip cleanly: status=%q err=%v", outcome.status, outcome.err)
	}
	if outcome.data["mode"] != string(featureproto.VerificationModeOff) ||
		outcome.data["modeSource"] != string(featureproto.VerificationModeSourceWorkspace) ||
		outcome.data["automaticEvaluation"] != false {
		t.Errorf("off payload = %#v, want mode/source and automaticEvaluation=false", outcome.data)
	}
	if _, evaluated := outcome.data["findings"]; evaluated {
		t.Errorf("off payload contains an invented evaluation: %#v", outcome.data)
	}
	if len(outcome.diagnostics()) != 0 || !strings.Contains(outcome.summaryLine(), "automatic evaluation skipped") {
		t.Errorf("off stream = %s, want a visible skip and no findings", outcome.stream)
	}
}

func TestArchitectureValidateDefaultsToReportAndIgnoresProjectPolicy(t *testing.T) {
	root := architectureFixture(t, false)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = nil
	ctx.Project.Options = map[string]json.RawMessage{
		"sdd": json.RawMessage(`{"verification":{"architecture":"enforce"}}`),
	}
	outcome := runJob(t, runArchitectureValidate, ctx)
	if outcome.err != nil {
		t.Fatalf("the built-in report default was replaced by a synthetic project override: %v", outcome.err)
	}
	if outcome.data["mode"] != string(featureproto.VerificationModeReport) ||
		outcome.data["modeSource"] != string(featureproto.VerificationModeSourceDefault) {
		t.Errorf("default policy = (%#v, %#v), want report/default",
			outcome.data["mode"], outcome.data["modeSource"])
	}
}

func TestArchitectureValidateRejectsUnreadableWorkspacePolicy(t *testing.T) {
	root := architectureFixture(t, true)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = map[string]json.RawMessage{
		"sdd": json.RawMessage(`{"verification":{"architecture":"audit"}}`),
	}
	outcome := runJob(t, runArchitectureValidate, ctx)
	if outcome.err == nil || outcome.exitCode() != protocolcli.ExitUsage || outcome.data != nil {
		t.Fatalf("unreadable policy did not fail closed: status=%q data=%#v err=%v",
			outcome.status, outcome.data, outcome.err)
	}
	if !strings.Contains(outcome.err.Error(), "options.sdd.verification.architecture") {
		t.Errorf("error = %q, want the exact committed policy key", outcome.err)
	}
}

// TestArchitectureValidateRefusesAContextWithoutWorkspaceMembership is the
// blind-validator guard for the workspace-once job.
//
// `architecture validate` asks two workspace-wide questions — is every project a
// manifest names a real member, and does any project depend across a domain
// boundary without a declared binding — and neither is answerable from a subset.
// Handed the selection instead of the membership, the job must say so. Passing
// would be the worst outcome available: a green gate over an unread graph.
func TestArchitectureValidateRefusesAContextWithoutWorkspaceMembership(t *testing.T) {
	root := architectureFixture(t, false)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.WorkspaceProjects = nil // an orchestrator that predates the member

	outcome := runJob(t, runArchitectureValidate, ctx)

	if outcome.err == nil {
		t.Fatalf("a selection-only context produced a workspace verdict: status=%q data=%#v",
			outcome.status, outcome.data)
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitUsage)
	}
	if !strings.Contains(outcome.err.Error(), "workspaceProjects") {
		t.Errorf("error = %q, want one naming the missing wire member", outcome.err)
	}
	// The refusal happens BEFORE the engine runs. Policy provenance remains
	// visible, but no evaluation report is invented.
	if outcome.data["mode"] != string(featureproto.VerificationModeEnforce) ||
		outcome.data["automaticEvaluation"] != true {
		t.Errorf("data = %#v, want policy provenance", outcome.data)
	}
	if _, evaluated := outcome.data["valid"]; evaluated {
		t.Errorf("data = %#v, want no invented evaluation", outcome.data)
	}
	if diagnostics := outcome.diagnostics(); len(diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want none: nothing was evaluated", diagnostics)
	}
}

// --- the two view builders --------------------------------------------------

// TestProjectValidationViewSeesTheWorkspaceAndReportsOnOneProject pins the
// two-tier split that decides what a per-project task may claim.
//
// MEMBERSHIP is the whole workspace, because a feature relation resolves against
// every durable manifest in it. PROJECTION is the job's own project, because
// everything this task owns and reports is its project's. Collapsing the two in
// either direction is a real defect: widening the projection reports every
// project's findings under one project's cache key, and narrowing the membership
// reports a real cross-project relation as dangling.
func TestProjectValidationViewSeesTheWorkspaceAndReportsOnOneProject(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	ctx := projectJobContext(root, billingMember(root), platformMember(root))
	// The RUN selected both projects. This task still acts on exactly one.
	ctx.Selection = &pctx.Selection{
		Mode:           pctx.SelectionModeImpacted,
		Scoped:         true,
		Baseline:       "origin/main",
		BaselineSource: "trunk",
		ProjectIDs:     []string{"/billing", "/platform"},
	}

	ws, selection, err := projectValidationView(ctx)
	if err != nil {
		t.Fatalf("projectValidationView: %v", err)
	}
	if len(ws.Projects) != 2 {
		t.Errorf("membership = %d project(s), want the whole workspace", len(ws.Projects))
	}
	if ws.ProjectByID("/platform") == nil {
		t.Error("the sibling that mints the parent identity is not in the view")
	}
	if selection.Mode != pctx.SelectionModeProjects || !selection.Scoped {
		t.Errorf("selection = %+v, want an explicitly scoped project projection", selection)
	}
	if len(selection.ProjectIDs) != 1 || selection.ProjectIDs[0] != "/billing" {
		t.Errorf("projection = %v, want only the job's own project", selection.ProjectIDs)
	}
	// The baseline names the RUN's ref, and both tasks that read this view are
	// cached on keys that read no ref: a replayed report would name another
	// run's.
	if selection.Baseline != "" || selection.BaselineSource != "" {
		t.Errorf("baseline = %q/%q, want none: no cache key reads the run's ref", selection.Baseline, selection.BaselineSource)
	}
}

// TestProjectValidationViewIsBuiltFromTheWireNotTheFilesystem states where the
// membership comes from. The fixture on disk has two projects; the wire names
// one; the view has one. An extension has no loader, and a view that
// re-discovered the tree would be a second answer to a question the orchestrator
// already answered.
func TestProjectValidationViewIsBuiltFromTheWireNotTheFilesystem(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	ws, _, err := projectValidationView(projectJobContext(root, billingMember(root)))
	if err != nil {
		t.Fatalf("projectValidationView: %v", err)
	}
	if len(ws.Projects) != 1 || ws.ProjectByID("/billing") == nil {
		t.Fatalf("membership = %+v, want exactly what the wire named", ws.Projects)
	}
	if ws.ProjectByID("/platform") != nil {
		t.Error("a project the wire did not name is in the view; this membership was discovered, not read")
	}
	// An older producer may still omit ProjectRef.config. The job's own project
	// continues to adopt the publish/options members that predate that carrier,
	// so additive wire evolution does not erase its authored facts.
	own := ws.ProjectByID("/billing")
	if own.Config == nil || own.Config.Options["publish"] == nil {
		t.Errorf("the job's own project = %+v, want its authored options adopted", own)
	}
}

// TestProjectValidationViewKeepsItsProjectionWithoutASelection covers the
// orchestrator that sends no `selection` block. The projection must stay the
// job's own project: reading an absent selection as "unscoped" would widen this
// task to the whole workspace and report every project's findings under one
// project's key.
func TestProjectValidationViewKeepsItsProjectionWithoutASelection(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	ctx := projectJobContext(root, billingMember(root), platformMember(root))
	ctx.Selection = nil

	_, selection, err := projectValidationView(ctx)
	if err != nil {
		t.Fatalf("projectValidationView: %v", err)
	}
	if !selection.Scoped || len(selection.ProjectIDs) != 1 || selection.ProjectIDs[0] != "/billing" {
		t.Errorf("selection = %+v, want the job's own project alone", selection)
	}
	if selection.Baseline != "" || selection.BaselineSource != "" {
		t.Errorf("baseline = %q/%q, want none: the run resolved none", selection.Baseline, selection.BaselineSource)
	}
}

// TestProjectValidationViewRefusesAContextWithNoProject is the guard for the
// measured `valid=true, 0 specs` shape.
//
// The second half of this test is the part that matters. It shows that the
// engine underneath answers CLEAN over an empty membership, so the refusal is
// the only thing standing between the gate and a step that is green on every
// project in the workspace, forever.
func TestProjectValidationViewRefusesAContextWithNoProject(t *testing.T) {
	root := validationFixture(t, invalidInvoicingSpec)
	// A workspace-once shaped context: no membership, and a project rooted at the
	// workspace root, which mints no member.
	empty := &pctx.Context{
		ProtocolVersion: 2,
		WorkspaceRoot:   root,
		Project:         pctx.Project{Name: "workspace", Path: ".", FullPath: root},
		Selection:       &pctx.Selection{Mode: pctx.SelectionModeAll},
	}

	for name, job := range map[string]cli.JobFunc{
		"features-validate": runFeaturesValidate,
		"specs-validate":    runSpecsValidate,
	} {
		outcome := runJob(t, job, empty)
		if outcome.err == nil {
			t.Fatalf("%s validated a context that names no project: status=%q data=%#v",
				name, outcome.status, outcome.data)
		}
		if outcome.exitCode() != protocolcli.ExitUsage {
			t.Errorf("%s exit code = %d, want %d", name, outcome.exitCode(), protocolcli.ExitUsage)
		}
		if !strings.Contains(outcome.err.Error(), "no project to validate") {
			t.Errorf("%s error = %q, want one naming the cause", name, outcome.err)
		}
	}

	// Without the guard: the engine reads the same empty view and reports a clean
	// run over a workspace whose spec is broken.
	blind, _ := wsview.FromContext(empty)
	report, err := sdd.BuildSpecValidationResult(blind, sdd.Selection{Mode: pctx.SelectionModeAll})
	if err != nil || !report.Valid || report.Summary.Specs != 0 {
		t.Fatalf("the engine no longer answers clean over an empty membership (%v, %+v); "+
			"the refusal above may have become redundant, and this test's claim is stale", err, report.Summary)
	}
}

// TestProjectValidationViewRefusesWhenTheMembershipOmitsItsProject is the second
// blind shape, and the subtler one: the wire carries a membership, so nothing
// looks empty, but the job's own project is not in it. The projection then names
// a project the view does not contain, and every engine answers about the empty
// set — a clean verdict for a project that was never read.
func TestProjectValidationViewRefusesWhenTheMembershipOmitsItsProject(t *testing.T) {
	root := validationFixture(t, invalidInvoicingSpec)
	ctx := projectJobContext(root, platformMember(root)) // the sibling only

	outcome := runJob(t, runSpecsValidate, ctx)
	if outcome.err == nil {
		t.Fatalf("specs-validate reported on a project the membership omits: status=%q data=%#v",
			outcome.status, outcome.data)
	}
	if !strings.Contains(outcome.err.Error(), "/billing") {
		t.Errorf("error = %q, want one naming the project that is missing", outcome.err)
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitUsage)
	}

	// Without the guard: a projection naming an absent project evaluates the
	// empty set and passes, over a workspace whose spec is broken.
	blind, _ := wsview.FromContext(ctx)
	report, err := sdd.BuildSpecValidationResult(blind, sdd.Selection{
		Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"},
	})
	if err != nil || !report.Valid || report.Summary.Specs != 0 {
		t.Fatalf("the engine no longer answers clean for an absent project (%v, %+v); "+
			"the refusal above may have become redundant, and this test's claim is stale", err, report.Summary)
	}
}

// TestWorkspaceValidationViewRequiresTheMembershipMember states the required
// wire members for the workspace-scoped view, one refusal per missing member,
// and the accepting case that keeps the refusals honest.
func TestWorkspaceValidationViewRequiresTheMembershipMember(t *testing.T) {
	root := architectureFixture(t, true)

	// No context at all: wsview yields no workspace rather than an empty one.
	if _, err := workspaceValidationView(nil); err == nil ||
		!strings.Contains(err.Error(), "no workspace") ||
		protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Errorf("a nil context produced %v, want a usage-classified refusal", err)
	}

	// A context with the selection but not the membership. wsview also attaches
	// its incomplete-view warning here and the engine fails closed on it; this
	// refusal exists so the message names the CAUSE instead of the symptom.
	selectionOnly := workspaceJobContext(root, architectureMembers(root))
	selectionOnly.WorkspaceProjects = nil
	if _, err := workspaceValidationView(selectionOnly); err == nil ||
		!strings.Contains(err.Error(), "workspaceProjects") ||
		protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Errorf("a selection-only context produced %v, want a refusal naming the member", err)
	}
	partial, _ := wsview.FromContext(selectionOnly)
	if !partial.HasWarningCode(wsview.WarningCodeProviderViewUnavailable) {
		t.Error("wsview no longer marks a selection-only view as incomplete; the refusal is now the only defense")
	}

	// The complete membership: accepted, whole, and unnarrowed.
	ws, err := workspaceValidationView(workspaceJobContext(root, architectureMembers(root)))
	if err != nil {
		t.Fatalf("a complete membership was refused: %v", err)
	}
	if len(ws.Projects) != 2 || ws.ProjectByID("/producer") == nil || ws.ProjectByID("/consumer") == nil {
		t.Fatalf("membership = %+v, want both members", ws.Projects)
	}
	if ws.HasWarningCode(wsview.WarningCodeProviderViewUnavailable) {
		t.Error("a complete membership was marked incomplete")
	}
	// The resolved direct edges travel with it. Without them the evaluation
	// observes no edges and finds nothing to object to.
	if consumer := ws.ProjectByID("/consumer"); len(consumer.Dependencies) != 1 {
		t.Errorf("consumer dependencies = %v, want the resolved direct edge", consumer.Dependencies)
	}
}

// --- the wire helpers -------------------------------------------------------

// TestPublishDiagnosticsPutsEveryFindingOnTheWire pins the shape the CLI reads.
// A verdict a reader cannot locate is a verdict they have to reproduce by hand,
// so the field spelling — "<path>" or "<path>#<member>" — lands in the file
// position unchanged, and an empty field yields no position at all rather than
// an invented one.
func TestPublishDiagnosticsPutsEveryFindingOnTheWire(t *testing.T) {
	findings := []diag.Diagnostic{
		{Severity: diag.Error, Code: "features.unknown_feature", Message: "unknown feature", Field: "billing/specs/a.json"},
		{Severity: diag.Warning, Code: "features.stale_evidence", Message: "stale", Field: "billing/putnami.features.json#features[0]"},
		{Severity: diag.Info, Code: "features.note", Message: "about the run as a whole"},
	}
	stream := captureStdout(t, func() { publishDiagnostics(jsonl.New(), findings) })
	lines := strings.Split(strings.TrimSpace(stream), "\n")
	events := make([]runtimeproto.Event, 0, len(lines))
	for _, line := range lines {
		var event runtimeproto.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unparsable line %q: %v", line, err)
		}
		events = append(events, event)
	}

	if len(events) != len(findings) {
		t.Fatalf("emitted %d event(s) for %d finding(s):\n%s", len(events), len(findings), stream)
	}
	for i, event := range events {
		if event.Type != runtimeproto.EventDiagnostic {
			t.Errorf("event[%d].type = %q, want diagnostic", i, event.Type)
		}
		if event.Message != findings[i].Message {
			t.Errorf("event[%d].message = %q, want %q", i, event.Message, findings[i].Message)
		}
		if event.Code != findings[i].Code {
			t.Errorf("event[%d].code = %q, want %q", i, event.Code, findings[i].Code)
		}
		if event.Severity == nil || string(*event.Severity) != string(findings[i].Severity) {
			t.Errorf("event[%d].severity = %v, want %q", i, event.Severity, findings[i].Severity)
		}
	}
	if events[0].Location == nil || events[0].Location.File != "billing/specs/a.json" {
		t.Errorf("event[0].location = %+v, want the document path verbatim", events[0].Location)
	}
	if events[1].Location == nil || events[1].Location.File != "billing/putnami.features.json#features[0]" {
		t.Errorf("event[1].location = %+v, want the path#member spelling verbatim", events[1].Location)
	}
	if events[2].Location != nil {
		t.Errorf("event[2].location = %+v, want none: the finding is about no single document", events[2].Location)
	}
}

// TestFinishCarriesTheWholeReportOnBothVerdicts pins what a job hands back.
//
// The report travels whole because the engine already decided what a run may
// claim; re-projecting it here would be a second opinion about the same verdict.
// A failing verdict returns the engine's error UNCHANGED, which is what lets its
// classification select the process exit code.
func TestFinishCarriesTheWholeReportOnBothVerdicts(t *testing.T) {
	report := sdd.SpecValidationReport{Valid: true, Summary: sdd.SpecValidationSummary{Specs: 3}}

	status, data, err := finish(report, nil)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK", status)
	}
	if data["valid"] != true {
		t.Errorf("valid = %#v, want the report's own value", data["valid"])
	}
	// Whole, not a projection: every member the report marshals to is present.
	var marshaled map[string]any
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatalf("marshal report: %v", marshalErr)
	}
	if err := json.Unmarshal(encoded, &marshaled); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	for member := range marshaled {
		if _, present := data[member]; !present {
			t.Errorf("the payload dropped %q", member)
		}
	}

	verdict := protocolcli.Classify(errors.New("specs validate failed"), protocolcli.ErrInvalidConfig)
	status, data, err = finish(report, verdict)
	if !errors.Is(err, verdict) {
		t.Errorf("error = %v, want the engine's verdict unchanged", err)
	}
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want the verdict's own classification preserved",
			protocolcli.ExitCodeForError(err))
	}
	if status != "" {
		t.Errorf("status = %q, want empty: the SDK reports FAILED from the error", status)
	}
	if data == nil {
		t.Error("a failing verdict dropped its report; the CLI's failure envelope carries it")
	}
}

// TestReportDataRefusesAReportItCannotEncode covers the two ways encoding can
// fail. Neither is reachable from today's three report types, and that is the
// point: a future report member that cannot be encoded must surface as a
// classified job failure rather than as a nil payload the CLI renders as an
// empty result.
func TestReportDataRefusesAReportItCannotEncode(t *testing.T) {
	if _, err := reportData(map[string]any{"broken": make(chan int)}); err == nil ||
		!strings.Contains(err.Error(), "encode validation report") ||
		protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Errorf("an unencodable report produced %v, want a classified encode failure", err)
	}
	// A report that encodes to something other than a JSON object: the payload
	// contract is a member map, and silently returning nil would publish an
	// empty result beside a passing status.
	if _, err := reportData([]int{1, 2, 3}); err == nil ||
		!strings.Contains(err.Error(), "decode validation report") ||
		protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Errorf("a non-object report produced %v, want a classified decode failure", err)
	}
	if _, _, err := finish(map[string]any{"broken": make(chan int)}, nil); err == nil {
		t.Error("finish accepted a report it cannot encode")
	}
}

// hasDiagnosticEvent reports whether the stream carried a diagnostic with code.
func hasDiagnosticEvent(events []runtimeproto.Event, code string) bool {
	return findDiagnosticEvent(events, code) != nil
}

// findDiagnosticEvent returns the first diagnostic carrying code, or nil.
func findDiagnosticEvent(events []runtimeproto.Event, code string) *runtimeproto.Event {
	for i, event := range events {
		if event.Code == code {
			return &events[i]
		}
	}
	return nil
}

// --- specs-ratchet-validate --------------------------------------------------

// ratchetFixture writes one spec-owning project whose specs verification mode
// is authored in its putnami.json, with one criterion-backed requirement — the
// smallest workspace whose enforce floor is non-empty.
func ratchetFixture(t *testing.T, mode string) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"gate","includes":["app"]}`)
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), ratchetProjectConfig(mode))
	writeFixture(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 2,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoicing",
    "type": "feature",
    "name": "Invoicing",
    "outcome": "Customers receive invoices",
    "owner": "billing",
    "target": "coded",
    "requirements": [
      {"id": "issue", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["issue-check"]}}
    ]
  }]
}`)
	writeFixture(t, filepath.Join(root, "app", "specs", "invoicing.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["Customers receive invoices"],
  "requirements": [{"id": "issue", "text": "An invoice is issued."}]
}`)
	return root
}

func ratchetProjectConfig(mode string) string {
	return `{"name":"billing-app","type":"application","options":{"sdd":{"verification":{"specs":"` + mode + `"}}}}`
}

// ratchetMembers is the membership the wire carries, WITH each project's parsed
// config: the effective mode is authored in the member's putnami.json, and the
// job reads it from the ref rather than from disk.
func ratchetMembers(t *testing.T, root, mode string) []pctx.ProjectRef {
	t.Helper()
	return []pctx.ProjectRef{{
		ID: "/app", Name: "billing-app", Path: "app", FullPath: filepath.Join(root, "app"),
		Config: json.RawMessage(ratchetProjectConfig(mode)),
	}}
}

// TestSpecsRatchetValidateAdoptsWithoutABaseline: an enforced project with no
// committed specs.baseline.json passes — initial adoption — and the wire
// carries the one warning nudge naming `specs baseline --update`.
func TestSpecsRatchetValidateAdoptsWithoutABaseline(t *testing.T) {
	root := ratchetFixture(t, "enforce")
	outcome := runJob(t, runSpecsRatchetValidate, workspaceJobContext(root, ratchetMembers(t, root, "enforce")))

	if outcome.err != nil {
		t.Fatalf("adoption failed: %v", outcome.err)
	}
	if outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Errorf("status = %q exit = %d, want OK and 0", outcome.status, outcome.exitCode())
	}
	if outcome.data["baselinePresent"] != false {
		t.Errorf("baselinePresent = %#v, want false", outcome.data["baselinePresent"])
	}
	if got := summaryOf(t, outcome.data, "grownProjects"); got != 1 {
		t.Errorf("summary.grownProjects = %v, want 1", got)
	}
	nudge := findDiagnosticEvent(outcome.diagnostics(), string(featureproto.WarningCodeRatchetGrowth))
	if nudge == nil || nudge.Severity == nil || *nudge.Severity != runtimeproto.SeverityWarning {
		t.Fatalf("diagnostic events = %+v, want the adoption warning on the wire", outcome.diagnostics())
	}
	if want := "specs ratchet: 1 enforced project(s), baseline 0"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
	assertReportDiagnosticsReachTheWire(t, outcome)
}

// TestSpecsRatchetValidateFailsOnARecordedRegression is the seeded violation:
// the committed baseline records the project as enforced, the worktree says
// report, and the job fails naming the baseline as the reviewed policy change.
func TestSpecsRatchetValidateFailsOnARecordedRegression(t *testing.T) {
	root := ratchetFixture(t, "report")
	writeFixture(t, filepath.Join(root, "app", featureproto.SpecsBaselineFilename),
		`{"$schema":"https://putnami.dev/schemas/putnami-specs-baseline.json","protocolVersion":2,"executableRequirements":["billing/invoicing#issue"]}`)
	outcome := runJob(t, runSpecsRatchetValidate, workspaceJobContext(root, ratchetMembers(t, root, "report")))

	if outcome.err == nil {
		t.Fatal("a recorded project regressed out of enforce and the job passed")
	}
	if outcome.exitCode() != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", outcome.exitCode(), protocolcli.ExitUsage)
	}
	if got := summaryOf(t, outcome.data, "regressedProjects"); got != 1 {
		t.Errorf("summary.regressedProjects = %v, want 1", got)
	}
	regression := findDiagnosticEvent(outcome.diagnostics(), string(featureproto.ErrorCodeRatchetRegression))
	if regression == nil || regression.Severity == nil || *regression.Severity != runtimeproto.SeverityError {
		t.Fatalf("diagnostic events = %+v, want the regression error on the wire", outcome.diagnostics())
	}
	if !strings.Contains(regression.Message, featureproto.SpecsBaselineFilename) {
		t.Errorf("regression message = %q, want it to name the committed baseline", regression.Message)
	}
	assertReportDiagnosticsReachTheWire(t, outcome)
}

// TestSpecsRatchetValidateHoldsAMatchingBaseline: the committed floor equals
// the derived one — silence, and a cache-shaped verdict with no warnings.
func TestSpecsRatchetValidateHoldsAMatchingBaseline(t *testing.T) {
	root := ratchetFixture(t, "enforce")
	writeFixture(t, filepath.Join(root, "app", featureproto.SpecsBaselineFilename),
		`{"$schema":"https://putnami.dev/schemas/putnami-specs-baseline.json","protocolVersion":2,"executableRequirements":["billing/invoicing#issue"]}`)
	outcome := runJob(t, runSpecsRatchetValidate, workspaceJobContext(root, ratchetMembers(t, root, "enforce")))

	if outcome.err != nil {
		t.Fatalf("a matching baseline failed: %v", outcome.err)
	}
	if outcome.data["baselinePresent"] != true {
		t.Errorf("baselinePresent = %#v, want true", outcome.data["baselinePresent"])
	}
	if found := findDiagnosticEvent(outcome.diagnostics(), string(featureproto.ErrorCodeRatchetRegression)); found != nil {
		t.Errorf("a matching floor put a ratchet diagnostic on the wire: %+v", found)
	}
	if found := findDiagnosticEvent(outcome.diagnostics(), string(featureproto.WarningCodeRatchetGrowth)); found != nil {
		t.Errorf("a matching floor put a growth nudge on the wire: %+v", found)
	}
}

// TestSpecsRatchetValidateRefusesAContextWithoutWorkspaceMembership mirrors the
// architecture job's refusal: the floor is a workspace-wide claim, and a
// context without the membership cannot prove it.
func TestSpecsRatchetValidateRefusesAContextWithoutWorkspaceMembership(t *testing.T) {
	root := ratchetFixture(t, "enforce")
	ctx := workspaceJobContext(root, ratchetMembers(t, root, "enforce"))
	ctx.WorkspaceProjects = nil
	if _, _, err := runSpecsRatchetValidate(ctx, jsonl.New(), nil); err == nil {
		t.Fatal("specs-ratchet-validate accepted a context without the workspace membership")
	}
}

// TestCodeownersSyncWritesTheDeclaredFileThroughTheWire runs the job the way
// the CLI does: the catch-all comes from the workspace option block the job
// context carries, the directory rule from the project's own putnami.json.
func TestCodeownersSyncWritesTheDeclaredFileThroughTheWire(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","options":{"sdd":{"owners":["@acme/billing"]}}}`)
	ctx := workspaceJobContext(root, []pctx.ProjectRef{{
		ID: "/app", Name: "billing-app", Path: "app", FullPath: filepath.Join(root, "app"),
	}})
	ctx.Workspace.Options = map[string]json.RawMessage{"sdd": json.RawMessage(`{"owners":["@lead"]}`)}

	outcome := runJob(t, runCodeownersSync, ctx)
	if outcome.err != nil || outcome.status != "OK" {
		t.Fatalf("status = %q err = %v, want OK", outcome.status, outcome.err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatalf("read CODEOWNERS: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n* @lead\n\n/app/ @acme/billing\n") {
		t.Errorf("CODEOWNERS =\n%s\nwant the catch-all then the /app/ rule", data)
	}
	if want := "codeowners: 2 rule(s), .github/CODEOWNERS rewritten"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
	if outcome.data["written"] != true {
		t.Errorf("written = %#v, want true", outcome.data["written"])
	}

	again := runJob(t, runCodeownersSync, ctx)
	if want := "codeowners: 2 rule(s), .github/CODEOWNERS up to date"; !strings.Contains(again.summaryLine(), want) {
		t.Errorf("second summary line = %q, want one containing %q", again.summaryLine(), want)
	}
}

// TestCodeownersSyncFailsOnAnInvalidDeclarationAndNamesTheFile keeps the
// failure on the wire, anchored on the file a reader has to open.
func TestCodeownersSyncFailsOnAnInvalidDeclarationAndNamesTheFile(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","options":{"sdd":{"owners":["billing"]}}}`)
	ctx := workspaceJobContext(root, []pctx.ProjectRef{{
		ID: "/app", Name: "billing-app", Path: "app", FullPath: filepath.Join(root, "app"),
	}})
	ctx.Workspace.Options = map[string]json.RawMessage{"sdd": json.RawMessage(`{"owners":["@lead"]}`)}

	outcome := runJob(t, runCodeownersSync, ctx)
	if outcome.err == nil || outcome.exitCode() == protocolcli.ExitSuccess {
		t.Fatalf("an invalid owner passed: status = %q", outcome.status)
	}
	if want := "codeowners: 1 finding(s), .github/CODEOWNERS not written"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary = %q, want %q", outcome.summaryLine(), want)
	}
	event := findDiagnosticEvent(outcome.diagnostics(), sdd.ErrorCodeInvalidOwners)
	if event == nil || event.Location == nil || !strings.HasPrefix(event.Location.File, "app/putnami.json") {
		t.Fatalf("diagnostic events = %+v, want %s naming app/putnami.json", outcome.diagnostics(), sdd.ErrorCodeInvalidOwners)
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "CODEOWNERS")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CODEOWNERS written after a failed run (stat err = %v)", err)
	}
}
