package scheduler

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestBuildJobContext_ConformsToProtocol round-trips a produced job
// context through the protocol's strict parser and validator. The
// orchestrator is the producer of go.putnami.dev/protocol/job; this
// test fails if BuildJobContext emits a shape the contract does not
// define.
func TestBuildJobContext_ConformsToProtocol(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "the-cli-produces-a-conforming-context", "the-cli-context-passes-the-strict-reader-as-version-2")
	t.Parallel()
	ws := &workspace.Workspace{
		Name: "test-workspace",
		Root: "/workspace",
		Config: &wsproto.Config{Options: map[string]map[string]any{
			"sdd": {"verification": map[string]any{"architecture": "report"}},
		}},
	}
	proj := &workspace.Project{
		ID:      "/packages/my-app",
		Name:    "my-app",
		Path:    "packages/my-app",
		Publish: []string{"npm", "docker"},
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				"generate": {"schema": false},
			},
		},
	}
	ext := &extension.ExtensionDescription{
		Name: "@putnami/typescript",
		Path: "/workspace/node_modules/@putnami/typescript",
	}
	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "build~transpile",
			ExtensionName: "@putnami/typescript",
			Defaults:      map[string]string{"target": "es2022"},
		},
		Selection: &protocoljob.Selection{
			Mode:           protocoljob.SelectionModeImpacted,
			Scoped:         true,
			Baseline:       "origin/main",
			BaselineSource: "trunk",
			ProjectIDs:     []string{"/packages/my-app"},
		},
	}

	ctx := BuildJobContext(ws, job,
		map[string]any{"fast": true, "coverage-threshold": "80"},
		map[string]any{"target": "es2020"},
		rootLineVersions(&cachekey.JobContextVersion{SHA: "abc1234", Suffix: "abc1234"}))

	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}

	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("produced context violates the job context protocol: %v\n%s", diags, data)
	}

	// JOB CONTEXT v2 IS ACTIVE (slice B6c): the document declares the version
	// and carries the typed identity v2 requires. ParseAndValidate above already
	// enforces that pairing — a v2 document without `identity` is
	// job.missing_field, and a v1 document WITH one is job.unexpected_field — so
	// what is asserted here is that the producer chose v2 at all.
	if parsed.ContextVersion() != protocoljob.ProtocolVersion2 || !parsed.IsV2() {
		t.Fatalf("produced context version = %d, want %d",
			parsed.ContextVersion(), protocoljob.ProtocolVersion2)
	}
	if parsed.Identity == nil {
		t.Fatal("a v2 job context must carry the typed task identity")
	}
	var workspaceSDD struct {
		Verification map[string]string `json:"verification"`
	}
	if err := json.Unmarshal(parsed.Workspace.Options["sdd"], &workspaceSDD); err != nil {
		t.Fatalf("workspace sdd options did not survive the producer/protocol round trip: %v", err)
	}
	if workspaceSDD.Verification["architecture"] != "report" {
		t.Errorf("workspace architecture policy = %q, want report", workspaceSDD.Verification["architecture"])
	}
	// The identity a subprocess reads must be the plan node's own, byte for
	// byte: that is the whole point of typing it, and a second derivation here
	// would be exactly the drift v2 exists to prevent.
	if want := job.TypedIdentity(); *parsed.Identity != want {
		t.Errorf("context identity = %+v, want the plan node's %+v", *parsed.Identity, want)
	}
	if parsed.Identity.Key != job.Key() {
		t.Errorf("identity.key = %q, want the plan key %q", parsed.Identity.Key, job.Key())
	}
	// staging is optional at v2 and deliberately absent: the orchestrator does
	// not run tasks in a staging tree (task_capture.go), so naming one
	// would hand the task directories it must not write to.
	if parsed.Staging != nil {
		t.Errorf("staging = %+v, want absent — no task runs in a staging tree", parsed.Staging)
	}
	// The run's resolved selection reaches the subprocess whole. It is the plan
	// node's own value, so a task cannot read one scope while the orchestrator
	// reports another.
	if parsed.Selection == nil {
		t.Fatal("a job planned under a resolved selection must carry it")
	}
	roundTripped, err := json.Marshal(parsed.Selection)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := json.Marshal(job.Selection)
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTripped) != string(planned) {
		t.Errorf("context selection = %s, want the plan node's %s", roundTripped, planned)
	}

	// Spot-check that contract semantics survive the round trip.
	if parsed.Job.Name != "build" {
		t.Errorf("job name = %q, want base command name build", parsed.Job.Name)
	}
	if !parsed.HasPublishChannel("docker") {
		t.Error("publish channels lost in round trip")
	}
	if got := parsed.Params.Float("coverage-threshold", 0, "coverageThreshold"); got != 80 {
		t.Errorf("param coercion across producer/consumer = %v, want 80", got)
	}
	if got := parsed.Params.String("target"); got != "es2022" {
		t.Errorf("target = %q, want es2022", got)
	}
	if raw, ok := parsed.Project.Options["generate"]; !ok {
		t.Fatal("project.options.generate missing from produced context")
	} else {
		var opts struct {
			Schema *bool `json:"schema"`
		}
		if err := json.Unmarshal(raw, &opts); err != nil {
			t.Fatalf("project.options.generate did not round trip as JSON: %v", err)
		}
		if opts.Schema == nil || *opts.Schema {
			t.Fatalf("project.options.generate.schema = %v, want false", opts.Schema)
		}
	}
}

// TestBuildJobContext_CarriesTheResolvedGenerationBinding pins the producer
// side of project.generatedFrom: a generated client's context carries what its
// putnami.json declares and the contract path the workspace load resolved, and
// passes the strict reader. An unresolved service carries no contract path, and
// a project that is not a client carries no member at all.
func TestBuildJobContext_CarriesTheResolvedGenerationBinding(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "the-cli-produces-a-conforming-context", "the-cli-context-passes-the-strict-reader-as-version-2")
	t.Parallel()
	ws := &workspace.Workspace{Name: "w", Root: "/workspace"}
	client := func(binding *workspace.GeneratedClientBinding) *ScheduledJob {
		return &ScheduledJob{
			Project: &workspace.Project{
				ID: "/clients/items", Name: "items-client", Path: "clients/items",
				Config: &wsproto.ProjectConfig{GeneratedFrom: &wsproto.GeneratedFrom{
					Service: "items", Language: wsproto.GeneratedLanguageGo,
					PackageName: "itemsclient", ClientName: "ItemsClient", OmitOperations: []string{"deleteItem"},
				}},
				GeneratedClient: binding,
			},
			Extension: &extension.ExtensionDescription{Name: "@putnami/clientgen", Path: "/workspace/tooling/clientgen-extension"},
			JobDef:    &extension.JobDefinition{Name: "client-generate~go", ExtensionName: "@putnami/clientgen"},
		}
	}
	parse := func(t *testing.T, job *ScheduledJob) *protocoljob.Context {
		t.Helper()
		data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		parsed, diags := protocoljob.ParseAndValidate(data)
		if diag.HasErrors(diags) {
			t.Fatalf("produced context violates the job context protocol: %v\n%s", diags, data)
		}
		return parsed
	}

	resolved := parse(t, client(&workspace.GeneratedClientBinding{
		ServiceID: "items", Language: "go", ProviderID: "/services/items",
		ContractPath: "services/items/schema/openapi.json", ContractSHA256: strings.Repeat("ab", 32),
	}))
	want := protocoljob.GeneratedFrom{
		Service: "items", Language: "go", PackageName: "itemsclient", ClientName: "ItemsClient",
		OmitOperations: []string{"deleteItem"}, ContractPath: "services/items/schema/openapi.json",
		ContractSHA256: strings.Repeat("ab", 32),
	}
	if got := resolved.Project.GeneratedFrom; got == nil || got.Service != want.Service || got.Language != want.Language ||
		got.PackageName != want.PackageName || got.ClientName != want.ClientName || got.ContractPath != want.ContractPath ||
		got.ContractSHA256 != want.ContractSHA256 || len(got.OmitOperations) != 1 || got.OmitOperations[0] != "deleteItem" {
		t.Fatalf("project.generatedFrom = %+v, want %+v", got, want)
	}

	unresolved := parse(t, client(&workspace.GeneratedClientBinding{
		ServiceID: "items", Language: "go", ContractSHA256: workspace.UnresolvedContractDigest,
	}))
	if got := unresolved.Project.GeneratedFrom; got == nil || got.ContractPath != "" || got.ContractSHA256 != "" || got.Service != "items" {
		t.Fatalf("unresolved project.generatedFrom = %+v, want the service with no contract path", got)
	}

	plain := client(nil)
	plain.Project.Config = &wsproto.ProjectConfig{}
	if got := parse(t, plain).Project.GeneratedFrom; got != nil {
		t.Fatalf("a project that is not a client carries project.generatedFrom = %+v", got)
	}
}
