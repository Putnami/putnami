package codegen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	protocfg "go.putnami.dev/protocol/config"
	sdkcodegen "go.putnami.dev/sdk/extension/codegen"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// stubVisitor is a configurable visitor used to drive the runner from tests
// without depending on the real openapi visitor's logic.
type stubVisitor struct {
	name   string
	result *sdkcodegen.Result
	err    error
	calls  int
}

func (s *stubVisitor) Name() string { return s.name }

func (s *stubVisitor) Visit(_ *sdkcodegen.Generation) (*sdkcodegen.Result, error) {
	s.calls++
	return s.result, s.err
}

// withVisitors registers visitors for the duration of a test, restoring the
// previous registry on cleanup. We can't reset the global registry from
// outside the SDK package, so we save and re-register on cleanup.
func withVisitors(t *testing.T, visitors ...sdkcodegen.Visitor) {
	t.Helper()
	previous := sdkcodegen.Visitors()
	for _, v := range visitors {
		sdkcodegen.Register(v)
	}
	t.Cleanup(func() {
		// The registry has no reset hook; the best we can do is leave
		// the test-registered visitors in place. Subsequent tests
		// should call withVisitors with their own stubs.
		_ = previous
	})
}

// promoteToApp turns a fixture project into a framework application: a main
// package (newTestContext already wrote one) requiring and importing
// go.putnami.dev/app. That flips describeWillCommit -> true, so
// build-generate defers the committed sidecars to build-describe — and it is the
// shape a project must have to opt out of committing generated schemas at all,
// because describe is what owns and restores the ceded .gen/schema tree.
func promoteToApp(t *testing.T, ctx *pctx.Context) {
	t.Helper()
	gomod := "module example.com/test\n\nrequire (\n\tgo.putnami.dev/app v0.1.0\n)\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "main.go"), []byte("package main\nimport _ \"go.putnami.dev/app\"\n"), 0o644); err != nil {
		t.Fatalf("writing main.go: %v", err)
	}
}

func newTestContext(t *testing.T) *pctx.Context {
	t.Helper()
	return newTestContextAt(t, t.TempDir())
}

// newTestContextAt is newTestContext for a caller-chosen project root, so a
// test can generate the same project under two different parent directories.
func newTestContextAt(t *testing.T, root string) *pctx.Context {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating project root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("writing main.go: %v", err)
	}
	return &pctx.Context{
		WorkspaceRoot: root,
		Workspace:     pctx.Workspace{Name: "ws", Version: "9.9.9"},
		Project: pctx.Project{
			Name:     "test-project",
			Path:     ".",
			FullPath: root,
		},
		Job:    pctx.Job{Name: "build-generate"},
		Params: pctx.Params{},
	}
}

// readManifest loads the generated .gen/generate-result.json so tests can
// assert on its contents. Fails the test on any IO/parse error.
func readManifest(t *testing.T, root string) *GenerateResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".gen", "generate-result.json"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	var m GenerateResult
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	return &m
}

func TestRunWithoutVisitorsWritesEmptyManifest(t *testing.T) {
	// No visitors registered for this test path; the runner should still
	// write a manifest so cache keys remain stable across runs.
	ctx := newTestContext(t)
	emit := jsonl.New()

	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
	}

	m := readManifest(t, ctx.Project.FullPath)
	if len(m.Schemas) != 0 {
		t.Errorf("expected 0 schemas, got %v", m.Schemas)
	}
}

func TestRunDispatchesVisitorsAndWritesSchemas(t *testing.T) {
	ctx := newTestContext(t)
	stub := &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{"hello":"world"}`),
			}},
		},
	}
	withVisitors(t, stub)

	emit := jsonl.New()
	status, data, err := Run(ctx, emit, nil)
	if err != nil || status != "OK" {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
	}
	if stub.calls != 1 {
		t.Errorf("visitor called %d times, want 1", stub.calls)
	}

	// Schema file written under .gen/ AND copied to project tree (default).
	for _, rel := range []string{".gen/schema/stub.json", "schema/stub.json"} {
		path := filepath.Join(ctx.Project.FullPath, rel)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file at %s: %v", rel, err)
		}
	}

	// Runner sets <visitorName>-spec to the .gen/ copy so it points at a file
	// that always exists, regardless of commit opt-out. It is recorded
	// PROJECT-RELATIVE: this manifest is cached and restored into other
	// checkouts.
	m := readManifest(t, ctx.Project.FullPath)
	const wantExport = ".gen/schema/stub.json"
	if got := m.Exports["stub-spec"]; got != wantExport {
		t.Errorf("manifest exports stub-spec = %q, want %q", got, wantExport)
	}
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, m.Exports["stub-spec"])); err != nil {
		t.Errorf("export path is missing on disk: %v", err)
	}
	if len(m.Schemas) != 2 {
		t.Errorf("manifest schemas = %v, want 2 entries", m.Schemas)
	}

	// Run-level data surfaces the same values the file does — it is stored in
	// the task's cache entry, so it must be checkout-independent too.
	exports, ok := data["exports"].(map[string]string)
	if !ok {
		t.Fatalf("expected exports in result data, got %#v", data)
	}
	if exports["stub-spec"] != wantExport {
		t.Errorf("result data stub-spec = %q, want %q", exports["stub-spec"], wantExport)
	}
}

// TestRunManifestIsCheckoutRelocatable pins the checkout-relocatability determinism invariant
// fixed: the manifest is captured as build-generate's cache output and restored
// into other worktrees, machines and CI runners, so generating the same project
// under two different parent directories must produce the same bytes. Before the
// fix, `exports` held absolute paths and the two differed — which is what
// `putnami cache verify` reported as double-run-artifact plus a hit-live tree
// difference on .gen/generate-result.json.
func TestRunManifestIsCheckoutRelocatable(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "relocatable-generation-manifest", "every-serialized-manifest-path-is-checkout-relocatable")
	withVisitors(t, &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{"hello":"world"}`),
			}},
			Exports: map[string]string{"loader": "./loader.js"},
		},
	})

	base := t.TempDir()
	read := func(root string) []byte {
		t.Helper()
		ctx := newTestContextAt(t, root)
		if status, _, err := Run(ctx, jsonl.New(), nil); err != nil || status != "OK" {
			t.Fatalf("Run(%s) = (%q, %v), want (OK, nil)", root, status, err)
		}
		data, err := os.ReadFile(filepath.Join(root, ".gen", "generate-result.json"))
		if err != nil {
			t.Fatalf("reading manifest: %v", err)
		}
		return data
	}

	// Deliberately different depths and names: an embedded checkout path
	// differs in both.
	first := read(filepath.Join(base, "checkout-a"))
	second := read(filepath.Join(base, "deeper", "nested", "checkout-b"))

	if !bytes.Equal(first, second) {
		t.Fatalf("manifest depends on the checkout location:\n--- a ---\n%s\n--- b ---\n%s", first, second)
	}
	if bytes.Contains(first, []byte(base)) {
		t.Fatalf("manifest embeds an absolute checkout path:\n%s", first)
	}

	var m GenerateResult
	if err := json.Unmarshal(first, &m); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	for key, value := range m.Exports {
		if filepath.IsAbs(value) {
			t.Errorf("exports[%q] = %q is absolute", key, value)
		}
	}
	for key, value := range m.Assets {
		if filepath.IsAbs(value) {
			t.Errorf("assets[%q] = %q is absolute", key, value)
		}
	}
	for _, value := range m.Schemas {
		if filepath.IsAbs(value) {
			t.Errorf("schemas entry %q is absolute", value)
		}
	}
	// A hook-shaped relative export survives the rewrite untouched: it was
	// already checkout-independent, which is all the manifest asks of it.
	if m.Exports["loader"] != "./loader.js" {
		t.Errorf("exports[loader] = %q, want ./loader.js", m.Exports["loader"])
	}
}

func TestRunExportPointsAtGenCopyEvenWhenCommitOptOut(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "generation-opt-out", "a-project-that-opts-out-still-gets-the-build-only-gen-copy")
	// With generate.schema=false, only the .gen/ copy exists. The runner's
	// <visitorName>-spec export must still point at a file on disk.
	//
	// The fixture is an APP because that is the only shape the opt-out is legal
	// on: .gen/schema is build-describe's declared output, so a project with no
	// describe phase has nothing that would restore the .gen copy on a cache hit
	// and the combination is refused instead
	// (TestRunRefusesCommitOptOutWithoutADescribePhase).
	ctx := newTestContext(t)
	promoteToApp(t, ctx)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}
	withVisitors(t, &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{}`),
			}},
		},
	})

	emit := jsonl.New()
	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
	}

	m := readManifest(t, ctx.Project.FullPath)
	exportPath := m.Exports["stub-spec"]
	if exportPath == "" {
		t.Fatal("expected stub-spec export")
	}
	if filepath.IsAbs(exportPath) {
		t.Errorf("export path must be project-relative, got %q", exportPath)
	}
	// Resolved against the project root it must name the .gen copy, which is
	// the only one that exists under this opt-out.
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, exportPath)); err != nil {
		t.Errorf("export path %q does not exist on disk: %v", exportPath, err)
	}
	if !strings.HasPrefix(exportPath, ".gen/") {
		t.Errorf("export path should point at the .gen copy, got %q", exportPath)
	}
}

func TestRunUsesStableVersionForVisitors(t *testing.T) {
	// Committed schemas must not embed git SHA / dirty markers from
	// ctx.Version.Full — that would churn on every commit. The runner
	// passes ctx.Version.Base when available, falls back to workspace
	// version, then "0.0.0".
	cases := []struct {
		name string
		ctx  func(*pctx.Context)
		want string
	}{
		{
			name: "Base preferred over Full",
			ctx: func(c *pctx.Context) {
				c.Version = &pctx.Version{Base: "1.2.3", Full: "1.2.3-abc-deadbeef"}
			},
			want: "1.2.3",
		},
		{
			name: "fall back to workspace version",
			ctx: func(c *pctx.Context) {
				c.Workspace.Version = "9.9.9"
				c.Version = nil
			},
			want: "9.9.9",
		},
		{
			name: "ultimate fallback is 0.0.0",
			ctx: func(c *pctx.Context) {
				c.Workspace.Version = ""
				c.Version = nil
			},
			want: "0.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newTestContext(t)
			tc.ctx(ctx)

			var seen string
			withVisitors(t, &recordingVisitor{
				name:  "captures-version",
				onSee: func(g *sdkcodegen.Generation) { seen = g.Project.Version },
			})

			emit := jsonl.New()
			if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
				t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
			}
			if seen != tc.want {
				t.Errorf("visitor saw version %q, want %q", seen, tc.want)
			}
		})
	}
}

// TestRunPassesTheProjectsOwnDeclaredVersion pins the input a producer of
// COMMITTED content must use: DeclaredVersion is read from the project's
// own putnami.json and is empty when the project declares none — it never falls
// back to the workspace version, which is what Version carries. The two are
// deliberately different values so a producer cannot confuse them.
func TestRunPassesTheProjectsOwnDeclaredVersion(t *testing.T) {
	cases := []struct {
		name        string
		projectJSON string
		want        string
	}{
		{name: "declared", projectJSON: `{"name":"test-project","version":"3.4.5"}`, want: "3.4.5"},
		{name: "inherited stays empty", projectJSON: `{"name":"test-project"}`, want: ""},
		{name: "no project config", projectJSON: "", want: ""},
		{name: "malformed config", projectJSON: `{`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newTestContext(t)
			ctx.Version = &pctx.Version{Base: "1.2.3", Full: "1.2.3-abc-deadbeef"}
			if tc.projectJSON != "" {
				if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "putnami.json"), []byte(tc.projectJSON), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var seen sdkcodegen.ProjectInfo
			withVisitors(t, &recordingVisitor{
				name:  "captures-declared-version",
				onSee: func(g *sdkcodegen.Generation) { seen = g.Project },
			})

			emit := jsonl.New()
			if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
				t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
			}
			if seen.DeclaredVersion != tc.want {
				t.Errorf("visitor saw declared version %q, want %q", seen.DeclaredVersion, tc.want)
			}
			if seen.Version != "1.2.3" {
				t.Errorf("workspace version = %q, want it unchanged at 1.2.3", seen.Version)
			}
		})
	}
}

// TestRunRefusesCommitOptOutWithoutADescribePhase pins the boundary the
// .gen/schema carve-out draws around options.generate.schema=false.
//
// A generated contract has exactly two durable homes: the tracked sidecar
// build-generate commits, and build-describe's cache entry for the ceded
// .gen/schema subtree. The opt-out removes the first. A project with no describe
// phase does not have the second — nothing restores .gen/schema for it — so the
// contract would live only until the next cache hit on build-generate's key.
// Refusing names the option, the project and the reason; the alternative is a
// build that silently stops producing a contract it produced yesterday.
func TestRunRefusesCommitOptOutWithoutADescribePhase(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "generation-opt-out", "opting-out-without-a-describe-phase-is-refused")
	// newTestContext writes a main package and a go.mod that does NOT require
	// go.putnami.dev/app, which is exactly the CLI/library shape describe skips.
	ctx := newTestContext(t)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}
	withVisitors(t, &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{}`),
			}},
		},
	})

	emit := jsonl.New()
	status, _, err := Run(ctx, emit, nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("Run = (%q, %v), want a refusal", status, err)
	}
	for _, want := range []string{"options.generate.schema=false", ctx.Project.Name, "describe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	// The refusal is raised before the first contract is written — the run has
	// already prepared .gen and cleared the previous run's staging mirror and
	// infra fragments by then, but no contract is staged, so a later reader sees
	// an absent artifact rather than one this run had no durable home for.
	if _, statErr := os.Stat(filepath.Join(ctx.Project.FullPath, ".gen", "schema", "stub.json")); !os.IsNotExist(statErr) {
		t.Errorf("a refused run still staged a contract: %v", statErr)
	}

	// Control: the same project without the opt-out is legal, and build-generate
	// remains the sole committer for it.
	ctx.Project.Options = nil
	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("Run without the opt-out = (%q, %v), want (OK, nil)", status, err)
	}
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "schema", "stub.json")); err != nil {
		t.Errorf("expected the committed copy for a project with no describe phase: %v", err)
	}
}

// TestContractHasNoDurableHome pins the other side of the refusal above.
//
// options.generate.schema is INHERITED: a workspace-level putnami.workspace.json default
// reaches every project in the workspace. A refusal keyed on the option alone
// would therefore fail the build of every Go library and CLI at once, most of
// which generate no contract and so have nothing to lose. The refusal is keyed on
// an artifact that would actually be suppressed, not on the option.
func TestContractHasNoDurableHome(t *testing.T) {
	for _, tc := range []struct {
		name            string
		rel             string
		commitSchemas   bool
		deferToDescribe bool
		want            bool
	}{
		{
			name: "ceded artifact, opted out, no describe phase",
			rel:  "schema/openapi.json",
			want: true,
		},
		{
			name:          "the committed sidecar is still written",
			rel:           "schema/openapi.json",
			commitSchemas: true,
		},
		{
			name:            "describe owns and restores the ceded subtree",
			rel:             "schema/openapi.json",
			deferToDescribe: true,
		},
		{
			// The artifact stays in build-generate's own declared output, so a
			// cache hit restores it whatever the option says.
			name: "artifact outside the ceded subtree",
			rel:  "assets/stub.json",
		},
		{
			name: "a path that merely starts with the word schema",
			rel:  "schemas.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contractHasNoDurableHome(tc.rel, tc.commitSchemas, tc.deferToDescribe); got != tc.want {
				t.Errorf("contractHasNoDurableHome(%q, commit=%v, defer=%v) = %v, want %v",
					tc.rel, tc.commitSchemas, tc.deferToDescribe, got, tc.want)
			}
		})
	}
}

// TestRunMirrorsCededSchemaForDescribe pins build-generate's half of the
// generate-stages/describe-converges hand-off.
//
// .gen/schema belongs to build-describe, so generate's own cache entry cannot
// carry what generate writes there and generate's restore DELETES the subtree.
// Describe still needs generate's static output — it is the `before` side of the
// OpenAPI merge and the source of every staged-but-unrewritten artifact — so
// generate keeps a copy under .gen/generate-staging/schema, which it does own.
// Without the mirror a generate cache hit would hand describe an empty tree and
// the merge would silently drop generate's contribution.
func TestRunMirrorsCededSchemaForDescribe(t *testing.T) {
	ctx := newTestContext(t)
	promoteToApp(t, ctx)
	withVisitors(t, &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{"a":1}`),
			}},
		},
	})

	emit := jsonl.New()
	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
	}

	contract := filepath.Join(ctx.Project.FullPath, ".gen", "schema", "stub.json")
	mirror := filepath.Join(ctx.Project.FullPath, ".gen", "generate-staging", "schema", "stub.json")
	want, err := os.ReadFile(contract)
	if err != nil {
		t.Fatalf("reading the contract copy: %v", err)
	}
	got, err := os.ReadFile(mirror)
	if err != nil {
		t.Fatalf("build-generate kept no mirror of the ceded subtree: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("mirror = %q, want the bytes at the contract path %q", got, want)
	}

	// The mirror is a private hand-off, never a second location a consumer could
	// learn: it appears in neither the manifest's schemas list nor its exports.
	m := readManifest(t, ctx.Project.FullPath)
	for _, value := range m.Schemas {
		if strings.Contains(value, "generate-staging") {
			t.Errorf("schemas lists the private mirror %q", value)
		}
	}
	for key, value := range m.Exports {
		if strings.Contains(value, "generate-staging") {
			t.Errorf("exports[%s] = %q points at the private mirror", key, value)
		}
	}

	// The mirror is rebuilt from scratch on every run. It lives inside
	// build-generate's own declared output, so a contract a removed producer left
	// there would be adopted into this task's cache entry and resurrected by every
	// later hit on that key.
	stale := filepath.Join(ctx.Project.FullPath, ".gen", "generate-staging", "schema", "removed-producer.json")
	if err := os.WriteFile(stale, []byte(`{"gone":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("second Run = (%q, %v), want (OK, nil)", status, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the mirror kept a contract the current producers no longer write: %v", err)
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Errorf("the rebuilt mirror lost a contract the producers still write: %v", err)
	}
}

// recordingVisitor invokes onSee with the Generation it receives, then
// returns an empty Result. Used by tests that need to inspect what the
// runner hands to visitors without producing artifacts.
type recordingVisitor struct {
	name  string
	onSee func(*sdkcodegen.Generation)
}

func (r *recordingVisitor) Name() string { return r.name }
func (r *recordingVisitor) Visit(g *sdkcodegen.Generation) (*sdkcodegen.Result, error) {
	r.onSee(g)
	return &sdkcodegen.Result{}, nil
}

func TestRunRespectsCommitOptOut(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "generation-opt-out", "opting-out-suppresses-the-committed-copy")
	ctx := newTestContext(t)
	// generate.schema=false → write under .gen/ only, no committed copy. The
	// fixture is an APP for the reason above: the opt-out is only legal where a
	// describe phase owns and restores .gen/schema. The describe half of the same
	// opt-out is proven by the collect-skips-the-commit-for-an-opted-out-project
	// check in describe_test.go.
	promoteToApp(t, ctx)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}
	withVisitors(t, &stubVisitor{
		name: "stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{}`),
			}},
		},
	})

	emit := jsonl.New()
	if status, _, err := Run(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, err)
	}

	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, ".gen", "schema", "stub.json")); err != nil {
		t.Errorf("expected .gen copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "schema", "stub.json")); !os.IsNotExist(err) {
		t.Errorf("expected NO committed copy when generate.schema=false, got err=%v", err)
	}
}

func TestRunSurfacesVisitorErrors(t *testing.T) {
	ctx := newTestContext(t)
	withVisitors(t, &stubVisitor{
		name: "broken",
		err:  errBoom,
	})

	emit := jsonl.New()
	status, _, err := Run(ctx, emit, nil)
	if err == nil {
		t.Fatal("expected error from broken visitor")
	}
	if status != "FAILED" {
		t.Errorf("status = %q, want FAILED", status)
	}
}

// TestWriteSchemaFileRejectsEscapingPaths is where the path-safety guarantee is
// actually proven. The Run-level test below cannot carry it: sdkcodegen's
// registry has no reset hook, so every stub visitor an earlier test registered
// is still installed, and Run short-circuits on one of those instead of ever
// reaching the traversal path — the assertion passes on somebody else's error.
// Writing straight to writeSchemaFile makes the check independent of test order
// and of what the registry happens to hold.
func TestWriteSchemaFileRejectsEscapingPaths(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "generation-path-safety", "a-generator-writing-outside-the-project-root-is-rejected")

	root := t.TempDir()
	genDir := filepath.Join(root, ".gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "escape.json")

	for _, relPath := range []string{
		"../escape.json",
		"../../escape.json",
		"nested/../../escape.json",
		outside,
		".",
	} {
		t.Run(relPath, func(t *testing.T) {
			written, genPath, err := writeSchemaFile(root, genDir, sdkcodegen.SchemaFile{
				RelPath: relPath,
				Content: []byte(`{}`),
			}, true)
			if err == nil {
				t.Fatalf("relpath %q was accepted (wrote %v at %q); a generator must not write outside the project root", relPath, written, genPath)
			}
			// The error quotes the relpath, so a Windows path shows doubled
			// backslashes: compare with the quoted form.
			if !strings.Contains(err.Error(), strconv.Quote(relPath)) {
				t.Errorf("error %q does not name the rejected relpath %q", err, relPath)
			}
			if written != nil {
				t.Errorf("rejected relpath %q still reported writes: %v", relPath, written)
			}
		})
	}

	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("a file appeared at %s; the rejection must happen before any write", outside)
	}
}

// TestRunRejectsTraversalPaths keeps the end-to-end shape: a visitor asking for
// an escaping path makes the whole generate job fail. It deliberately asserts
// only the job status — see the note above for why it cannot assert the cause.
func TestRunRejectsTraversalPaths(t *testing.T) {
	ctx := newTestContext(t)
	withVisitors(t, &stubVisitor{
		name: "evil",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "../escape.json",
				Content: []byte(`{}`),
			}},
		},
	})

	emit := jsonl.New()
	status, _, err := Run(ctx, emit, nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("expected FAILED with error, got (%q, %v)", status, err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(ctx.Project.FullPath), "escape.json")); !os.IsNotExist(statErr) {
		t.Error("the escaping artifact was written next to the project root")
	}
}

func TestRunGeneratesConfigSchemaArtifact(t *testing.T) {
	ctx := newTestContext(t)
	configSrc := "package main\n\n" +
		"import pconfig \"go.putnami.dev/config\"\n\n" +
		"type ServerCfg struct {\n" +
		"\tPort int `json:\"port\" default:\"8080\" env:\"PORT\"`\n" +
		"}\n\n" +
		"var ServerConfig = pconfig.Config[ServerCfg](\"cacheServer\")\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "config.go"), []byte(configSrc), 0o644); err != nil {
		t.Fatalf("writing config.go: %v", err)
	}

	emit := jsonl.New()
	_, _, _ = Run(ctx, emit, nil)

	schemaPath := filepath.Join(ctx.Project.FullPath, "schema", "config.json")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading generated config schema: %v", err)
	}
	var schema struct {
		AppName string `json:"appName"`
		Configs []struct {
			Path   string `json:"path"`
			Fields []struct {
				Name string `json:"name"`
				Env  string `json:"env"`
			} `json:"fields"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parsing config schema: %v", err)
	}
	if schema.AppName != ctx.Project.Name {
		t.Fatalf("schema appName = %q, want %q", schema.AppName, ctx.Project.Name)
	}
	if len(schema.Configs) != 1 || schema.Configs[0].Path != "cacheServer" {
		t.Fatalf("schema configs = %+v, want cacheServer block", schema.Configs)
	}
	if len(schema.Configs[0].Fields) != 1 || schema.Configs[0].Fields[0].Name != "port" || schema.Configs[0].Fields[0].Env != "PORT" {
		t.Fatalf("schema fields = %+v, want port env PORT", schema.Configs[0].Fields)
	}
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "schema", "config.jsonschema.json")); err != nil {
		t.Fatalf("expected config JSON Schema companion: %v", err)
	}
}

// A release tag or a workspace bump moves the stable version. The committed
// schema/config.json must not follow it, or every build after a tag rewrites a
// tracked file: it carries the version the project declares, or "0.0.0". The
// .gen fallback is per-run output and keeps the stable version.
func TestCommittedConfigSchemaDoesNotCarryTheWorkspaceVersion(t *testing.T) {
	configSrc := "package main\n\n" +
		"import pconfig \"go.putnami.dev/config\"\n\n" +
		"type ServerCfg struct {\n" +
		"\tPort int `json:\"port\" default:\"8080\" env:\"PORT\"`\n" +
		"}\n\n" +
		"var ServerConfig = pconfig.Config[ServerCfg](\"cacheServer\")\n"
	readVersion := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var schema struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		return schema.Version
	}
	for _, tc := range []struct {
		name        string
		projectJSON string
		want        string
	}{
		{name: "no declared version", want: "0.0.0"},
		{name: "declared version", projectJSON: `{"name":"test-project","version":"3.4.5"}`, want: "3.4.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newTestContext(t)
			ctx.Version = &pctx.Version{Base: "1.2.3", Full: "1.2.3-abc-deadbeef"}
			root := ctx.Project.FullPath
			if err := os.WriteFile(filepath.Join(root, "config.go"), []byte(configSrc), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.projectJSON != "" {
				if err := os.WriteFile(filepath.Join(root, "putnami.json"), []byte(tc.projectJSON), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			committed := filepath.Join(root, "schema", "config.json")

			// build-generate commits the schema of a project with no describe phase.
			_, _, _ = Run(ctx, jsonl.New(), nil)
			if got := readVersion(t, committed); got != tc.want {
				t.Fatalf("build-generate stamped the committed schema with %q, want %q", got, tc.want)
			}

			// describe commits it for an app, and stages the fallback otherwise.
			genDir := filepath.Join(root, ".gen")
			if err := os.MkdirAll(genDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := mergeDependencyConfigSchema(ctx, root, genDir, true); err != nil {
				t.Fatalf("mergeDependencyConfigSchema: %v", err)
			}
			if got := readVersion(t, committed); got != tc.want {
				t.Fatalf("describe stamped the committed schema with %q, want %q", got, tc.want)
			}
			if err := mergeDependencyConfigSchema(ctx, root, genDir, false); err != nil {
				t.Fatalf("mergeDependencyConfigSchema: %v", err)
			}
			if got := readVersion(t, filepath.Join(genDir, "config-schema.json")); got != "1.2.3" {
				t.Fatalf("the .gen fallback carries %q, want the stable version 1.2.3", got)
			}
		})
	}
}

// sentinel error reused across cases; declared at package scope so both the
// stubVisitor and the assertion can reference it.
var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// TestRunSyncsCommittedInfraRequirements proves the build-generate entry
// point (not just the standalone config-extract command) syncs the committed
// generator-owned infra/requirements.json that deploy planning and build
// aggregation consume, derived from the config schema's sensitive fields.
func TestRunSyncsCommittedInfraRequirements(t *testing.T) {
	ctx := newTestContext(t)
	configSrc := "package main\n\n" +
		"import \"go.putnami.dev/config\"\n\n" +
		"type Options struct {\n" +
		"\tPassword string `json:\"password\" env:\"DB_PASSWORD\" sensitive:\"true\"`\n" +
		"}\n\n" +
		"var DatabaseConfig = config.Config[Options](\"database\")\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "config.go"), []byte(configSrc), 0o644); err != nil {
		t.Fatalf("writing config.go: %v", err)
	}

	// At least one visitor must be registered for the full generate path to run;
	// production always links the openapi visitor. An empty-result stub stands
	// in here.
	withVisitors(t, &stubVisitor{name: "infra-sidecar-stub", result: &sdkcodegen.Result{}})

	// Infra requirements are synced before the visitor loop, so they land
	// regardless of whether other (globally registered, non-resettable) test
	// visitors make Run report FAILED. Assert on the committed contract, not
	// Run's status.
	emit := jsonl.New()
	_, _, _ = Run(ctx, emit, nil)

	requirements := filepath.Join(ctx.Project.FullPath, "infra", "requirements.json")
	data, err := os.ReadFile(requirements)
	if err != nil {
		t.Fatalf("reading committed infra requirements: %v", err)
	}
	var m struct {
		ProtocolVersion int      `json:"protocolVersion"`
		Secrets         []string `json:"secrets"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing infra requirements: %v", err)
	}
	if len(m.Secrets) != 1 || m.Secrets[0] != "db_password" {
		t.Fatalf("requirements secrets = %v, want [db_password]", m.Secrets)
	}
}

// TestRunDefersCommittedSidecarsWhenDescribeWillRun proves the generate-side
// half of the single-committer rule: for an app project (a main package built on go.putnami.dev/app,
// so a describe phase will run) build-generate must NOT write the committed
// sidecars. It stages the schema to .gen/ and leaves both schema/ and the
// committed infra/requirements.json untouched, so a later skipped / canceled /
// cached describe can't strand a degraded stub or a deletion in the tracked
// tree. Describe is the sole committer (see the describe-side tests).
//
// Contrast with TestRunSyncsCommittedInfraRequirements, whose non-app context
// has no describe phase and so commits requirements.json directly.
func TestRunDefersCommittedSidecarsWhenDescribeWillRun(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "generate-defers-the-committed-sidecar-when-describe-will-run")
	ctx := newTestContext(t)
	// Promote the fixture to a framework app with an authored import.
	promoteToApp(t, ctx)
	// A sensitive config field would, absent deferral, sync a committed
	// requirements.json (exactly what TestRunSyncsCommittedInfraRequirements
	// asserts for a non-app project).
	configSrc := "package main\n\n" +
		"import \"go.putnami.dev/config\"\n\n" +
		"type Options struct {\n" +
		"\tPassword string `json:\"password\" env:\"DB_PASSWORD\" sensitive:\"true\"`\n" +
		"}\n\n" +
		"var DatabaseConfig = config.Config[Options](\"database\")\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "config.go"), []byte(configSrc), 0o644); err != nil {
		t.Fatalf("writing config.go: %v", err)
	}
	withVisitors(t, &stubVisitor{
		name: "defer-stub",
		result: &sdkcodegen.Result{
			SchemaFiles: []sdkcodegen.SchemaFile{{
				RelPath: "schema/stub.json",
				Content: []byte(`{"hello":"world"}`),
			}},
		},
	})

	// Status is intentionally ignored: a previously-registered (non-resettable)
	// broken visitor can make Run report FAILED. The committed-tree contract is
	// what matters, and config extraction + the requirements sync run before the
	// visitor loop regardless.
	emit := jsonl.New()
	_, _, _ = Run(ctx, emit, nil)

	// requirements.json: deferred — not written to the tracked tree.
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "infra", "requirements.json")); !os.IsNotExist(err) {
		t.Errorf("expected NO committed requirements.json while deferring to describe, got err=%v", err)
	}
	// schema sidecar: deferred — staged to .gen/ but not committed.
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "schema", "stub.json")); !os.IsNotExist(err) {
		t.Errorf("expected NO committed schema/stub.json while deferring to describe, got err=%v", err)
	}
	// config schema: also deferred. build-generate must NOT commit
	// schema/config.json — it stages the workload's own blocks to
	// .gen/config-schema.json and lets describe be the sole committer of the
	// converged own+plugin schema. If generate committed here, a late generate
	// could clobber describe's merged (own+events) schema and drop the plugin
	// block.
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "schema", "config.json")); !os.IsNotExist(err) {
		t.Errorf("expected NO committed schema/config.json while deferring to describe, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, ".gen", "config-schema.json")); err != nil {
		t.Errorf("expected build-generate to STAGE the config schema at .gen/config-schema.json, got err=%v", err)
	}
}

// newAppContext returns a test context for an app project (main package built on
// go.putnami.dev/app, so describeWillCommit -> true) whose source declares one
// own config block ("server"). This is the deferToDescribe case.
func newAppContext(t *testing.T) *pctx.Context {
	t.Helper()
	ctx := newTestContext(t)
	gomod := "module example.com/app\n\ngo 1.25\n\nrequire (\n\tgo.putnami.dev/app v0.1.0\n)\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	configSrc := "package main\n\n" +
		"import (\n_ \"go.putnami.dev/app\"\n\"go.putnami.dev/config\"\n)\n\n" +
		"type ServerOptions struct {\n" +
		"\tHost string `json:\"host\"`\n" +
		"}\n\n" +
		"var ServerConfig = config.Config[ServerOptions](\"server\")\n\n" +
		"func main() {}\n"
	if err := os.WriteFile(filepath.Join(ctx.Project.FullPath, "config.go"), []byte(configSrc), 0o644); err != nil {
		t.Fatalf("writing config.go: %v", err)
	}
	// Remove the empty main.go newTestContext wrote so the project has a single
	// package main declaration.
	_ = os.Remove(filepath.Join(ctx.Project.FullPath, "main.go"))
	return ctx
}

func readConfigManifest(t *testing.T, projectPath string) protocfg.SchemaManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectPath, "schema", "config.json"))
	if err != nil {
		t.Fatalf("reading committed config schema: %v", err)
	}
	var m protocfg.SchemaManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing config schema: %v", err)
	}
	return m
}

func configPaths(m protocfg.SchemaManifest) map[string]bool {
	out := map[string]bool{}
	for _, b := range m.Configs {
		out[b.Path] = true
	}
	return out
}

// TestConfigSchemaDescribeIsSoleCommitter_LateGenerateCannotClobberMerge pins the
// Determinism invariant: describe is the sole committer of schema/config.json
// for app projects, so a late-scheduled build~generate / test~generate can no
// longer clobber describe's merged (own+plugin) schema and drop a plugin block.
//
// It reproduces the two-committer race deterministically by driving the real
// functions in the exact order the --impacted scheduler can produce:
//
//  1. build~generate runs (deferToDescribe): asserts it does NOT commit
//     schema/config.json, only stages own blocks to .gen/config-schema.json.
//  2. build~describe merges a plugin-contributed "events" block from
//     .gen/config-deps.json: asserts the committed schema now holds BOTH the own
//     "server" block and the plugin "events" block, at the merged schemaHash.
//  3. A LATE test~generate runs again: asserts the committed schema is UNCHANGED
//     (still own+events) — generate never reverts describe's merge.
func TestConfigSchemaDescribeIsSoleCommitter_LateGenerateCannotClobberMerge(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "single-schema-committer", "a-late-generate-cannot-clobber-the-describe-merge")
	ctx := newAppContext(t)
	projectPath := ctx.Project.FullPath
	committed := filepath.Join(projectPath, "schema", "config.json")

	// Step 1: build~generate. It defers to describe, so it must NOT write the
	// committed config schema — it stages own blocks to .gen/config-schema.json.
	// Run's status is ignored: config extraction runs before the visitor loop, so
	// a previously-registered (non-resettable) broken visitor can make Run report
	// FAILED without affecting the committed-tree contract under test.
	_, _, _ = Run(ctx, jsonl.New(), nil)
	if _, err := os.Stat(committed); !os.IsNotExist(err) {
		t.Fatalf("build~generate must NOT commit schema/config.json (describe is sole committer), got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, ".gen", "config-schema.json")); err != nil {
		t.Fatalf("build~generate must stage config schema to .gen/config-schema.json, got err=%v", err)
	}

	// Step 2: build~describe. Provide the plugin's contributed block via the
	// .gen/config-deps.json fragment (the describe binary's output — stubbed
	// directly so we don't compile/run a real app), then run the real merge.
	genDir := filepath.Join(projectPath, ".gen")
	deps := configDepsDocument{Blocks: []protocfg.Block{{
		Path:   "events",
		Fields: []protocfg.FieldSchema{{Name: "broker", Type: protocfg.FieldTypeString}},
	}}}
	depsData, err := json.Marshal(&deps)
	if err != nil {
		t.Fatalf("marshal config-deps: %v", err)
	}
	if err := os.WriteFile(filepath.Join(genDir, configDepsFragmentName), depsData, 0o644); err != nil {
		t.Fatalf("writing config-deps.json: %v", err)
	}
	if err := mergeDependencyConfigSchema(ctx, projectPath, genDir, true); err != nil {
		t.Fatalf("mergeDependencyConfigSchema: %v", err)
	}
	merged := readConfigManifest(t, projectPath)
	if paths := configPaths(merged); !paths["server"] || !paths["events"] {
		t.Fatalf("merged schema must contain both own server and plugin events blocks, got %v", paths)
	}
	mergedHash := merged.SchemaHash
	wantHash := protocfg.ComputeSchemaHash(merged.Configs)
	if mergedHash == "" || mergedHash != wantHash {
		t.Fatalf("merged schemaHash = %q, want %q", mergedHash, wantHash)
	}

	// Step 3: a LATE test~generate runs (same project, no lock, own-only blocks).
	// Before the fix this reverted the committed schema to own-only and dropped
	// the events block. Now generate stages to .gen and never touches the file.
	_, _, _ = Run(ctx, jsonl.New(), nil)
	after := readConfigManifest(t, projectPath)
	if paths := configPaths(after); !paths["server"] || !paths["events"] {
		t.Fatalf("late generate clobbered describe's merge; schema = %v, want server+events", paths)
	}
	if after.SchemaHash != mergedHash {
		t.Fatalf("late generate flipped schemaHash: got %q, want %q", after.SchemaHash, mergedHash)
	}
}

// TestConfigSchemaDescribeCommitsOwnOnlyWhenNoDepBlocks pins the highest-risk
// regression: an app with OWN config blocks but NO config-owning
// plugins. build-generate defers (stages to .gen only), so describe MUST write
// the committed own-only schema even though .gen/config-deps.json is absent —
// otherwise the committed schema/config.json would vanish for such projects.
// This proves removing mergeDependencyConfigSchema's empty-fragment early-return
// is load-bearing.
func TestConfigSchemaDescribeCommitsOwnOnlyWhenNoDepBlocks(t *testing.T) {
	ctx := newAppContext(t)
	projectPath := ctx.Project.FullPath
	committed := filepath.Join(projectPath, "schema", "config.json")

	// build~generate defers: no committed schema yet. Run's status is ignored
	// (see the sole-committer test) — config extraction runs before visitors.
	_, _, _ = Run(ctx, jsonl.New(), nil)
	if _, err := os.Stat(committed); !os.IsNotExist(err) {
		t.Fatalf("build~generate must NOT commit schema/config.json, got err=%v", err)
	}

	// build~describe with NO config-deps.json fragment (no config-owning plugins).
	// describe must still commit the own-only schema.
	genDir := filepath.Join(projectPath, ".gen")
	if err := mergeDependencyConfigSchema(ctx, projectPath, genDir, true); err != nil {
		t.Fatalf("mergeDependencyConfigSchema: %v", err)
	}
	m := readConfigManifest(t, projectPath)
	paths := configPaths(m)
	if len(paths) != 1 || !paths["server"] {
		t.Fatalf("describe must commit own-only schema (server block), got %v", paths)
	}
}

// TestConfigSchemaDescribeWritesNothingWhenNoConfigAtAll proves the zero-total-
// blocks path: a workload with a main package but NO config blocks at
// all still gets NO committed schema/config.json — matching prior semantics even
// though describe now always calls the merge.
func TestConfigSchemaDescribeWritesNothingWhenNoConfigAtAll(t *testing.T) {
	ctx := newTestContext(t)
	projectPath := ctx.Project.FullPath
	// App project with a main package but no config.Config[T] anywhere.
	gomod := "module example.com/app\n\ngo 1.25\n\nrequire (\n\tgo.putnami.dev/app v0.1.0\n)\n"
	if err := os.WriteFile(filepath.Join(projectPath, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "main.go"), []byte("package main\n\nimport _ \"go.putnami.dev/app\"\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("writing main.go: %v", err)
	}

	_, _, _ = Run(ctx, jsonl.New(), nil)
	genDir := filepath.Join(projectPath, ".gen")
	if err := mergeDependencyConfigSchema(ctx, projectPath, genDir, true); err != nil {
		t.Fatalf("mergeDependencyConfigSchema: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("a workload with no config must get NO committed schema/config.json, got err=%v", err)
	}
}
