package sdd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func TestFeaturesDiffComparesImmutableMembership(t *testing.T) {
	root := featureCommandsWorkspace(t, false).Root
	initFeatureCommandsGit(t, root)
	commitFeatureCommandsGit(t, root, "base features")
	base := strings.TrimSpace(runFeatureCommandsGit(t, root, "rev-parse", "HEAD"))

	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"features-test","includes":["app","new"]}`)
	writeFixtureFile(t, filepath.Join(root, "new", "putnami.json"), `{"name":"new","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "new", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "new",
  "features": [{
    "id": "new/search",
    "type": "feature",
    "name": "Search",
    "outcome": "Customers find products",
    "owner": "discovery",
    "target": "modeled"
  }]
}`)
	commitFeatureCommandsGit(t, root, "change membership")
	head := strings.TrimSpace(runFeatureCommandsGit(t, root, "rev-parse", "HEAD"))

	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"dirty-private","includes":[]}`)
	beforeStatus := runFeatureCommandsGit(t, root, "status", "--porcelain=v1", "--untracked-files=all")
	report, err := BuildFeatureDiffResult(root, base, head)
	if err != nil {
		t.Fatalf("features diff: %v", err)
	}
	if report.Compatibility != featureengine.SnapshotCompatibility || report.Base.Commit != "git:"+base || report.Head.Commit != "git:"+head {
		t.Fatalf("diff metadata = %+v", report.Delta)
	}
	if len(report.Added) != 1 || report.Added[0].ID != "new/search" {
		t.Fatalf("added = %+v", report.Added)
	}
	if len(report.Removed) != 1 || report.Removed[0].ID != "orders/checkout" {
		t.Fatalf("removed = %+v", report.Removed)
	}
	if after := runFeatureCommandsGit(t, root, "status", "--porcelain=v1", "--untracked-files=all"); after != beforeStatus {
		t.Fatalf("features diff mutated worktree status:\nbefore=%q\nafter=%q", beforeStatus, after)
	}
}

func TestFeaturesDiffCarriesSafeTypedRevisionAndStructuralFailures(t *testing.T) {
	root := featureCommandsWorkspace(t, false).Root
	initFeatureCommandsGit(t, root)
	commitFeatureCommandsGit(t, root, "valid features")
	valid := strings.TrimSpace(runFeatureCommandsGit(t, root, "rev-parse", "HEAD"))

	_, err := BuildFeatureDiffResult(root, "-private-ref", valid)
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("unsafe ref result = %v", err)
	}
	revisionFailure, ok := ResultData(err).(FeatureDiffReport)
	if !ok || len(revisionFailure.BaseDiagnostics) != 1 || revisionFailure.BaseDiagnostics[0].Field != "baseRevision" {
		t.Fatalf("revision failure = %#v", ResultData(err))
	}
	encoded, marshalErr := json.Marshal(revisionFailure)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(encoded, []byte("private-ref")) || bytes.Contains([]byte(err.Error()), []byte("private-ref")) {
		t.Fatalf("unsafe ref leaked through failure: %s / %v", encoded, err)
	}

	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{"protocolVersion":1,"namespace":"billing","features":[],"unknown":true}`)
	commitFeatureCommandsGit(t, root, "invalid features")
	invalid := strings.TrimSpace(runFeatureCommandsGit(t, root, "rev-parse", "HEAD"))
	_, err = BuildFeatureDiffResult(root, valid, invalid)
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("structural failure = %v", err)
	}
	structuralFailure, ok := ResultData(err).(FeatureDiffReport)
	if !ok || len(structuralFailure.HeadDiagnostics) == 0 {
		t.Fatalf("structural failure data = %#v", ResultData(err))
	}
	if structuralFailure.Compatibility != featureengine.SnapshotCompatibility {
		t.Fatalf("failure compatibility = %q", structuralFailure.Compatibility)
	}
}

func TestFeatureEnginesExposeWarningOnlyAssessmentAndProvisionalSnapshot(t *testing.T) {
	ws := featureCommandsWorkspace(t, false)

	validateData, err := BuildFeatureValidationResult(ws, Selection{})
	if err != nil {
		t.Fatalf("features validate: %v", err)
	}
	if !validateData.Valid || validateData.Summary.Features != 2 || validateData.Summary.Missing != 1 {
		t.Fatalf("validate data = %+v", validateData)
	}
	if validateData.Counts.Errors != 0 || validateData.Counts.Warnings != 1 || len(validateData.Diagnostics) != 1 {
		t.Fatalf("validate diagnostics = %+v", validateData)
	}

	snapshotData, err := BuildFeatureSnapshotResult(ws, Selection{})
	if err != nil {
		t.Fatalf("features snapshot: %v", err)
	}
	if snapshotData.Snapshot == nil || snapshotData.Compatibility != featureengine.SnapshotCompatibility {
		t.Fatalf("snapshot data = %+v", snapshotData)
	}
	if len(snapshotData.Diagnostics) != 1 {
		t.Fatalf("snapshot diagnostics = %#v", snapshotData.Diagnostics)
	}
	// The human header the command layer prints is this projection, so the two
	// counts it states are asserted where they are computed.
	if counts := summarizeFeatures(snapshotData.Snapshot); counts.Features != 2 || counts.IncompleteFeatures != 1 {
		t.Fatalf("snapshot summary = %+v", counts)
	}
	if snapshotData.Diagnostics[0].Code != "features.missing_evidence" {
		t.Fatalf("snapshot diagnostic = %+v", snapshotData.Diagnostics[0])
	}
}

func TestFeaturesValidateCarriesTypedFailureData(t *testing.T) {
	ws := featureCommandsWorkspace(t, true)
	returned, err := BuildFeatureValidationResult(ws, Selection{})
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("features validate error = %v, want ErrInvalidConfig", err)
	}
	report, ok := ResultData(err).(FeatureValidationReport)
	if !ok {
		t.Fatalf("failure data = %T, want FeatureValidationReport", ResultData(err))
	}
	if report.Valid || report.Counts.Errors == 0 || len(report.Diagnostics) == 0 {
		t.Fatalf("failure report = %+v", report)
	}
	// The renderer prints every diagnostic of the report it is handed, so the
	// attached copy and the returned one must be one document.
	if len(returned.Diagnostics) != len(report.Diagnostics) || returned.Counts != report.Counts {
		t.Fatalf("returned report = %+v, want the attached one", returned)
	}
}

func TestFeaturesInspectUsesExactIDsAndReportsAmbiguousLeafCandidates(t *testing.T) {
	ws := featureCommandsWorkspace(t, false)

	report, err := BuildFeatureContextResult(ws, "billing/checkout")
	if err != nil {
		t.Fatalf("features inspect exact ID: %v", err)
	}
	if report.Feature == nil || report.Feature.ID != "billing/checkout" {
		t.Fatalf("inspection feature = %+v", report.Feature)
	}
	if report.Feature.Current != featureproto.MaturityModeled || report.Feature.Target != featureproto.MaturityCoded {
		t.Fatalf("inspection maturity = %s -> %s", report.Feature.Current, report.Feature.Target)
	}
	if len(report.Feature.Requirements) != 1 || report.Feature.Requirements[0].State != featureengine.VerificationMissing {
		t.Fatalf("inspection requirements = %+v", report.Feature.Requirements)
	}

	_, err = BuildFeatureContextResult(ws, "checkout")
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("ambiguous inspection error = %v, want ErrUsage", err)
	}
	failure, ok := ResultData(err).(FeatureInspectionReport)
	if !ok {
		t.Fatalf("ambiguous failure data = %T", ResultData(err))
	}
	wantCandidates := []string{"billing/checkout", "orders/checkout"}
	if strings.Join(failure.Candidates, ",") != strings.Join(wantCandidates, ",") || failure.CandidateCount != 2 {
		t.Fatalf("ambiguous candidates = %#v (%d)", failure.Candidates, failure.CandidateCount)
	}
	if len(failure.Diagnostics) == 0 || failure.Diagnostics[0].Code != featureproto.ErrorCodeUnknownFeature {
		t.Fatalf("ambiguous diagnostics = %+v", failure.Diagnostics)
	}
}

func TestFeaturesInspectReportsNativeLeafAmbiguityWithoutLegacyFallback(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["admin","tasks"]}`)
	for _, project := range []string{"admin", "tasks"} {
		writeFixtureFile(t, filepath.Join(root, project, "putnami.json"), `{"name":"`+project+`","type":"application"}`)
	}
	writeFeatureDesignGraph(t, root, "admin", "admin/manage", "Manage administration")
	writeFeatureDesignGraph(t, root, "tasks", "tasks/manage", "Manage tasks")
	ws := fixtureWorkspace("design-test", root, appProject("admin", "admin"), appProject("tasks", "tasks"))

	report, err := BuildFeatureContextResult(ws, "manage")
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("native ambiguity error = %v, want ErrUsage", err)
	}
	if strings.Join(report.Candidates, ",") != "admin/manage,tasks/manage" || report.CandidateCount != 2 {
		t.Fatalf("native ambiguity candidates = %#v (%d)", report.Candidates, report.CandidateCount)
	}
	if len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, "ambiguous") {
		t.Fatalf("native ambiguity diagnostics = %+v", report.Diagnostics)
	}
	failure, ok := ResultData(err).(FeatureInspectionReport)
	if !ok || strings.Join(failure.Candidates, ",") != "admin/manage,tasks/manage" {
		t.Fatalf("native ambiguity result data = %#v", ResultData(err))
	}

	_, err = BuildAgentFeatureContextResult(ws, "manage")
	if !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), "admin/manage, tasks/manage") {
		t.Fatalf("agent native ambiguity = %v", err)
	}
}

func TestFeaturesInspectScopesNativeDesignAndFollowsGeneratedClientToProducerOperation(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","type":"application"}`)
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "app",
		Nodes: []featureproto.DesignNode{
			{ID: "feature:items/manage", Kind: featureproto.DesignNodeFeature, Name: "Item management", Properties: map[string]string{"outcome": "Consumers list items", "owner": "catalog"}},
			{ID: "module:app", Kind: featureproto.DesignNodeModule, Name: "app"},
			{ID: "api.operation:GET:/items", Kind: featureproto.DesignNodeAPIOperation, Name: "GET /items", Properties: map[string]string{"method": "GET", "path": "/items", "operationId": "getItems"}},
			{ID: "service:catalog", Kind: featureproto.DesignNodeService, Name: "catalog"},
			{ID: "client.generated:ts:@example/items/ItemsClient", Kind: featureproto.DesignNodeClient, Name: "ItemsClient", Properties: map[string]string{"language": "ts", "package": "@example/items", "feature": "items/manage"}},
			{ID: "api.operation:DELETE:/unrelated", Kind: featureproto.DesignNodeAPIOperation, Name: "DELETE /unrelated"},
		},
		Edges: []featureproto.DesignEdge{
			{From: "feature:items/manage", To: "module:app", Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityExact},
			{From: "module:app", To: "api.operation:GET:/items", Kind: featureproto.DesignEdgeExposes, Authority: featureproto.DesignAuthorityExact},
			{From: "api.operation:GET:/items", To: "service:catalog", Kind: featureproto.DesignEdgeInjects, Authority: featureproto.DesignAuthorityExact},
			{From: "client.generated:ts:@example/items/ItemsClient", To: "api.operation:GET:/items", Kind: featureproto.DesignEdgeGeneratedFrom, Authority: featureproto.DesignAuthorityExact, Properties: map[string]string{"operationId": "getItems"}},
		},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "app", ".gen", featureproto.DesignGraphArtifact), string(encoded))
	ws := fixtureWorkspace("design-test", root, appProject("app", "app"))

	report, err := BuildFeatureContextResult(ws, "items/manage")
	if err != nil {
		t.Fatal(err)
	}
	if report.Design == nil || len(report.Design.Implementations) != 1 {
		t.Fatalf("native design = %+v", report.Design)
	}
	implementation := report.Design.Implementations[0]
	if findDesignNode(implementation.Nodes, "client.generated:ts:@example/items/ItemsClient") == nil {
		t.Fatal("generated client was not reached through the reverse generatedFrom edge")
	}
	if findDesignNode(implementation.Nodes, "api.operation:DELETE:/unrelated") != nil {
		t.Fatal("unrelated design node leaked into scoped inspection")
	}

	// The human renderer's per-node detail line is built from these two engine
	// helpers; the line itself is the command layer's to assert.
	if got := designNodeAuthority(implementation, "service:catalog"); got != string(featureproto.DesignAuthorityExact) {
		t.Errorf("dependency authority = %q", got)
	}
	if got := generatedClientOperationDetails(implementation, "client.generated:ts:@example/items/ItemsClient"); got != "getItems→GET /items" {
		t.Errorf("generated-from detail = %q", got)
	}

	agentContext, err := BuildAgentFeatureContextResult(ws, "items/manage")
	if err != nil {
		t.Fatal(err)
	}
	if len(agentContext.Implementations) != 1 {
		t.Fatalf("agent context implementations = %+v", agentContext.Implementations)
	}
	compact := agentContext.Implementations[0]
	if len(compact.Surfaces) != 1 || len(compact.Dependencies) != 1 || len(compact.GeneratedClients) != 1 {
		t.Fatalf("compact feature context = %+v", compact)
	}
	if compact.GeneratedClients[0].GeneratedFrom != "getItems→GET /items" {
		t.Fatalf("generated client fact = %+v", compact.GeneratedClients[0])
	}
	// The derived value stays out of Properties so an agent can trust every
	// entry there to be a declaration from the graph itself.
	if _, derivedInProperties := compact.GeneratedClients[0].Properties["generatedFrom"]; derivedInProperties {
		t.Errorf("derived generatedFrom leaked into declared properties: %+v", compact.GeneratedClients[0].Properties)
	}
	if len(compact.CriticalPaths) != 3 || compact.CriticalPathCount != 3 {
		t.Fatalf("representative critical paths = %+v (%d total)", compact.CriticalPaths, compact.CriticalPathCount)
	}
	compactJSON, err := json.Marshal(agentContext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(compactJSON, []byte(`"edges"`)) || bytes.Contains(compactJSON, []byte("unrelated")) {
		t.Fatalf("agent context leaked raw or unrelated graph data: %s", compactJSON)
	}
}

// TestFeaturesInspectMintsWorkspaceProjectAndCommandNodes pins the workspace
// enrichment: the owning project, its putnami.json bin commands, and direct
// dependencies between co-implementing projects appear as their own exact node
// kinds — never as features — and stay out of critical paths.
func TestFeaturesInspectMintsWorkspaceProjectAndCommandNodes(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["api","worker"]}`)
	writeFixtureFile(t, filepath.Join(root, "api", "putnami.json"),
		`{"name":"api","type":"application","dependencies":["worker"],"bin":{"tasks-admin":"src/bin/admin.ts"}}`)
	writeFixtureFile(t, filepath.Join(root, "worker", "putnami.json"), `{"name":"worker","type":"library"}`)
	for _, project := range []string{"api", "worker"} {
		graph := &featureproto.DesignGraph{
			Compatibility: featureproto.DesignGraphCompatibility,
			Project:       project,
			Nodes: []featureproto.DesignNode{
				{ID: "feature:tasks/manage", Kind: featureproto.DesignNodeFeature, Name: "Task management", Properties: map[string]string{"outcome": "Tasks are managed", "owner": "samples"}},
				{ID: "module:" + project, Kind: featureproto.DesignNodeModule, Name: project},
				{ID: "module:" + project + "/nested", Kind: featureproto.DesignNodeModule, Name: "nested"},
			},
			Edges: []featureproto.DesignEdge{
				{From: "feature:tasks/manage", To: "module:" + project, Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityExact},
				{From: "module:" + project, To: "module:" + project + "/nested", Kind: featureproto.DesignEdgeContains, Authority: featureproto.DesignAuthorityExact},
			},
		}
		encoded, err := featureproto.MarshalDesignGraph(graph)
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(root, project, ".gen", featureproto.DesignGraphArtifact), string(encoded))
	}
	// The two putnami.json files above are what workspace.Load read. Their
	// members — the bin map, the dependency and the type — are stated here
	// because the wire delivers them rather than the file.
	ws := fixtureWorkspace("design-test", root,
		&workspace.Project{
			ID: "/api", Name: "api", SourceName: "api", Type: "application", Path: "api",
			Dependencies: []string{"worker"},
			Config: &wsproto.ProjectConfig{
				Name: "api", Type: "application", Dependencies: []string{"worker"},
				Bin: &wsproto.BinConfig{Map: map[string]string{"tasks-admin": "src/bin/admin.ts"}},
			},
		},
		&workspace.Project{
			ID: "/worker", Name: "worker", SourceName: "worker", Type: "library", Path: "worker",
			Config: &wsproto.ProjectConfig{Name: "worker", Type: "library"},
		})

	report, err := BuildFeatureContextResult(ws, "tasks/manage")
	if err != nil {
		t.Fatal(err)
	}
	if report.Design == nil || len(report.Design.Implementations) != 2 {
		t.Fatalf("native design = %+v", report.Design)
	}
	api, worker := report.Design.Implementations[0], report.Design.Implementations[1]
	if api.Project != "api" || worker.Project != "worker" {
		t.Fatalf("implementation order = %s, %s", api.Project, worker.Project)
	}

	projectNode := findDesignNode(api.Nodes, "project:api")
	if projectNode == nil || projectNode.Kind != featureproto.DesignNodeProject {
		t.Fatalf("project node = %+v", projectNode)
	}
	if projectNode.Properties["type"] != "application" || projectNode.Properties["path"] != "api" {
		t.Fatalf("project properties = %+v", projectNode.Properties)
	}
	if projectNode.Provenance == nil || projectNode.Provenance.Path != "putnami.json" {
		t.Fatalf("project provenance = %+v", projectNode.Provenance)
	}
	command := findDesignNode(api.Nodes, "command:tasks-admin")
	if command == nil || command.Kind != featureproto.DesignNodeCommand || command.Properties["entry"] != "src/bin/admin.ts" {
		t.Fatalf("command node = %+v", command)
	}
	if dependency := findDesignNode(api.Nodes, "project:worker"); dependency == nil || dependency.Kind != featureproto.DesignNodeProject {
		t.Fatalf("co-implementing dependency node = %+v", dependency)
	}
	wantEdges := []featureproto.DesignEdge{
		{From: "project:api", To: "module:api", Kind: featureproto.DesignEdgeContains, Authority: featureproto.DesignAuthorityExact},
		{From: "project:api", To: "command:tasks-admin", Kind: featureproto.DesignEdgeExposes, Authority: featureproto.DesignAuthorityExact},
		{From: "project:api", To: "project:worker", Kind: featureproto.DesignEdgeDependsOn, Authority: featureproto.DesignAuthorityExact},
	}
	for _, want := range wantEdges {
		found := false
		for _, edge := range api.Edges {
			if edge.From == want.From && edge.To == want.To && edge.Kind == want.Kind && edge.Authority == want.Authority {
				found = true
			}
		}
		if !found {
			t.Errorf("workspace edge %s -%s-> %s missing", want.From, want.Kind, want.To)
		}
	}
	for _, edge := range api.Edges {
		if edge.Kind == featureproto.DesignEdgeContains && edge.From == "project:api" && edge.To == "module:api/nested" {
			t.Error("project claimed a nested module its root module already contains")
		}
	}

	if worker.Project != "worker" {
		t.Fatalf("worker implementation = %+v", worker)
	}
	workerNode := findDesignNode(worker.Nodes, "project:worker")
	if workerNode == nil || workerNode.Properties["type"] != "library" {
		t.Fatalf("worker project node = %+v", workerNode)
	}
	for _, edge := range worker.Edges {
		if edge.Kind == featureproto.DesignEdgeDependsOn {
			t.Errorf("worker minted an undeclared dependency edge: %+v", edge)
		}
	}
	for _, implementation := range report.Design.Implementations {
		if findDesignNode(implementation.Nodes, "command:worker") != nil {
			t.Error("a project without a bin declaration minted a command")
		}
		for _, criticalPath := range implementation.CriticalPaths {
			for _, node := range criticalPath.Nodes {
				if strings.HasPrefix(node, "project:") || strings.HasPrefix(node, "command:") {
					t.Errorf("critical path routed through a workspace node: %+v", criticalPath)
				}
			}
		}
	}

	agentContext, err := BuildAgentFeatureContextResult(ws, "tasks/manage")
	if err != nil {
		t.Fatal(err)
	}
	compact := agentContext.Implementations[0]
	if len(compact.Projects) != 2 || len(compact.Commands) != 1 {
		t.Fatalf("compact workspace facts = projects %+v commands %+v", compact.Projects, compact.Commands)
	}
	if compact.Projects[0].Authority != string(featureproto.DesignAuthorityExact) || compact.Commands[0].ID != "command:tasks-admin" {
		t.Fatalf("compact workspace fact detail = %+v / %+v", compact.Projects[0], compact.Commands[0])
	}

}

// writeTypedClientDesignFixture publishes one project whose graph holds an
// opaque-token producer feature, a hand-written typed client consumed by another
// feature's module, and a generated client over the same operation.
func writeTypedClientDesignFixture(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["identity"]}`)
	writeFixtureFile(t, filepath.Join(root, "identity", "putnami.json"), `{"name":"identity","type":"application"}`)
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "cloud/identity",
		Nodes: []featureproto.DesignNode{
			{ID: "feature:identity/opaque-tokens", Kind: featureproto.DesignNodeFeature, Name: "Opaque tokens", Properties: map[string]string{"outcome": "Tokens can be introspected", "owner": "cloud"}},
			{ID: "module:opaque-tokens", Kind: featureproto.DesignNodeModule, Name: "opaque-tokens"},
			{ID: "api.operation:POST:/v1/introspect", Kind: featureproto.DesignNodeAPIOperation, Name: "POST /v1/introspect", Properties: map[string]string{"method": "POST", "path": "/v1/introspect", "operationId": "introspectToken"}},
			{ID: "feature:identity/control-plane", Kind: featureproto.DesignNodeFeature, Name: "Control plane", Properties: map[string]string{"outcome": "Operators manage tenants", "owner": "cloud"}},
			{ID: "module:control-plane", Kind: featureproto.DesignNodeModule, Name: "control-plane"},
			{ID: "client.typed:cloud/identity:introspectauth", Kind: featureproto.DesignNodeTypedClient, Name: "introspectauth", Properties: map[string]string{
				"producer": "cloud/identity", "language": "go", "operations": "POST /v1/introspect",
			}},
			{ID: "client.generated:go:cloud/identity/IdentityClient", Kind: featureproto.DesignNodeClient, Name: "IdentityClient", Properties: map[string]string{
				"producer": "cloud/identity", "language": "go", "package": "cloud/identity", "feature": "identity/opaque-tokens",
			}},
		},
		Edges: []featureproto.DesignEdge{
			{From: "feature:identity/opaque-tokens", To: "module:opaque-tokens", Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityExact},
			{From: "module:opaque-tokens", To: "api.operation:POST:/v1/introspect", Kind: featureproto.DesignEdgeExposes, Authority: featureproto.DesignAuthorityExact},
			{From: "feature:identity/control-plane", To: "module:control-plane", Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityExact},
			// The consumer's producer feature is not declared, so the call edge
			// stays explicitly unmodeled instead of claiming a lineage.
			{From: "module:control-plane", To: "client.typed:cloud/identity:introspectauth", Kind: featureproto.DesignEdgeCalls, Authority: featureproto.DesignAuthorityUnmodeled},
			{From: "client.typed:cloud/identity:introspectauth", To: "api.operation:POST:/v1/introspect", Kind: featureproto.DesignEdgeCalls, Authority: featureproto.DesignAuthorityExact, Properties: map[string]string{"operationId": "introspectToken"}},
			{From: "client.generated:go:cloud/identity/IdentityClient", To: "api.operation:POST:/v1/introspect", Kind: featureproto.DesignEdgeGeneratedFrom, Authority: featureproto.DesignAuthorityExact, Properties: map[string]string{"operationId": "introspectToken"}},
		},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "identity", ".gen", featureproto.DesignGraphArtifact), string(encoded))
	return fixtureWorkspace("design-test", root, appProject("identity", "identity"))
}

// TestFeaturesInspectProjectsTypedClientsApartFromGeneratedClients pins that a
// hand-written client is never presented as a generated one and that its
// unmodeled lineage survives the projection.
func TestFeaturesInspectProjectsTypedClientsApartFromGeneratedClients(t *testing.T) {
	ws := writeTypedClientDesignFixture(t)

	inspection, err := BuildFeatureContextResult(ws, "identity/control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Design == nil || len(inspection.Design.Implementations) != 1 {
		t.Fatalf("typed-client design = %+v", inspection.Design)
	}
	// The renderer marks a hand-written client with the authority of the edge
	// that reached it, and asks for generated-from provenance only for a
	// generated client. Both answers are computed here.
	typedID := "client.typed:cloud/identity:introspectauth"
	if got := designNodeAuthority(inspection.Design.Implementations[0], typedID); got != string(featureproto.DesignAuthorityUnmodeled) {
		t.Errorf("typed client authority = %q, want currently-unmodeled", got)
	}
	if got := generatedClientOperationDetails(inspection.Design.Implementations[0], typedID); got != "" {
		t.Errorf("hand-written client carries a generated-client fact: %q", got)
	}

	agentContext, err := BuildAgentFeatureContextResult(ws, "identity/control-plane")
	if err != nil {
		t.Fatal(err)
	}
	compact := agentContext.Implementations[0]
	if len(compact.TypedClients) != 1 || compact.TypedClients[0].ID != "client.typed:cloud/identity:introspectauth" {
		t.Fatalf("typed clients = %+v", compact.TypedClients)
	}
	if compact.TypedClients[0].GeneratedFrom != "" {
		t.Errorf("typed client carries generated-client provenance: %+v", compact.TypedClients[0])
	}
	if compact.TypedClients[0].Authority != string(featureproto.DesignAuthorityUnmodeled) {
		t.Errorf("typed client authority = %q, want currently-unmodeled", compact.TypedClients[0].Authority)
	}
	if _, guessed := compact.TypedClients[0].Properties["feature"]; guessed {
		t.Errorf("typed client properties invented a producer feature: %+v", compact.TypedClients[0].Properties)
	}
	// The generated bucket keeps holding exactly the generated client, with its
	// own generated-from provenance intact.
	if len(compact.GeneratedClients) != 1 || compact.GeneratedClients[0].ID != "client.generated:go:cloud/identity/IdentityClient" {
		t.Fatalf("generated clients = %+v", compact.GeneratedClients)
	}
	if compact.GeneratedClients[0].GeneratedFrom != "introspectToken→POST /v1/introspect" {
		t.Errorf("generated client provenance = %+v", compact.GeneratedClients[0])
	}
}

// TestFeaturesInspectReachesTypedConsumersFromTheProducerFeature pins the
// reverse traversal: the producer must be able to answer "who calls me".
func TestFeaturesInspectReachesTypedConsumersFromTheProducerFeature(t *testing.T) {
	ws := writeTypedClientDesignFixture(t)

	agentContext, err := BuildAgentFeatureContextResult(ws, "identity/opaque-tokens")
	if err != nil {
		t.Fatal(err)
	}
	compact := agentContext.Implementations[0]
	if len(compact.TypedClients) != 1 {
		t.Fatalf("producer feature did not reach its typed consumer: %+v", compact)
	}
	consumers := make([]string, 0, len(compact.Modules))
	for _, module := range compact.Modules {
		consumers = append(consumers, module.ID)
	}
	if !slices.Contains(consumers, "module:control-plane") {
		t.Fatalf("consumer module = %v, want the typed consumer reached through the client", consumers)
	}
	var typedPath *FeatureDesignCriticalPath
	for index, criticalPath := range compact.CriticalPaths {
		if criticalPath.Nodes[len(criticalPath.Nodes)-1] == "client.typed:cloud/identity:introspectauth" {
			typedPath = &compact.CriticalPaths[index]
		}
	}
	if typedPath == nil {
		t.Fatalf("no critical path reaches the typed client: %+v", compact.CriticalPaths)
	}
	// Reaching the client backwards from the operation it calls is exactly
	// declared; only the consumer's producer-feature lineage is unmodeled, and
	// that edge is not on this path.
	if typedPath.Authority != string(featureproto.DesignAuthorityExact) {
		t.Fatalf("path authority = %q, want exact for the declared call to the operation", typedPath.Authority)
	}
	if !slices.Contains(typedPath.Relations, "<calls") {
		t.Fatalf("path relations = %v, want the reversed call to the typed client", typedPath.Relations)
	}
}

// TestDesignAuthorityLadderRanksEveryProtocolAuthority is the anti-rot gate: a
// hardcoded ladder ranks an authority it has never heard of as exact, which
// would let an unmodeled step publish an exact path.
func TestDesignAuthorityLadderRanksEveryProtocolAuthority(t *testing.T) {
	ordered := featureproto.OrderedDesignAuthorities()
	for _, authority := range ordered {
		if _, ranked := designAuthorityRanks[authority]; !ranked {
			t.Errorf("protocol authority %q has no rank in the CLI ladder", authority)
		}
	}
	if len(designAuthorityRanks) != len(ordered) {
		t.Errorf("CLI ladder ranks %d authorities, protocol declares %d", len(designAuthorityRanks), len(ordered))
	}
	for index := 1; index < len(ordered); index++ {
		strong, weak := ordered[index-1], ordered[index]
		if got := weakestDesignAuthority(strong, weak); got != weak {
			t.Errorf("weakestDesignAuthority(%q, %q) = %q, want %q", strong, weak, got, weak)
		}
		if got := weakestDesignAuthority(weak, strong); got != weak {
			t.Errorf("weakestDesignAuthority(%q, %q) = %q, want %q", weak, strong, got, weak)
		}
	}
	const unknown = featureproto.DesignAuthority("from-a-newer-producer")
	for _, authority := range ordered {
		if got := weakestDesignAuthority(authority, unknown); got != unknown {
			t.Errorf("weakestDesignAuthority(%q, unknown) = %q, want the unknown authority", authority, got)
		}
	}
}

func TestReconstructDesignPathKeepsTheWeakestAuthority(t *testing.T) {
	predecessor := map[string]designTraversal{
		"module:app": {
			from: "feature:items/manage",
			edge: featureproto.DesignEdge{
				From: "feature:items/manage", To: "module:app",
				Kind: featureproto.DesignEdgeImplementedBy, Authority: featureproto.DesignAuthorityDerived,
			},
			authority: featureproto.DesignAuthorityDerived,
		},
		"service:catalog": {
			from: "module:app",
			edge: featureproto.DesignEdge{
				From: "module:app", To: "service:catalog",
				Kind: featureproto.DesignEdgeInjects, Authority: featureproto.DesignAuthorityHeuristic,
			},
			authority: featureproto.DesignAuthorityHeuristic,
		},
	}
	criticalPath, ok := reconstructDesignPath("feature:items/manage", "service:catalog", predecessor)
	if !ok {
		t.Fatal("critical path was not reconstructed")
	}
	if criticalPath.Authority != string(featureproto.DesignAuthorityHeuristic) {
		t.Fatalf("critical path authority = %q, want weakest heuristic", criticalPath.Authority)
	}
}

func TestBuildFeatureCatalogMergesImplementationsWithoutLoadingSubgraphs(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["consumer","producer"]}`)
	writeFixtureFile(t, filepath.Join(root, "consumer", "putnami.json"), `{"name":"consumer","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "producer", "putnami.json"), `{"name":"producer","type":"application"}`)

	for _, project := range []string{"producer", "consumer"} {
		graph := &featureproto.DesignGraph{
			Compatibility: featureproto.DesignGraphCompatibility,
			Project:       project,
			Nodes: []featureproto.DesignNode{{
				ID: "feature:items/manage", Kind: featureproto.DesignNodeFeature, Name: "Item management",
				Properties: map[string]string{"outcome": "Consumers list items", "owner": "catalog"},
			}},
			Edges: []featureproto.DesignEdge{},
		}
		encoded, err := featureproto.MarshalDesignGraph(graph)
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(root, project, ".gen", featureproto.DesignGraphArtifact), string(encoded))
	}

	ws := fixtureWorkspace("design-test", root, appProject("consumer", "consumer"), appProject("producer", "producer"))

	catalog, err := BuildFeatureCatalogResult(ws, "items catalog", Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Compatibility != featureproto.DesignGraphCompatibility || catalog.Graphs != 2 || len(catalog.Features) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	if catalog.Query != "items catalog" {
		t.Fatalf("catalog query = %q", catalog.Query)
	}
	feature := catalog.Features[0]
	if feature.ID != "items/manage" || strings.Join(feature.Projects, ",") != "consumer,producer" {
		t.Fatalf("feature summary = %+v", feature)
	}

	context, err := BuildFeatureContextResult(ws, feature.ID)
	if err != nil {
		t.Fatal(err)
	}
	if context.Design == nil || len(context.Design.Implementations) != 2 || len(context.Diagnostics) != 0 {
		t.Fatalf("feature context = %+v", context)
	}
}

func TestBuildAgentFeatureContextDoesNotEvaluateLegacyWorkspaceEvidence(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{"protocolVersion":1,"namespace":"legacy","features":[],"unknown":true}`)

	ws := fixtureWorkspace("design-test", root, appProject("app", "app"))

	_, err := BuildAgentFeatureContextResult(ws, "legacy/feature")
	if !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("feature_context error = %v, want native design not found without legacy evaluation", err)
	}
}

func TestBuildFeatureCatalogAndAgentContextExposeExactRootManifestAuthority(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"manifest-test","includes":["tooling"]}`)
	writeFixtureFile(t, filepath.Join(root, "tooling", "putnami.json"), `{"name":"tooling","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "tooling", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "tooling",
  "features": [{
    "id": "tooling/release-status",
    "type": "feature",
    "name": "Release status",
    "outcome": "Maintainers see release readiness",
    "owner": "release-engineering",
    "target": "coded",
    "relations": [{"kind":"dependsOn","target":"platform/releases"}],
    "requirements": [{"id":"implementation","stage":"coded","evidenceKinds":["artifact"]}]
  }]
}`)

	ws := fixtureWorkspace("manifest-test", root, appProject("tooling", "tooling"))

	catalog, err := BuildFeatureCatalogResult(ws, "release engineering", Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Graphs != 0 || catalog.Manifests != 1 || len(catalog.Features) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	feature := catalog.Features[0]
	if feature.ID != "tooling/release-status" || strings.Join(feature.Projects, ",") != "tooling" || len(feature.Declarations) != 1 {
		t.Fatalf("manifest summary = %+v", feature)
	}
	declaration := feature.Declarations[0]
	if declaration.Kind != featureAuthorityManifest || declaration.Source != "tooling/putnami.features.json" || declaration.Type != featureproto.FeatureTypeFeature || declaration.Target != featureproto.MaturityCoded {
		t.Fatalf("manifest declaration = %+v", declaration)
	}
	if declaration.Provenance == nil || declaration.Provenance.Path != declaration.Source || declaration.Provenance.Symbol != "features[0]" {
		t.Fatalf("manifest provenance = %+v", declaration.Provenance)
	}
	if len(declaration.Relations) != 1 || len(declaration.Requirements) != 1 {
		t.Fatalf("durable manifest fields were not preserved: %+v", declaration)
	}

	context, err := BuildAgentFeatureContextResult(ws, "release-status")
	if err != nil {
		t.Fatal(err)
	}
	if context.ID != feature.ID || len(context.Declarations) != 1 || len(context.Implementations) != 0 {
		t.Fatalf("manifest-only feature context = %+v", context)
	}
}

func TestFeatureMCPAuthorityMergePreservesCollisionsAndDegradesAroundInvalidManifest(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"mixed-test","includes":["consumer","invalid","tooling"]}`)
	for _, project := range []string{"consumer", "invalid", "tooling"} {
		writeFixtureFile(t, filepath.Join(root, project, "putnami.json"),
			`{"name":"`+project+`","type":"application"}`)
	}
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "consumer",
		Nodes: []featureproto.DesignNode{
			{
				ID: "feature:shared/status", Kind: featureproto.DesignNodeFeature, Name: "Native status",
				Properties: map[string]string{"outcome": "Operators see native status", "owner": "runtime"},
				Provenance: &featureproto.DesignProvenance{Path: "status/module.go", Line: 12, Symbol: "StatusFeature"},
			},
			// A technical node can look like a feature by filename or ID and must
			// remain technical because its declared kind is not feature.
			{ID: "feature:spec/invented", Kind: featureproto.DesignNodeConfig, Name: "feature.spec.json"},
		},
		Edges: []featureproto.DesignEdge{},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "consumer", ".gen", featureproto.DesignGraphArtifact), string(encoded))
	writeFixtureFile(t, filepath.Join(root, "tooling", featureproto.ManifestFilename), `{
  "protocolVersion":1,
  "namespace":"shared",
  "features":[{
    "id":"shared/status","type":"feature","name":"Durable status",
    "outcome":"Maintainers see durable status","owner":"release","target":"modeled"
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, "tooling", "spec", featureproto.ManifestFilename),
		`{"protocolVersion":1,"namespace":"spec","features":[{"id":"spec/nested","type":"feature","name":"Nested","outcome":"Must stay hidden","owner":"nobody","target":"modeled"}]}`)
	writeFixtureFile(t, filepath.Join(root, "invalid", featureproto.ManifestFilename),
		`{"protocolVersion":1,"namespace":"invalid","features":[],"unknown":true}`)

	ws := fixtureWorkspace("mixed-test", root,
		appProject("consumer", "consumer"), appProject("invalid", "invalid"), appProject("tooling", "tooling"))

	catalog, err := BuildFeatureCatalogResult(ws, "", Selection{})
	if err != nil {
		t.Fatalf("invalid manifest must not hide healthy authorities: %v", err)
	}
	if catalog.Graphs != 1 || catalog.Manifests != 1 || len(catalog.Features) != 1 {
		t.Fatalf("mixed catalog = %+v", catalog)
	}
	feature := catalog.Features[0]
	if feature.ID != "shared/status" || feature.Name != "Native status" || len(feature.Declarations) != 2 {
		t.Fatalf("mixed feature = %+v", feature)
	}
	if got := strings.Join(feature.Projects, ","); got != "consumer,tooling" {
		t.Fatalf("projects = %q", got)
	}
	if got := strings.Join(feature.ConflictingSources, ","); got != "tooling/putnami.features.json" {
		t.Fatalf("conflicting sources = %q", got)
	}
	if feature.Declarations[0].Source != "consumer/.gen/"+featureproto.DesignGraphArtifact || feature.Declarations[1].Source != "tooling/putnami.features.json" {
		t.Fatalf("declaration order/provenance = %+v", feature.Declarations)
	}
	if len(catalog.Unreadable) != 1 || catalog.Unreadable[0].Path != "invalid/putnami.features.json" || !strings.Contains(catalog.Unreadable[0].Reason, featureproto.ErrorCodeUnknownField) {
		t.Fatalf("unreadable = %+v", catalog.Unreadable)
	}
	filtered, err := BuildFeatureCatalogResult(ws, "durable release", Selection{})
	if err != nil || len(filtered.Features) != 1 || filtered.Features[0].ID != feature.ID {
		t.Fatalf("query must match every preserved declaration: catalog=%+v err=%v", filtered, err)
	}

	context, err := BuildAgentFeatureContextResult(ws, "shared/status")
	if err != nil {
		t.Fatalf("collision must remain inspectable: %v", err)
	}
	if context.Name != "Native status" || len(context.Declarations) != 2 || len(context.Implementations) != 1 || context.Implementations[0].Project != "consumer" {
		t.Fatalf("mixed feature context = %+v", context)
	}
	if got := strings.Join(context.ConflictingSources, ","); got != "tooling/putnami.features.json" {
		t.Fatalf("context conflicting sources = %q", got)
	}
}

// writeFeatureDesignGraph writes one project's native design graph fixture with
// a single feature node.
func writeFeatureDesignGraph(t *testing.T, root, project, featureID, name string) {
	t.Helper()
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       project,
		Nodes: []featureproto.DesignNode{{
			ID: "feature:" + featureID, Kind: featureproto.DesignNodeFeature, Name: name,
		}},
		Edges: []featureproto.DesignEdge{},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, project, ".gen", featureproto.DesignGraphArtifact), string(encoded))
}

// TestBuildFeatureCatalogDegradesAroundUnreadableAndDivergentGraphs pins the
// discovery contract: list_features is the call an agent makes when it does not
// yet know the feature id, so a stale artifact in one project and a renamed
// declaration in another must not make every healthy feature invisible.
func TestBuildFeatureCatalogDegradesAroundUnreadableAndDivergentGraphs(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"design-test","includes":["healthy","stale","producer","consumer"]}`)
	for _, project := range []string{"healthy", "stale", "producer", "consumer"} {
		writeFixtureFile(t, filepath.Join(root, project, "putnami.json"),
			`{"name":"`+project+`","type":"application"}`)
	}
	writeFeatureDesignGraph(t, root, "healthy", "billing/invoices", "Invoices")
	writeFeatureDesignGraph(t, root, "producer", "auth/tokens", "Tokens")
	writeFeatureDesignGraph(t, root, "consumer", "auth/tokens", "Tokens (renamed)")
	// A graph left behind by an older CLI: present, parseable JSON, unsupported
	// compatibility marker.
	writeFixtureFile(t, filepath.Join(root, "stale", ".gen", featureproto.DesignGraphArtifact),
		`{"compatibility":"provisional-0","project":"stale","nodes":[],"edges":[]}`)

	ws := fixtureWorkspace("design-test", root, appProject("healthy", "healthy"), appProject("stale", "stale"),
		appProject("producer", "producer"), appProject("consumer", "consumer"))

	catalog, err := BuildFeatureCatalogResult(ws, "", Selection{})
	if err != nil {
		t.Fatalf("list_features must stay available while graphs are degraded: %v", err)
	}
	ids := make([]string, 0, len(catalog.Features))
	for _, feature := range catalog.Features {
		ids = append(ids, feature.ID)
	}
	if strings.Join(ids, ",") != "auth/tokens,billing/invoices" {
		t.Fatalf("catalog features = %v, want both the divergent and the unrelated healthy feature", ids)
	}
	if catalog.Graphs != 3 {
		t.Errorf("readable graphs = %d, want 3", catalog.Graphs)
	}

	// The divergence is reported on the feature it belongs to, first
	// declaration in path order wins, and the healthy feature is untouched.
	divergent := catalog.Features[0]
	if divergent.Name != "Tokens (renamed)" || strings.Join(divergent.Conflicts, ",") != "producer" {
		t.Errorf("divergent summary = %+v, want the first graph in path order to win and name the other project", divergent)
	}
	if len(catalog.Features[1].Conflicts) != 0 {
		t.Errorf("unrelated feature carries conflicts: %+v", catalog.Features[1])
	}

	// The skipped artifact is named rather than silently dropped.
	if len(catalog.Unreadable) != 1 ||
		catalog.Unreadable[0].Path != "stale/.gen/"+featureproto.DesignGraphArtifact ||
		!strings.Contains(catalog.Unreadable[0].Reason, "provisional-0") {
		t.Fatalf("unreadable = %+v", catalog.Unreadable)
	}
	if strings.Contains(catalog.Unreadable[0].Path, root) {
		t.Errorf("unreadable path leaked an absolute path: %q", catalog.Unreadable[0].Path)
	}

	// Filtering still works over the degraded catalog.
	filtered, err := BuildFeatureCatalogResult(ws, "billing", Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Features) != 1 || filtered.Features[0].ID != "billing/invoices" {
		t.Fatalf("filtered catalog = %+v", filtered.Features)
	}
}

// TestBuildFeatureCatalogSkipsUnprefixedFeatureNodes keeps the two native
// readers in agreement. inspectNativeFeatureDesign has always skipped feature
// nodes without the feature:<id> identity and ValidateDesignGraph accepts them,
// so the catalog must skip them too rather than treat the same graph as fatal.
func TestBuildFeatureCatalogSkipsUnprefixedFeatureNodes(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","type":"application"}`)
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "app",
		Nodes: []featureproto.DesignNode{
			{ID: "auth/opaque-tokens", Kind: featureproto.DesignNodeFeature, Name: "Unprefixed"},
			{ID: "feature:", Kind: featureproto.DesignNodeFeature, Name: "Empty id"},
			{ID: "feature:billing/invoices", Kind: featureproto.DesignNodeFeature, Name: "Invoices"},
		},
		Edges: []featureproto.DesignEdge{},
	}
	if err := featureproto.ValidateDesignGraph(graph); err != nil {
		t.Fatalf("fixture must stay protocol-valid for this test to mean anything: %v", err)
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "app", ".gen", featureproto.DesignGraphArtifact), string(encoded))

	ws := fixtureWorkspace("design-test", root, appProject("app", "app"))

	catalog, err := BuildFeatureCatalogResult(ws, "", Selection{})
	if err != nil {
		t.Fatalf("a graph features inspect tolerates must not fail the catalog: %v", err)
	}
	if len(catalog.Features) != 1 || catalog.Features[0].ID != "billing/invoices" {
		t.Fatalf("catalog features = %+v, want only the well-formed declaration", catalog.Features)
	}
}

// TestCompactFeatureContextBucketsEveryDesignNodeKind fails when the protocol
// gains a node kind that feature_context does not categorize. Without it a new
// kind disappears from the compact projection with no compile error.
func TestCompactFeatureContextBucketsEveryDesignNodeKind(t *testing.T) {
	bucketed := make(map[featureproto.DesignNodeKind]bool, len(agentFeatureFactBuckets))
	for _, bucket := range agentFeatureFactBuckets {
		for _, kind := range bucket.kinds {
			if bucketed[kind] {
				t.Errorf("node kind %q is bucketed twice", kind)
			}
			bucketed[kind] = true
		}
	}
	for _, kind := range featureproto.OrderedDesignNodeKinds() {
		// The feature node is the report root (id/name/outcome/owner), not a
		// technical fact, so it is the one deliberate exclusion.
		if kind == featureproto.DesignNodeFeature {
			continue
		}
		if !bucketed[kind] {
			t.Errorf("design node kind %q has no compact feature_context bucket", kind)
		}
		delete(bucketed, kind)
	}
	delete(bucketed, featureproto.DesignNodeFeature)
	for kind := range bucketed {
		t.Errorf("compact bucket %q is not a supported design node kind", kind)
	}
}

func TestFeatureContextProjectsNativeCompositionKinds(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"design-test","includes":["tasks"]}`)
	writeFixtureFile(t, filepath.Join(root, "tasks", "putnami.json"), `{"name":"tasks","type":"application"}`)

	// Three levels up, not five: this package sits at
	// tooling/sdd-extension/internal/sdd rather than
	// tooling/cli/internal/commands/sdd.
	fixturePath := filepath.Join("..", "..", "..", "..", "protocols", "features", "fixtures", "equivalence", "go-typescript-design.golden.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := featureproto.ParseDesignGraph(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for index := range graph.Edges {
		if graph.Edges[index].From == "module:tasks/tasks" && graph.Edges[index].To == "config:tasks" {
			graph.Edges[index].Provenance = &featureproto.DesignProvenance{Path: "tasks/config.go", Symbol: "ConfigDefinitions"}
		}
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "tasks", ".gen", featureproto.DesignGraphArtifact), string(encoded))

	ws := fixtureWorkspace("design-test", root, appProject("tasks", "tasks"))

	context, err := BuildAgentFeatureContextResult(ws, "tasks/manage")
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Implementations) != 1 {
		t.Fatalf("implementations = %+v", context.Implementations)
	}
	implementation := context.Implementations[0]
	if len(implementation.Config) != 1 || len(implementation.Infra) != 1 || len(implementation.Lifecycle) != 3 || len(implementation.Tests) != 1 {
		t.Fatalf("native fact buckets = config %+v infra %+v lifecycle %+v tests %+v", implementation.Config, implementation.Infra, implementation.Lifecycle, implementation.Tests)
	}
	config := implementation.Config[0]
	if config.ID != "config:tasks" || config.Authority != string(featureproto.DesignAuthorityExact) || config.Properties["path"] != "tasks" || config.Provenance == nil || config.Provenance.Path != "tasks/config.go" {
		t.Fatalf("config fact lost identity, authority, properties, or provenance: %+v", config)
	}
	if implementation.Infra[0].ID != "infra:database:tasks" || implementation.Infra[0].Properties["kind"] != "database" {
		t.Fatalf("infra fact = %+v", implementation.Infra[0])
	}
	if implementation.Tests[0].ID != "test:integration:tasks/create" || implementation.Tests[0].Properties["kind"] != "integration" {
		t.Fatalf("test fact = %+v", implementation.Tests[0])
	}

}

func TestFeaturesInspectRejectsUnboundedSelectorWithoutEchoingIt(t *testing.T) {
	ws := featureCommandsWorkspace(t, false)
	selector := strings.Repeat("private", featureSelectorMaxBytes)

	_, err := BuildFeatureContextResult(ws, selector)
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("features inspect error = %v, want ErrUsage", err)
	}
	report, ok := ResultData(err).(FeatureInspectionReport)
	if !ok {
		t.Fatalf("failure data = %T, want FeatureInspectionReport", ResultData(err))
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if report.Requested != "" || bytes.Contains(encoded, []byte(selector)) {
		t.Fatalf("invalid selector leaked into typed failure data: %s", encoded)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != featureproto.ErrorCodeInvalidID {
		t.Fatalf("invalid selector diagnostics = %+v", report.Diagnostics)
	}
}

func featureCommandsWorkspace(t *testing.T, invalid bool) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"features-test","includes":["app","orders"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "orders", "putnami.json"), `{"name":"orders","type":"application"}`)
	manifest := `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/checkout",
    "type": "feature",
    "name": "Checkout",
    "outcome": "Customers can pay",
    "owner": "payments",
    "target": "coded",
    "requirements": [{"id":"implementation","stage":"coded","evidenceKinds":["capability"]}]
  }]
}`
	if invalid {
		manifest = `{"protocolVersion":1,"namespace":"billing","features":[],"unknown":true}`
	}
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), manifest)
	writeFixtureFile(t, filepath.Join(root, "orders", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "orders",
  "features": [{
    "id": "orders/checkout",
    "type": "feature",
    "name": "Order checkout",
    "outcome": "Orders complete",
    "owner": "orders",
    "target": "modeled"
  }]
}`)
	return fixtureWorkspace("features-test", root, appProject("app", "app"), appProject("orders", "orders"))
}

func initFeatureCommandsGit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		runFeatureCommandsGit(t, root, args...)
	}
}

func commitFeatureCommandsGit(t *testing.T, root, message string) {
	t.Helper()
	runFeatureCommandsGit(t, root, "add", "-A")
	runFeatureCommandsGit(t, root, "commit", "-q", "-m", message)
}

func runFeatureCommandsGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
