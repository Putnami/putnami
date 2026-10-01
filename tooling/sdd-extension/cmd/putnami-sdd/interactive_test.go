package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The command layer's own tests: arity, refusals, output-mode resolution, and
// the human rendering.
//
// Six of them arrived with the code they exercise. Moving the engines
// deliberately left the rendering in `tooling/cli/internal/commands/sdd`, so its
// tests stayed too; this commit is where they follow. Each says where it came
// from. What they lost in the move is the CLI's dispatcher — the extension's
// equivalent is dispatchInteractive — and what they gained is that they now
// exercise the surface a user actually reaches after the extraction.
//
// The byte-for-byte comparison against the built-in is NOT here: it needs both
// implementations, and only `tooling/cli` has both until the old copy is removed. It
// lives in tooling/cli/internal/cli/sdd_extraction_parity_test.go.

// --- the harness ------------------------------------------------------------

// runSubcommand drives the real dispatch for one subcommand and returns what a
// user would see. The context is a value rather than a file so a test states
// the wire it is arguing about.
func runSubcommand(
	t *testing.T,
	ctx *pctx.Context,
	group, name string,
	args ...string,
) (stdout, stderr string, code int) {
	t.Helper()
	sub, found := interactiveSubcommands()[group+" "+name]
	if !found {
		t.Fatalf("no subcommand %q %q", group, name)
	}
	var out, errOut bytes.Buffer
	if sub.arity != nil {
		if err := sub.arity(args); err != nil {
			run := &interactiveRun{ctx: ctx, stdout: &out, stderr: &errOut, mode: resolvedOutputMode(ctx)}
			return out.String(), errOut.String(), run.emit(sub, outcome{err: err})
		}
	}
	code = dispatchInteractive(sub, ctx, args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// wireContext is what the CLI writes for an interactive SDD invocation: the
// workspace root, the complete membership, the resolved selection, and the
// params it forwards.
func wireContext(root string, projects []*wsview.Project, selection *pctx.Selection, params map[string]any) *pctx.Context {
	refs := make([]pctx.ProjectRef, 0, len(projects))
	for _, project := range projects {
		refs = append(refs, pctx.ProjectRef{
			ID: project.ID, Name: project.Name, SourceName: project.SourceName,
			Path: project.Path, Dependencies: project.Dependencies,
		})
	}
	encoded := pctx.Params{}
	for name, value := range params {
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		encoded[name] = raw
	}
	return &pctx.Context{
		ProtocolVersion:   2,
		WorkspaceRoot:     root,
		WorkspaceProjects: refs,
		Selection:         selection,
		Params:            encoded,
	}
}

// jsonParams is the param bag for a `--output=json` invocation.
func jsonParams() map[string]any { return map[string]any{"output": "json"} }

// decodeEnvelope decodes one result envelope and its payload.
func decodeEnvelope[T any](t *testing.T, stdout string) (protocolcli.ResultV2, T) {
	t.Helper()
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", stdout, err)
	}
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatalf("re-encode payload: %v", err)
	}
	var payload T
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return envelope, payload
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// featureWorkspace is core's manyProjectWorkspace, minus what the extension has
// no loader for: the membership is stated instead of discovered.
func featureWorkspace(t *testing.T) (string, []*wsview.Project) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"scoped","includes":["billing","shipping","unrelated"]}`)
	for _, project := range []string{"billing", "shipping", "unrelated"} {
		writeFixture(t, filepath.Join(root, project, "putnami.json"),
			`{"name":"@acme/`+project+`","type":"application"}`)
	}
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	writeFixture(t, filepath.Join(root, "unrelated", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "warehouse",
  "features": [{
    "id": "warehouse/stock",
    "type": "feature",
    "name": "Stock levels",
    "outcome": "Operators see stock levels",
    "owner": "warehouse",
    "target": "modeled"
  }]
}`)
	projects := make([]*wsview.Project, 0, 3)
	for _, name := range []string{"billing", "shipping", "unrelated"} {
		projects = append(projects, &wsview.Project{
			ID: "/" + name, Name: "@acme/" + name, SourceName: "@acme/" + name,
			Type: "application", Path: name,
			Config: &wsproto.ProjectConfig{Name: "@acme/" + name, Type: "application"},
		})
	}
	return root, projects
}

// billingOnly is what the CLI resolves for `--projects @acme/billing` over the
// fixture above.
func billingOnly() *pctx.Selection {
	return &pctx.Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"}}
}

func wholeWorkspace() *pctx.Selection {
	return &pctx.Selection{
		Mode:       pctx.SelectionModeAll,
		ProjectIDs: []string{"/billing", "/shipping", "/unrelated"},
	}
}

// specWorkspace is core's specs fixture, membership stated.
func specWorkspace(t *testing.T) (string, []*wsview.Project) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"specs-test","includes":["app","docs"]}`)
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","type":"application"}`)
	writeFixture(t, filepath.Join(root, "docs", "putnami.json"), `{"name":"docs-site","type":"application"}`)
	writeFixture(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice-export",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers can export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }, {
    "id": "billing/refunds",
    "type": "feature",
    "name": "Refunds",
    "outcome": "Customers receive refunds",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	return root, []*wsview.Project{
		{ID: "/app", Name: "billing-app", SourceName: "billing-app", Type: "application", Path: "app"},
		{ID: "/docs", Name: "docs-site", SourceName: "docs-site", Type: "application", Path: "docs"},
	}
}

func specSelection() *pctx.Selection {
	return &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/app", "/docs"}}
}

// --- moved from tooling/cli/internal/commands/sdd/scope_test.go -------------

// TestScopedFeatureHumanOutputExplainsTheScope pins the compact scope summary a
// human answer opens with. Moved verbatim in intent; what changed is that the
// selection arrives resolved on the wire instead of being parsed here.
func TestScopedFeatureHumanOutputExplainsTheScope(t *testing.T) {
	root, projects := featureWorkspace(t)

	scoped, _, code := runSubcommand(t, wireContext(root, projects, billingOnly(), nil), "features", "validate")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("scoped validation exited %d:\n%s", code, scoped)
	}
	for _, want := range []string{"1 project(s) selected", "1 feature(s) in scope", "external record(s) followed"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("human output missing %q:\n%s", want, scoped)
		}
	}

	unscoped, _, code := runSubcommand(t, wireContext(root, projects, wholeWorkspace(), nil), "features", "list")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("unscoped list exited %d:\n%s", code, unscoped)
	}
	if !strings.Contains(unscoped, "Scope: the whole workspace") {
		t.Errorf("unscoped output does not state its own scope:\n%s", unscoped)
	}
}

// TestFeaturesListRendersBothSurfaces pins the subcommand in both modes: the
// structured one under its exact command name in a v2 envelope, the human one
// stating the same facts.
func TestFeaturesListRendersBothSurfaces(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "one-engine-two-surfaces", "features-list-renders-both-surfaces-from-one-value")
	root, projects := featureWorkspace(t)

	structured, _, code := runSubcommand(t,
		wireContext(root, projects, billingOnly(), jsonParams()), "features", "list")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("features list exited %d:\n%s", code, structured)
	}
	envelope, report := decodeEnvelope[sdd.FeatureCatalogReport](t, structured)
	if envelope.Command != "features list" || envelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("envelope = %+v", envelope)
	}
	if len(report.Features) != 1 || report.Features[0].ID != "billing/invoice" {
		t.Fatalf("catalog = %+v", report.Features)
	}

	human, _, code := runSubcommand(t, wireContext(root, projects, billingOnly(), nil), "features", "list")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("human features list exited %d:\n%s", code, human)
	}
	for _, want := range []string{"Features (1)", "billing/invoice", "Invoice export"} {
		if !strings.Contains(human, want) {
			t.Errorf("human catalog missing %q:\n%s", want, human)
		}
	}
}

// --- moved from tooling/cli/internal/commands/sdd/specs_test.go -------------

// TestSpecCommandsRenderHumanAndStructuredSurfaces pins that all four `specs`
// subcommands answer in both modes, that the structured mode uses the
// protocol-v2 envelope under its exact command name, and that the human mode
// states the same facts.
func TestSpecCommandsRenderHumanAndStructuredSurfaces(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "one-engine-two-surfaces", "spec-commands-render-both-surfaces-from-one-value")
	root, projects := specWorkspace(t)
	writeFixture(t, filepath.Join(root, "app", "specs", "renamed.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": ["Customers export an issued invoice"],
  "nonGoals": ["Exporting a draft invoice"],
  "requirements": [{"id": "format", "text": "An export states its format in the response content type."}],
  "decisions": ["app/doc/adr/0001-invoice-export-format.md"]
}`)
	writeFixture(t, filepath.Join(root, "app", "doc", "adr", "0001-invoice-export-format.md"), "# Format")

	machine := wireContext(root, projects, specSelection(), jsonParams())
	human := wireContext(root, projects, specSelection(), nil)

	listOut, _, code := runSubcommand(t, machine, "specs", "list")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("specs list exited %d:\n%s", code, listOut)
	}
	listEnvelope, listReport := decodeEnvelope[sdd.SpecCatalogReport](t, listOut)
	if listEnvelope.Command != "specs list" || listEnvelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("list envelope = %+v", listEnvelope)
	}
	if listReport.Counts.Specs != 1 || listReport.Counts.AuthoredFeatures != 2 || listReport.Counts.FeaturesWithoutSpec != 1 {
		t.Fatalf("list counts = %+v", listReport.Counts)
	}
	summary := listReport.Specs[0]
	if summary.Feature != "billing/invoice-export" || summary.Path != "app/specs/renamed.json" || summary.Project != "billing-app" {
		t.Fatalf("list summary = %+v, want the exact document provenance", summary)
	}
	if !summary.Valid || summary.OutcomeCount != 1 || summary.NonGoals != 1 || summary.Requirements != 1 || summary.Decisions != 1 {
		t.Fatalf("list summary counts = %+v", summary)
	}

	listHuman, _, _ := runSubcommand(t, human, "specs", "list")
	for _, want := range []string{
		"Specs (1)", "billing/invoice-export",
		"app/specs/renamed.json (billing-app)", "Customers export an issued invoice",
	} {
		if !strings.Contains(listHuman, want) {
			t.Errorf("human list missing %q:\n%s", want, listHuman)
		}
	}

	inspectOut, _, code := runSubcommand(t, machine, "specs", "inspect", "billing/invoice-export")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("specs inspect exited %d:\n%s", code, inspectOut)
	}
	inspectEnvelope, inspectReport := decodeEnvelope[sdd.SpecContextReport](t, inspectOut)
	if inspectEnvelope.Command != "specs inspect" || inspectEnvelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("inspect envelope = %+v", inspectEnvelope)
	}
	if inspectReport.Spec == nil || inspectReport.Source.Path != "app/specs/renamed.json" || inspectReport.Source.Project != "billing-app" {
		t.Fatalf("inspect data = %+v", inspectReport)
	}
	if len(inspectReport.Declarations) != 1 || inspectReport.Declarations[0].Source != "app/"+featureproto.ManifestFilename {
		t.Fatalf("inspect declarations = %+v, want the durable manifest declaration", inspectReport.Declarations)
	}
	if len(inspectReport.Decisions) != 1 || !inspectReport.Decisions[0].Exists {
		t.Fatalf("inspect decisions = %+v, want the linked record resolved", inspectReport.Decisions)
	}

	inspectHuman, _, _ := runSubcommand(t, human, "specs", "inspect", "billing/invoice-export")
	for _, want := range []string{
		"Feature billing/invoice-export", "Declared: Invoice export",
		"[format]", "[present] app/doc/adr/0001-invoice-export-format.md",
	} {
		if !strings.Contains(inspectHuman, want) {
			t.Errorf("human inspect missing %q:\n%s", want, inspectHuman)
		}
	}

	validateOut, _, code := runSubcommand(t, machine, "specs", "validate")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("specs validate exited %d:\n%s", code, validateOut)
	}
	for _, want := range []string{`"publishableProjects"`, `"projectsWithoutSupport"`, `"projectsWithoutFeature"`} {
		if !strings.Contains(validateOut, want) {
			t.Errorf("structured validation output is missing %s:\n%s", want, validateOut)
		}
	}
	for _, retired := range []string{`"publishablePackages"`, `"packagesWithoutSupport"`, `"packagesWithoutFeature"`} {
		if strings.Contains(validateOut, retired) {
			t.Errorf("structured validation output still carries retired field %s:\n%s", retired, validateOut)
		}
	}
	validateEnvelope, validateReport := decodeEnvelope[sdd.SpecValidationReport](t, validateOut)
	if validateEnvelope.Command != "specs validate" || validateEnvelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("validate envelope = %+v", validateEnvelope)
	}
	if !validateReport.Valid || validateReport.Counts.Errors != 0 {
		t.Fatalf("validate data = %+v", validateReport)
	}

	validateHuman, _, _ := runSubcommand(t, human, "specs", "validate")
	for _, want := range []string{"Spec validation passed", "1 spec(s)", "publishable project(s)", "Completeness:"} {
		if !strings.Contains(validateHuman, want) {
			t.Errorf("human validate missing %q:\n%s", want, validateHuman)
		}
	}

	dryRun := wireContext(root, projects, specSelection(), map[string]any{"output": "json", "dry-run": true})
	initOut, _, code := runSubcommand(t, dryRun, "specs", "init", "billing/refunds")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("specs init --dry-run exited %d:\n%s", code, initOut)
	}
	initEnvelope, initReport := decodeEnvelope[sdd.SpecInitReport](t, initOut)
	if initEnvelope.Command != "specs init" || initEnvelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("init envelope = %+v", initEnvelope)
	}
	if initReport.Created || !initReport.DryRun || initReport.Path != "app/specs/refunds.json" {
		t.Fatalf("init data = %+v", initReport)
	}

	dryRunHuman := wireContext(root, projects, specSelection(), map[string]any{"dry-run": true})
	initHuman, _, _ := runSubcommand(t, dryRunHuman, "specs", "init", "billing/refunds")
	for _, want := range []string{"Would create app/specs/refunds.json", `"feature": "billing/refunds"`} {
		if !strings.Contains(initHuman, want) {
			t.Errorf("human init missing %q:\n%s", want, initHuman)
		}
	}
}

// TestRenderedPayloadIsTheBuilderResult is core's TestSpecCLIAndMCPShareOneResult,
// with the half that has not landed yet removed.
//
// Core argued that the CLI and the MCP tools answer with the same value because
// both call one builder. The MCP tools arrive in a later commit, so what is provable
// here is the other half of the same claim: the payload the command WRITES is
// the value the builder RETURNED, unchanged. Once the tools land, they call the
// same builder and the original assertion is restored whole.
func TestRenderedPayloadIsTheBuilderResult(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "one-engine-two-surfaces", "the-rendered-payload-is-the-builder-result")
	root, projects := specWorkspace(t)
	writeFixture(t, filepath.Join(root, "app", "specs", "invoice.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": ["An intended outcome"],
  "requirements": []
}`)
	view := wsview.NewWorkspace(root, &wsproto.Config{Name: "specs-test"}, projects)

	catalog, err := sdd.BuildSpecCatalogResult(view, *specSelection())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	listOut, _, _ := runSubcommand(t,
		wireContext(root, projects, specSelection(), jsonParams()), "specs", "list")
	_, rendered := decodeEnvelope[sdd.SpecCatalogReport](t, listOut)
	if rendered.Counts != catalog.Counts || len(rendered.Specs) != len(catalog.Specs) {
		t.Fatalf("rendered catalog %+v differs from the builder's %+v", rendered, catalog)
	}

	context, err := sdd.BuildSpecContextResult(view, "billing/invoice-export")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	inspectOut, _, _ := runSubcommand(t,
		wireContext(root, projects, specSelection(), jsonParams()), "specs", "inspect", "billing/invoice-export")
	_, renderedContext := decodeEnvelope[sdd.SpecContextReport](t, inspectOut)
	if renderedContext.Source != context.Source || renderedContext.Feature != context.Feature {
		t.Fatalf("rendered context %+v differs from the builder's %+v", renderedContext, context)
	}
	if len(renderedContext.Diagnostics) != len(context.Diagnostics) {
		t.Fatalf("rendered and builder diagnostics differ: %+v vs %+v", renderedContext.Diagnostics, context.Diagnostics)
	}
}

// --- moved from tooling/cli/internal/commands/sdd/features_test.go ----------

// TestTypedClientOperationDetailsStayBounded pins the two detail helpers the
// design renderer composes its node lines from.
func TestTypedClientOperationDetailsStayBounded(t *testing.T) {
	if got := typedClientOperationDetails(featureproto.DesignNode{}); got != "" {
		t.Errorf("client without a declared operation list = %q, want empty", got)
	}
	node := featureproto.DesignNode{Properties: map[string]string{
		"operations": "DELETE /f, GET /a, GET /b, POST /c, POST /d, PUT /e",
	}}
	if got, want := typedClientOperationDetails(node), "DELETE /f, GET /a, GET /b, POST /c, … 2 more"; got != want {
		t.Errorf("operation details = %q, want %q", got, want)
	}
	if got := appendDesignDetail("", "producer=cloud/identity"); got != "producer=cloud/identity" {
		t.Errorf("first detail = %q, want no separator", got)
	}
	if got := appendDesignDetail("language=go", "producer=cloud/identity"); got != "language=go · producer=cloud/identity" {
		t.Errorf("appended detail = %q", got)
	}
}

// TestFeaturesInspectOmitsHollowReportOnHardFailure guards the human renderer:
// a failure that produces no report at all must surface the error alone, not an
// empty "Feature inspection failed" block that buries it.
//
// Core made workspace.Load fail with colliding project names. An extension has
// no loader and cannot fail that way, so the hard failure is asserted at the
// layer that owns the guard: a builder that returned no report — no resolved
// revision — must produce no human block, whatever made it fail. The guard is
// the same line either way, `report.Revision.Kind == ""`.
func TestFeaturesInspectOmitsHollowReportOnHardFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	sub := interactiveSubcommands()["features inspect"]
	hard := &interactiveRun{args: []string{"items/manage"}, stdout: &out, stderr: &errOut}
	code := hard.emit(sub, runFeaturesInspect(hard))
	if code == protocolcli.ExitSuccess {
		t.Fatal("features inspect must fail when there is no workspace to inspect")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("hard failure printed a hollow report:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "putnami: ") {
		t.Errorf("hard failure printed no message on stderr: %q", errOut.String())
	}

	// A failure the builder CAN describe still renders its report: the guard
	// suppresses the empty case only, it does not swallow real diagnostics.
	root, projects := featureWorkspace(t)
	described, _, code := runSubcommand(t,
		wireContext(root, projects, wholeWorkspace(), nil), "features", "inspect", "items/manage")
	if code == protocolcli.ExitSuccess {
		t.Fatal("unknown feature must still fail")
	}
	if !strings.Contains(described, "Feature inspection failed") {
		t.Errorf("describable failure lost its report:\n%s", described)
	}
}

// TestFeatureInspectionHumanRendersExactTechnicalProvenance pins the deepest
// rendering in the four groups: one feature, one stale evidence record, and the
// exact provenance of the contribution it is bound to.
func TestFeatureInspectionHumanRendersExactTechnicalProvenance(t *testing.T) {
	binding := "source-v1:sha256:" + strings.Repeat("a", 64)
	reference := capabilityproto.ContributionReference{
		OwnerProject: "dependency", Kind: capabilityproto.ContributionKindConfig, Key: "service",
	}
	contribution := featureengine.ContributionAssessment{
		Identity:             reference,
		ProtocolVersion:      capabilityproto.ProtocolVersionV2,
		Referenceable:        true,
		Containers:           []string{"app/schema/capabilities.json", "dependency/schema/capabilities.json"},
		CurrentSourceBinding: binding,
		SourceState:          "current",
		Provenance: featureengine.ContributionProvenance{
			Project: "dependency", Package: "go.putnami.dev/dependency", Version: "v1.2.3",
			SourceKind: capabilityproto.SourceKindFramework,
			Declaration: &capabilityproto.DeclarationLocation{
				Root: capabilityproto.LocationRootProject, Path: "config/service.go", Symbol: "RegisterService",
			},
			Artifacts: []capabilityproto.ArtifactLocation{{
				Root: capabilityproto.LocationRootProject, Path: "schema/config.json",
				Digest: "sha256:" + strings.Repeat("b", 64),
			}},
		},
	}
	feature := featureengine.FeatureAssessment{
		ID: "billing/checkout", Type: featureproto.FeatureTypeFeature, Name: "Checkout",
		Outcome: "Customers pay", Owner: "payments",
		Source: "app/putnami.features.json", Target: featureproto.MaturityCoded, Current: featureproto.MaturityModeled,
		Requirements: []featureengine.RequirementAssessment{{
			ID: "implementation", Stage: featureproto.MaturityCoded, Claimed: true, State: featureengine.VerificationStale,
			EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability},
			Evidence: []featureengine.EvidenceAssessment{{
				ID:       "billing/checkout-implementation",
				Document: "app/schema/feature-evidence/build.json",
				Outcome:  featureproto.EvidenceOutcomeSupports,
				Issuer:   featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "go-build"},
				Source: featureproto.SourceSelector{
					Root: featureproto.LocationRootProject, OwnerProject: "dependency", Binding: binding,
				},
				Subject: featureproto.EvidenceSubject{
					Kind: featureproto.EvidenceKindCapability, Contribution: &reference,
				},
				Provenance: featureproto.EvidenceProvenance{
					Root: featureproto.LocationRootProject, Path: "app.go", Symbol: "Build",
				},
				State:        featureengine.EvidenceStale,
				StaleReasons: []string{featureengine.StaleContributionBinding},
				Contribution: &contribution,
			}},
		}},
	}
	report := sdd.FeatureInspectionReport{
		Revision:            featureengine.Revision{Kind: featureengine.RevisionKindWorktree, Head: "git:" + strings.Repeat("c", 40)},
		SourceBindings:      []featureengine.SourceBinding{},
		Requested:           feature.ID,
		Feature:             &feature,
		RelatedUnclassified: []featureengine.ContributionAssessment{contribution},
		Diagnostics:         []diag.Diagnostic{},
	}

	var out bytes.Buffer
	printFeatureInspectionHuman(&out, report)
	for _, want := range []string{
		"Feature billing/checkout · Checkout",
		"Maturity: modeled → coded",
		"[stale] coded · implementation",
		"contribution-binding-mismatch",
		"owner=dependency · kind=config · key=service",
		"producer: project=dependency · sourceKind=framework · package=go.putnami.dev/dependency · version=v1.2.3",
		"declaration: project:config/service.go#RegisterService",
		"artifact: project:schema/config.json · sha256:",
		"transport containers: app/schema/capabilities.json, dependency/schema/capabilities.json",
		"Related unclassified contributions",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inspection output missing %q:\n%s", want, out.String())
		}
	}
}

// --- the command layer's own contract ---------------------------------------

// TestPositionalArityMessagesAreTheHandlersVerbatim pins the messages a user
// reads. They were the CLI handlers' (registry_commands.go:200-367) and they are
// user-visible contract: a paraphrase here breaks a script that matched on them
// and, worse, tells the user something different about the same mistake.
func TestPositionalArityMessagesAreTheHandlersVerbatim(t *testing.T) {
	cases := []struct {
		group, name string
		args        []string
		want        string
	}{
		{"features", "list", []string{"a", "b"}, "features list takes at most one [query]"},
		{"features", "validate", []string{"x"}, "features validate takes no positional arguments"},
		{"features", "snapshot", []string{"x"}, "features snapshot takes no positional arguments"},
		{"features", "inspect", nil, "features inspect requires exactly one <feature-id>"},
		{"features", "inspect", []string{"a", "b"}, "features inspect requires exactly one <feature-id>"},
		{"features", "diff", []string{"a"}, "features diff requires exactly <base-revision> and <head-revision>"},
		{"specs", "list", []string{"x"}, "specs list takes no positional arguments"},
		{"specs", "validate", []string{"x"}, "specs validate takes no positional arguments"},
		{"specs", "inspect", nil, "specs inspect requires exactly one <feature-id>"},
		{"specs", "init", nil, "specs init requires exactly one <feature-id>"},
		{"architecture", "validate", []string{"x"}, "architecture validate takes no positional arguments"},
		{"architecture", "snapshot", []string{"x"}, "architecture snapshot takes no positional arguments"},
		{"architecture", "inspect", nil, "architecture inspect requires exactly one <domain-id>"},
	}
	for _, testCase := range cases {
		sub := interactiveSubcommands()[testCase.group+" "+testCase.name]
		err := sub.arity(testCase.args)
		if err == nil {
			t.Errorf("%s %s accepted %v", testCase.group, testCase.name, testCase.args)
			continue
		}
		if err.Error() != testCase.want {
			t.Errorf("%s %s error = %q, want %q", testCase.group, testCase.name, err.Error(), testCase.want)
		}
		if !isUsageError(err) {
			t.Errorf("%s %s error is not a usage error; the CLI exited 2 on it", testCase.group, testCase.name)
		}
	}
}

// TestSelectionIsRefusedOnExactTargets is `rejectProjectSelection`, verbatim:
// the four subcommands the inventory marks refuse every selection flag,
// and the ten that honor them accept the same wire without complaint.
func TestSelectionIsRefusedOnExactTargets(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "refuse-rather-than-widen", "an-unresolved-interactive-selection-is-refused")
	scoped := billingOnly()
	all := map[string]any{"all": true}
	baseline := map[string]any{"baseline": "origin/main"}

	refusing := [][2]string{
		{"features", "inspect"}, {"features", "diff"},
		{"specs", "inspect"}, {"specs", "init"},
	}
	want := "%s takes an exact target, so project selection does not apply: " +
		"drop --projects/--impacted/--all/--baseline/--tag/--exclude-tag/--exclude " +
		"(use `putnami features list` or `putnami specs list` to narrow by project)"

	for _, pair := range refusing {
		path := pair[0] + " " + pair[1]
		for label, ctx := range map[string]*pctx.Context{
			"--projects": wireContext("/w", nil, scoped, nil),
			"--all":      wireContext("/w", nil, nil, all),
			"--baseline": wireContext("/w", nil, nil, baseline),
		} {
			run := &interactiveRun{ctx: ctx}
			err := run.rejectProjectSelection(path)
			if err == nil {
				t.Errorf("%s accepted %s", path, label)
				continue
			}
			if got := err.Error(); got != strings.Replace(want, "%s", path, 1) {
				t.Errorf("%s %s message = %q", path, label, got)
			}
			if !isUsageError(err) {
				t.Errorf("%s %s is not a usage error", path, label)
			}
		}
	}

	// The unscoped whole-workspace default is not a selection flag, so nothing
	// is refused for it — otherwise every plain `putnami features inspect X`
	// would fail.
	run := &interactiveRun{ctx: wireContext("/w", nil, wholeWorkspace(), nil)}
	if err := run.rejectProjectSelection("features inspect"); err != nil {
		t.Errorf("an unscoped run was refused: %v", err)
	}

	for _, pair := range [][2]string{
		{"features", "list"}, {"features", "validate"}, {"features", "snapshot"},
		{"specs", "list"}, {"specs", "validate"},
		{"architecture", "validate"}, {"architecture", "snapshot"}, {"architecture", "inspect"},
		{"contracts", "generate"}, {"contracts", "check"},
	} {
		if interactiveSubcommands()[pair[0]+" "+pair[1]].rejectsSelection {
			t.Errorf("%s %s refuses selection; the inventory says it honors it", pair[0], pair[1])
		}
	}
}

// TestRefusalReachesTheUserOnBothStreams runs one refusal through the whole
// dispatch, because the message being right is only half the contract: a
// machine reader must find it in the envelope on stdout, a human must find it on
// stderr, and the process must exit 2 either way.
func TestRefusalReachesTheUserOnBothStreams(t *testing.T) {
	root, projects := featureWorkspace(t)
	message := "features inspect takes an exact target"

	machine := wireContext(root, projects, billingOnly(), jsonParams())
	stdout, stderr, code := runSubcommand(t, machine, "features", "inspect", "billing/invoice")
	if code != protocolcli.ExitUsage {
		t.Errorf("structured refusal exited %d, want %d", code, protocolcli.ExitUsage)
	}
	if !strings.Contains(stdout, message) {
		t.Errorf("the refusal is not in the envelope:\n%s", stdout)
	}
	if !strings.Contains(stderr, "putnami: "+message) {
		t.Errorf("the refusal is not on stderr: %q", stderr)
	}

	human := wireContext(root, projects, billingOnly(), nil)
	stdout, stderr, code = runSubcommand(t, human, "features", "inspect", "billing/invoice")
	if code != protocolcli.ExitUsage {
		t.Errorf("human refusal exited %d, want %d", code, protocolcli.ExitUsage)
	}
	if stdout != "" {
		t.Errorf("a human-mode refusal wrote to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "putnami: "+message) {
		t.Errorf("the refusal is not on stderr: %q", stderr)
	}
}

// TestOutputModeComesFromTheParamNotAFlag pins the reserved-flag rule.
//
// `--output` is a reserved global a manifest may not declare and the CLI
// consumes before dispatch, so the only way the resolved mode reaches this
// binary is params["output"]. Reading a flag instead would leave every
// invocation in human mode while the caller asked for JSON.
func TestOutputModeComesFromTheParamNotAFlag(t *testing.T) {
	root, projects := featureWorkspace(t)

	for _, mode := range []string{"json", "jsonl"} {
		ctx := wireContext(root, projects, wholeWorkspace(), map[string]any{"output": mode})
		out, _, code := runSubcommand(t, ctx, "features", "list")
		if code != protocolcli.ExitSuccess {
			t.Fatalf("--output=%s exited %d:\n%s", mode, code, out)
		}
		var envelope protocolcli.ResultV2
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("--output=%s did not write one envelope: %v\n%s", mode, err, out)
		}
		indented := strings.Contains(out, "\n  ")
		if (mode == "json") != indented {
			t.Errorf("--output=%s framing is wrong: indented=%v", mode, indented)
		}
	}

	// No param at all is the human mode, and it must NOT write an envelope.
	human, _, _ := runSubcommand(t, wireContext(root, projects, wholeWorkspace(), nil), "features", "list")
	if strings.Contains(human, `"protocolVersion"`) {
		t.Errorf("an unset output mode wrote a machine envelope:\n%s", human)
	}
}

// TestSpecsInitHonorsTheForwardedDryRun pins the one non-read-only subcommand's
// safety valve, end to end: the global `--dry-run` reaches it as a param, and
// under it nothing is written.
func TestSpecsInitHonorsTheForwardedDryRun(t *testing.T) {
	root, projects := specWorkspace(t)
	target := filepath.Join(root, "app", "specs", "refunds.json")

	dry := wireContext(root, projects, specSelection(), map[string]any{"output": "json", "dry-run": true})
	out, _, code := runSubcommand(t, dry, "specs", "init", "billing/refunds")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("--dry-run wrote %s", target)
	}
	_, report := decodeEnvelope[sdd.SpecInitReport](t, out)
	if !report.DryRun || report.Created || report.Contents == "" {
		t.Fatalf("dry-run report = %+v, want the exact bytes it would write", report)
	}

	wet := wireContext(root, projects, specSelection(), jsonParams())
	if out, _, code := runSubcommand(t, wet, "specs", "init", "billing/refunds"); code != protocolcli.ExitSuccess {
		t.Fatalf("specs init exited %d:\n%s", code, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("without --dry-run the spec was not created: %v", err)
	}
}

// TestContractsTargetsOneProjectFromTheRawSelector pins the resolution order
// `resolveContractsProject` had: the subcommand's own --project first, then the
// RAW global selector, and a refusal for the two spellings that name a set
// rather than a project.
func TestContractsTargetsOneProjectFromTheRawSelector(t *testing.T) {
	root, projects := specWorkspace(t)
	view := wsview.NewWorkspace(root, nil, projects)

	resolve := func(args []string, params map[string]any) (*wsview.Project, error) {
		run := &interactiveRun{ctx: wireContext(root, projects, nil, params), ws: view, args: args}
		return run.contractsProject()
	}

	if project, err := resolve([]string{"--project", "billing-app"}, nil); err != nil || project.ID != "/app" {
		t.Fatalf("--project = %+v, %v", project, err)
	}
	if project, err := resolve(nil, map[string]any{"projects": "billing-app"}); err != nil || project.ID != "/app" {
		t.Fatalf("global selector = %+v, %v", project, err)
	}
	// The subcommand's own flag wins over the global.
	if project, err := resolve([]string{"--project", "docs-site"}, map[string]any{"projects": "billing-app"}); err != nil || project.ID != "/docs" {
		t.Fatalf("--project did not win over --projects: %+v, %v", project, err)
	}

	for label, params := range map[string]map[string]any{
		"nothing":     nil,
		"--all":       {"projects": "*"},
		"--impacted":  {"projects": "[impacted]"},
		"empty value": {"projects": "   "},
	} {
		_, err := resolve(nil, params)
		if err == nil || !isUsageError(err) {
			t.Errorf("%s: error = %v, want a usage error naming --project", label, err)
			continue
		}
		if !strings.Contains(err.Error(), "contracts requires a single target project") {
			t.Errorf("%s: message = %q", label, err.Error())
		}
	}

	if _, err := resolve([]string{"--project", "@acme/absent"}, nil); err == nil ||
		!strings.Contains(err.Error(), "project not found: @acme/absent") {
		t.Errorf("unknown project error = %v", err)
	}
	if _, err := resolve([]string{"--project"}, nil); err == nil ||
		!strings.Contains(err.Error(), "--project requires a value") {
		t.Errorf("valueless --project error = %v", err)
	}
}

// TestPositionalArgsIgnoreSubcommandFlags keeps the arity checks from counting a
// flag as a positional: `contracts check --project x` has no positional at all,
// and `features list --output=json` would otherwise look like a two-argument
// invocation.
func TestPositionalArgsIgnoreSubcommandFlags(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{}},
		{[]string{"query"}, []string{"query"}},
		{[]string{"--project", "billing-app"}, []string{}},
		{[]string{"--project=billing-app"}, []string{}},
		{[]string{"--dry-run", "billing/refunds"}, []string{"billing/refunds"}},
		{[]string{"HEAD~1", "HEAD"}, []string{"HEAD~1", "HEAD"}},
	}
	for _, testCase := range cases {
		got := positionalArgs(testCase.args)
		if len(got) != len(testCase.want) {
			t.Errorf("positionalArgs(%v) = %v, want %v", testCase.args, got, testCase.want)
			continue
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Errorf("positionalArgs(%v) = %v, want %v", testCase.args, got, testCase.want)
				break
			}
		}
	}
}

// TestSplitReservedArgsTakesTheContextAndLeavesTheRest pins the argument split
// the CLI's argv shape requires: the orchestrator appends --putnamiContext AFTER
// the user's tokens, and the reserved output flags never arrive at all but are
// stripped identically to the SDK's own parser if they somehow do.
func TestSplitReservedArgsTakesTheContextAndLeavesTheRest(t *testing.T) {
	contextFile, positionals, err := splitReservedArgs(
		[]string{"billing/invoice", "--project", "x", "--putnamiContext", "/tmp/ctx.json"})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if contextFile != "/tmp/ctx.json" {
		t.Errorf("context file = %q", contextFile)
	}
	if strings.Join(positionals, " ") != "billing/invoice --project x" {
		t.Errorf("positionals = %v", positionals)
	}

	if _, _, err := splitReservedArgs([]string{"billing/invoice"}); err == nil || !isUsageError(err) {
		t.Errorf("a missing --putnamiContext must be a usage error, got %v", err)
	}

	_, stripped, err := splitReservedArgs(
		[]string{"--output", "json", "--json", "--output=jsonl", "q", "--putnamiContext=/tmp/c.json"})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if strings.Join(stripped, " ") != "q" {
		t.Errorf("reserved output flags were not stripped: %v", stripped)
	}
}

// TestWireSelectionFallsBackToTheWholeWorkspace pins the degradation for an
// orchestrator older than D3: no `selection` block means the unscoped default,
// which is the answer the CLI resolves for an invocation with no selection
// flags — not an empty one.
func TestWireSelectionFallsBackToTheWholeWorkspace(t *testing.T) {
	root, projects := featureWorkspace(t)
	ctx := wireContext(root, projects, nil, nil)
	view, _ := wsview.FromContext(ctx)

	selection := wireSelection(ctx, view)
	if selection.Mode != pctx.SelectionModeAll || selection.Scoped {
		t.Fatalf("fallback selection = %+v, want the unscoped default", selection)
	}
	if len(selection.ProjectIDs) != len(projects) {
		t.Fatalf("fallback selection covers %d projects, want %d", len(selection.ProjectIDs), len(projects))
	}
}

// TestSuccessPayloadIsRoundTrippedForKeyOrder pins the one adaptation the
// extraction needed: a built-in structured command's payload is decoded into
// map[string]any and re-encoded by the CLI, so its keys come out alphabetical.
// A struct encodes in declaration order, and the difference is visible to every
// consumer that byte-compares.
func TestSuccessPayloadIsRoundTrippedForKeyOrder(t *testing.T) {
	type payload struct {
		Zebra string `json:"zebra"`
		Apple string `json:"apple"`
	}
	encoded, err := json.MarshalIndent(capturedPayload(payload{Zebra: "z", Apple: "a"}), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"apple"`) ||
		strings.Index(string(encoded), `"apple"`) > strings.Index(string(encoded), `"zebra"`) {
		t.Fatalf("payload keys were not sorted: %s", encoded)
	}
	if capturedPayload(nil) != nil {
		t.Error("a nil payload must stay nil, not become an empty object")
	}
}

func isUsageError(err error) bool {
	return protocolcli.ExitCodeForError(err) == protocolcli.ExitUsage
}
