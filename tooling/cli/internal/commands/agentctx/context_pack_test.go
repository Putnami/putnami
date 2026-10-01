package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentcontext "go.putnami.dev/protocol/agentcontext"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The two non-deterministic inputs are pinned as constants so the fixture output
// is fully reproducible across runs.
const (
	fixtureRevision = "0123456789abcdef0123456789abcdef01234567"
	fixtureVersion  = "9.9.9"
)

// secretInfraManifest is a per-project infra manifest that declares a secret, so
// its reference must be classified secret/sensitive by the aggregator.
const secretInfraManifest = `{
  "protocolVersion": 2,
  "secrets": ["jwks_signing_key"]
}`

// appCapabilitiesManifest is the app's committed capability manifest. Its
// contribution provenance points at a spread of evidence paths that exercise
// every representative-source filter: an in-project source (kept), the main file
// (deduped against the applicationMain range), a non-source .mod path (dropped),
// and another project's source (dropped by the in-project prefix filter).
const appCapabilitiesManifest = `{
  "protocolVersion": 1,
  "project": "/svc/app",
  "discoverers": [
    {
      "name": "app-service",
      "kind": "source",
      "provenance": {
        "project": "/svc/app",
        "sourceKind": "generated",
        "evidencePath": "svc/app/service.go"
      }
    },
    {
      "name": "app-main",
      "kind": "source",
      "provenance": {
        "project": "/svc/app",
        "sourceKind": "generated",
        "evidencePath": "svc/app/main.go"
      }
    },
    {
      "name": "app-mod",
      "kind": "config",
      "provenance": {
        "project": "/svc/app",
        "sourceKind": "framework",
        "evidencePath": "svc/app/go.mod"
      }
    },
    {
      "name": "lib-helper",
      "kind": "source",
      "provenance": {
        "project": "/svc/lib",
        "sourceKind": "framework",
        "evidencePath": "svc/lib/helper.go"
      }
    }
  ]
}`

// buildFixtureWorkspace writes a controlled two-project workspace under a temp
// root: an application "app" (with a main, a describe hook, committed schema/
// infra/migration artifacts including a secret-declaring infra manifest) that
// depends on a library "lib". It returns the loaded workspace and the app
// project, so a test pins BuildDocument's inputs entirely.
func buildFixtureWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	root := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Application project files: markers, composition roots, and committed
	// by-reference artifacts.
	write("svc/app/go.mod", "module example/app\n\ngo 1.25\n")
	write("svc/app/main.go", "package main\n\nfunc main() {}\n")
	write("svc/app/service.go", "package main\n\nfunc Service() {}\n")
	write("svc/app/describe.go", "package main\n")
	write("svc/app/schema/capabilities.json", appCapabilitiesManifest+"\n")
	write("svc/app/schema/contracts.json", `{"protocolVersion":1,"name":"example/app"}`+"\n")
	write("svc/app/schema/config.json", `{"protocolVersion":1}`+"\n")
	write("svc/app/infra/requirements.json", secretInfraManifest+"\n")
	write("svc/app/migrations/bundle.json", `{"bundleProtocol":"migration-bundle.v1"}`+"\n")

	// Library project: a dependency of app. helper.go exists so an app capability
	// evidence path pointing INTO lib is provably excluded by the in-project
	// filter (existence is not the reason it is dropped).
	write("svc/lib/go.mod", "module example/lib\n\ngo 1.25\n")
	write("svc/lib/helper.go", "package lib\n")

	app := &workspace.Project{
		ID:           "/svc/app",
		Name:         "example/app",
		Path:         "svc/app",
		Type:         "application",
		Tags:         []string{"go", "e2e"},
		Config:       &wsproto.ProjectConfig{Main: "main.go"},
		Dependencies: []string{"example/lib"},
	}
	lib := &workspace.Project{
		ID:   "/svc/lib",
		Name: "example/lib",
		Path: "svc/lib",
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{app, lib})
	return ws, app
}

// TestBuildDocument_Deterministic pins the acceptance-critical invariants: the
// aggregated document is byte-stable, round-trips through the strict protocol
// parser, passes the fail-closed publish-safety gate, and classifies a
// secret-declaring infra manifest as secret/sensitive AND records its path in
// the sensitive set the gate runs with.
func TestBuildDocument_Deterministic(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)

	doc, opts, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}

	// Byte-stability: the same inputs must serialize identically every time.
	first, err := canonicalDocumentBytes(doc, opts, app.ID)
	if err != nil {
		t.Fatalf("canonicalDocumentBytes: %v", err)
	}
	for i := range 3 {
		doc2, opts2, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
		if err != nil {
			t.Fatalf("BuildDocument iteration %d: %v", i, err)
		}
		got, err := canonicalDocumentBytes(doc2, opts2, app.ID)
		if err != nil {
			t.Fatalf("canonicalDocumentBytes iteration %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d: document not byte-stable\n--- first:\n%s\n--- got:\n%s", i, first, got)
		}
	}

	// Round-trip cleanly through the strict protocol parser + validator.
	parsed, diags := agentcontext.ParseAndValidateDocument(first)
	if parsed == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip failed to parse/validate: %v", diags)
	}

	// Fail-closed publish-safety gate passes on a correctly flagged document.
	if diags := agentcontext.ValidatePublishSafety(doc, opts); diag.HasErrors(diags) {
		t.Fatalf("publish-safety gate rejected a valid document: %v", diags)
	}

	// Canonical framing: two-space indent + trailing newline, protocol version,
	// and the pinned schema URI.
	if !strings.HasSuffix(string(first), "\n") {
		t.Error("canonical bytes must end with a trailing newline")
	}
	if doc.Schema != agentContextSchemaURI {
		t.Errorf("schema = %q, want %q", doc.Schema, agentContextSchemaURI)
	}
	if doc.ProtocolVersion != agentcontext.ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", doc.ProtocolVersion, agentcontext.ProtocolVersion)
	}

	// Infra reference is classified secret/sensitive, and its path is in the
	// caller-owned sensitive set (so the gate passes BECAUSE it is flagged).
	if len(doc.Infra) != 1 {
		t.Fatalf("infra refs = %d, want 1", len(doc.Infra))
	}
	infraRef := doc.Infra[0]
	if !infraRef.Sensitive || infraRef.Kind != infraSecretKind {
		t.Errorf("infra ref = %+v, want sensitive=true kind=%q", infraRef, infraSecretKind)
	}
	if !opts.SensitivePaths[infraRef.Path] {
		t.Errorf("sensitive set %v missing infra path %q", opts.SensitivePaths, infraRef.Path)
	}
	if !strings.HasPrefix(infraRef.Digest, "sha256:") || len(infraRef.Digest) != len("sha256:")+64 {
		t.Errorf("infra digest %q is not a sha256:<64hex> address", infraRef.Digest)
	}

	// Identity graph: declared dependency NAMES resolve to ids; languages derive
	// from the go.mod marker.
	if got := doc.Identity.Dependencies; len(got) != 1 || got[0] != "/svc/lib" {
		t.Errorf("dependencies = %v, want [/svc/lib]", got)
	}
	if got := doc.Identity.Languages; len(got) != 1 || got[0] != "go" {
		t.Errorf("languages = %v, want [go]", got)
	}

	// Composition roots: applicationMain from Main, describeEntrypoint from the
	// describe.go hook.
	kinds := map[agentcontext.RootKind]string{}
	for _, r := range doc.CompositionRoots {
		kinds[r.Kind] = r.Path
	}
	if kinds[agentcontext.RootKindApplicationMain] != "svc/app/main.go" {
		t.Errorf("applicationMain root = %q, want svc/app/main.go", kinds[agentcontext.RootKindApplicationMain])
	}
	if kinds[agentcontext.RootKindDescribeEntrypoint] != "svc/app/describe.go" {
		t.Errorf("describeEntrypoint root = %q, want svc/app/describe.go", kinds[agentcontext.RootKindDescribeEntrypoint])
	}
}

// TestBuildDocument_DependentsResolveToIDs asserts a library's direct dependents
// are aggregated as project ids (from the id-keyed dependency graph), not names.
func TestBuildDocument_DependentsResolveToIDs(t *testing.T) {
	ws, _ := buildFixtureWorkspace(t)
	lib := ws.ProjectByID("/svc/lib")
	if lib == nil {
		t.Fatal("fixture lib project missing")
	}
	doc, _, err := BuildDocument(ws, lib, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(lib): %v", err)
	}
	if got := doc.Identity.Dependents; len(got) != 1 || got[0] != "/svc/app" {
		t.Errorf("dependents = %v, want [/svc/app]", got)
	}
}

func TestBuildIdentity_GroupedProjectUsesLogicalIDAndPhysicalPath(t *testing.T) {
	root := t.TempDir()
	projectPath := "identity/(workloads)/auth-server"
	writeFile := func(rel, content string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	writeFile("putnami.workspace.json", `{"includes":["identity/(workloads)/auth-server"]}`)
	writeFile(projectPath+"/go.mod", "module example/auth-server\n\ngo 1.25\n")

	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := ws.ProjectByID("/identity/auth-server")
	if p == nil {
		t.Fatal("grouped project missing by logical ID")
	}
	identity := buildIdentity(ws, p, filepath.Join(root, filepath.FromSlash(projectPath)))
	if identity.ID != "/identity/auth-server" || identity.Path != projectPath {
		t.Errorf("generated identity = {ID:%q Path:%q}, want logical ID and physical path", identity.ID, identity.Path)
	}
}

// TestBuildCompositionRoots_GoConventionMain asserts the Go-entrypoint fallback:
// a Go application declares no entrypoint at all, so an applicationMain root
// must be derived from a conventional root main.go that declares `package main`
// — and a main.go declaring a non-main package must NOT be misreported as one.
func TestBuildCompositionRoots_GoConventionMain(t *testing.T) {
	root := t.TempDir()
	writeFile := func(rel, content string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Go application: no declared entrypoint, a root main.go declaring `package main`.
	writeFile("svc/goapp/main.go", "// a leading comment\npackage main\n\nfunc main() {}\n")
	// A library-shaped project whose root main.go declares a non-main package.
	writeFile("svc/golib/main.go", "package golib\n")

	goapp := &workspace.Project{ID: "/svc/goapp", Name: "goapp", Path: "svc/goapp", Type: "application"}
	golib := &workspace.Project{ID: "/svc/golib", Name: "golib", Path: "svc/golib", Type: "application"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{goapp, golib})

	appDoc, _, err := BuildDocument(ws, goapp, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(goapp): %v", err)
	}
	if len(appDoc.CompositionRoots) != 1 {
		t.Fatalf("goapp composition roots = %+v, want a single applicationMain", appDoc.CompositionRoots)
	}
	rootEntry := appDoc.CompositionRoots[0]
	if rootEntry.Kind != agentcontext.RootKindApplicationMain || rootEntry.Path != "svc/goapp/main.go" || rootEntry.Provenance != "convention=main.go" {
		t.Errorf("goapp root = %+v, want applicationMain svc/goapp/main.go convention=main.go", rootEntry)
	}

	libDoc, _, err := BuildDocument(ws, golib, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(golib): %v", err)
	}
	if len(libDoc.CompositionRoots) != 0 {
		t.Errorf("golib composition roots = %+v, want none (main.go is not package main)", libDoc.CompositionRoots)
	}
}

// TestSelectContextProjects_MissingProjectValue asserts a --project flag with no
// value is a usage error, not the flag-absent "all projects" default — so
// `context pack --project --check` (or a trailing --project) reports invalid
// usage instead of silently checking/writing every project.
func TestSelectContextProjects_MissingProjectValue(t *testing.T) {
	ws, _ := buildFixtureWorkspace(t)

	for _, args := range [][]string{
		{"--project", "--check"}, // value slot is another flag
		{"--project"},            // trailing, no value
		{"--project="},           // empty inline value
	} {
		got, err := selectContextProjects(ws, args)
		if err == nil {
			t.Errorf("selectContextProjects(%v) = %d projects, nil; want a usage error", args, len(got))
			continue
		}
		if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
			t.Errorf("selectContextProjects(%v) exit code = %d, want %d (usage)", args, code, protocolcli.ExitUsage)
		}
	}

	// An absent flag still targets every project; a real value targets exactly one.
	if all, err := selectContextProjects(ws, []string{"--check"}); err != nil || len(all) == 0 {
		t.Errorf("selectContextProjects([--check]) = (%d, %v); want all projects, no error", len(all), err)
	}
	if one, err := selectContextProjects(ws, []string{"--project", "/svc/app"}); err != nil || len(one) != 1 {
		t.Errorf("selectContextProjects([--project /svc/app]) = (%d, %v); want exactly one project", len(one), err)
	}
}

// TestCanonicalDocumentBytes_FailsClosedOnUnflaggedSensitive asserts the emit
// path fails closed: a document that references a caller-flagged sensitive path
// WITHOUT the sensitivity flag is rejected with an exit-2 classified error, no
// bytes are produced (the document is never written), and the machine-readable
// violation travels on the error.
func TestCanonicalDocumentBytes_FailsClosedOnUnflaggedSensitive(t *testing.T) {
	sensitivePath := "svc/app/infra/requirements.json"
	doc := &agentcontext.Document{
		Schema:          agentContextSchemaURI,
		ProtocolVersion: agentcontext.ProtocolVersion,
		Identity:        agentcontext.Identity{ID: "/svc/app", Name: "example/app", Path: "svc/app"},
		// The reference is structurally valid but NOT marked sensitive.
		Infra: []agentcontext.ArtifactRef{{Path: sensitivePath, Digest: "sha256:" + strings.Repeat("a", 64)}},
		Provenance: agentcontext.Provenance{
			WorkspaceRevision: fixtureRevision,
			Generator:         agentcontext.Generator{Name: contextGeneratorName, Version: fixtureVersion},
			AggregationMethod: agentcontext.AggregationMethodByReference,
		},
	}
	opts := agentcontext.PublishSafetyOptions{SensitivePaths: map[string]bool{sensitivePath: true}}

	data, err := canonicalDocumentBytes(doc, opts, doc.Identity.ID)
	if err == nil {
		t.Fatal("expected the publish-safety gate to fail closed, got nil error")
	}
	if data != nil {
		t.Fatalf("unsafe document produced %d bytes; nothing must be serialized", len(data))
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
	report, ok := shared.ResultData(err).(ContextSafetyReport)
	if !ok {
		t.Fatalf("error does not carry a ContextSafetyReport: %#v", shared.ResultData(err))
	}
	found := false
	for _, v := range report.Violations {
		if v.Code == agentcontext.ErrorCodeUnredactedSensitive {
			found = true
		}
	}
	if !found {
		t.Errorf("safety report %+v missing an unredacted_sensitive violation", report.Violations)
	}
}

// TestContextCheck_FreshThenDrift asserts the freshness gate: right after pack,
// check is clean (exit 0); mutating a referenced artifact (so its digest, and
// thus the aggregated document, changes) makes check report drift and return the
// exit-2 error with the drift report populated.
func TestContextCheck_FreshThenDrift(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)
	lib := ws.ProjectByID("/svc/lib")
	projects := []*workspace.Project{app, lib}

	// Before packing, every document is missing → drift.
	pre, preErr := ContextCheck(ws, projects, fixtureRevision, fixtureVersion)
	if preErr == nil || pre.Outcome != ContextOutcomeDrift {
		t.Fatalf("pre-pack check should drift as missing, got outcome=%q err=%v", pre.Outcome, preErr)
	}

	if _, err := ContextPack(ws, projects, fixtureRevision, fixtureVersion); err != nil {
		t.Fatalf("ContextPack: %v", err)
	}

	// The written document exists at the protocol's emit path.
	docPath := filepath.Join(ws.Root, "svc", "app", agentcontext.DocumentEmitDir, agentcontext.DocumentFilename)
	if _, err := os.Stat(docPath); err != nil {
		t.Fatalf("pack did not write %s: %v", docPath, err)
	}

	// Freshly packed → clean, exit 0.
	clean, cleanErr := ContextCheck(ws, projects, fixtureRevision, fixtureVersion)
	if cleanErr != nil {
		t.Fatalf("post-pack check returned error: %v", cleanErr)
	}
	if clean.Outcome != ContextOutcomeClean || len(clean.Drift) != 0 {
		t.Fatalf("post-pack check = %+v, want clean with no drift", clean)
	}

	// Mutate a referenced artifact so the fresh aggregation no longer matches.
	capPath := filepath.Join(ws.Root, "svc", "app", "schema", "capabilities.json")
	if err := os.WriteFile(capPath, []byte(`{"protocolVersion":1,"changed":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("mutate capabilities: %v", err)
	}

	drift, driftErr := ContextCheck(ws, projects, fixtureRevision, fixtureVersion)
	if driftErr == nil {
		t.Fatal("check should fail after a referenced artifact changed")
	}
	if drift.Outcome != ContextOutcomeDrift {
		t.Fatalf("outcome = %q, want %q", drift.Outcome, ContextOutcomeDrift)
	}
	if protocolcli.ExitCodeForError(driftErr) != protocolcli.ExitUsage {
		t.Fatalf("drift exit code = %d, want %d", protocolcli.ExitCodeForError(driftErr), protocolcli.ExitUsage)
	}
	if len(drift.Drift) != 1 || drift.Drift[0].Project != "/svc/app" || drift.Drift[0].Reason != "stale" {
		t.Fatalf("drift = %+v, want a single stale /svc/app entry", drift.Drift)
	}
	if data, ok := shared.ResultData(driftErr).(ContextCheckReport); !ok || data.Outcome != ContextOutcomeDrift {
		t.Fatalf("drift error does not carry the report as ResultData: %#v", shared.ResultData(driftErr))
	}
}

// assertBytesDiv4 asserts a SourceRange is a whole-file range (startLine 1,
// endLine >= startLine) whose token estimate is the ceil(bytes/4) heuristic over
// the referenced file's committed bytes.
func assertBytesDiv4(t *testing.T, label, abs string, s agentcontext.SourceRange) {
	t.Helper()
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	if s.StartLine != 1 {
		t.Errorf("%s startLine = %d, want 1", label, s.StartLine)
	}
	if s.EndLine < s.StartLine {
		t.Errorf("%s endLine = %d, want >= startLine %d", label, s.EndLine, s.StartLine)
	}
	if s.Tokens.Method != agentcontext.TokenMethodBytesDiv4 {
		t.Errorf("%s token method = %q, want %q", label, s.Tokens.Method, agentcontext.TokenMethodBytesDiv4)
	}
	if want := (len(data) + 3) / 4; s.Tokens.Estimated != want {
		t.Errorf("%s token estimate = %d, want ceil(%d/4)=%d", label, s.Tokens.Estimated, len(data), want)
	}
}

// TestBuildDocument_RepresentativeSources pins deterministic representative-source
// selection: the applicationMain file yields a why=main range, an in-project
// capability evidence path yields a why=capability-evidence range (each with a
// bytes/4 token estimate), and evidence paths that point at a non-source file
// (go.mod) or another project (svc/lib/helper.go) are excluded — as is the main
// file when an evidence path also names it (deduped, never a second range).
func TestBuildDocument_RepresentativeSources(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)

	doc, _, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}

	byPath := map[string]agentcontext.SourceRange{}
	for _, s := range doc.RepresentativeSources {
		if _, dup := byPath[s.Path]; dup {
			t.Fatalf("representativeSources has a duplicate range for %q", s.Path)
		}
		byPath[s.Path] = s
	}

	main, ok := byPath["svc/app/main.go"]
	if !ok {
		t.Fatalf("representativeSources %+v missing the main range", doc.RepresentativeSources)
	}
	if main.Why != agentcontext.SourceReasonMain {
		t.Errorf("main why = %q, want %q", main.Why, agentcontext.SourceReasonMain)
	}
	assertBytesDiv4(t, "main.go", filepath.Join(ws.Root, "svc", "app", "main.go"), main)

	svc, ok := byPath["svc/app/service.go"]
	if !ok {
		t.Fatalf("representativeSources %+v missing the capability-evidence range", doc.RepresentativeSources)
	}
	if svc.Why != agentcontext.SourceReasonCapabilityEvidence {
		t.Errorf("service.go why = %q, want %q", svc.Why, agentcontext.SourceReasonCapabilityEvidence)
	}
	assertBytesDiv4(t, "service.go", filepath.Join(ws.Root, "svc", "app", "service.go"), svc)

	if _, bad := byPath["svc/app/go.mod"]; bad {
		t.Error("representativeSources must exclude a non-source (.mod) evidence path")
	}
	if _, bad := byPath["svc/lib/helper.go"]; bad {
		t.Error("representativeSources must exclude another project's evidence path")
	}

	// Exactly main + one capability-evidence, ordered main first.
	if len(doc.RepresentativeSources) != 2 {
		t.Fatalf("representativeSources = %+v, want exactly main + one capability-evidence", doc.RepresentativeSources)
	}
	if doc.RepresentativeSources[0].Why != agentcontext.SourceReasonMain ||
		doc.RepresentativeSources[1].Why != agentcontext.SourceReasonCapabilityEvidence {
		t.Errorf("order = [%q,%q], want [main, capability-evidence]",
			doc.RepresentativeSources[0].Why, doc.RepresentativeSources[1].Why)
	}
}

func TestCapabilityReaders_AcceptV2DeclarationArtifactsAndProviderKinds(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)
	projectDir := filepath.Join(ws.Root, filepath.FromSlash(app.Path))
	for _, relative := range []string{"declared.ts", "generated.ts", "workspace.ts", "package.ts"} {
		if err := os.WriteFile(filepath.Join(projectDir, relative), []byte("export const value = true;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{
  "protocolVersion": 2,
  "project": "example/app",
  "schemas": [{
    "identity": {"ownerProject":"example/app","kind":"schema","subkind":"route","key":"users"},
    "name": "users",
    "kind": "route",
    "provenance": {
      "project": "example/app",
      "package": "example/app",
      "version": "1.0.0",
      "sourceKind": "manual",
      "declaration": {"root":"project","path":"declared.ts"},
      "artifacts": [
        {"root":"project","path":"generated.ts"},
        {"root":"workspace","path":"svc/app/workspace.ts"},
        {"root":"package","path":"package.ts"}
      ]
    }
  }]
}`
	if err := os.WriteFile(filepath.Join(projectDir, "schema", "capabilities.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err := capabilityEvidenceSources(ws.Root, projectDir, app, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, source := range sources {
		paths[source.Path] = true
	}
	for _, expected := range []string{"svc/app/declared.ts", "svc/app/generated.ts", "svc/app/workspace.ts", "svc/app/package.ts"} {
		if !paths[expected] {
			t.Errorf("v2 declaration/artifact source %q missing from %v", expected, paths)
		}
	}
	if declared := declaredConformanceKinds(projectDir); !declared["schema"] {
		t.Fatalf("v2 provider projection = %v, want schema", declared)
	}
}

// TestBuildDocument_Docs pins the adjacent-doc relationship heuristic: a README
// that names an existing project source file is checked; a doc that names none is
// unchecked; and the docs section is sorted by path.
func TestBuildDocument_Docs(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("svc/app/go.mod", "module example/app\n\ngo 1.25\n")
	write("svc/app/main.go", "package main\n\nfunc main() {}\n")
	write("svc/app/README.md", "# App\n\nSee main.go for the entrypoint.\n")
	write("svc/app/docs/design.md", "# Design\n\nNo source references here.\n")

	app := &workspace.Project{ID: "/svc/app", Name: "example/app", Path: "svc/app", Type: "application",
		Config: &wsproto.ProjectConfig{Main: "main.go"}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{app})

	doc, _, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}

	rel := map[string]agentcontext.DocRelationship{}
	for _, d := range doc.Docs {
		rel[d.Path] = d.Relationship
	}
	if rel["svc/app/README.md"] != agentcontext.DocRelationshipChecked {
		t.Errorf("README relationship = %q, want checked", rel["svc/app/README.md"])
	}
	if rel["svc/app/docs/design.md"] != agentcontext.DocRelationshipUnchecked {
		t.Errorf("docs/design.md relationship = %q, want unchecked", rel["svc/app/docs/design.md"])
	}
	if len(doc.Docs) != 2 || doc.Docs[0].Path != "svc/app/README.md" || doc.Docs[1].Path != "svc/app/docs/design.md" {
		t.Fatalf("docs = %+v, want [README, docs/design.md] sorted by path", doc.Docs)
	}
}

// TestBuildDocument_OverridesApplied pins author-override application: an
// overrides file that adds a source, removes an auto-selected source, adds and
// removes a doc, and force-flags a path sensitive is fully reflected in the
// built document — the flagged entry carries sensitive=true, its path is in
// opts.SensitivePaths, and the document still passes the fail-closed
// publish-safety gate BECAUSE it is flagged.
func TestBuildDocument_OverridesApplied(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)
	write := func(rel, content string) {
		abs := filepath.Join(ws.Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	// An auto doc to remove.
	write("svc/app/README.md", "# App\n\nStart at main.go.\n")
	write("svc/app/schema/agent-context.overrides.json", `{
  "protocolVersion": 1,
  "addSources": [
    {"path": "svc/app/override_src.go", "startLine": 1, "endLine": 10, "why": "override", "tokens": {"estimated": 5, "method": "bytes/4"}}
  ],
  "removeSources": ["svc/app/main.go"],
  "addDocs": [
    {"path": "svc/app/docs/manual.md", "relationship": "checked"}
  ],
  "removeDocs": ["svc/app/README.md"],
  "sensitive": ["svc/app/service.go"]
}`)

	doc, opts, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}

	src := map[string]agentcontext.SourceRange{}
	for _, s := range doc.RepresentativeSources {
		src[s.Path] = s
	}
	if _, present := src["svc/app/main.go"]; present {
		t.Error("removeSources should have dropped the main range")
	}
	over, ok := src["svc/app/override_src.go"]
	if !ok || over.Why != agentcontext.SourceReasonOverride {
		t.Errorf("override source = %+v (present=%v), want why=override", over, ok)
	}
	svc, ok := src["svc/app/service.go"]
	if !ok {
		t.Fatal("the capability-evidence range should survive overrides")
	}
	if !svc.Sensitive {
		t.Error("the author-flagged service.go range must be sensitive=true")
	}
	if !opts.SensitivePaths["svc/app/service.go"] {
		t.Errorf("sensitive set %v must contain the author-flagged path", opts.SensitivePaths)
	}

	docRel := map[string]agentcontext.DocRelationship{}
	for _, d := range doc.Docs {
		docRel[d.Path] = d.Relationship
	}
	if _, present := docRel["svc/app/README.md"]; present {
		t.Error("removeDocs should have dropped the README doc")
	}
	if docRel["svc/app/docs/manual.md"] != agentcontext.DocRelationshipChecked {
		t.Errorf("added doc relationship = %q, want checked", docRel["svc/app/docs/manual.md"])
	}

	if diags := agentcontext.ValidatePublishSafety(doc, opts); diag.HasErrors(diags) {
		t.Fatalf("publish-safety gate rejected the override-flagged document: %v", diags)
	}
	if _, err := canonicalDocumentBytes(doc, opts, app.ID); err != nil {
		t.Fatalf("canonicalDocumentBytes after overrides: %v", err)
	}
}

// conformanceCapabilitiesManifest declares a datasource-bound migration so the
// canonical capabilities.AvailableProviderKinds projection reports the "datasource"
// (and "migration") kinds — the declared-kind set a datasource conformance pack
// matches against.
const conformanceCapabilitiesManifest = `{
  "protocolVersion": 1,
  "project": "/svc/app",
  "migrations": [
    {
      "name": "app",
      "datasource": "primary",
      "provenance": {"project": "/svc/app", "sourceKind": "framework"}
    }
  ]
}`

// buildConformanceFixture writes a two-project workspace where the application
// "app" declares a datasource capability and its library dependency "lib" OWNS a
// conformance pack (a stable id, matching capabilityKinds, and a committed corpus).
// withOptIn controls whether app ships a *_test.go referencing a conformance
// runner token; policy, when non-empty, is written into options.test.infra. It
// returns the loaded workspace and the app project.
func buildConformanceFixture(t *testing.T, withOptIn bool, policy string) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Application: declares a datasource capability and depends on lib.
	write("svc/app/go.mod", "module example/app\n\ngo 1.25\n")
	write("svc/app/main.go", "package main\n\nfunc main() {}\n")
	write("svc/app/schema/capabilities.json", conformanceCapabilitiesManifest+"\n")
	if withOptIn {
		write("svc/app/service_test.go",
			"package app\n\nimport \"testing\"\n\nfunc TestConformance(t *testing.T) { conformance.Run(t) }\n")
	}

	// Library dependency OWNS the conformance pack and its committed corpus, so the
	// fixture digest is referenced relative to lib (not the selecting app).
	write("svc/lib/go.mod", "module example/lib\n\ngo 1.25\n")
	write("svc/lib/conformance/pack.json",
		`{"id":"putnami.example.conformance","corpus":"manifest.json","capabilityKinds":["datasource"],"languages":["go","typescript"]}`+"\n")
	write("svc/lib/conformance/manifest.json", `{"cases":["c1","c2"]}`+"\n")

	cfg := &wsproto.Config{}
	if policy != "" {
		cfg.Options = map[string]map[string]any{"test": {"infra": policy}}
	}
	app := &workspace.Project{
		ID: "/svc/app", Name: "example/app", Path: "svc/app", Type: "application",
		Config: &wsproto.ProjectConfig{Main: "main.go"}, Dependencies: []string{"example/lib"},
	}
	lib := &workspace.Project{ID: "/svc/lib", Name: "example/lib", Path: "svc/lib"}
	ws := workspace.NewWorkspace(root, cfg, []*workspace.Project{app, lib})
	return ws, app
}

// TestBuildTestsSection_Populated pins the populated tests path: a project that
// declares a matching capability, whose dependency ships a conformance pack the
// project has opted into, gets that pack referenced by id/kinds/languages, its
// corpus referenced by path + sha256 digest, an empty absence reason, the default
// auto policy — and the document stays byte-stable and round-trips clean.
func TestBuildTestsSection_Populated(t *testing.T) {
	ws, app := buildConformanceFixture(t, true, "")

	doc, opts, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	tests := doc.Tests
	if tests == nil {
		t.Fatal("document has no tests section")
	}
	if tests.Policy != agentcontext.TestPolicyAuto {
		t.Errorf("policy = %q, want %q", tests.Policy, agentcontext.TestPolicyAuto)
	}
	if tests.AbsenceReason != "" {
		t.Errorf("absenceReason = %q, want empty (packs are present)", tests.AbsenceReason)
	}
	if len(tests.Packs) != 1 {
		t.Fatalf("packs = %+v, want exactly one", tests.Packs)
	}
	pack := tests.Packs[0]
	if pack.ID != "putnami.example.conformance" {
		t.Errorf("pack id = %q, want putnami.example.conformance", pack.ID)
	}
	if len(pack.CapabilityKinds) != 1 || pack.CapabilityKinds[0] != "datasource" {
		t.Errorf("pack capabilityKinds = %v, want [datasource]", pack.CapabilityKinds)
	}
	if len(pack.Languages) != 2 || pack.Languages[0] != "go" || pack.Languages[1] != "typescript" {
		t.Errorf("pack languages = %v, want [go typescript]", pack.Languages)
	}

	// Fixture digest references lib's committed corpus by path + content digest.
	if len(tests.FixtureDigests) != 1 {
		t.Fatalf("fixtureDigests = %+v, want exactly one", tests.FixtureDigests)
	}
	fd := tests.FixtureDigests[0]
	if fd.Path != "svc/lib/conformance/manifest.json" {
		t.Errorf("fixture path = %q, want svc/lib/conformance/manifest.json", fd.Path)
	}
	corpus, err := os.ReadFile(filepath.Join(ws.Root, "svc", "lib", "conformance", "manifest.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if fd.Digest != digestBytes(corpus) {
		t.Errorf("fixture digest = %q, want %q", fd.Digest, digestBytes(corpus))
	}

	// Byte-stability across repeated builds of the populated (packs + digests) path.
	first, err := canonicalDocumentBytes(doc, opts, app.ID)
	if err != nil {
		t.Fatalf("canonicalDocumentBytes: %v", err)
	}
	for i := range 3 {
		doc2, opts2, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
		if err != nil {
			t.Fatalf("BuildDocument iteration %d: %v", i, err)
		}
		got, err := canonicalDocumentBytes(doc2, opts2, app.ID)
		if err != nil {
			t.Fatalf("canonicalDocumentBytes iteration %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d: tests section not byte-stable\n--- first:\n%s\n--- got:\n%s", i, first, got)
		}
	}

	// Round-trips clean through the strict parser and passes the publish-safety gate.
	parsed, diags := agentcontext.ParseAndValidateDocument(first)
	if parsed == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip failed to parse/validate: %v", diags)
	}
	if diags := agentcontext.ValidatePublishSafety(doc, opts); diag.HasErrors(diags) {
		t.Fatalf("publish-safety gate rejected the populated document: %v", diags)
	}
}

// TestBuildTestsSection_PolicyIsConfigNotCI asserts the DETERMINISM-CRITICAL policy
// source: the policy is derived ONLY from committed config (options.test.infra),
// never from the CI environment — so the artifact is byte-identical in CI and
// locally at a fixed tree. With CI set but no config the policy is still auto (not
// the require the RUNTIME policy would pick under CI — dbtestenv.ResolveMode in
// the extension SDK); with a committed require it flows through.
func TestBuildTestsSection_PolicyIsConfigNotCI(t *testing.T) {
	t.Setenv("CI", "1")

	wsAuto, appAuto := buildConformanceFixture(t, true, "")
	docAuto, _, err := BuildDocument(wsAuto, appAuto, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(auto): %v", err)
	}
	if docAuto.Tests.Policy != agentcontext.TestPolicyAuto {
		t.Errorf("policy = %q, want auto (the CI env var must not influence it)", docAuto.Tests.Policy)
	}

	wsReq, appReq := buildConformanceFixture(t, true, "require")
	docReq, _, err := BuildDocument(wsReq, appReq, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(require): %v", err)
	}
	if docReq.Tests.Policy != agentcontext.TestPolicyRequire {
		t.Errorf("policy = %q, want require (from committed options.test.infra)", docReq.Tests.Policy)
	}
}

// TestBuildTestsSection_ApplicableButUnopted asserts a pack that matches the
// project's declared kinds but that no committed test source references is NOT
// selected: packs and fixture digests stay empty and the absence reason is
// no-packs (the project declares kinds, so it is a supported project type).
func TestBuildTestsSection_ApplicableButUnopted(t *testing.T) {
	ws, app := buildConformanceFixture(t, false, "")

	doc, _, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	tests := doc.Tests
	if tests == nil {
		t.Fatal("document has no tests section")
	}
	if len(tests.Packs) != 0 || len(tests.FixtureDigests) != 0 {
		t.Fatalf("tests = %+v, want no packs and no fixture digests", tests)
	}
	if tests.AbsenceReason != agentcontext.AbsenceReasonNoPacks {
		t.Errorf("absenceReason = %q, want %q", tests.AbsenceReason, agentcontext.AbsenceReasonNoPacks)
	}
}

// TestBuildTestsSection_NoCapabilities asserts a project that declares no
// capability kinds gets the unsupported-project-type absence reason — even when it
// OWNS a conformance pack — because it has nothing to conformance-certify.
func TestBuildTestsSection_NoCapabilities(t *testing.T) {
	ws, _ := buildConformanceFixture(t, true, "")
	lib := ws.ProjectByID("/svc/lib")
	if lib == nil {
		t.Fatal("fixture lib project missing")
	}

	doc, _, err := BuildDocument(ws, lib, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(lib): %v", err)
	}
	tests := doc.Tests
	if tests == nil {
		t.Fatal("document has no tests section")
	}
	if len(tests.Packs) != 0 {
		t.Fatalf("packs = %+v, want none", tests.Packs)
	}
	if tests.AbsenceReason != agentcontext.AbsenceReasonUnsupportedProjectType {
		t.Errorf("absenceReason = %q, want %q", tests.AbsenceReason, agentcontext.AbsenceReasonUnsupportedProjectType)
	}
	if tests.Policy != agentcontext.TestPolicyAuto {
		t.Errorf("policy = %q, want auto", tests.Policy)
	}
}

// TestBuildDocument_OverridesMalformedFailsClosed asserts a present-but-invalid
// overrides file makes BuildDocument fail closed with an exit-2 classified error
// rather than being silently ignored.
func TestBuildDocument_OverridesMalformedFailsClosed(t *testing.T) {
	ws, app := buildFixtureWorkspace(t)
	abs := filepath.Join(ws.Root, "svc", "app", "schema", "agent-context.overrides.json")
	if err := os.WriteFile(abs, []byte(`{"protocolVersion":1,"bogus":true}`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}

	_, _, err := BuildDocument(ws, app, fixtureRevision, fixtureVersion)
	if err == nil {
		t.Fatal("a malformed overrides file must make BuildDocument fail closed")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
}
