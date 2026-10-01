package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The rendering the four groups do beyond a catalog row: the design projection,
// the two revision comparison, the architecture verdicts and the contract
// compiler's report.
//
// Each one runs through the real dispatch and asserts the LINE a user reads,
// because that is the whole subject of this layer. The payload behind it is the
// engines', and the engines have their own tests in internal/sdd.

// --- features inspect over a design graph -----------------------------------

// TestFeatureDesignRendersEveryNodeGroupItReaches pins the deepest human
// rendering in the four groups: one feature's design projection, its node
// groups, the derived generated-from detail, and its critical paths.
func TestFeatureDesignRendersEveryNodeGroupItReaches(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["app"]}`)
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","type":"application"}`)

	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "app",
		Nodes: []featureproto.DesignNode{
			{
				ID: "feature:items/manage", Kind: featureproto.DesignNodeFeature, Name: "Item management",
				Properties: map[string]string{"outcome": "Consumers list items", "owner": "catalog"},
			},
			{ID: "module:app", Kind: featureproto.DesignNodeModule, Name: "app"},
			{
				ID: "api.operation:GET:/items", Kind: featureproto.DesignNodeAPIOperation, Name: "GET /items",
				Properties: map[string]string{"method": "GET", "path": "/items", "operationId": "getItems"},
			},
			{ID: "service:catalog", Kind: featureproto.DesignNodeService, Name: "catalog"},
			{
				ID: "client.generated:ts:@example/items/ItemsClient", Kind: featureproto.DesignNodeClient, Name: "ItemsClient",
				Properties: map[string]string{"language": "ts", "package": "@example/items", "feature": "items/manage"},
			},
			{
				ID: "client.typed:go:catalog/Client", Kind: featureproto.DesignNodeTypedClient, Name: "CatalogClient",
				Properties: map[string]string{"language": "go", "producer": "catalog", "operations": "GET /items"},
			},
			{
				ID: "data.table:items", Kind: featureproto.DesignNodeDataTable, Name: "items",
				Properties: map[string]string{"columns": "id, name"},
			},
			{ID: "event.topic:items.changed", Kind: featureproto.DesignNodeEventTopic, Name: "items.changed"},
		},
		Edges: []featureproto.DesignEdge{
			{From: "feature:items/manage", To: "module:app", Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityExact},
			{From: "module:app", To: "api.operation:GET:/items", Kind: featureproto.DesignEdgeExposes, Authority: featureproto.DesignAuthorityExact},
			{From: "api.operation:GET:/items", To: "service:catalog", Kind: featureproto.DesignEdgeInjects, Authority: featureproto.DesignAuthorityExact},
			{From: "service:catalog", To: "data.table:items", Kind: featureproto.DesignEdgeWrites, Authority: featureproto.DesignAuthorityExact},
			{From: "service:catalog", To: "event.topic:items.changed", Kind: featureproto.DesignEdgePublishes, Authority: featureproto.DesignAuthorityExact},
			{
				From: "client.generated:ts:@example/items/ItemsClient", To: "api.operation:GET:/items",
				Kind: featureproto.DesignEdgeGeneratedFrom, Authority: featureproto.DesignAuthorityExact,
				Properties: map[string]string{"operationId": "getItems"},
			},
			{From: "client.typed:go:catalog/Client", To: "api.operation:GET:/items", Kind: featureproto.DesignEdgeCalls, Authority: featureproto.DesignAuthorityExact},
		},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "app", ".gen", featureproto.DesignGraphArtifact), string(encoded))

	projects := []*wsview.Project{{ID: "/app", Name: "app", SourceName: "app", Type: "application", Path: "app"}}
	selection := &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/app"}}

	human, _, code := runSubcommand(t,
		wireContext(root, projects, selection, nil), "features", "inspect", "items/manage")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("features inspect exited %d:\n%s", code, human)
	}
	for _, want := range []string{
		"Feature items/manage · Item management",
		"Design graph: ",
		"API:",
		"[exact] GET /items · operationId=getItems",
		"Dependencies:",
		"Data:",
		"Events:",
		"Generated clients:",
		"generatedFrom=getItems→GET /items",
		"Typed clients:",
		"producer=catalog",
		"calls=GET /items",
		"Critical paths:",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("design rendering missing %q:\n%s", want, human)
		}
	}

	structured, _, code := runSubcommand(t,
		wireContext(root, projects, selection, jsonParams()), "features", "inspect", "items/manage")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("structured features inspect exited %d:\n%s", code, structured)
	}
	envelope, report := decodeEnvelope[sdd.FeatureInspectionReport](t, structured)
	if envelope.Command != "features inspect" || report.Design == nil || len(report.Design.Implementations) != 1 {
		t.Fatalf("structured inspection = %+v", report.Design)
	}
}

// TestDesignPathLabelRendersBothEdgeDirections pins the arrow the critical-path
// line is drawn with: a reverse relation (a consumer reached from the producer)
// prints as an incoming arrow, and a forward one as an outgoing arrow. The two
// read as different facts and must not be spelled the same.
func TestDesignPathLabelRendersBothEdgeDirections(t *testing.T) {
	forward := sdd.FeatureDesignCriticalPath{
		Nodes:     []string{"feature:a", "module:b", "api:c"},
		Relations: []string{"implemented-by", "exposes"},
	}
	if got, want := designPathLabel(forward), "feature:a -implemented-by-> module:b -exposes-> api:c"; got != want {
		t.Errorf("forward path = %q, want %q", got, want)
	}
	reverse := sdd.FeatureDesignCriticalPath{
		Nodes:     []string{"feature:a", "client:b"},
		Relations: []string{"<generated-from"},
	}
	if got, want := designPathLabel(reverse), "feature:a <-generated-from- client:b"; got != want {
		t.Errorf("reverse path = %q, want %q", got, want)
	}
	if got := designPathLabel(sdd.FeatureDesignCriticalPath{}); got != "" {
		t.Errorf("empty path = %q, want empty", got)
	}
}

// --- features diff ----------------------------------------------------------

// TestFeaturesDiffRendersTheDeltaAndItsFailures covers both halves of the one
// subcommand that reads git history: a resolvable comparison, and a revision
// that names no commit.
func TestFeaturesDiffRendersTheDeltaAndItsFailures(t *testing.T) {
	root, projects := featureWorkspace(t)
	commitFixtureRepository(t, root)
	ctx := wireContext(root, projects, wholeWorkspace(), nil)

	human, _, code := runSubcommand(t, ctx, "features", "diff", "HEAD", "HEAD")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("features diff exited %d:\n%s", code, human)
	}
	for _, want := range []string{"Feature diff (", "Base: ", "Head: ", "0 added · 0 removed"} {
		if !strings.Contains(human, want) {
			t.Errorf("diff rendering missing %q:\n%s", want, human)
		}
	}

	broken, stderr, code := runSubcommand(t, ctx, "features", "diff", "refs/heads/absent", "HEAD")
	if code == protocolcli.ExitSuccess {
		t.Fatal("an unresolvable revision must fail")
	}
	if !strings.Contains(broken, "Comparison unavailable") || !strings.Contains(broken, "Base diagnostics") {
		t.Errorf("unresolvable diff lost its explanation:\n%s", broken)
	}
	if !strings.Contains(stderr, "putnami: ") {
		t.Errorf("unresolvable diff printed no message: %q", stderr)
	}

	structured, _, _ := runSubcommand(t,
		wireContext(root, projects, wholeWorkspace(), jsonParams()), "features", "diff", "HEAD", "HEAD")
	envelope, _ := decodeEnvelope[sdd.FeatureDiffReport](t, structured)
	if envelope.Command != "features diff" || envelope.Status != protocolcli.StatusSuccess {
		t.Fatalf("diff envelope = %+v", envelope)
	}
}

// --- architecture -----------------------------------------------------------

// TestArchitectureRendersItsThreeVerdicts covers the whole `architecture`
// group's rendering over one two-domain workspace with a real cross-domain edge.
func TestArchitectureRendersItsThreeVerdicts(t *testing.T) {
	root, projects := architectureWorkspace(t)
	human := wireContext(root, projects, wholeWorkspace(), nil)
	machine := wireContext(root, projects, wholeWorkspace(), jsonParams())

	validate, _, code := runSubcommand(t, human, "architecture", "validate")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("architecture validate exited %d:\n%s", code, validate)
	}
	for _, want := range []string{
		"Architecture validation passed",
		"2 domain(s)",
		"Edges: 1 declared · 1 observed",
		"Baseline: no local baseline file",
		"Diagnostics: 0 error(s)",
	} {
		if !strings.Contains(validate, want) {
			t.Errorf("validation rendering missing %q:\n%s", want, validate)
		}
	}

	snapshot, _, code := runSubcommand(t, human, "architecture", "snapshot")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("architecture snapshot exited %d:\n%s", code, snapshot)
	}
	for _, want := range []string{"Architecture snapshot", "Coverage: projects=", "Edges: 1 declared"} {
		if !strings.Contains(snapshot, want) {
			t.Errorf("snapshot rendering missing %q:\n%s", want, snapshot)
		}
	}

	inspect, _, code := runSubcommand(t, human, "architecture", "inspect", "consumer")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("architecture inspect exited %d:\n%s", code, inspect)
	}
	for _, want := range []string{
		"Architecture domain consumer",
		"Owner: consumer-team",
		"Relationships: 1 inbound · 0 outbound · 1 observed",
	} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspection rendering missing %q:\n%s", want, inspect)
		}
	}

	unknown, _, code := runSubcommand(t, human, "architecture", "inspect", "absent")
	if code == protocolcli.ExitSuccess {
		t.Fatal("an undeclared domain must fail")
	}
	if !strings.Contains(unknown, `Architecture domain "absent" unavailable`) {
		t.Errorf("unknown-domain rendering:\n%s", unknown)
	}

	structured, _, _ := runSubcommand(t, machine, "architecture", "validate")
	envelope, report := decodeEnvelope[sdd.ArchitectureValidationReport](t, structured)
	if envelope.Command != "architecture validate" || !report.Valid || report.Summary.Domains != 2 {
		t.Fatalf("architecture envelope = %+v / %+v", envelope, report)
	}
}

// TestArchitectureFailingVerdictStillRendersItsFindings pins the other half of
// the ratchet: an undeclared cross-domain dependency fails the run AND explains
// itself, on stdout for a human and in the failure envelope for a machine.
func TestArchitectureFailingVerdictStillRendersItsFindings(t *testing.T) {
	root, projects := architectureWorkspace(t)
	// Remove the binding that legitimizes the observed edge.
	writeFixture(t, filepath.Join(root, "consumer", archproto.ManifestFilename), consumerManifest(false))

	human, _, code := runSubcommand(t, wireContext(root, projects, wholeWorkspace(), nil), "architecture", "validate")
	if code == protocolcli.ExitSuccess {
		t.Fatal("an undeclared cross-domain dependency must fail the run")
	}
	for _, want := range []string{
		"Architecture validation failed",
		"Architecture findings (",
		// The docs hint prints on the failure path only; the passing output
		// is parity-recorded and asserts its own byte stability.
		"Docs: " + validationDocsURL,
	} {
		if !strings.Contains(human, want) {
			t.Errorf("failing rendering missing %q:\n%s", want, human)
		}
	}

	structured, _, _ := runSubcommand(t,
		wireContext(root, projects, wholeWorkspace(), jsonParams()), "architecture", "validate")
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(structured), &envelope); err != nil {
		t.Fatalf("decode failure envelope: %v\n%s", err, structured)
	}
	if envelope.Status == protocolcli.StatusSuccess || envelope.Data == nil {
		t.Fatalf("failure envelope = %+v, want the report attached", envelope)
	}
}

func architectureWorkspace(t *testing.T) (string, []*wsview.Project) {
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
	writeFixture(t, filepath.Join(root, "consumer", archproto.ManifestFilename), consumerManifest(true))
	return root, []*wsview.Project{
		{ID: "/producer", Name: "producer", Type: "library", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Type: "application", Path: "consumer", Dependencies: []string{"/producer"}},
	}
}

func consumerManifest(binding bool) string {
	bindings := ""
	if binding {
		bindings = `,
    "bindings": [{"kind":"project-dependency","consumerProject":"/consumer","producerProject":"/producer"}]`
	}
	return `{
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
    "justification": "The consumer stores the authoritative producer identity only"` + bindings + `
  }]
}`
}

// --- contracts --------------------------------------------------------------

// TestContractsRendersGenerateAndCheck covers the compiler's two subcommands,
// including the exit-2 drift cycle D2 keeps interactive.
func TestContractsRendersGenerateAndCheck(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"contracts-test","includes":["payments"]}`)
	writeFixture(t, filepath.Join(root, "payments", "putnami.json"), `{"name":"@acme/payments","type":"library"}`)
	writeFixture(t, filepath.Join(root, "payments", sdd.ContractsSourcePath), `{
  "protocolVersion": 1,
  "name": "go.putnami.dev/acme/payments",
  "enums": [ { "name": "Currency", "values": [ { "name": "USD", "value": "usd" } ] } ],
  "scopes": [ { "name": "payments:read" } ],
  "capabilities": [ { "name": "readPayments", "scopes": ["payments:read"] } ],
  "grants": [ { "name": "paymentsReader", "capability": "readPayments" } ],
  "claims": [ { "name": "sub", "type": "string", "required": true } ]
}`)
	projects := []*wsview.Project{
		{ID: "/payments", Name: "@acme/payments", SourceName: "@acme/payments", Type: "library", Path: "payments"},
	}
	selection := &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/payments"}}
	human := wireContext(root, projects, selection, nil)

	generate, _, code := runSubcommand(t, human, "contracts", "generate", "--project", "@acme/payments")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("contracts generate exited %d:\n%s", code, generate)
	}
	if !strings.Contains(generate, "Generated 4 contract artifact(s) for go.putnami.dev/acme/payments") {
		t.Errorf("generate rendering:\n%s", generate)
	}

	clean, _, code := runSubcommand(t, human, "contracts", "check", "--project", "@acme/payments")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("contracts check exited %d:\n%s", code, clean)
	}
	if !strings.Contains(clean, "Contracts up to date for go.putnami.dev/acme/payments") {
		t.Errorf("clean check rendering:\n%s", clean)
	}

	// Drift is the exit-2 contract: a corrupted committed artifact fails the
	// command AND names what drifted.
	writeFixture(t, filepath.Join(root, "payments", "schema", "contracts.gen.go"), "// drifted\n")
	drift, _, code := runSubcommand(t, human, "contracts", "check", "--project", "@acme/payments")
	if code != protocolcli.ExitUsage {
		t.Fatalf("contracts check on drift exited %d, want %d", code, protocolcli.ExitUsage)
	}
	for _, want := range []string{"Contract check failed for", "drift: schema/contracts.gen.go (stale)"} {
		if !strings.Contains(drift, want) {
			t.Errorf("drift rendering missing %q:\n%s", want, drift)
		}
	}

	structured, _, code := runSubcommand(t,
		wireContext(root, projects, selection, jsonParams()), "contracts", "check", "--project", "@acme/payments")
	if code != protocolcli.ExitUsage {
		t.Fatalf("structured drift check exited %d, want %d", code, protocolcli.ExitUsage)
	}
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(structured), &envelope); err != nil {
		t.Fatalf("decode drift envelope: %v\n%s", err, structured)
	}
	if envelope.Command != "contracts check" || envelope.Status == protocolcli.StatusSuccess || envelope.Data == nil {
		t.Fatalf("drift envelope = %+v, want the report attached to a failure", envelope)
	}
}

// TestContractsMissingManifestFailsWithoutAReport pins the early-error branch:
// a project with no authored manifest produced no report at all, so nothing is
// rendered and the classified error stands alone.
func TestContractsMissingManifestFailsWithoutAReport(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"contracts-test","includes":["bare"]}`)
	writeFixture(t, filepath.Join(root, "bare", "putnami.json"), `{"name":"@acme/bare","type":"library"}`)
	projects := []*wsview.Project{{ID: "/bare", Name: "@acme/bare", SourceName: "@acme/bare", Path: "bare"}}
	ctx := wireContext(root, projects, &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/bare"}}, nil)

	for _, name := range []string{"generate", "check"} {
		stdout, stderr, code := runSubcommand(t, ctx, "contracts", name, "--project", "@acme/bare")
		if code == protocolcli.ExitSuccess {
			t.Fatalf("contracts %s succeeded without a manifest", name)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("contracts %s printed a report it does not have:\n%s", name, stdout)
		}
		if !strings.Contains(stderr, "no contract manifest") {
			t.Errorf("contracts %s error = %q", name, stderr)
		}
	}
}

// --- the engine seam --------------------------------------------------------

// TestCommandSupportWrappersReachTheEngine keeps the exported seam honest: each
// wrapper must return the engine's own answer, not a second computation of it.
func TestCommandSupportWrappersReachTheEngine(t *testing.T) {
	if counts := sdd.SummarizeFeatures(nil); counts.Features != 0 || counts.Requirements != 0 {
		t.Errorf("SummarizeFeatures(nil) = %+v, want zero counts", counts)
	}
	if counts := sdd.SummarizeArchitecture(nil); counts.Domains != 0 {
		t.Errorf("SummarizeArchitecture(nil) = %+v, want zero counts", counts)
	}
	if got := sdd.DesignNodeAuthority(sdd.FeatureDesignImplementation{}, "any"); got != string(featureproto.DesignAuthorityExact) {
		t.Errorf("DesignNodeAuthority with no path = %q, want exact", got)
	}
	if got := sdd.GeneratedClientOperationDetails(sdd.FeatureDesignImplementation{}, "any"); got != "" {
		t.Errorf("GeneratedClientOperationDetails with no edges = %q, want empty", got)
	}
	if got := sdd.CompactFactProvenance(sdd.FeatureDesignImplementation{}, featureproto.DesignNode{}); got != nil {
		t.Errorf("CompactFactProvenance with no provenance = %+v, want nil", got)
	}
	view := wsview.NewWorkspace("/w", nil, []*wsview.Project{{ID: "/app", Name: "app", Path: "app"}})
	if project := sdd.ResolveProjectSelector(view, "app"); project == nil || project.ID != "/app" {
		t.Errorf("ResolveProjectSelector by name = %+v", project)
	}
	if project := sdd.ResolveProjectSelector(view, "/app"); project == nil || project.ID != "/app" {
		t.Errorf("ResolveProjectSelector by id = %+v", project)
	}
}

// commitFixtureRepository makes a fixture tree a git repository with one commit,
// which is what `features diff` reads instead of the worktree.
func commitFixtureRepository(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "fixture@example.test"},
		{"config", "user.name", "Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// --- specs verify with a measured threshold check ----------------------------

// TestSpecVerifyRendersTheMeasuredAggregate pins the threshold line a user
// reads: the recomputed state plus the observed aggregate the
// verdict was decided from — never a producer-declared outcome.
func TestSpecVerifyRendersTheMeasuredAggregate(t *testing.T) {
	var buffer strings.Builder
	printSpecVerifyHuman(&buffer, sdd.SpecVerifyReport{
		Groups: []featureproto.SpecVerificationGroup{{
			Project: "/go/framework/logger", Feature: "go/structured-logging",
			Mode: featureproto.VerificationModeReport, ModeSource: "default",
			Requirements: []featureproto.SpecRequirementVerdict{{
				Requirement: "flush-latency", State: featureproto.RequirementVerified,
				Checks: []featureproto.SpecCheckVerdict{{
					Check: "flush-benchmark", State: featureproto.CheckSatisfied, Reason: "objective-met",
					Path: "bench_test.go", Symbol: "TestFlushLatency",
					Measurement: &featureproto.ObservationMeasurement{
						Name: "logger.flush.duration", Aggregation: featureproto.AggregationP95, Value: 12.5, Unit: "ms",
					},
				}},
			}},
		}},
	})
	rendered := buffer.String()
	if !strings.Contains(rendered, "measured 12.5ms (p95 logger.flush.duration)") {
		t.Fatalf("measured aggregate not rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "bench_test.go#TestFlushLatency") {
		t.Fatalf("provenance lost from the measured line:\n%s", rendered)
	}
}
