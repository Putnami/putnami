package infraagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/infra"
	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
)

// --- test helpers ---------------------------------------------------------

// closureContext builds the job context an orchestrator hands an aggregating
// task: the workload, its resolved type, and its dependency closure (the
// workload first, then its dependencies, exactly as core resolves it).
func closureContext(root, workloadPath, workloadName, projectType string, deps ...string) *pctx.Context {
	closure := make([]pctx.ProjectRef, 0, 1+len(deps))
	closure = append(closure, pctx.ProjectRef{
		ID:       "/" + workloadPath,
		Name:     workloadName,
		Path:     workloadPath,
		FullPath: filepath.Join(root, workloadPath),
	})
	for _, dep := range deps {
		closure = append(closure, pctx.ProjectRef{
			ID:       "/" + dep,
			Name:     dep,
			Path:     dep,
			FullPath: filepath.Join(root, dep),
		})
	}
	return &pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{
			Name:              workloadName,
			Path:              workloadPath,
			FullPath:          filepath.Join(root, workloadPath),
			Type:              projectType,
			DependencyClosure: closure,
		},
	}
}

func writeProjectFile(t *testing.T, root, projPath, rel, content string) {
	t.Helper()
	full := filepath.Join(root, projPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func aggregatedPath(root, projPath string) string {
	return filepath.Join(root, projPath, ".gen", "requirements.json")
}

// readAggregated reads the emitted manifest and round-trips it through the
// protocol's strict parser+validator, failing the test if it is malformed.
func readAggregated(t *testing.T, root, projPath string) *infra.AggregatedManifest {
	t.Helper()
	data, err := os.ReadFile(aggregatedPath(root, projPath))
	if err != nil {
		t.Fatalf("read aggregated manifest: %v", err)
	}
	m, diags := infra.ParseAndValidateAggregatedManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("emitted manifest failed strict parse/validate: %v", diags)
	}
	return m
}

func hasDiagCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// noHTTP2 is the TypeScript extension's runtime compatibility hook, stated here
// as the SDK's own fixture: Bun-backed services do not serve h2c, so a TS
// workload must opt out whatever the source of its runtime block.
func noHTTP2(rt *infra.Runtime) {
	http2 := false
	if rt.Protocols == nil {
		rt.Protocols = &infra.RuntimeProtocols{}
	}
	rt.Protocols.HTTP2 = &http2
}

const dbManifest = `{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres","schemas":["iam"]}]}`

// --- Aggregate ------------------------------------------------------------

func TestAggregate_EmptyWorkloadEmitsRuntimeDefaults(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")

	result := Aggregate(ctx, Options{})

	if len(result.Diagnostics) != 0 {
		t.Errorf("diags = %v, want none for a workload with no contributions", result.Diagnostics)
	}
	if result.Outcome != OutcomeEmitted {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeEmitted)
	}
	// A workload with no requirements still emits a manifest carrying the
	// framework's runtime defaults so deployers always have a runtime block.
	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Scaling == nil || m.Runtime.Scaling.Max == nil {
		t.Fatalf("runtime defaults not present: %+v", m.Runtime)
	}
	if *m.Runtime.Scaling.Max != infra.DefaultScalingMax {
		t.Errorf("scaling.max = %d, want default %d", *m.Runtime.Scaling.Max, infra.DefaultScalingMax)
	}
	if len(m.Databases) != 0 || len(m.Storage) != 0 || len(m.Secrets) != 0 {
		t.Errorf("expected only runtime block, got %+v", m)
	}
}

func TestAggregate_SingleContribution(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "aggregation-merges-the-dependency-closure")
	root := t.TempDir()
	ctx := closureContext(root, "app", "go.putnami.dev/app", "application", "lib")
	ctx.Project.DependencyClosure[1].Name = "go.putnami.dev/lib"

	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)

	result := Aggregate(ctx, Options{})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	if result.Contributions != 1 {
		t.Errorf("contributions = %d, want 1", result.Contributions)
	}

	m := readAggregated(t, root, "app")
	// The workload and its sources are named by project id, not by name.
	if m.Workload != "app" {
		t.Errorf("workload = %q, want the project id app", m.Workload)
	}
	if m.Schema != AggregatedSchemaURL {
		t.Errorf("$schema = %q, want %q", m.Schema, AggregatedSchemaURL)
	}
	if len(m.Databases) != 1 {
		t.Fatalf("databases = %d, want 1", len(m.Databases))
	}
	db := m.Databases[0]
	if db.Name != "primary" || db.Engine != infra.EnginePostgres {
		t.Errorf("database = %+v, want primary/postgres", db)
	}
	if len(db.Sources) != 1 || db.Sources[0].Project != "lib" ||
		db.Sources[0].Contributor != infra.GeneratedRequirementsContributor {
		t.Errorf("sources = %+v, want [{lib %s}]", db.Sources, infra.GeneratedRequirementsContributor)
	}

	// Atomic write must not leave its temp file behind.
	if _, err := os.Stat(aggregatedPath(root, "app") + ".tmp"); err == nil {
		t.Error("temp file left behind after atomic write")
	}
}

// A deployer matches the declaration to the release-set member of the same
// workload, and that member names the project by id. A project whose name is
// not its path, or whose path holds a grouping folder the id drops, must still
// be named by id: here "sites/(web)/docs" has the id "/sites/docs" and the
// name "docs.example". The closure is in id order, so the workload is not its
// first entry.
func TestAggregate_NamesTheWorkloadAndSourcesByProjectID(t *testing.T) {
	root := t.TempDir()
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{
			Name:     "docs.example",
			Path:     "sites/(web)/docs",
			FullPath: filepath.Join(root, "sites/(web)/docs"),
			Type:     "application",
			DependencyClosure: []pctx.ProjectRef{
				{ID: "/libs/content", Name: "@example/content", Path: "libs/(internal)/content",
					FullPath: filepath.Join(root, "libs/(internal)/content")},
				{ID: "/sites/docs", Name: "docs.example", Path: "sites/(web)/docs",
					FullPath: filepath.Join(root, "sites/(web)/docs")},
			},
		},
	}
	writeProjectFile(t, root, "libs/(internal)/content", "infra/requirements.json", dbManifest)

	result := Aggregate(ctx, Options{})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	m := readAggregated(t, root, "sites/(web)/docs")
	if m.Workload != distribution.MemberProjectID("/sites/docs") || m.Workload != "sites/docs" {
		t.Errorf("workload = %q, want the release-set member project sites/docs", m.Workload)
	}
	if len(m.Databases) != 1 || len(m.Databases[0].Sources) != 1 ||
		m.Databases[0].Sources[0].Project != "libs/content" {
		t.Errorf("databases = %+v, want one source named libs/content", m.Databases)
	}
}

// The typed task identity is the first answer, so a context whose closure does
// not list the workload still names it by id.
func TestWorkloadID_PrefersTheProjectScopedIdentity(t *testing.T) {
	ctx := &pctx.Context{Project: pctx.Project{Name: "docs.example", Path: "sites/(web)/docs"}}
	ctx.Identity = &job.TaskIdentity{Scope: job.TaskScopeProject}
	ctx.Identity.Project.ID = "/sites/docs"
	if got := workloadID(ctx); got != "sites/docs" {
		t.Errorf("workloadID = %q, want sites/docs", got)
	}

	// A workspace-scoped identity names the workspace, not the project: the
	// closure answers instead, and without one the path does.
	ctx.Identity.Scope = job.TaskScopeWorkspace
	ctx.Identity.Project.ID = "/"
	if got := workloadID(ctx); got != "sites/(web)/docs" {
		t.Errorf("workloadID without closure = %q, want the path sites/(web)/docs", got)
	}

	// A project at the workspace root has no id to name it by and keeps its name.
	ctx.Identity = nil
	ctx.Project.Path = ""
	if got := workloadID(ctx); got != "docs.example" {
		t.Errorf("workloadID at the root = %q, want the name docs.example", got)
	}
}

func TestAggregate_NormalizesV1GeneratedContribution(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-v1-generated-manifest-is-normalized")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":1,"databases":[{"name":"primary","engine":"postgres"}]}`)

	result := Aggregate(ctx, Options{})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeInvalidProtocolVersion) {
		t.Fatalf("diags = %v, want a generated-manifest migration warning", result.Diagnostics)
	}
	if result.Contributions != 1 {
		t.Fatalf("contributions = %d, want the v1 generated manifest to contribute", result.Contributions)
	}

	m := readAggregated(t, root, "app")
	if m.ProtocolVersion != infra.ProtocolVersion || len(m.Databases) != 1 || m.Databases[0].Name != "primary" {
		t.Fatalf("aggregated manifest = %+v, want a v%d primary database", m, infra.ProtocolVersion)
	}
}

// A library is consumed, never deployed. The workload gate lives here — one
// rule for every language — so no extension has to re-derive it.
func TestAggregate_SkipsLibraries(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "libraries-are-skipped")
	root := t.TempDir()
	ctx := closureContext(root, "lib", "lib", "library")
	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)

	result := Aggregate(ctx, Options{})

	if result.Outcome != OutcomeSkipped {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeSkipped)
	}
	if _, err := os.Stat(aggregatedPath(root, "lib")); !os.IsNotExist(err) {
		t.Errorf("a library must not receive an aggregated manifest, stat err = %v", err)
	}
}

// An unclassified project is a workload: that is the orchestrator's own
// default, and disagreeing with it would silently stop emitting manifests for
// every project that never wrote a type.
func TestAggregate_UnclassifiedProjectIsAWorkload(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "an-unclassified-project-is-treated-as-a-workload")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "")

	if result := Aggregate(ctx, Options{}); result.Outcome != OutcomeEmitted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeEmitted)
	}
	readAggregated(t, root, "app")
}

func TestAggregate_IgnoresEphemeralGeneratorScratch(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "generator-scratch-is-ignored")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	// The committed generator-owned manifest is the source of truth. The
	// per-producer .gen/infra sidecar is only generator scratch and must not be
	// read by deploy aggregation directly.
	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", ".gen/infra/storage.json",
		`{"protocolVersion":2,"storage":[{"name":"scratch-only"}]}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if len(m.Databases) != 1 {
		t.Fatalf("databases = %d, want committed requirement only", len(m.Databases))
	}
	if len(m.Storage) != 0 {
		t.Fatalf("ephemeral scratch sidecar was merged directly: %+v", m.Storage)
	}
}

func TestAggregate_ConflictDiagnostic(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-conflict-is-reported-as-a-diagnostic")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "liba", "libb")

	writeProjectFile(t, root, "liba", "infra/requirements.json",
		`{"protocolVersion":2,"storage":[{"name":"assets","retention":"30d"}]}`)
	writeProjectFile(t, root, "libb", "infra/requirements.json",
		`{"protocolVersion":2,"storage":[{"name":"assets","retention":"90d"}]}`)

	result := Aggregate(ctx, Options{})
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeConflictingValue) {
		t.Errorf("diags = %v, want a %s finding", result.Diagnostics, infra.ErrorCodeConflictingValue)
	}

	// The manifest is still emitted; the prior (closure-order) value is kept.
	m := readAggregated(t, root, "app")
	if len(m.Storage) != 1 {
		t.Fatalf("storage = %d, want 1", len(m.Storage))
	}
	if m.Storage[0].Retention != "30d" {
		t.Errorf("retention = %q, want 30d (prior kept on conflict)", m.Storage[0].Retention)
	}
}

func TestAggregate_RuntimeFromFile(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":10,"concurrency":50}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Ingress == nil || m.Runtime.Scaling == nil {
		t.Fatalf("runtime = %+v, want ingress + scaling populated", m.Runtime)
	}
	if m.Runtime.Ingress.Domain == nil || *m.Runtime.Ingress.Domain != "api.example.com" {
		t.Errorf("ingress.domain = %v, want api.example.com", m.Runtime.Ingress.Domain)
	}
	if m.Runtime.Scaling.Max == nil || *m.Runtime.Scaling.Max != 10 {
		t.Errorf("scaling.max = %v, want 10", m.Runtime.Scaling.Max)
	}
}

// The language's compatibility hook reaches the synthesized defaults.
func TestAggregate_CompatibilityHookAppliesToDefaults(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-compatibility-hook-applies-to-runtime-defaults")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	result := Aggregate(ctx, Options{RuntimeCompatibility: noHTTP2})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	defaultsPath := filepath.Join(root, "app", ".gen", "infra", "runtime.json")
	data, err := os.ReadFile(defaultsPath)
	if err != nil {
		t.Fatalf("expected runtime defaults sidecar at %s: %v", defaultsPath, err)
	}
	var rt infra.Runtime
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal defaults sidecar: %v", err)
	}
	if rt.Protocols == nil || rt.Protocols.HTTP2 == nil || *rt.Protocols.HTTP2 {
		t.Errorf("runtime.protocols.http2 = %+v, want false", rt.Protocols)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Protocols == nil || m.Runtime.Protocols.HTTP2 == nil || *m.Runtime.Protocols.HTTP2 {
		t.Errorf("aggregated runtime.protocols.http2 = %+v, want false", m.Runtime)
	}
}

// ...and to a developer-authored runtime, so an authored file cannot request a
// runtime the language cannot serve.
func TestAggregate_CompatibilityHookAppliesToAuthoredRuntime(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-compatibility-hook-applies-to-an-authored-runtime")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domain":"api.example.com"},"protocols":{"http2":true}}`)

	result := Aggregate(ctx, Options{RuntimeCompatibility: noHTTP2})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Protocols == nil || m.Runtime.Protocols.HTTP2 == nil || *m.Runtime.Protocols.HTTP2 {
		t.Errorf("aggregated runtime.protocols.http2 = %+v, want false", m.Runtime)
	}
}

// No hook means no language constraint: the runtime block is exactly what the
// framework defaults and the authored file say.
func TestAggregate_NoCompatibilityHookLeavesProtocolsAlone(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime != nil && m.Runtime.Protocols != nil && m.Runtime.Protocols.HTTP2 != nil {
		t.Errorf("runtime.protocols.http2 = %v, want unset without a language hook", *m.Runtime.Protocols.HTTP2)
	}
}

func TestAggregate_RuntimeSignBlobSurvivesAggregation(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	// A developer declares the workload signs as its own runtime identity
	// (e.g. object-storage presigned URLs). The strict runtime.json loader
	// must accept the key, and the aggregator must carry it into the manifest
	// a deploy target consumes — otherwise the deployer never sees the intent.
	writeProjectFile(t, root, "app", "infra/runtime.json", `{"security":{"signBlob":true}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Security == nil {
		t.Fatalf("runtime.security = %+v, want it populated", m.Runtime)
	}
	if m.Runtime.Security.SignBlob == nil || !*m.Runtime.Security.SignBlob {
		t.Errorf("security.signBlob = %v, want true to survive aggregation", m.Runtime.Security.SignBlob)
	}
}

func TestAggregate_RuntimeEventsGatewaySurvivesAggregation(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/runtime.json", `{"security":{"eventsGateway":true}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Security == nil {
		t.Fatalf("runtime.security = %+v, want it populated", m.Runtime)
	}
	if m.Runtime.Security.EventsGateway == nil || !*m.Runtime.Security.EventsGateway {
		t.Errorf("security.eventsGateway = %v, want true to survive aggregation", m.Runtime.Security.EventsGateway)
	}
}

func TestAggregate_RuntimeDefaultsWrittenAsSidecar(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	// No developer-authored runtime.json — the framework synthesizes defaults
	// and writes a visible sidecar so operators can see what will deploy.
	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	defaultsPath := filepath.Join(root, "app", ".gen", "infra", "runtime.json")
	data, err := os.ReadFile(defaultsPath)
	if err != nil {
		t.Fatalf("expected runtime defaults sidecar at %s: %v", defaultsPath, err)
	}
	var rt infra.Runtime
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal defaults sidecar: %v", err)
	}
	if rt.Scaling == nil || rt.Scaling.Max == nil || *rt.Scaling.Max != infra.DefaultScalingMax {
		t.Errorf("scaling.max = %+v, want default %d", rt.Scaling, infra.DefaultScalingMax)
	}
	if rt.Ingress == nil || rt.Ingress.Public == nil || *rt.Ingress.Public != infra.DefaultIngressPublic {
		t.Errorf("ingress.public = %+v, want default %v", rt.Ingress, infra.DefaultIngressPublic)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Scaling == nil || *m.Runtime.Scaling.Max != infra.DefaultScalingMax {
		t.Errorf("aggregated runtime did not pick up defaults: %+v", m.Runtime)
	}
}

func TestAggregate_DeveloperRuntimeRemovesDefaultsSidecar(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	// A prior build left a defaults sidecar on disk.
	writeProjectFile(t, root, "app", ".gen/infra/runtime.json",
		`{"scaling":{"max":1,"concurrency":500},"ingress":{"public":false}}`)
	// The developer now authors their own runtime.json — defaults must go.
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":50,"concurrency":80}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}

	defaultsPath := filepath.Join(root, "app", ".gen", "infra", "runtime.json")
	if _, err := os.Stat(defaultsPath); !os.IsNotExist(err) {
		t.Errorf("stale runtime defaults sidecar was not removed, stat err = %v", err)
	}

	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Scaling == nil || *m.Runtime.Scaling.Max != 50 {
		t.Errorf("developer runtime did not win over defaults: %+v", m.Runtime)
	}
}

func TestAggregate_RuntimeNegativeScalingDiag(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/runtime.json", `{"scaling":{"max":-1}}`)

	result := Aggregate(ctx, Options{})
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeInvalidScaling) {
		t.Errorf("diags = %v, want a %s finding", result.Diagnostics, infra.ErrorCodeInvalidScaling)
	}
}

// Minimum residency is deployer-owned cost policy, so a workload that authors
// scaling.min is rejected by the strict runtime reader. What the rejection
// costs is the point of this test: the reader fails the whole file, so the
// workload also loses the ingress and security intent it declared alongside the
// bad key — and Job still reports OK. Pinning the amputation keeps the blast
// radius visible to whoever next changes the Runtime types, and pinning the
// message keeps the author's only signal from decaying back into
// encoding/json's bare "unknown field".
func TestAggregate_RuntimeScalingMinIsRejectedAndCostsTheWholeBlock(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domains":["api.example.com"]},`+
			`"security":{"platformAuth":"disabled"},`+
			`"scaling":{"min":1,"max":10}}`)

	result := Aggregate(ctx, Options{})
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeUnknownField) {
		t.Fatalf("diags = %v, want a %s finding for a runtime.json carrying scaling.min",
			result.Diagnostics, infra.ErrorCodeUnknownField)
	}
	msg := result.Diagnostics[0].Message
	for _, want := range []string{"min", "deployer-owned cost policy", infra.MigrationGuide, "runtime.json"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if m := readAggregated(t, root, "app"); m.Runtime != nil {
		t.Errorf("runtime = %+v, want nil: one rejected key drops ingress and security with it", m.Runtime)
	}
}

// The workload-owned ceilings survive the same edit, so the guard above is
// pinning the cost-policy rejection rather than a runtime block that stopped
// working altogether.
func TestAggregate_RuntimeKeepsWorkloadOwnedCeilings(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domains":["api.example.com"]},`+
			`"security":{"platformAuth":"disabled"},`+
			`"scaling":{"max":10,"concurrency":50}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Scaling == nil {
		t.Fatalf("runtime = %+v, want a scaling block", m.Runtime)
	}
	if m.Runtime.Scaling.Max == nil || *m.Runtime.Scaling.Max != 10 {
		t.Errorf("scaling.max = %v, want 10", m.Runtime.Scaling.Max)
	}
	if m.Runtime.Scaling.Concurrency == nil || *m.Runtime.Scaling.Concurrency != 50 {
		t.Errorf("scaling.concurrency = %v, want 50", m.Runtime.Scaling.Concurrency)
	}
	if m.Runtime.Security == nil || m.Runtime.Security.PlatformAuth == nil ||
		*m.Runtime.Security.PlatformAuth != "disabled" {
		t.Errorf("security = %+v, want platformAuth disabled to survive alongside the ceilings", m.Runtime.Security)
	}
}

func TestAggregate_InvalidContributionDiag(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")

	// Unknown field → strict parse rejects; the project contributes nothing
	// and the failure is surfaced (attributed to the project).
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databazes":[]}`)

	result := Aggregate(ctx, Options{})
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeUnknownField) {
		t.Errorf("diags = %v, want a %s finding", result.Diagnostics, infra.ErrorCodeUnknownField)
	}
	// The bad contribution is skipped; the workload still emits a manifest
	// with the framework's runtime defaults.
	m := readAggregated(t, root, "app")
	if m.Runtime == nil {
		t.Error("expected runtime defaults in the manifest despite parse failure")
	}
	if len(m.Databases) != 0 {
		t.Errorf("expected no databases (only contribution failed to parse), got %+v", m.Databases)
	}
	// Diagnostic must name the contributing project for traceability.
	var found bool
	for _, d := range result.Diagnostics {
		if strings.Contains(d.Message, "project lib") {
			found = true
		}
	}
	if !found {
		t.Errorf("diags = %v, want message attributed to 'project lib'", result.Diagnostics)
	}
}

func TestAggregate_RuntimeOnlyEmits(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	// No infra/requirements.json anywhere — only a workload runtime block.
	writeProjectFile(t, root, "app", "infra/runtime.json", `{"ingress":{"public":true},"scaling":{"max":2}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	m := readAggregated(t, root, "app")
	if m.Runtime == nil || m.Runtime.Scaling == nil || m.Runtime.Scaling.Max == nil || *m.Runtime.Scaling.Max != 2 {
		t.Errorf("runtime = %+v, want scaling.max=2 (runtime-only workload must still emit)", m.Runtime)
	}
}

func TestAggregate_RuntimeDefaultsReplaceStale(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	// A manifest from a prior build, but the workload now declares nothing.
	writeProjectFile(t, root, "app", ".gen/requirements.json",
		`{"protocolVersion":2,"workload":"app","databases":[{"name":"old","engine":"postgres",`+
			`"sources":[{"project":"app","contributor":"manual"}]}]}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	// Stale resource entries are dropped; the workload's manifest now
	// carries only the framework's runtime defaults.
	m := readAggregated(t, root, "app")
	if len(m.Databases) != 0 {
		t.Errorf("stale databases not dropped, got %+v", m.Databases)
	}
	if m.Runtime == nil {
		t.Error("expected runtime defaults in the replacement manifest")
	}
}

func TestAggregate_OverridesSuppressRequirement(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres"}],"secrets":["jwks"]}`)
	// The workload owner suppresses the database a library declared.
	writeProjectFile(t, root, "app", "infra/overrides.json",
		`{"ignore":{"databases":[{"name":"primary","engine":"postgres"}]}}`)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	m := readAggregated(t, root, "app")
	if len(m.Databases) != 0 {
		t.Errorf("database not suppressed by overrides: %+v", m.Databases)
	}
	if len(m.Secrets) != 1 {
		t.Errorf("unrelated secret should remain: %+v", m.Secrets)
	}
}

func TestAggregate_MalformedOverridesDiag(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", "infra/overrides.json", `{"ignore":{},"bogus":1}`)

	result := Aggregate(ctx, Options{})
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeUnknownField) {
		t.Errorf("want unknown_field diag for malformed overrides, got %v", result.Diagnostics)
	}
	// Manifest still emitted, unchanged, when overrides fail to parse.
	m := readAggregated(t, root, "app")
	if len(m.Databases) != 1 {
		t.Errorf("db should remain when overrides fail to parse: %+v", m.Databases)
	}
}

// A closure the orchestrator did not resolve must not silently erase the
// workload's OWN declaration: the fallback reads the project itself.
func TestAggregate_NoClosureStillReadsOwnRequirements(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	ctx.Project.DependencyClosure = nil
	writeProjectFile(t, root, "app", "infra/requirements.json", dbManifest)

	if result := Aggregate(ctx, Options{}); diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unexpected errors: %v", result.Diagnostics)
	}
	m := readAggregated(t, root, "app")
	if len(m.Databases) != 1 {
		t.Errorf("databases = %+v, want the workload's own contribution", m.Databases)
	}
}

// A workload whose runtime file is unreadable and which declares no
// requirements has nothing to say. Anything left by a prior build is REMOVED:
// a deployer must not read requirements this build could not compute.
func TestAggregate_ClearsManifestWhenNothingCanBeDeclared(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	writeProjectFile(t, root, "app", ".gen/requirements.json",
		`{"protocolVersion":2,"workload":"app","secrets":[{"name":"old",`+
			`"sources":[{"project":"app","contributor":"manual"}]}]}`)
	writeProjectFile(t, root, "app", "infra/runtime.json", `{ this is not json`)

	result := Aggregate(ctx, Options{})

	if result.Outcome != OutcomeCleared {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeCleared)
	}
	if !hasDiagCode(result.Diagnostics, infra.ErrorCodeParseError) {
		t.Errorf("diags = %v, want a %s finding", result.Diagnostics, infra.ErrorCodeParseError)
	}
	if _, err := os.Stat(aggregatedPath(root, "app")); !os.IsNotExist(err) {
		t.Errorf("stale manifest survived a build that declared nothing, stat err = %v", err)
	}
}

func TestRemoveAggregatedManifest_MissingIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if diags := RemoveAggregatedManifest(filepath.Join(root, "app")); len(diags) != 0 {
		t.Errorf("diags = %v, want none for an absent manifest", diags)
	}
}

func TestIsWorkload(t *testing.T) {
	for _, tc := range []struct {
		projectType string
		want        bool
	}{
		{"", true},
		{"application", true},
		{"library", false},
		{"template", false},
	} {
		if got := IsWorkload(tc.projectType); got != tc.want {
			t.Errorf("IsWorkload(%q) = %v, want %v", tc.projectType, got, tc.want)
		}
	}
}

func TestResult_DataCarriesTheOutcome(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	data := Aggregate(ctx, Options{}).Data()
	if data["outcome"] != string(OutcomeEmitted) {
		t.Errorf("data[outcome] = %v, want %q", data["outcome"], OutcomeEmitted)
	}
	if data["manifest"] != AggregatedManifestFile {
		t.Errorf("data[manifest] = %v, want %q", data["manifest"], AggregatedManifestFile)
	}
}

// The result data a cache entry replays is the same in every checkout: it
// names the manifest relative to the project, never by the absolute path of
// the checkout that stored the entry.
func TestResult_DataIsTheSameInEveryCheckout(t *testing.T) {
	run := func() map[string]any {
		root := t.TempDir()
		ctx := closureContext(root, "app", "app", "application", "lib")
		writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
		return Aggregate(ctx, Options{}).Data()
	}
	first, second := run(), run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("data differs between two checkouts:\n%v\n%v", first, second)
	}
	for key, value := range first {
		if text, ok := value.(string); ok && filepath.IsAbs(text) {
			t.Errorf("data[%s] = %q is an absolute path", key, text)
		}
	}
}

// The project-relative files are where Aggregate writes: a language declares
// them as the task's outputs, so a cache entry holds exactly what a run writes.
func TestAggregatedFiles_AreWhereAggregateWrites(t *testing.T) {
	if AggregatedManifestFile != ".gen/requirements.json" {
		t.Errorf("AggregatedManifestFile = %q", AggregatedManifestFile)
	}
	if RuntimeDefaultsFile != ".gen/infra/runtime.json" {
		t.Errorf("RuntimeDefaultsFile = %q", RuntimeDefaultsFile)
	}
	root := filepath.Join(t.TempDir(), "app")
	if got, want := AggregatedManifestPath(root), filepath.Join(root, ".gen", "requirements.json"); got != want {
		t.Errorf("AggregatedManifestPath = %q, want %q", got, want)
	}
	if got, want := runtimeDefaultsPath(root), filepath.Join(root, ".gen", "infra", "runtime.json"); got != want {
		t.Errorf("runtimeDefaultsPath = %q, want %q", got, want)
	}
}

// Every file a run leaves under the workload's .gen is one of the two files a
// language declares as the task's outputs, so a cache hit restores everything
// an execution would have written: the manifest and, without an authored
// runtime, the defaults sidecar.
func TestAggregate_WritesOnlyTheDeclaredFiles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		authored bool
		want     []string
	}{
		{name: "defaults", want: []string{AggregatedManifestFile, RuntimeDefaultsFile}},
		{name: "authored runtime", authored: true, want: []string{AggregatedManifestFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctx := closureContext(root, "app", "app", "application", "lib")
			writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
			if tc.authored {
				writeProjectFile(t, root, "app", "infra/runtime.json",
					`{"ingress":{"public":true},"scaling":{"max":5,"concurrency":80}}`)
			}

			if result := Aggregate(ctx, Options{}); result.Err != nil || diag.HasErrors(result.Diagnostics) {
				t.Fatalf("Aggregate = %v, %v", result.Err, result.Diagnostics)
			}

			var got []string
			appRoot := filepath.Join(root, "app")
			err := filepath.WalkDir(filepath.Join(appRoot, ".gen"), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				rel, err := filepath.Rel(appRoot, path)
				got = append(got, filepath.ToSlash(rel))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(got)
			sort.Strings(tc.want)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("files under .gen = %v, want %v", got, tc.want)
			}
		})
	}
}

// A write or removal that fails sets Err, whatever the outcome: each one can
// leave an earlier manifest or sidecar in place. The failure is also a finding.
func TestAggregate_AFailedWriteOrRemovalSetsErr(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocked string
		runtime string
		deps    []string
		outcome Outcome
	}{
		{name: "manifest write", blocked: AggregatedManifestFile, deps: []string{"lib"}, outcome: OutcomeEmitted},
		{name: "defaults sidecar write", blocked: RuntimeDefaultsFile, deps: []string{"lib"}, outcome: OutcomeEmitted},
		{name: "stale sidecar removal", blocked: RuntimeDefaultsFile, deps: []string{"lib"},
			runtime: `{"ingress":{"public":true},"scaling":{"max":5,"concurrency":80}}`, outcome: OutcomeEmitted},
		{name: "cleared manifest removal", blocked: AggregatedManifestFile,
			runtime: `{ this is not json`, outcome: OutcomeCleared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctx := closureContext(root, "app", "app", "application", tc.deps...)
			writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
			if tc.runtime != "" {
				writeProjectFile(t, root, "app", "infra/runtime.json", tc.runtime)
			}
			// A non-empty directory where the file goes can be neither
			// replaced by a rename nor removed.
			writeProjectFile(t, root, "app", tc.blocked+"/blocker", "x")

			result := Aggregate(ctx, Options{})

			if result.Err == nil {
				t.Fatalf("Err = nil, want the failed %s", tc.name)
			}
			if result.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", result.Outcome, tc.outcome)
			}
			if !strings.Contains(result.Err.Error(), filepath.Join("app", filepath.FromSlash(tc.blocked))) {
				t.Errorf("Err = %v, want it to name %s", result.Err, tc.blocked)
			}
			if !diag.HasErrors(result.Diagnostics) {
				t.Errorf("diags = %v, want the failure reported as a finding too", result.Diagnostics)
			}
		})
	}

	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	if result := Aggregate(ctx, Options{}); result.Err != nil {
		t.Errorf("Err = %v for a run whose writes succeed", result.Err)
	}
}
