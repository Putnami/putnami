package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func job(projID, projName, jobName string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project: &workspace.Project{ID: projID, Name: projName},
		JobDef:  &extension.JobDefinition{Name: jobName},
	}
}

// TestRunRecordFromCanonicalSession pins the projection that answers run_jobs.
// The inputs are the ones an earlier, now-deleted tally loop was pinned
// against — a failed task with one diagnostic and one log event, a cache hit,
// and a coalesced task — so the buckets still land where they always did, now
// on the v2 document that became the only answer.
//
// The two halves are asserted apart because that is what runRecord separates:
// the ANSWER carries the verdict, the counts and each failure's diagnostics,
// while allDiagnostics keeps the run's FULL list (warnings included) off-wire
// for get_diagnostics to read back.
func TestRunRecordFromCanonicalSession(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "canonical-result", "machine-surfaces-reduce-through-the-canonical-model")
	buildApp := job("/packages/app", "app", "build")
	testLib := job("/packages/lib", "lib", "test")
	lintLib := job("/packages/lib", "lib", "lint")
	planned := []*jobs.ScheduledJob{buildApp, testLib, lintLib}

	results := map[string]*jobs.JobResult{
		testLib.Key(): {
			Status: "failed",
			Error:  &jobs.JobError{Message: "2 tests failed"},
			Events: []jobs.RawJobEvent{
				{Type: jobs.EventTypeDiagnostic, Data: map[string]any{
					"severity": "error",
					"message":  "boom",
					"code":     "E001",
					"file":     "packages/lib/x.go",
					"line":     float64(12),
					"column":   float64(3),
				}},
				{Type: jobs.EventTypeLog, Message: "noise"},
			},
		},
		lintLib.Key():  {Status: "success", CacheHit: true},
		buildApp.Key(): {Status: "success", Coalesced: true},
	}

	// The canonical reduction the scheduler publishes as SchedulerResult.Session:
	// tasks folded in plan order, with their structured records extracted.
	var reducer jobs.SessionReducer
	for _, planned := range planned {
		task := jobs.TaskResultOf(planned, results[planned.Key()])
		reducer.Observe(jobs.TaskEndEvent(&task))
	}
	session := reducer.Result()
	session.Duration = 1500 * time.Millisecond

	record := &runRecord{
		result:         machine.RunFrom(session, planned, results).MCPRun([]string{"test", "build"}),
		allDiagnostics: diagnosticsFrom(session.Diagnostics),
	}

	if record.result.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d — a document without it is version 1",
			record.result.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if record.result.Tool != protocolcli.MCPToolRunJobs {
		t.Errorf("tool = %q, want %q", record.result.Tool, protocolcli.MCPToolRunJobs)
	}
	run := record.result.Run
	if run == nil {
		t.Fatal("a run_jobs answer must carry the run summary")
	}
	if run.Counts.Total != 3 || run.Counts.Succeeded != 2 || run.Counts.Failed != 1 {
		t.Errorf("counts = %+v, want total=3 succeeded=2 failed=1 (reuse is counted apart)", run.Counts)
	}
	if run.Reuse.LocalCache != 1 || run.Reuse.Coalesced != 1 {
		t.Errorf("reuse = %+v, want localCache=1 coalesced=1", run.Reuse)
	}
	if run.Outcome != protocolcli.RunOutcomeFailure {
		t.Errorf("outcome = %q, want %q when a task failed", run.Outcome, protocolcli.RunOutcomeFailure)
	}
	if run.DurationMs != 1500 {
		t.Errorf("durationMs = %d, want 1500", run.DurationMs)
	}
	if len(run.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(run.Failures))
	}
	f := run.Failures[0]
	if f.Identity.Project.Name != "lib" || f.Identity.Task.Name != "test" || f.Error.Message != "2 tests failed" {
		t.Errorf("failure = %+v, want the typed lib/test identity with the error", f)
	}
	if len(f.Diagnostics) != 1 {
		t.Fatalf("failure diagnostics = %d, want 1 (log event must be ignored)", len(f.Diagnostics))
	}
	if d := f.Diagnostics[0]; d.File != "packages/lib/x.go" || d.Line != 12 || d.Column != 3 ||
		d.Severity != "error" || d.Code != "E001" {
		t.Errorf("diagnostic = %+v, want file/line/col/sev/code populated", d)
	}
	if len(record.allDiagnostics) != 1 {
		t.Fatalf("allDiagnostics = %d, want 1", len(record.allDiagnostics))
	}
	if d := record.allDiagnostics[0]; d.Project != "lib" || d.Job != "test" {
		t.Errorf("diagnostic provenance = %s/%s, want lib/test", d.Project, d.Job)
	}
}

func TestSchemaObjectIsValidJSON(t *testing.T) {
	t.Parallel()
	withReq := schemaObject(`"project":{"type":"string"}`, []string{"project"})
	var m map[string]any
	if err := json.Unmarshal(withReq, &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("type = %v, want object", m["type"])
	}
	req, _ := m["required"].([]any)
	if len(req) != 1 || req[0] != "project" {
		t.Errorf("required = %v, want [project]", m["required"])
	}

	var empty map[string]any
	if err := json.Unmarshal(schemaObject("", nil), &empty); err != nil {
		t.Fatalf("empty schema is not valid JSON: %v", err)
	}
	if _, ok := empty["required"]; ok {
		t.Error("no-arg schema should omit required")
	}
	if empty["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", empty["additionalProperties"])
	}
}

func TestDecodeArgsRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var args describeArgs
	err := decodeArgs(json.RawMessage(`{"project":"app","extra":true}`), &args)
	if err == nil {
		t.Fatal("decodeArgs accepted an unknown field")
	}
	if got := err.Error(); got != `invalid arguments: json: unknown field "extra"` {
		t.Fatalf("err = %q, want unknown field error", got)
	}
}

func TestResolveProject(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if p := resolveProject(ws, "/packages/app"); p == nil || p.Name != "app" {
		t.Errorf("resolveProject by id failed: %v", p)
	}
	if p := resolveProject(ws, "lib"); p == nil || p.ID != "/packages/lib" {
		t.Errorf("resolveProject by name failed: %v", p)
	}
	if p := resolveProject(ws, "nope"); p != nil {
		t.Errorf("resolveProject(nope) = %v, want nil", p)
	}
}

func TestToolDeps(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence", "graph-tools-answer-from-the-loaded-workspace")
	srv := newFixtureServer(t)
	out, err := srv.toolDeps(context.Background(), json.RawMessage(`{"project":"app","direction":"dependencies"}`))
	if err != nil {
		t.Fatalf("toolDeps dependencies: %v", err)
	}
	m := out.(map[string]any)
	projects := m["projects"].([]projectRef)
	if len(projects) != 1 || projects[0].Name != "lib" {
		t.Fatalf("dependencies = %+v, want lib", projects)
	}

	out, err = srv.toolDeps(context.Background(), json.RawMessage(`{"project":"lib","direction":"dependents","transitive":true}`))
	if err != nil {
		t.Fatalf("toolDeps dependents: %v", err)
	}
	m = out.(map[string]any)
	projects = m["projects"].([]projectRef)
	if len(projects) != 1 || projects[0].Name != "app" {
		t.Fatalf("dependents = %+v, want app", projects)
	}
}

func TestToolFindOwner(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence", "graph-tools-answer-from-the-loaded-workspace")
	srv := newFixtureServer(t)
	out, err := srv.toolFindOwner(context.Background(), json.RawMessage(`{"path":"packages/app/src/main.ts"}`))
	if err != nil {
		t.Fatalf("toolFindOwner: %v", err)
	}
	m := out.(map[string]any)
	projects := m["projects"].([]projectRef)
	if len(projects) != 1 || projects[0].Name != "app" {
		t.Fatalf("owners = %+v, want app", projects)
	}
}

func TestToolWhyImpacted(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence", "graph-tools-answer-from-the-loaded-workspace")
	srv := newFixtureServer(t)
	out, err := srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"lib","to":"app"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted: %v", err)
	}
	m := out.(map[string]any)
	if impacted, _ := m["impacted"].(bool); !impacted {
		t.Fatalf("impacted = %v, want true", m["impacted"])
	}
	path := m["path"].([]projectRef)
	if len(path) != 2 || path[0].Name != "lib" || path[1].Name != "app" {
		t.Fatalf("path = %+v, want lib -> app", path)
	}
	// One edge per hop, each with its kind: an agent reading the path can see
	// what relation carried the impact, not only that one did.
	edges := m["edges"].([]impactEdgeRef)
	if want := []impactEdgeRef{{From: "/packages/lib", To: "/packages/app", Kind: "dependency"}}; !slices.Equal(edges, want) {
		t.Fatalf("edges = %+v, want %+v", edges, want)
	}
}

// whyImpactedReply is the why_impacted answer read as its wire shape. The tool
// answers with the untyped map every graph tool shares, and a test that
// asserts through one more untyped type assertion adds to a count this tree
// deliberately pins. Marshaling the answer and decoding it once reads exactly
// what an agent receives, in named fields.
type whyImpactedReply struct {
	Impacted  bool            `json:"impacted"`
	Self      bool            `json:"self"`
	Message   string          `json:"message"`
	Path      []projectRef    `json:"path"`
	Edges     []impactEdgeRef `json:"edges"`
	TaskScope []string        `json:"taskScope"`
}

func decodeWhyImpacted(t *testing.T, out any) whyImpactedReply {
	t.Helper()
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal why_impacted answer: %v", err)
	}
	var reply whyImpactedReply
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatalf("decode why_impacted answer %s: %v", encoded, err)
	}
	return reply
}

// extensionFixtureWorkspace is fixtureWorkspace plus an in-workspace extension
// project that every project runs under. Nothing declares a dependency on it,
// so the only path from it to a consumer is the impact-only extension-consumer
// edge — the edge DependentPath never walked.
func extensionFixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib","packages/ext"],"extensions":["./packages/ext"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	write("packages/ext/putnami.json", `{"name":"ext","type":"library"}`)
	workspace.InvalidateLoadCache(dir)
	recordWorkspaceIndex(t, dir)
	return dir
}

// contractFixtureWorkspace is a provider, the generated client of its
// contract, and a consumer of that client. Nothing declares a dependency on
// the provider, so the only path from it to the consumer crosses the derived
// contract edge.
func contractFixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json",
		`{"name":"fixture","includes":["services/catalog","clients/catalog-ts","apps/storefront"]}`)
	write("services/catalog/putnami.json", `{"name":"catalog","type":"application"}`)
	write("services/catalog/schema/openapi.json",
		`{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,`+
			`"service":{"id":"catalog.items","audience":"api://catalog.items"}},"paths":{}}`)
	write("clients/catalog-ts/putnami.json", `{"name":"catalog-ts","type":"library"}`)
	write("clients/catalog-ts/client.putnami.json",
		`{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"ts",`+
			`"service":{"id":"catalog.items","audience":"api://catalog.items"},`+
			`"contractSha256":"1111111111111111111111111111111111111111111111111111111111111111"}`)
	write("apps/storefront/putnami.json", `{"name":"storefront","type":"application","dependencies":["catalog-ts"]}`)
	workspace.InvalidateLoadCache(dir)
	recordWorkspaceIndex(t, dir)
	return dir
}

// TestToolWhyImpactedNamesTheContractEdge pins that the derived provider →
// generated client relation reaches the MCP answer under its own kind, so an
// operator asking why a client and its consumers run reads `contract` rather
// than a dependency neither project declares.
func TestToolWhyImpactedNamesTheContractEdge(t *testing.T) {
	t.Parallel()
	dir := contractFixtureWorkspace(t)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	out, err := srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"catalog","to":"storefront"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted: %v", err)
	}
	reply := decodeWhyImpacted(t, out)
	if !reply.Impacted {
		t.Fatalf("impacted = false (%q), want true: storefront consumes the generated client", reply.Message)
	}
	want := []impactEdgeRef{
		{From: "/services/catalog", To: "/clients/catalog-ts", Kind: "contract"},
		{From: "/clients/catalog-ts", To: "/apps/storefront", Kind: "dependency"},
	}
	if !slices.Equal(reply.Edges, want) {
		t.Fatalf("edges = %+v, want %+v", reply.Edges, want)
	}
	// A contract edge selects in full: the client runs every task, so it has
	// an empty scope rather than an extension's.
	if len(reply.TaskScope) != 0 {
		t.Errorf("taskScope = %v, want none: a contract edge selects in full", reply.TaskScope)
	}
}

// TestToolImpactedNamesTheContractThatMoved pins the contract edge on the wire,
// through the real loader and a real diff: a provider source change reaches no
// client, and a change to the provider's committed contract reaches the client
// over a `contract` reason that names that contract and the digest the tree
// holds for it.
func TestToolImpactedNamesTheContractThatMoved(t *testing.T) {
	t.Parallel()
	dir := contractFixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	impacted := func() impactedAnswer {
		t.Helper()
		workspace.InvalidateLoadCache(dir)
		srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})
		out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
		if err != nil {
			t.Fatalf("toolImpacted: %v", err)
		}
		return out.(impactedAnswer)
	}

	write("services/catalog/src/items.ts", "export const items = []\n")
	answer := impacted()
	if got, want := answerProjectIDs(answer), []string{"/services/catalog"}; !slices.Equal(got, want) {
		t.Fatalf("a provider source change selected %v, want %v", got, want)
	}

	if err := os.Remove(filepath.Join(dir, "services", "catalog", "src", "items.ts")); err != nil {
		t.Fatal(err)
	}
	contract := `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"catalog.items","audience":"api://catalog.items"}},"paths":{"/items":{}}}`
	write("services/catalog/schema/openapi.json", contract)
	answer = impacted()
	sum := sha256.Sum256([]byte(contract))
	want := impactedReason{
		Project: "/clients/catalog-ts", From: "/services/catalog", Kind: "contract",
		Via: "services/catalog/schema/openapi.json", ContractSHA256: hex.EncodeToString(sum[:]),
	}
	var got *impactedReason
	for i := range answer.Reasons {
		if answer.Reasons[i].Project == want.Project {
			got = &answer.Reasons[i]
		}
	}
	if got == nil || got.From != want.From || got.Kind != want.Kind || got.Via != want.Via || got.ContractSHA256 != want.ContractSHA256 {
		t.Fatalf("reason for the client = %+v, want %+v (reasons %+v)", got, want, answer.Reasons)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"via":"services/catalog/schema/openapi.json","contractSha256":"`+want.ContractSHA256+`"`) {
		t.Errorf("wire reason = %s, want via and contractSha256", encoded)
	}
	if !slices.Contains(answerProjectIDs(answer), "/apps/storefront") {
		t.Errorf("a contract change selected %v, want the client's consumer too", answerProjectIDs(answer))
	}
}

func answerProjectIDs(answer impactedAnswer) []string {
	ids := make([]string, 0, len(answer.Projects))
	for _, p := range answer.Projects {
		ids = append(ids, p.ID)
	}
	return ids
}

// The pair the operator's session denied: `why_impacted` walked the scheduling
// graph, which has no extension-consumer edges, and answered "does not
// transitively impact" for a project `impacted` had selected. Both tools now
// walk one neighbor function, and the edge is named.
func TestToolWhyImpactedCrossesExtensionConsumerEdge(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "why-impacted-walks-the-edges-impacted-widens-through")
	dir := extensionFixtureWorkspace(t)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	out, err := srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"ext","to":"app"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted: %v", err)
	}
	reply := decodeWhyImpacted(t, out)
	if !reply.Impacted {
		t.Fatalf("impacted = false (%q), want true: app runs under ext", reply.Message)
	}
	if want := []impactEdgeRef{{From: "/packages/ext", To: "/packages/app", Kind: "extension-consumer"}}; !slices.Equal(reply.Edges, want) {
		t.Fatalf("edges = %+v, want %+v", reply.Edges, want)
	}
	if len(reply.Path) != 2 || reply.Path[0].Name != "ext" || reply.Path[1].Name != "app" {
		t.Fatalf("path = %+v, want ext -> app", reply.Path)
	}
	// app runs ext's tasks only: the scope `impacted` would plan with.
	if want := []string{"/packages/ext"}; !slices.Equal(reply.TaskScope, want) {
		t.Fatalf("taskScope = %v, want %v", reply.TaskScope, want)
	}
	// lib feeds app through a dependency edge: full, an empty scope.
	out, err = srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"lib","to":"app"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted lib->app: %v", err)
	}
	if full := decodeWhyImpacted(t, out); !full.Impacted || len(full.TaskScope) != 0 || full.TaskScope == nil {
		t.Fatalf("lib->app = %+v, want impacted with an empty (not absent) task scope", full)
	}

	// The reverse pair stays denied, with an empty edge list rather than a
	// missing key.
	out, err = srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"app","to":"ext"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted reverse: %v", err)
	}
	if reverse := decodeWhyImpacted(t, out); reverse.Impacted || len(reverse.Edges) != 0 {
		t.Fatalf("reverse pair = %+v, want not impacted with no edges: extension edges are one-directional", reverse)
	}
}

// Every project in the answer carries its reason, at the same index: a seeded
// project names the changed file that claimed it and how, a propagated one
// names the project and edge kind that reached it first.
func TestToolImpactedReturnsAReasonPerProject(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "the-selection-explains-every-project-it-holds")
	dir := fixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})
	if err := os.WriteFile(filepath.Join(dir, "packages", "lib", "lib.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted: %v", err)
	}
	answer := out.(impactedAnswer)
	// Workspace order, which the fixture's includes fix as app then lib: a
	// propagated project can precede the seed that reached it.
	want := []impactedReason{
		{Project: "/packages/app", From: "/packages/lib", Kind: "dependency"},
		{Project: "/packages/lib", Seeds: []impactSeedRef{{File: "packages/lib/lib.ts", Kind: "path-owner", Via: "packages/lib"}}},
	}
	if !reflect.DeepEqual(answer.Reasons, want) {
		t.Fatalf("reasons = %+v, want %+v", answer.Reasons, want)
	}
	if len(answer.Reasons) != len(answer.Projects) {
		t.Fatalf("%d reasons for %d projects", len(answer.Reasons), len(answer.Projects))
	}
	for i, reason := range answer.Reasons {
		if reason.Project != answer.Projects[i].ID {
			t.Errorf("reasons[%d].project = %q, projects[%d].id = %q", i, reason.Project, i, answer.Projects[i].ID)
		}
	}
	// Absent, not empty, when nothing was selected.
	if err := os.Remove(filepath.Join(dir, "packages", "lib", "lib.ts")); err != nil {
		t.Fatal(err)
	}
	out, err = srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted on a clean tree: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	if strings.Contains(string(encoded), "reasons") {
		t.Errorf("answer %s carries reasons, want the key absent when nothing was selected", encoded)
	}
}

// The assertion the operator's session would have failed: for every project
// `impacted` lists, `why_impacted` from the changed project agrees. Both read
// one record over one edge union, so the agreement is by construction, and
// this pins that no surface grew a walk of its own.
func TestToolImpactedAndWhyImpactedAgree(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "why-impacted-walks-the-edges-impacted-widens-through")
	dir := extensionFixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})
	if err := os.WriteFile(filepath.Join(dir, "packages", "ext", "hook.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The extension's implementation rebuilds the binary both consumers run,
	// so both are selected for ext's tasks only.
	out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted: %v", err)
	}
	answer := out.(impactedAnswer)
	if answer.Count != 3 {
		t.Fatalf("impacted = %+v, want ext and both of its consumers for an implementation change", answer.Projects)
	}
	for _, p := range answer.Projects {
		args, err := json.Marshal(whyImpactedArgs{From: "ext", To: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		out, err := srv.toolWhyImpacted(context.Background(), args)
		if err != nil {
			t.Fatalf("toolWhyImpacted(ext, %s): %v", p.ID, err)
		}
		reply := decodeWhyImpacted(t, out)
		if !reply.Impacted {
			t.Errorf("impacted lists %s but why_impacted denies it: %q", p.ID, reply.Message)
		}
		want := []string{"/packages/ext"}
		if p.ID == "/packages/ext" {
			want = []string{}
		}
		if !slices.Equal(reply.TaskScope, want) {
			t.Errorf("why_impacted(ext, %s) taskScope = %v, want %v: the scope `impacted` plans with", p.ID, reply.TaskScope, want)
		}
	}
	for _, reason := range answer.Reasons {
		if reason.Project == "/packages/ext" {
			if reason.TaskScope != nil {
				t.Errorf("reason for the changed extension = %+v, want no task scope", reason)
			}
			continue
		}
		if reason.From != "/packages/ext" || reason.Kind != "extension-consumer" || !slices.Equal(reason.TaskScope, []string{"/packages/ext"}) {
			t.Errorf("reason for %s = %+v, want reached from ext over an extension-consumer edge, scoped to ext's tasks", reason.Project, reason)
		}
	}
}

func TestToolWhyImpactedSelf(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	out, err := srv.toolWhyImpacted(context.Background(), json.RawMessage(`{"from":"lib","to":"lib"}`))
	if err != nil {
		t.Fatalf("toolWhyImpacted self: %v", err)
	}
	m := out.(map[string]any)
	if impacted, _ := m["impacted"].(bool); !impacted {
		t.Fatalf("impacted = %v, want true for self", m["impacted"])
	}
	if self, _ := m["self"].(bool); !self {
		t.Fatalf("self = %v, want true", m["self"])
	}
	path := m["path"].([]projectRef)
	if len(path) != 1 || path[0].Name != "lib" {
		t.Fatalf("path = %+v, want [lib]", path)
	}
	if message, _ := m["message"].(string); message == "" {
		t.Fatal("message is empty, want explicit self-case note")
	}
	if edges := m["edges"].([]impactEdgeRef); len(edges) != 0 {
		t.Fatalf("edges = %+v, want none for the self case", edges)
	}
}

// An agent reading `count: 0` cannot tell "nothing changed" from "everything
// that changed sits at the workspace root, where the mapping attributes it to
// no project". The answer carries the evidence for the second case so the tool
// does not have to be believed on faith — and carries the key only when there
// is something to report, so the ordinary answer shape is unchanged.
func TestToolImpactedReportsUnownedRootFiles(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	if err := os.WriteFile(filepath.Join(dir, "bun.lock"), []byte(`{"lockfileVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted: %v", err)
	}
	answer := out.(impactedAnswer)
	if answer.Count != 0 {
		t.Fatalf("count = %d, want 0: a root path belongs to no project", answer.Count)
	}
	if !slices.Equal(answer.UnownedRootFiles, []string{"bun.lock"}) {
		t.Fatalf("unownedRootFiles = %v, want [bun.lock]", answer.UnownedRootFiles)
	}

	if err := os.WriteFile(filepath.Join(dir, "packages", "lib", "lib.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "bun.lock")); err != nil {
		t.Fatal(err)
	}
	out, err = srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted after project change: %v", err)
	}
	answer = out.(impactedAnswer)
	if answer.Count == 0 {
		t.Fatalf("count = %d, want the project that owns the changed file", answer.Count)
	}
	// Absent on the wire, not empty: an agent must not have to distinguish
	// "nothing unattributed" from "field present and empty".
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	if strings.Contains(string(encoded), "unownedRootFiles") {
		t.Errorf("answer %s carries unownedRootFiles, want the key absent when everything was attributed", encoded)
	}
}

// TestToolImpactedMatchesCLIResolution pins MCP/CLI parity by construction:
// both surfaces must resolve the same baseline (same tier) and select the same
// projects over the same tree. A second resolution algorithm on either side
// would make one surface's answer unverifiable from the other.
func TestToolImpactedMatchesCLIResolution(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence", "impacted-matches-the-cli-resolution")
	dir := fixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	if err := os.WriteFile(filepath.Join(dir, "packages", "lib", "lib.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	selection, err := workspace.ImpactedSelectionForBaseline(ws, "")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}

	out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolImpacted: %v", err)
	}
	answer := out.(impactedAnswer)

	if answer.Baseline != selection.Baseline {
		t.Errorf("baseline: MCP %q vs CLI %q", answer.Baseline, selection.Baseline)
	}
	if answer.BaselineSource != string(selection.BaselineSource) {
		t.Errorf("baselineSource: MCP %q vs CLI %q", answer.BaselineSource, selection.BaselineSource)
	}
	cliIDs := make([]string, 0, len(selection.Projects))
	for _, p := range selection.Projects {
		cliIDs = append(cliIDs, p.ID)
	}
	mcpIDs := make([]string, 0, len(answer.Projects))
	for _, ref := range answer.Projects {
		mcpIDs = append(mcpIDs, ref.ID)
	}
	if !slices.Equal(mcpIDs, cliIDs) {
		t.Errorf("projects: MCP %v vs CLI %v", mcpIDs, cliIDs)
	}
	// The reasons are the CLI's trace projected onto the wire, not a second
	// account of the mapping.
	if want := impactedReasons(selection); !reflect.DeepEqual(answer.Reasons, want) {
		t.Errorf("reasons: MCP %+v vs CLI trace %+v", answer.Reasons, want)
	}
	if len(answer.Reasons) == 0 {
		t.Error("reasons are empty over a non-empty selection")
	}
	// The fixture has no remote refs, so the shared resolution lands on the
	// local-trunk fallback tier — and the wire says so.
	if answer.BaselineSource != string(git.BaselineSourceLocalTrunk) {
		t.Errorf("baselineSource = %q, want %q", answer.BaselineSource, git.BaselineSourceLocalTrunk)
	}
}

func TestToolTopoSort(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence", "graph-tools-answer-from-the-loaded-workspace")
	srv := newFixtureServer(t)
	out, err := srv.toolTopoSort(context.Background(), nil)
	if err != nil {
		t.Fatalf("toolTopoSort: %v", err)
	}
	m := out.(map[string]any)
	if m["count"].(int) != 2 {
		t.Fatalf("count = %v, want 2", m["count"])
	}
	projects := m["projects"].([]projectRef)
	if len(projects) != 2 || projects[0].Name != "lib" || projects[1].Name != "app" {
		t.Fatalf("projects = %+v, want lib before app", projects)
	}
}

func TestRemoveProjectIDDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	ids := []string{"/app", "/lib", "/core"}
	out := removeProjectID(ids, "/app")

	if len(out) != 2 || out[0] != "/lib" || out[1] != "/core" {
		t.Fatalf("out = %v, want [/lib /core]", out)
	}
	if len(ids) != 3 || ids[0] != "/app" || ids[1] != "/lib" || ids[2] != "/core" {
		t.Fatalf("input mutated to %v", ids)
	}
}

func TestRunJobsDryRunSummarizesPlanWithoutStoringDiagnostics(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "plan-only-and-dry-runs-execute-nothing")
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary", "dry-run-executes-nothing")
	spectest.Proves(t, "cli/diagnostics-results", "diagnostic-recall", "a-dry-run-never-replaces-the-recall")
	srv, root := jobFixtureServer(t)
	out, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["build"],"dryRun":true}`))
	if err != nil {
		t.Fatalf("toolRunJobs dryRun: %v", err)
	}
	// The tool answers with the v2 plan document — the only one it has since
	// planResult was deleted with the rest of the v1 emitters.
	doc, ok := out.(*protocolcli.MCPResult)
	if !ok {
		t.Fatalf("dry run answered %T, want the v2 MCP document", out)
	}
	if doc.Plan == nil || !doc.Plan.DryRun {
		t.Fatal("v2 dry run result must carry plan.dryRun=true")
	}
	if doc.Plan.Metrics.Tasks != 2 || doc.Plan.Metrics.Projects != 2 {
		t.Fatalf("v2 plan = %d tasks over %d projects, want 2 over 2", doc.Plan.Metrics.Tasks, doc.Plan.Metrics.Projects)
	}
	if srv.lastRun != nil {
		t.Fatal("dry run must not update lastRun diagnostics state")
	}

	// The retired selection variable cannot bring the v1 answer back: there is
	// no v1 result shape left for it to select.
	t.Setenv(machine.RetiredSelectionEnv, "v1")
	out, err = srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["build"],"dryRun":true}`))
	if err != nil {
		t.Fatalf("toolRunJobs dryRun (retired v1 selection): %v", err)
	}
	if _, ok := out.(*protocolcli.MCPResult); !ok {
		t.Fatalf("dry run with %s=v1 answered %T, want the v2 MCP document",
			machine.RetiredSelectionEnv, out)
	}
	if srv.lastRun != nil {
		t.Fatal("dry run must not update lastRun diagnostics state")
	}

	// An explicit selector takes the MCP argument-resolution path. On a cold
	// workspace it must still remain a preview: the new lazy graph preparation
	// callback is reserved for real graph reads and executions.
	prepareCalls := 0
	srv.opts.PrepareWorkspaceView = func(context.Context) error {
		prepareCalls++
		return nil
	}
	_, err = srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["build"],"projects":["app"],"dryRun":true}`))
	if err != nil {
		t.Fatalf("targeted toolRunJobs dryRun: %v", err)
	}
	if prepareCalls != 0 {
		t.Fatalf("dry run invoked persistent workspace preparation %d time(s)", prepareCalls)
	}
	for _, path := range []string{workspace.SnapshotPath(root), filepath.Join(root, ".putnami", workspaceSyncLockFilenameForTest)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s: %v", path, err)
		}
	}
}

const workspaceSyncLockFilenameForTest = "workspace-index.lock"

// sideEffectFixture writes a workspace whose extension declares one command of
// every classification the side-effect gate has to tell apart: a well-known verb
// carrying none (lint), the two whose verb defaults carry one (publish →
// registry, deploy → cloud), a CUSTOM verb that declares `sideEffects` in its
// own manifest traits (release), and a custom verb that declares none (ship).
//
// Every command runs the same script, which writes "<command>.ran" into the
// project directory. That marker is what makes a refusal observable as an
// ABSENCE OF EFFECT rather than as an error string: before the gate, run_jobs
// handed `publish` and `deploy` straight to engine.Run.
func sideEffectFixture(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	write("putnami.workspace.json",
		`{"name":"fixture-ws","includes":["packages/app"],"extensions":["/tools/demo"]}`, 0o644)
	write("packages/app/putnami.json",
		`{"name":"app","type":"application","extensions":["@putnami/demo"]}`, 0o644)
	write("packages/app/demo.activate", "", 0o644)
	write("tools/demo/putnami.json", `{"name":"@putnami/demo"}`, 0o644)
	// Jobs run with cwd = the project directory, so the marker lands beside the
	// project's own files. Each task starts /bin/sh with the script as an
	// argument instead of executing the script: this test is parallel, and a
	// sibling's fork can inherit the descriptor that writes the script, which
	// Linux answers with ETXTBSY when the script itself is executed. sh only
	// reads the script.
	write("tools/demo/mark.sh", ": > \"$1.ran\"\nexit 0\n", 0o644)

	command := func(name string, traits string) string {
		return `"` + name + `":{"description":"Demo ` + name + ` fixture.",` + traits +
			`"activationFiles":["demo.activate"],"run":[{"id":"` + name + `","task":"` + name + `-task"}]}`
	}
	task := func(name string) string {
		return `"` + name + `-task":{"kind":"command","command":"/bin/sh",` +
			`"args":["{extensionRoot}/mark.sh","` + name + `"],"cache":false,"timeoutMs":30000}`
	}
	names := []string{"lint", "publish", "deploy", "release", "ship"}
	commands := []string{
		command("lint", ""),
		command("publish", ""),
		command("deploy", ""),
		// The custom verb the well-known table knows nothing about: only its
		// declared traits classify it.
		command("release", `"traits":{"sideEffects":"registry"},`),
		command("ship", ""),
	}
	tasks := make([]string, 0, len(names))
	for _, name := range names {
		tasks = append(tasks, task(name))
	}
	write("tools/demo/putnami.extension.json", `{
  "name": "@putnami/demo",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {`+strings.Join(commands, ",")+`},
  "tasks": {`+strings.Join(tasks, ",")+`}
}`, 0o644)

	workspace.InvalidateLoadCache(dir)
	// The store is isolated once for the whole binary by TestMain
	// (PUTNAMI_STORE_DIR), so the fixture needs no t.Setenv and stays
	// compatible with t.Parallel.
	return dir
}

func sideEffectFixtureServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := sideEffectFixture(t)
	return NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"}), dir
}

// ranMarker reports whether the fixture's job for command actually executed.
func ranMarker(t *testing.T, dir, command string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, "packages", "app", command+".ran"))
	return err == nil
}

// TestRejectSideEffectingCommands_ReadsTheDeclaredTrait pins that the gate is
// the protocol's CommandTraits.SideEffects, not a list of names: `release` is a
// verb the well-known table has never heard of and is refused purely because its
// manifest declares the trait, while `ship` — a custom verb declaring none — is
// accepted.
func TestRejectSideEffectingCommands_ReadsTheDeclaredTrait(t *testing.T) {
	t.Parallel()
	srv, _ := sideEffectFixtureServer(t)
	for _, tc := range []struct {
		name     string
		commands []string
		// classification is what the refusal must name; empty means the run is
		// allowed through.
		classification string
	}{
		{name: "publish is refused", commands: []string{"publish"}, classification: "registry"},
		{name: "deploy is refused", commands: []string{"deploy"}, classification: "cloud"},
		{
			name:           "manifest-declared custom verb is refused",
			commands:       []string{"release"},
			classification: "registry",
		},
		{
			name:           "refused among ordinary commands",
			commands:       []string{"lint", "deploy"},
			classification: "cloud",
		},
		{name: "lint is allowed", commands: []string{"lint"}},
		{name: "custom verb without the trait is allowed", commands: []string{"ship"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := srv.rejectSideEffectingCommands(tc.commands)
			if tc.classification == "" {
				if err != nil {
					t.Fatalf("rejectSideEffectingCommands(%v) = %v, want it accepted", tc.commands, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("rejectSideEffectingCommands(%v) = nil, want a refusal", tc.commands)
			}
			if !strings.Contains(err.Error(), tc.classification) {
				t.Errorf("refusal %q does not name what is mutated (%q)", err, tc.classification)
			}
			if !strings.Contains(err.Error(), "terminal") {
				t.Errorf("refusal %q does not tell the caller what to do instead", err)
			}
		})
	}
}

// TestRejectSideEffectingCommands_FallsBackToTheWellKnownVerbs pins the closed
// side of resolution: a workspace where nothing declares `deploy` still refuses
// it, because the verb's own default traits classify it. Otherwise the gate
// would depend on which extensions happen to be installed.
func TestRejectSideEffectingCommands_FallsBackToTheWellKnownVerbs(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t) // no extensions at all
	if err := srv.rejectSideEffectingCommands([]string{"deploy"}); err == nil {
		t.Fatal("deploy accepted in a workspace with no extension declaring it")
	}
	if err := srv.rejectSideEffectingCommands([]string{"publish"}); err == nil {
		t.Fatal("publish accepted in a workspace with no extension declaring it")
	}
	if err := srv.rejectSideEffectingCommands([]string{"lint", "test", "build"}); err != nil {
		t.Fatalf("ordinary commands refused: %v", err)
	}
}

// TestRunJobs_SideEffectingCommandNeverReachesTheEngine is the regression proper:
// the refusal has to happen BEFORE engine.Run, so the registry push never
// executes. The marker file is the evidence — an error string alone would still
// pass if the job had already run.
func TestRunJobs_SideEffectingCommandNeverReachesTheEngine(t *testing.T) {
	t.Parallel()
	srv, dir := sideEffectFixtureServer(t)

	out, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["publish"],"projects":["app"]}`))
	if err == nil {
		t.Fatalf("run_jobs publish succeeded (%v), want a refusal", out)
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Errorf("refusal = %q, want it to name the registry mutation", err)
	}
	if ranMarker(t, dir, "publish") {
		t.Fatal("publish executed: the gate ran after engine.Run instead of before it")
	}
	if srv.lastRun != nil {
		t.Error("a refused run must not become the session's last run")
	}

	// Positive control: the same fixture really does execute an ordinary command,
	// so the assertion above is about the GATE, not about a workspace that plans
	// nothing.
	lint, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["lint"],"projects":["app"]}`))
	if err != nil {
		t.Fatalf("run_jobs lint: %v", err)
	}
	// run_jobs answers a failed job with a nil error and the failure in its
	// document, and an empty plan with a nil error and the engine's notice. The
	// control prints both, so a lint that planned nothing, failed to start or
	// exited non-zero names its own cause.
	answer, _ := json.Marshal(lint)
	notices := strings.TrimSpace(srv.notices.String())
	doc, ok := lint.(*protocolcli.MCPResult)
	if !ok || doc.Run == nil || !doc.Run.Succeeded() || doc.Run.Counts.Succeeded != 1 {
		t.Fatalf("control: run_jobs lint did not run its one task to success, so the publish assertion proves nothing: %s (engine notices: %q)",
			answer, notices)
	}
	if !ranMarker(t, dir, "lint") {
		t.Fatalf("control: lint succeeded without writing its marker: %s (engine notices: %q)", answer, notices)
	}
}

// TestRunJobs_PlanningASideEffectingCommandIsAllowed pins that the refusal
// guards EXECUTION only, matching how dryRun already bypasses the long-lived
// refusal: planning schedules no subprocess, so an agent can still answer "what
// would deploy do?" without being able to deploy.
func TestRunJobs_PlanningASideEffectingCommandIsAllowed(t *testing.T) {
	t.Parallel()
	srv, dir := sideEffectFixtureServer(t)

	out, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["deploy"],"projects":["app"],"dryRun":true}`))
	if err != nil {
		t.Fatalf("planning deploy was refused: %v", err)
	}
	doc, ok := out.(*protocolcli.MCPResult)
	if !ok {
		t.Fatalf("dry run answered %T, want the v2 MCP document", out)
	}
	if doc.Plan == nil || !doc.Plan.DryRun {
		t.Fatal("v2 dry run result must carry plan.dryRun=true")
	}
	if doc.Plan.Metrics.Tasks != 1 {
		t.Fatalf("planned %d tasks, want the one deploy task", doc.Plan.Metrics.Tasks)
	}
	if ranMarker(t, dir, "deploy") {
		t.Fatal("planning deploy executed it")
	}
}

func TestGetDiagnosticsBeforeAnyRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "diagnostic-recall", "the-latest-executed-run-is-recalled-without-rerunning")
	srv := newFixtureServer(t)
	out, err := srv.toolGetDiagnostics(context.Background(), nil)
	if err != nil {
		t.Fatalf("toolGetDiagnostics: %v", err)
	}
	m := out.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("count = %v, want 0 before any run", m["count"])
	}
	if _, ok := m["message"]; !ok {
		t.Error("expected an explanatory message when no run has happened")
	}
}

func TestGetDiagnosticsFiltersBySeverity(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "diagnostic-recall", "the-latest-executed-run-is-recalled-without-rerunning")
	srv := newFixtureServer(t)
	srv.lastRun = &runRecord{allDiagnostics: []diagnostic{
		{Severity: "error", Message: "e1"},
		{Severity: "warning", Message: "w1"},
		{Severity: "error", Message: "e2"},
	}}
	out, err := srv.toolGetDiagnostics(context.Background(), json.RawMessage(`{"severity":"error"}`))
	if err != nil {
		t.Fatalf("toolGetDiagnostics: %v", err)
	}
	m := out.(map[string]any)
	if m["count"].(int) != 2 {
		t.Errorf("filtered count = %v, want 2", m["count"])
	}
}

// TestGraphToolsFailClosedWithoutARecordedView is the honesty half of
// `cli.workspace-probe-view.v1`.
//
// Project identity and dependency edges are a PROJECTION of the provider
// answers. A workspace nobody has probed has no copy of them, and answering an
// empty-but-well-formed graph reads as "there is nothing here" rather than
// "nobody has told me yet". The contract declares `onMissing: fail-closed`, so
// every graph tool returns its partial answer AND an error whose payload names
// the command that rebuilds the copy.
func TestGraphToolsFailClosedWithoutARecordedView(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-equivalence",
		"graph-tools-report-the-recorded-view-they-answered-from")
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"unprobed","includes":["packages/app","packages/lib"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	workspace.InvalidateLoadCache(dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})
	ctx := context.Background()

	calls := map[string]func() (any, error){
		"list_projects":    func() (any, error) { return srv.toolListProjects(ctx, nil) },
		"describe_project": func() (any, error) { return srv.toolDescribeProject(ctx, json.RawMessage(`{"project":"app"}`)) },
		"deps":             func() (any, error) { return srv.toolDeps(ctx, json.RawMessage(`{"project":"app"}`)) },
		"find_owner":       func() (any, error) { return srv.toolFindOwner(ctx, json.RawMessage(`{"path":"packages/app/x.go"}`)) },
		"why_impacted":     func() (any, error) { return srv.toolWhyImpacted(ctx, json.RawMessage(`{"from":"lib","to":"app"}`)) },
		"topo_sort":        func() (any, error) { return srv.toolTopoSort(ctx, nil) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			out, err := call()
			if err == nil {
				t.Fatalf("%s answered without a recorded view: %+v", name, out)
			}
			// The payload comes back WITH the error: the MCP dispatcher renders
			// both, so the caller sees what was available and why it is not
			// authoritative, rather than an error with nothing behind it.
			if out == nil {
				t.Fatalf("%s returned no payload beside its refusal", name)
			}
			view := decodeWorkspaceView(t, out)
			if view.Freshness != workspace.RecordedAbsent {
				t.Errorf("workspaceView = %+v, want an absent copy", view)
			}
			if !strings.Contains(view.Message, "projects sync") {
				t.Errorf("message = %q, want the command that rebuilds the copy", view.Message)
			}
		})
	}
}

// TestGraphToolsReportAFreshRecordedView is the ordinary case: the block is on
// every answer, so a consumer never has to infer freshness from the absence of a
// warning.
func TestGraphToolsReportAFreshRecordedView(t *testing.T) {
	srv := newFixtureServer(t)
	out, err := srv.toolDeps(context.Background(), json.RawMessage(`{"project":"app"}`))
	if err != nil {
		t.Fatalf("toolDeps: %v", err)
	}
	view := decodeWorkspaceView(t, out)
	if view.Freshness != workspace.RecordedFresh {
		t.Errorf("workspaceView = %+v, want a fresh copy", view)
	}
	if view.ObservedAt == "" || view.ProbeDigest == "" {
		t.Errorf("workspaceView = %+v, want the observation time and provenance the contract declares", view)
	}
}

// decodeWorkspaceView reads the honesty block off a tool answer through JSON.
// Going through the wire rather than a type assertion is what makes the check
// the same for a map payload and a typed one — and it is the shape an MCP client
// receives.
func decodeWorkspaceView(t *testing.T, answer any) workspace.RecordedView {
	t.Helper()
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode tool answer: %v", err)
	}
	var payload struct {
		WorkspaceView workspace.RecordedView `json:"workspaceView"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode tool answer: %v", err)
	}
	return payload.WorkspaceView
}

// A source workspace has no published CLI version. Reporting the empty
// cli.Version would hand an agent "unpinned", the opposite of the truth: a
// source workspace is the strictest declaration there is. Report the sentinel.
func TestLockedVersionsReportsTheSourceWorkspaceSentinel(t *testing.T) {
	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	lf.SetExtension("@putnami/go", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	pins, err := lockedVersions(ws)
	if err != nil {
		t.Fatalf("lockedVersions: %v", err)
	}
	if pins.CLI != lockfile.SourceWorkspace {
		t.Errorf("pinnedVersions.cli = %q, want %q", pins.CLI, lockfile.SourceWorkspace)
	}
	if pins.Extensions["@putnami/go"] != "1.0.0" {
		t.Errorf("extension pins must be unaffected: %v", pins.Extensions)
	}

	// A published pin still reports its version.
	lf.SetCLI(lockfile.LockEntry{Version: "1.4.2"})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	pins, err = lockedVersions(ws)
	if err != nil {
		t.Fatalf("lockedVersions: %v", err)
	}
	if pins.CLI != "1.4.2" {
		t.Errorf("pinnedVersions.cli = %q, want 1.4.2", pins.CLI)
	}
}
