package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	ext "go.putnami.dev/protocol/extension"
)

// The v2 corpus lives in fixtures/v2 rather than joining fixtures/{valid,
// invalid}: those two directories are the v1 corpus other runtimes' SDK
// conformance suites read, and v2 must leave every v1 fixture exactly
// as it was.

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestConformance_ProtocolVersion2 pins the v2 constant the same way
// TestConformance_ProtocolVersion pins v1.
func TestConformance_ProtocolVersion2(t *testing.T) {
	if ProtocolVersion2 != 2 {
		t.Fatalf("ProtocolVersion2 = %d, want 2 — bumping requires a migration story", ProtocolVersion2)
	}
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — v2 is additive, v1 is still produced", ProtocolVersion)
	}
}

func TestConformance_V2ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid v2 fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data := readFixture(t, path)
			ctx, diags := ParseAndValidate(data)
			if diag.HasErrors(diags) {
				t.Fatalf("valid v2 fixture %s produced errors: %v", path, diags)
			}
			if !ctx.IsV2() {
				t.Errorf("fixture %s is not read as v2 (version %d)", path, ctx.ContextVersion())
			}
			if ctx.Identity == nil {
				t.Fatalf("fixture %s carries no identity", path)
			}
			if ctx.Identity.Key != ctx.Identity.DerivedKey() {
				t.Errorf("fixture %s key %q disagrees with the derived key %q",
					path, ctx.Identity.Key, ctx.Identity.DerivedKey())
			}
			// Lenient parse must accept everything strict parse accepts.
			if _, err := Parse(data); err != nil {
				t.Errorf("lenient parse of %s failed: %v", path, err)
			}
		})
	}
}

func TestConformance_V2InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid v2 fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, diags := ParseAndValidate(readFixture(t, path)); !diag.HasErrors(diags) {
				t.Errorf("invalid v2 fixture %s should produce errors but none found", path)
			}
		})
	}
}

// TestV2InvalidFixtureCoverage is the v2 half of TestInvalidFixtureCoverage:
// every version-scoped reject branch must be triggered by a fixture, so
// relaxing one of them turns a red fixture green and fails here.
func TestV2InvalidFixtureCoverage(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	fields := map[string]bool{}
	for _, path := range files {
		_, diags := ParseAndValidate(readFixture(t, path))
		for _, d := range diags {
			codes[d.Code] = true
			fields[d.Field] = true
		}
	}

	for _, code := range []string{
		ErrorCodeInvalidVersion,
		ErrorCodeUnexpectedField,
		ErrorCodeInvalidValue,
		ErrorCodeInvalidKey,
		ErrorCodeInvalidMetadata,
		ErrorCodeMissingField,
	} {
		if !codes[code] {
			t.Errorf("no invalid v2 fixture triggers %q — that reject branch is unguarded", code)
		}
	}
	for _, field := range []string{
		"protocolVersion",
		"workspace.options",
		"identity",
		"identity.key",
		"identity.scope",
		"identity.task.command",
		"identity.provider.extension",
		"staging",
		"staging.project",
		"staging.workspace",
		"extension.runtimePath",
		"extension.cacheRoot",
		"project.metadata",
		"project.metadata.@putnami/go",
		"project.type",
		"project.dependencyClosure",
		"project.dependencyClosure[1].path",
		"selectedProjects[1].sourceName",
		"selectedProjects[1].version",
		"selectedProjects[1].dependencies",
		"selectedProjects[0].extensions",
		"workspaceProjects",
		"workspaceProjects[1].path",
		"userScope",
		"userScope.callerDir",
		"invocation",
		"invocation.id",
		"invocation.artifactRoot",
		"selection",
		"selection.mode",
		"selection.scoped",
		"selection.projects",
		"selection.projects[0]",
		"selection.releaseSetProjects",
		"selection.emptyImpact",
	} {
		if !fields[field] {
			t.Errorf("no invalid v2 fixture reports on %q", field)
		}
	}
}

// TestV1FixturesStayV1 is the additivity guard: every v1 fixture must still
// validate exactly as before and must still be read as version 1.
func TestV1FixturesStayV1(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid v1 fixtures found")
	}
	for _, path := range files {
		ctx, diags := ParseAndValidate(readFixture(t, path))
		if diag.HasErrors(diags) {
			t.Errorf("v1 fixture %s regressed: %v", path, diags)
			continue
		}
		if ctx.ContextVersion() != ProtocolVersion {
			t.Errorf("v1 fixture %s reads as version %d", path, ctx.ContextVersion())
		}
		if ctx.Identity != nil || ctx.Staging != nil || ctx.Invocation != nil {
			t.Errorf("v1 fixture %s carries v2 members", path)
		}
		if ctx.Extension.RuntimePath != "" || ctx.Extension.CacheRoot != "" || ctx.Project.Metadata != nil {
			t.Errorf("v1 fixture %s carries the v2 lifecycle members", path)
		}
		if ctx.Project.Type != "" || ctx.Project.DependencyClosure != nil {
			t.Errorf("v1 fixture %s carries the v2 project-graph members", path)
		}
		if ctx.Selection != nil {
			t.Errorf("v1 fixture %s carries the v2 selection member", path)
		}
		if ctx.WorkspaceProjects != nil {
			t.Errorf("v1 fixture %s carries the v2 workspaceProjects member", path)
		}
		if ctx.UserScope != nil {
			t.Errorf("v1 fixture %s carries the v2 userScope member", path)
		}
		if ctx.Workspace.Options != nil {
			t.Errorf("v1 fixture %s carries the v2 workspace.options member", path)
		}
		// fixtures/valid is the corpus other runtimes' SDK conformance suites
		// read. A member added here is a member an older strict parser rejects
		// with DisallowUnknownFields, so the v1 corpus stays frozen.
		for i, project := range ctx.SelectedProjects {
			if project.SourceName != "" {
				t.Errorf("v1 fixture %s carries selectedProjects[%d].sourceName", path, i)
			}
			if project.Version != "" {
				t.Errorf("v1 fixture %s carries selectedProjects[%d].version", path, i)
			}
			if project.Dependencies != nil {
				t.Errorf("v1 fixture %s carries selectedProjects[%d].dependencies", path, i)
			}
		}
	}
}

// TestValidate_V2LifecycleMembers pins the four pre-declared lifecycle members.
// They may be INERT — a producer emits each only for work that declares the
// corresponding lifecycle — but they are not unconstrained: the rules below are
// what a later producer wires into rather than re-negotiates. See
// doc/adr/0001-v2-optional-members.md.
func TestValidate_V2LifecycleMembers(t *testing.T) {
	cases := []struct {
		name  string
		mutas func(*Context)
		code  string
		field string
	}{
		{
			name:  "all four are optional within v2",
			mutas: func(*Context) {},
		},
		{
			name: "an absolute runtime path is accepted",
			mutas: func(c *Context) {
				c.Extension.RuntimePath = "/ws/.putnami/runtimes/@putnami/go/tc1-abc/bin/putnami-go"
				c.Extension.CacheRoot = "/home/dev/.putnami/cache/extensions/@putnami/go"
			},
		},
		{
			name:  "a relative runtime path is not",
			mutas: func(c *Context) { c.Extension.RuntimePath = "bin/putnami-go" },
			code:  ErrorCodeInvalidValue,
			field: "extension.runtimePath",
		},
		{
			name:  "a relative extension cache root is not",
			mutas: func(c *Context) { c.Extension.CacheRoot = ".putnami/cache" },
			code:  ErrorCodeInvalidValue,
			field: "extension.cacheRoot",
		},
		{
			name:  "a Windows current-drive runtime path is not absolute",
			mutas: func(c *Context) { c.Extension.RuntimePath = `\bin\putnami-go` },
			code:  ErrorCodeInvalidValue,
			field: "extension.runtimePath",
		},
		{
			name: "a non-secret invocation locator is accepted",
			mutas: func(c *Context) {
				c.Invocation = &Invocation{
					ID:           "inv-123",
					ArtifactRoot: "/ws/.putnami/invocations/inv-123/artifacts",
				}
			},
		},
		{
			name: "a Windows current-drive invocation root is not absolute",
			mutas: func(c *Context) {
				c.Invocation = &Invocation{ID: "inv-123", ArtifactRoot: `\current-drive`}
			},
			code:  ErrorCodeInvalidValue,
			field: "invocation.artifactRoot",
		},
		{
			name: "an empty metadata container is accepted",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{}
			},
		},
		{
			name: "namespaced metadata blocks are accepted",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{
					"@putnami/go":         json.RawMessage(`{"module":"go.putnami.dev/p"}`),
					"@putnami/typescript": json.RawMessage(`{}`),
				}
			},
		},
		{
			name: "an unowned metadata block is not",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{"": json.RawMessage(`{"a":1}`)}
			},
			code:  ErrorCodeInvalidMetadata,
			field: "project.metadata",
		},
		{
			name: "a scalar metadata block is not",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{"@putnami/go": json.RawMessage(`"module"`)}
			},
			code:  ErrorCodeInvalidMetadata,
			field: "project.metadata.@putnami/go",
		},
		{
			name: "an array metadata block is not",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{"@putnami/go": json.RawMessage(`[]`)}
			},
			code:  ErrorCodeInvalidMetadata,
			field: "project.metadata.@putnami/go",
		},
		{
			name: "a null metadata block is not",
			mutas: func(c *Context) {
				c.Project.Metadata = map[string]json.RawMessage{"@putnami/go": json.RawMessage(`null`)}
			},
			code:  ErrorCodeInvalidMetadata,
			field: "project.metadata.@putnami/go",
		},
		{
			name:  "a resolved project type is accepted",
			mutas: func(c *Context) { c.Project.Type = "library" },
		},
		{
			name: "a dependency closure that locates every member is accepted",
			mutas: func(c *Context) {
				c.Project.DependencyClosure = []ProjectRef{
					{ID: "/p", Name: "p", Path: "p", FullPath: "/ws/p"},
					{ID: "/libs/core", Name: "core", Path: "libs/core", FullPath: "/ws/libs/core"},
				}
			},
		},
		{
			name: "an empty closure is accepted",
			mutas: func(c *Context) {
				c.Project.DependencyClosure = []ProjectRef{}
			},
		},
		{
			name: "a closure member with no absolute path is not",
			mutas: func(c *Context) {
				c.Project.DependencyClosure = []ProjectRef{{ID: "/p", Name: "p", Path: "p"}}
			},
			code:  ErrorCodeMissingField,
			field: "project.dependencyClosure[0].fullPath",
		},
		{
			name: "a closure member with no name is not",
			mutas: func(c *Context) {
				c.Project.DependencyClosure = []ProjectRef{{ID: "/p", Path: "p", FullPath: "/ws/p"}}
			},
			code:  ErrorCodeMissingField,
			field: "project.dependencyClosure[0].name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			tc.mutas(ctx)
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}
}

// TestValidate_V1RejectsLifecycleMembers is the version boundary for the four
// pre-declared members, from the same side as TestValidate_V1RejectsV2Members: a
// v1 consumer would ignore exactly the members a v2 consumer would trust, so a
// v1 document may not carry them at all.
func TestValidate_V1RejectsLifecycleMembers(t *testing.T) {
	cases := []struct {
		field string
		mutas func(*Context)
	}{
		{"extension.runtimePath", func(c *Context) { c.Extension.RuntimePath = "/e/bin/e" }},
		{"extension.cacheRoot", func(c *Context) { c.Extension.CacheRoot = "/home/dev/.cache/e" }},
		{"project.metadata", func(c *Context) {
			c.Project.Metadata = map[string]json.RawMessage{"e": json.RawMessage(`{}`)}
		}},
		{"project.type", func(c *Context) { c.Project.Type = "application" }},
		{"project.dependencyClosure", func(c *Context) {
			c.Project.DependencyClosure = []ProjectRef{
				{ID: "/p", Name: "p", Path: "p", FullPath: "/ws/p"},
			}
		}},
		{"invocation", func(c *Context) {
			c.Invocation = &Invocation{ID: "inv-1", ArtifactRoot: "/tmp/inv-1"}
		}},
		{"selection", func(c *Context) {
			c.Selection = &Selection{Mode: SelectionModeAll, ProjectIDs: []string{"/p"}}
		}},
		{"workspace.options", func(c *Context) {
			c.Workspace.Options = map[string]json.RawMessage{"sdd": json.RawMessage(`{}`)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			ctx := validV2Context()
			ctx.ProtocolVersion = 0 // v1
			ctx.Identity = nil
			tc.mutas(ctx)
			assertDiagnostic(t, Validate(ctx), ErrorCodeUnexpectedField, tc.field)
		})
	}
}

func TestValidate_V2WorkspaceOptions(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]json.RawMessage
		present bool
		field   string
	}{
		{name: "absent is accepted"},
		{name: "empty object is accepted", options: map[string]json.RawMessage{}},
		{name: "named object blocks are accepted", options: map[string]json.RawMessage{
			"sdd": json.RawMessage(`{"verification":{"architecture":"report"}}`),
		}},
		{name: "present null is rejected", present: true, field: "workspace.options"},
		{name: "null block is rejected", options: map[string]json.RawMessage{
			"sdd": json.RawMessage(`null`),
		}, field: "workspace.options.sdd"},
		{name: "scalar block is rejected", options: map[string]json.RawMessage{
			"sdd": json.RawMessage(`"report"`),
		}, field: "workspace.options.sdd"},
		{name: "array block is rejected", options: map[string]json.RawMessage{
			"sdd": json.RawMessage(`[]`),
		}, field: "workspace.options.sdd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.Workspace.Options = tc.options
			ctx.presence.workspaceOptions = tc.present
			diags := Validate(ctx)
			if tc.field == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("valid workspace options rejected: %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, ErrorCodeInvalidValue, tc.field)
		})
	}
}

func TestV2WorkspaceOptionsAreOmittedWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(validV2Context())
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Workspace map[string]json.RawMessage `json:"workspace"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document.Workspace["options"]; exists {
		t.Errorf("workspace.options must be omitted when unset: %s", encoded)
	}
}

// TestV2LifecycleMembersAreOmittedWhenAbsent pins that pre-declaring the four
// members changed no bytes on the wire: a document that does not set them
// serializes exactly as it did before they existed.
func TestV2LifecycleMembersAreOmittedWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(validV2Context())
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Extension  map[string]json.RawMessage `json:"extension"`
		Project    map[string]json.RawMessage `json:"project"`
		Invocation json.RawMessage            `json:"invocation"`
		Selection  json.RawMessage            `json:"selection"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"runtimePath", "cacheRoot"} {
		if _, ok := document.Extension[member]; ok {
			t.Errorf("extension.%s must be omitted when unset", member)
		}
	}
	if _, ok := document.Project["metadata"]; ok {
		t.Error("project.metadata must be omitted when unset")
	}
	if document.Invocation != nil {
		t.Error("invocation must be omitted when unset")
	}
	if document.Selection != nil {
		t.Error("selection must be omitted when unset")
	}
}

// TestV2MetadataIsNamespacedPerExtension pins the ownership rule the merge
// contract rests on: a provider's block is reachable only under its own name,
// so two providers contributing to one project never need a per-field merge
// rule and never overwrite each other.
func TestV2MetadataIsNamespacedPerExtension(t *testing.T) {
	ctx, diags := ParseAndValidate(readFixture(t, "fixtures", "v2", "valid", "lifecycle-members.json"))
	if diag.HasErrors(diags) {
		t.Fatalf("valid fixture rejected: %v", diags)
	}
	if len(ctx.Project.Metadata) != 2 {
		t.Fatalf("metadata = %v, want two namespaced blocks", ctx.Project.Metadata)
	}
	var goBlock struct {
		Module     string `json:"module"`
		ModuleRoot string `json:"moduleRoot"`
	}
	if err := json.Unmarshal(ctx.Project.Metadata["@putnami/go"], &goBlock); err != nil {
		t.Fatalf("a provider must decode its own block: %v", err)
	}
	if goBlock.Module != "go.putnami.dev/p" {
		t.Errorf("module = %q, want go.putnami.dev/p", goBlock.Module)
	}
	// The Go block carries its module root SEPARATELY from the project path:
	// probe metadata keys on paths and never assumes the project root is the
	// module root.
	if goBlock.ModuleRoot == "" {
		t.Error("the module root is metadata, not an assumption about the project path")
	}
	if _, ok := ctx.Project.Metadata["@putnami/python"]; ok {
		t.Error("a provider that contributed nothing must have no block")
	}
	// Authored options and derived metadata are distinct maps: a probe result
	// can never silently overwrite what a project's putnami.json states.
	if ctx.Project.Options != nil {
		t.Error("the fixture declares no authored options")
	}
	if ctx.Extension.RuntimePath == "" || ctx.Extension.CacheRoot == "" {
		t.Error("the fixture exercises both extension-owned paths")
	}
	if ctx.Invocation == nil || ctx.Invocation.ID == "" || ctx.Invocation.ArtifactRoot == "" {
		t.Error("the fixture exercises the invocation locator")
	}
}

// TestV2ProjectReferencesCarryTheResolvedVersion pins the second half of the
// package-reference pair.
//
// A selector that locates a contribution by PACKAGE names both the package and
// its version. With only the name on the wire, a consumer either widens the
// match or fails it — and failing is the quiet outcome: the reference degrades
// to "source unavailable" while every versionless reference beside it still
// resolves, so a narrowing shows up as a missing verdict rather than an error.
//
// TestV2WorkspaceProjectsCarryTheResolvedExtensions asserts the member a
// workspace task reads to know which extension's options apply to a sibling,
// and that a reference without it encodes as it did before the member.
func TestV2WorkspaceProjectsCarryTheResolvedExtensions(t *testing.T) {
	ctx, diags := ParseAndValidate(readFixture(t, "fixtures", "v2", "valid", "project-references.json"))
	if diag.HasErrors(diags) {
		t.Fatalf("valid fixture rejected: %v", diags)
	}
	if got := ctx.WorkspaceProjects[0].Extensions; len(got) != 1 || got[0] != "/typescript/extension" {
		t.Errorf("workspaceProjects[0].extensions = %q, want [/typescript/extension]", got)
	}
	if got := ctx.WorkspaceProjects[1].Extensions; got != nil {
		t.Errorf("workspaceProjects[1].extensions = %q, want none — the fixture declares none", got)
	}
	encoded, err := json.Marshal(ProjectRef{Name: "a", Path: "a", FullPath: "/ws/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "extensions") {
		t.Errorf("encoded reference = %s, want extensions omitted when empty", encoded)
	}
}

// The member is asserted on BOTH project-reference sites, because the contract
// declares one shape: a member populated in only one of them makes a consumer's
// "this project declared nothing" reading depend on where it read the reference.
func TestV2ProjectReferencesCarryTheResolvedVersion(t *testing.T) {
	ctx, diags := ParseAndValidate(readFixture(t, "fixtures", "v2", "valid", "project-references.json"))
	if diag.HasErrors(diags) {
		t.Fatalf("valid fixture rejected: %v", diags)
	}
	if len(ctx.SelectedProjects) != 3 {
		t.Fatalf("selectedProjects = %d, want the fixture's three", len(ctx.SelectedProjects))
	}
	if got, want := ctx.SelectedProjects[0].Version, "1.4.0"; got != want {
		t.Errorf("selectedProjects[0].version = %q, want %q", got, want)
	}
	if got, want := ctx.SelectedProjects[1].Version, "0.9.2"; got != want {
		t.Errorf("selectedProjects[1].version = %q, want %q", got, want)
	}
	// Optional WITHIN v2: an orchestrator older than the member emits none, and
	// a consumer must read that as "nobody said" and match by name alone rather
	// than invent a version. An unversioned match is wider than intended; a
	// match against an invented version is simply wrong.
	if got := ctx.SelectedProjects[2].Version; got != "" {
		t.Errorf("selectedProjects[2].version = %q, want empty — the fixture declares none", got)
	}
	if len(ctx.Project.DependencyClosure) != 2 {
		t.Fatalf("dependencyClosure = %d, want the fixture's two", len(ctx.Project.DependencyClosure))
	}
	if got, want := ctx.Project.DependencyClosure[1].Version, "0.9.2"; got != want {
		t.Errorf("dependencyClosure[1].version = %q, want %q", got, want)
	}

	// Omitempty keeps a reference that has nothing to say byte-identical to what
	// an orchestrator that predates the member produced.
	encoded, err := json.Marshal(ProjectRef{Name: "a", Path: "a", FullPath: "/ws/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "version") {
		t.Errorf("encoded reference = %s, want version omitted when empty", encoded)
	}
}

// A v1 document may not carry the member, for the same reason it may not carry
// sourceName: a v1 consumer resolves a package reference by name alone where a
// v2 consumer resolves it by name AND version, so the two answer one
// package-scoped selector differently from the same bytes.
func TestValidate_V1RejectsProjectReferenceVersion(t *testing.T) {
	ctx := validV2Context()
	ctx.ProtocolVersion = 0 // v1
	ctx.Identity = nil
	ctx.SelectedProjects = []ProjectRef{
		{Name: "a", Path: "a", FullPath: "/ws/a"},
		{Name: "b", Path: "b", FullPath: "/ws/b", Version: "1.0.0"},
	}
	assertDiagnostic(t, Validate(ctx), ErrorCodeUnexpectedField, "selectedProjects[1].version")

	// The same document at v2 is accepted: the member is optional within v2, so
	// one reference carrying it and one not is a legal document.
	ctx.ProtocolVersion = ProtocolVersion2
	ctx.Identity = &TaskIdentity{
		Key:      "/p:build",
		Scope:    TaskScopeProject,
		Project:  ProjectIdentity{ID: "/p", Name: "p"},
		Task:     TaskRef{Name: "build", Command: "build", Kind: "build"},
		Provider: ProviderIdentity{Extension: "@putnami/go"},
	}
	if diags := Validate(ctx); diag.HasErrors(diags) {
		t.Fatalf("a v2 document with a versioned project reference was rejected: %v", diags)
	}
}

func TestStrictPresencePreservesEmptyAndNullLifecycleMembers(t *testing.T) {
	cases := []struct {
		fixture string
		want    []struct{ code, field string }
	}{
		{
			fixture: "v1-with-empty-lifecycle-members.json",
			want: []struct{ code, field string }{
				{ErrorCodeUnexpectedField, "extension.runtimePath"},
				{ErrorCodeUnexpectedField, "extension.cacheRoot"},
				{ErrorCodeUnexpectedField, "project.metadata"},
				{ErrorCodeUnexpectedField, "invocation"},
			},
		},
		{
			fixture: "v2-with-empty-lifecycle-members.json",
			want: []struct{ code, field string }{
				{ErrorCodeInvalidValue, "extension.runtimePath"},
				{ErrorCodeInvalidValue, "extension.cacheRoot"},
				{ErrorCodeInvalidMetadata, "project.metadata"},
				{ErrorCodeInvalidValue, "invocation"},
			},
		},
		{
			fixture: "v2-invalid-invocation.json",
			want: []struct{ code, field string }{
				{ErrorCodeMissingField, "invocation.id"},
				{ErrorCodeInvalidValue, "invocation.artifactRoot"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, got := ParseAndValidate(readFixture(t, "fixtures", "v2", "invalid", tc.fixture))
			if len(got) != len(tc.want) {
				t.Fatalf("diagnostics = %v, want exactly %d", got, len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].Code != want.code || got[i].Field != want.field {
					t.Errorf("diagnostic[%d] = (%s, %s), want (%s, %s)",
						i, got[i].Code, got[i].Field, want.code, want.field)
				}
			}
		})
	}
}

// TestV1DocumentBytesUnchanged pins that a v1 context still serializes to the
// v1 wire: protocolVersion, identity and staging are all omitted, so a producer
// that has not migrated emits byte-identical documents.
func TestV1DocumentBytesUnchanged(t *testing.T) {
	ctx, err := Parse(readFixture(t, "fixtures", "valid", "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"protocolVersion", "identity", "staging"} {
		if _, ok := members[member]; ok {
			t.Errorf("a v1 document must not serialize %q", member)
		}
	}
}

func TestContextVersion(t *testing.T) {
	var nilCtx *Context
	if got := nilCtx.ContextVersion(); got != 0 {
		t.Errorf("nil context version = %d, want 0", got)
	}
	if nilCtx.IsV2() {
		t.Error("a nil context is not v2")
	}
	if got := (&Context{}).ContextVersion(); got != ProtocolVersion {
		t.Errorf("absent protocolVersion reads as %d, want %d", got, ProtocolVersion)
	}
	if got := (&Context{ProtocolVersion: 2}).ContextVersion(); got != ProtocolVersion2 {
		t.Errorf("protocolVersion 2 reads as %d", got)
	}
	if got := (&Context{ProtocolVersion: 7}).ContextVersion(); got != 7 {
		t.Errorf("an unknown version must be reported as-is, got %d", got)
	}
}

// TestStagingRootsMatchTaskContract is the anti-drift pin on the staging
// vocabulary: the roots a job stages under ARE the declared-output roots of the
// extension task contract v3, so a rename on either side fails here instead of
// silently mis-resolving every declared output.
func TestStagingRootsMatchTaskContract(t *testing.T) {
	if StagingRootProject != ext.OutputRootProject {
		t.Errorf("StagingRootProject = %q, task contract says %q", StagingRootProject, ext.OutputRootProject)
	}
	if StagingRootWorkspace != ext.OutputRootWorkspace {
		t.Errorf("StagingRootWorkspace = %q, task contract says %q", StagingRootWorkspace, ext.OutputRootWorkspace)
	}
	if StagingRootCommandOutput != ext.OutputRootCommandOutput {
		t.Errorf("StagingRootCommandOutput = %q, task contract says %q",
			StagingRootCommandOutput, ext.OutputRootCommandOutput)
	}

	// Every declared output resolves through PathFor: a root the staging tree
	// cannot resolve would leave a declared output homeless.
	staging := &Staging{
		Root:          "/s",
		Project:       "/s/project",
		Workspace:     "/s/workspace",
		CommandOutput: "/s/command-output",
	}
	for _, output := range []ext.DeclaredOutput{
		{Kind: ext.OutputKindDirectory, Path: "dist"},
		{Kind: ext.OutputKindDirectory, Root: ext.OutputRootProject, Path: ".gen"},
		{Kind: ext.OutputKindFile, Root: ext.OutputRootWorkspace, Path: "bun.lock"},
		{Kind: ext.OutputKindFile, Root: ext.OutputRootCommandOutput, Path: "coverage.out"},
	} {
		if got := staging.PathFor(output.EffectiveRoot()); got == "" {
			t.Errorf("declared output root %q does not resolve to a staging path", output.EffectiveRoot())
		}
	}
}

// TestInvocationScopedOutputsAreNotStaged is the other half of the pin above:
// an invocation-scoped output must NOT resolve to a staging root.
//
// Staging is the tree the orchestrator captures. The whole point of an
// invocation-scoped artifact — a secret, a DSN for a container that dies with
// the run — is that it never enters cache traffic, so resolving one to a
// staging directory would place it exactly where capture looks. `""` is the
// honest answer: the invocation scratch is a different tree. Related tasks
// locate it through Context.Invocation.ArtifactRoot, not through staging.
func TestInvocationScopedOutputsAreNotStaged(t *testing.T) {
	invocation := &Invocation{
		ID:           "inv-123",
		ArtifactRoot: "/s/invocations/inv-123/artifacts",
	}
	if invocation.ArtifactRoot == "" {
		t.Fatal("the invocation artifact root is the typed path source")
	}
	staging := &Staging{
		Root:          "/s",
		Project:       "/s/project",
		Workspace:     "/s/workspace",
		CommandOutput: "/s/command-output",
	}
	for _, output := range []ext.DeclaredOutput{
		{Kind: ext.OutputKindRuntimeFile, Scope: ext.OutputScopeInvocation, Sensitive: true, Path: "db/dsn.env"},
		{Kind: ext.OutputKindFile, Scope: ext.OutputScopeInvocation, Path: "db/lease.json"},
	} {
		root := output.EffectiveRoot()
		if root != ext.OutputRootInvocation {
			t.Fatalf("invocation-scoped output resolved to root %q", root)
		}
		if got := staging.PathFor(root); got != "" {
			t.Errorf("invocation-scoped output resolved to the staging path %q", got)
		}
		for _, staged := range StagingRoots {
			if root == staged {
				t.Errorf("the invocation root collides with the staging root %q", staged)
			}
		}
	}
}

func TestStagingPathFor(t *testing.T) {
	var nilStaging *Staging
	if got := nilStaging.PathFor(StagingRootProject); got != "" {
		t.Errorf("nil staging resolved %q", got)
	}
	staging := &Staging{Root: "/s", Project: "/s/p", Workspace: "/s/w", CommandOutput: "/s/c"}
	cases := map[string]string{
		StagingRootProject:       "/s/p",
		StagingRootWorkspace:     "/s/w",
		StagingRootCommandOutput: "/s/c",
		"unknown-root":           "",
	}
	for root, want := range cases {
		if got := staging.PathFor(root); got != want {
			t.Errorf("PathFor(%q) = %q, want %q", root, got, want)
		}
	}

	if len(StagingRoots) != 3 {
		t.Fatalf("StagingRoots = %v, want the three declared-output roots", StagingRoots)
	}
	for i := 1; i < len(StagingRoots); i++ {
		if StagingRoots[i-1] >= StagingRoots[i] {
			t.Errorf("StagingRoots is not in canonical (sorted) order: %v", StagingRoots)
		}
	}
	for _, root := range StagingRoots {
		if staging.PathFor(root) == "" {
			t.Errorf("StagingRoots lists %q but PathFor does not resolve it", root)
		}
	}
}

func validV2Context() *Context {
	return &Context{
		ProtocolVersion: ProtocolVersion2,
		WorkspaceRoot:   "/ws",
		OutputPath:      "/ws/out",
		CacheRoot:       "/ws/cache",
		Workspace:       Workspace{Name: "putnami"},
		Project:         Project{Name: "p", Path: "p", FullPath: "/ws/p"},
		Extension:       Extension{Name: "@putnami/go", Root: "/ws/go/extension"},
		Job:             Job{Name: "build"},
		Identity: &TaskIdentity{
			Key:      "/p:build",
			Scope:    TaskScopeProject,
			Project:  ProjectIdentity{ID: "/p", Name: "p"},
			Task:     TaskRef{Name: "build", Command: "build", Kind: "build"},
			Provider: ProviderIdentity{Extension: "@putnami/go"},
		},
		Params: Params{},
	}
}

func TestValidate_V2Identity(t *testing.T) {
	if diags := Validate(validV2Context()); diag.HasErrors(diags) {
		t.Fatalf("valid v2 context rejected: %v", diags)
	}

	cases := []struct {
		name  string
		muthe func(*Context)
		code  string
		field string
	}{
		{
			name:  "missing identity",
			muthe: func(c *Context) { c.Identity = nil },
			code:  ErrorCodeMissingField,
			field: "identity",
		},
		{
			name:  "derived key mismatch",
			muthe: func(c *Context) { c.Identity.Key = "/other:build" },
			code:  ErrorCodeInvalidKey,
			field: "identity.key",
		},
		{
			name:  "missing key",
			muthe: func(c *Context) { c.Identity.Key = "" },
			code:  ErrorCodeMissingField,
			field: "identity.key",
		},
		{
			name:  "missing scope",
			muthe: func(c *Context) { c.Identity.Scope = "" },
			code:  ErrorCodeMissingField,
			field: "identity.scope",
		},
		{
			name:  "unknown scope",
			muthe: func(c *Context) { c.Identity.Scope = "module" },
			code:  ErrorCodeInvalidValue,
			field: "identity.scope",
		},
		{
			name: "missing project id",
			muthe: func(c *Context) {
				c.Identity.Project.ID = ""
				c.Identity.Key = c.Identity.DerivedKey()
			},
			code:  ErrorCodeMissingField,
			field: "identity.project.id",
		},
		{
			name:  "missing project name",
			muthe: func(c *Context) { c.Identity.Project.Name = "" },
			code:  ErrorCodeMissingField,
			field: "identity.project.name",
		},
		{
			name: "missing task name",
			muthe: func(c *Context) {
				c.Identity.Task.Name = ""
				c.Identity.Key = c.Identity.DerivedKey()
			},
			code:  ErrorCodeMissingField,
			field: "identity.task.name",
		},
		{
			name:  "missing task command",
			muthe: func(c *Context) { c.Identity.Task.Command = "" },
			code:  ErrorCodeMissingField,
			field: "identity.task.command",
		},
		{
			name:  "missing task kind",
			muthe: func(c *Context) { c.Identity.Task.Kind = "" },
			code:  ErrorCodeMissingField,
			field: "identity.task.kind",
		},
		{
			name:  "missing provider extension",
			muthe: func(c *Context) { c.Identity.Provider.Extension = "" },
			code:  ErrorCodeMissingField,
			field: "identity.provider.extension",
		},
		{
			name:  "workspace scope is accepted",
			muthe: func(c *Context) { c.Identity.Scope = TaskScopeWorkspace },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			tc.muthe(ctx)
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}
}

// TestValidate_V2Selection pins the resolved-selection contract member by
// member. The rules are the ones a consumer acts on — `scoped` licenses a
// workspace-wide verdict, `emptyImpact` distinguishes "nothing changed" from
// "nothing matched", sortedness keeps two runs byte-identical — so each of them
// is asserted from both sides.
func TestValidate_V2Selection(t *testing.T) {
	cases := []struct {
		name      string
		selection *Selection
		code      string
		field     string
	}{
		{
			name:      "an unscoped whole-workspace selection is accepted",
			selection: &Selection{Mode: SelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
		},
		{
			name: "an explicit selector is scoped",
			selection: &Selection{
				Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"},
			},
		},
		{
			name: "an impacted selection carries its baseline and tier",
			selection: &Selection{
				Mode: SelectionModeImpacted, Scoped: true,
				Baseline: "origin/main", BaselineSource: "upstream",
				ProjectIDs: []string{"/a"},
			},
		},
		{
			name: "the impacted no-op selects nothing",
			selection: &Selection{
				Mode: SelectionModeImpacted, Scoped: true, Baseline: "origin/main",
				ProjectIDs: []string{}, EmptyImpact: true,
			},
		},
		{
			name:      "an empty selection is an empty array, never a missing one",
			selection: &Selection{Mode: SelectionModeAll, ProjectIDs: []string{}},
		},
		{
			name:      "a null selection member is not an absent one",
			selection: nil,
			code:      ErrorCodeInvalidValue,
			field:     "selection",
		},
		{
			name:      "a mode is required",
			selection: &Selection{Scoped: true, ProjectIDs: []string{"/a"}},
			code:      ErrorCodeMissingField,
			field:     "selection.mode",
		},
		{
			name:      "the mode vocabulary is closed",
			selection: &Selection{Mode: "changed", Scoped: true, ProjectIDs: []string{"/a"}},
			code:      ErrorCodeInvalidValue,
			field:     "selection.mode",
		},
		{
			name:      "a whole-workspace run cannot claim to be scoped",
			selection: &Selection{Mode: SelectionModeAll, Scoped: true, ProjectIDs: []string{"/a"}},
			code:      ErrorCodeInvalidValue,
			field:     "selection.scoped",
		},
		{
			name:      "a narrowed run cannot claim to be unscoped",
			selection: &Selection{Mode: SelectionModeImpacted, ProjectIDs: []string{"/a"}},
			code:      ErrorCodeInvalidValue,
			field:     "selection.scoped",
		},
		{
			name:      "a null project list is neither answer",
			selection: &Selection{Mode: SelectionModeAll},
			code:      ErrorCodeInvalidValue,
			field:     "selection.projects",
		},
		{
			name: "project ids are sorted",
			selection: &Selection{
				Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/b", "/a"},
			},
			code:  ErrorCodeInvalidValue,
			field: "selection.projects",
		},
		{
			name: "a duplicate id is not a sorted list either",
			selection: &Selection{
				Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a", "/a"},
			},
			code:  ErrorCodeInvalidValue,
			field: "selection.projects",
		},
		{
			name: "a blank project id locates nothing",
			selection: &Selection{
				Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"", "/a"},
			},
			code:  ErrorCodeMissingField,
			field: "selection.projects[0]",
		},
		{
			name: "only an impacted run has an impacted no-op",
			selection: &Selection{
				Mode: SelectionModeAll, ProjectIDs: []string{}, EmptyImpact: true,
			},
			code:  ErrorCodeInvalidValue,
			field: "selection.emptyImpact",
		},
		{
			name: "the no-op cannot report selected projects",
			selection: &Selection{
				Mode: SelectionModeImpacted, Scoped: true,
				ProjectIDs: []string{"/a"}, EmptyImpact: true,
			},
			code:  ErrorCodeInvalidValue,
			field: "selection.emptyImpact",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.Selection = tc.selection
			ctx.presence.selection = true
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}

	// An absent selection stays absent: a consumer of a producer that predates
	// the member must not be handed an invented one.
	ctx := validV2Context()
	if diags := Validate(ctx); diag.HasErrors(diags) {
		t.Fatalf("a context without selection was rejected: %v", diags)
	}
}

// TestSelectionModesAreClosedAndSorted pins the vocabulary itself: the wire
// values a producer may emit, in the canonical order the contract states.
func TestSelectionModesAreClosedAndSorted(t *testing.T) {
	if len(SelectionModes) != 3 {
		t.Fatalf("SelectionModes = %v, want the three selection modes", SelectionModes)
	}
	for i := 1; i < len(SelectionModes); i++ {
		if SelectionModes[i-1] >= SelectionModes[i] {
			t.Errorf("SelectionModes is not in canonical (sorted) order: %v", SelectionModes)
		}
	}
	declared := map[string]bool{
		SelectionModeAll:      true,
		SelectionModeImpacted: true,
		SelectionModeProjects: true,
	}
	for _, mode := range SelectionModes {
		if !declared[mode] {
			t.Errorf("SelectionModes lists %q, which no constant declares", mode)
		}
	}
}

func TestValidate_V2Staging(t *testing.T) {
	valid := Staging{
		Root:          "/ws/.putnami/staging/p/build",
		Project:       "/ws/.putnami/staging/p/build/project",
		Workspace:     "/ws/.putnami/staging/p/build/workspace",
		CommandOutput: "/ws/.putnami/staging/p/build/command-output",
	}
	ctx := validV2Context()
	ctx.Staging = &valid
	if diags := Validate(ctx); diag.HasErrors(diags) {
		t.Fatalf("valid staging tree rejected: %v", diags)
	}

	cases := []struct {
		name    string
		staging Staging
		code    string
		field   string
	}{
		{
			name:    "missing root",
			staging: Staging{Project: "/s/p", Workspace: "/s/w", CommandOutput: "/s/c"},
			code:    ErrorCodeMissingField,
			field:   "staging.root",
		},
		{
			name:    "missing project path",
			staging: Staging{Root: "/s", Workspace: "/s/w", CommandOutput: "/s/c"},
			code:    ErrorCodeMissingField,
			field:   "staging.project",
		},
		{
			name:    "relative root",
			staging: Staging{Root: "s", Project: "s/p", Workspace: "s/w", CommandOutput: "s/c"},
			code:    ErrorCodeInvalidValue,
			field:   "staging.root",
		},
		{
			name:    "root outside the staging tree",
			staging: Staging{Root: "/s", Project: "/s/p", Workspace: "/elsewhere/w", CommandOutput: "/s/c"},
			code:    ErrorCodeInvalidValue,
			field:   "staging.workspace",
		},
		{
			name:    "root equal to the staging root",
			staging: Staging{Root: "/s", Project: "/s", Workspace: "/s/w", CommandOutput: "/s/c"},
			code:    ErrorCodeInvalidValue,
			field:   "staging.project",
		},
		{
			name:    "nested roots",
			staging: Staging{Root: "/s", Project: "/s/tree", Workspace: "/s/tree/nested", CommandOutput: "/s/c"},
			code:    ErrorCodeInvalidValue,
			field:   "staging.workspace",
		},
		{
			name:    "identical roots",
			staging: Staging{Root: "/s", Project: "/s/same", Workspace: "/s/same", CommandOutput: "/s/c"},
			code:    ErrorCodeInvalidValue,
			field:   "staging.workspace",
		},
		{
			name: "segment-boundary lookalikes are not overlaps",
			staging: Staging{
				Root: "/s", Project: "/s/tree", Workspace: "/s/tree2", CommandOutput: "/s/c",
			},
		},
		{
			name: "windows drive paths are absolute",
			staging: Staging{
				Root:          `C:\ws\staging`,
				Project:       `C:\ws\staging\project`,
				Workspace:     `C:\ws\staging\workspace`,
				CommandOutput: `C:\ws\staging\command-output`,
			},
		},
		{
			name: "trailing separators do not hide an overlap",
			staging: Staging{
				Root: "/s", Project: "/s/same/", Workspace: "/s/same", CommandOutput: "/s/c",
			},
			code:  ErrorCodeInvalidValue,
			field: "staging.workspace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			staging := tc.staging
			ctx.Staging = &staging
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}
}

// TestValidate_V1RejectsV2Members pins the version boundary from the other
// side: a document that does not declare v2 may not carry v2 members, because a
// v1 consumer would ignore exactly the members a v2 consumer would trust.
func TestValidate_V1RejectsV2Members(t *testing.T) {
	ctx := validV2Context()
	ctx.ProtocolVersion = 0 // v1
	ctx.Staging = &Staging{Root: "/s", Project: "/s/p", Workspace: "/s/w", CommandOutput: "/s/c"}

	diags := Validate(ctx)
	assertDiagnostic(t, diags, ErrorCodeUnexpectedField, "identity")
	assertDiagnostic(t, diags, ErrorCodeUnexpectedField, "staging")

	// An explicit protocolVersion 1 behaves the same as an absent one.
	ctx.ProtocolVersion = ProtocolVersion
	diags = Validate(ctx)
	assertDiagnostic(t, diags, ErrorCodeUnexpectedField, "identity")
}

func TestValidate_UnknownVersion(t *testing.T) {
	ctx := validV2Context()
	ctx.ProtocolVersion = 3
	diags := Validate(ctx)
	assertDiagnostic(t, diags, ErrorCodeInvalidVersion, "protocolVersion")

	// Forward compatibility stays on the lenient path: an SDK that only parses
	// still reads the members it knows.
	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("lenient parse of a future version failed: %v", err)
	}
	if parsed.Job.Name != "build" {
		t.Errorf("job name = %q, want build", parsed.Job.Name)
	}
}

func TestValidate_NegativeVersion(t *testing.T) {
	ctx := validV2Context()
	ctx.ProtocolVersion = -1
	assertDiagnostic(t, Validate(ctx), ErrorCodeInvalidVersion, "protocolVersion")
}

func TestIsAbsolutePath(t *testing.T) {
	absolute := []string{"/", "/ws", "/ws/staging", `\\server\share`, `C:\ws`, "c:/ws"}
	relative := []string{"", ".", "ws", "ws/staging", "./ws", "C:", "C:ws", `\current-drive`}
	for _, p := range absolute {
		if !isAbsolutePath(p) {
			t.Errorf("isAbsolutePath(%q) = false, want true", p)
		}
	}
	for _, p := range relative {
		if isAbsolutePath(p) {
			t.Errorf("isAbsolutePath(%q) = true, want false", p)
		}
	}
}

func TestPathContainment(t *testing.T) {
	if !pathUnder("/a", "/a/b") {
		t.Error("/a/b must be under /a")
	}
	if pathUnder("/a", "/ab") {
		t.Error("containment must be tested at a segment boundary")
	}
	if pathUnder("/a", "/a") {
		t.Error("a path is not strictly under itself")
	}
	if !pathUnder("/", "/a") {
		t.Error("everything absolute is under the filesystem root")
	}
	if !pathsOverlap("/a/b", "/a/b/") || !pathsOverlap("/a", "/a/b") || !pathsOverlap("/a/b", "/a") {
		t.Error("overlap must be symmetric and separator-insensitive")
	}
	if pathsOverlap("/a/b", "/a/c") {
		t.Error("sibling trees do not overlap")
	}
	if !pathsOverlap(`C:\a`, "C:/a/b") {
		t.Error("overlap must normalize separators")
	}
}

// assertDiagnostic fails unless diags carries the exact {code, field} pair.
func assertDiagnostic(t *testing.T, diags []diag.Diagnostic, code, field string) {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && d.Field == field {
			return
		}
	}
	rendered := make([]string, len(diags))
	for i, d := range diags {
		rendered[i] = d.Code + "@" + d.Field
	}
	t.Errorf("want diagnostic {code=%q field=%q}, got [%s]", code, field, strings.Join(rendered, " "))
}

// TestValidate_V2WorkspaceProjects pins the complete-membership member.
//
// The rules are the ones the consumer acts on. A workspace-wide validator asks
// "is this id a real member of this workspace?", so an entry with no id cannot
// answer for itself; and it must get the same bytes for the same workspace from
// any run, so the listing is sorted by id.
func TestValidate_V2WorkspaceProjects(t *testing.T) {
	ref := func(id, path string) ProjectRef {
		return ProjectRef{ID: id, Name: id[1:], Path: path, FullPath: "/ws/" + path}
	}
	cases := []struct {
		name     string
		projects []ProjectRef
		code     string
		field    string
	}{
		{
			name:     "a sorted, fully located membership is accepted",
			projects: []ProjectRef{ref("/a", "a"), ref("/b", "b")},
		},
		{
			name:     "an empty workspace is an empty array",
			projects: []ProjectRef{},
		},
		{
			name:     "a null membership is not an absent one",
			projects: nil,
			code:     ErrorCodeInvalidValue,
			field:    "workspaceProjects",
		},
		{
			name:     "an id is required, because the member exists to answer about ids",
			projects: []ProjectRef{{Name: "a", Path: "a", FullPath: "/ws/a"}},
			code:     ErrorCodeMissingField,
			field:    "workspaceProjects[0].id",
		},
		{
			name:     "a member with no path cannot be opened",
			projects: []ProjectRef{{ID: "/a", Name: "a", FullPath: "/ws/a"}},
			code:     ErrorCodeMissingField,
			field:    "workspaceProjects[0].path",
		},
		{
			name:     "a member with no absolute path cannot be opened either",
			projects: []ProjectRef{{ID: "/a", Name: "a", Path: "a"}},
			code:     ErrorCodeMissingField,
			field:    "workspaceProjects[0].fullPath",
		},
		{
			name:     "membership is sorted by id",
			projects: []ProjectRef{ref("/b", "b"), ref("/a", "a")},
			code:     ErrorCodeInvalidValue,
			field:    "workspaceProjects",
		},
		{
			name:     "a duplicate id is not a sorted listing either",
			projects: []ProjectRef{ref("/a", "a"), ref("/a", "a")},
			code:     ErrorCodeInvalidValue,
			field:    "workspaceProjects",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.WorkspaceProjects = tc.projects
			ctx.presence.workspaceProjects = true
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}

	// Absent stays absent: an orchestrator that predates the member must not
	// look like one reporting an empty workspace.
	if diags := Validate(validV2Context()); diag.HasErrors(diags) {
		t.Fatalf("a context without workspaceProjects was rejected: %v", diags)
	}
}

// TestProjectRefDependenciesAreV2Only pins the additivity of the edge list on
// both project-reference carriers.
//
// The direction of the disagreement is what makes it a version boundary: a v1
// consumer reads no edges and reports no undeclared dependency, while a v2
// consumer reads them and fails the run. The same bytes, two verdicts.
func TestProjectRefDependenciesAreV2Only(t *testing.T) {
	v1 := validV2Context()
	v1.ProtocolVersion = 0
	v1.Identity = nil
	v1.SelectedProjects = []ProjectRef{
		{Name: "a", Path: "a", FullPath: "/ws/a"},
		{Name: "b", Path: "b", FullPath: "/ws/b", Dependencies: []string{"/a"}},
	}
	assertDiagnostic(t, Validate(v1), ErrorCodeUnexpectedField, "selectedProjects[1].dependencies")

	// An empty list is a resolved answer, not an absent one, so it is rejected
	// at v1 as well: a v1 consumer would drop the member and never learn that
	// the producer had already resolved the graph.
	v1.SelectedProjects[1].Dependencies = []string{}
	assertDiagnostic(t, Validate(v1), ErrorCodeUnexpectedField, "selectedProjects[1].dependencies")

	v2 := validV2Context()
	v2.WorkspaceProjects = []ProjectRef{
		{ID: "/a", Name: "a", Path: "a", FullPath: "/ws/a"},
		{ID: "/b", Name: "b", Path: "b", FullPath: "/ws/b", Dependencies: []string{"/a"}},
	}
	v2.presence.workspaceProjects = true
	if diags := Validate(v2); diag.HasErrors(diags) {
		t.Fatalf("v2 rejected resolved dependency edges: %v", diags)
	}
}

// TestProjectRefConfigIsV2OnlyAndObjectShaped pins the raw authored-config
// carrier. Its schema belongs to protocols/workspace; this wire owns only the
// version boundary and the invariant that a present config is an object.
func TestProjectRefConfigIsV2OnlyAndObjectShaped(t *testing.T) {
	v1 := validV2Context()
	v1.ProtocolVersion = 0
	v1.Identity = nil
	v1.SelectedProjects = []ProjectRef{
		{Name: "a", Path: "a", FullPath: "/ws/a", Config: json.RawMessage(`{"featureAuthority":{"none":"reviewed"}}`)},
	}
	assertDiagnostic(t, Validate(v1), ErrorCodeUnexpectedField, "selectedProjects[0].config")

	valid := validV2Context()
	valid.WorkspaceProjects = []ProjectRef{
		{ID: "/a", Name: "a", Path: "a", FullPath: "/ws/a", Config: json.RawMessage(`{"featureAuthority":{"none":"reviewed"}}`)},
	}
	valid.presence.workspaceProjects = true
	if diags := Validate(valid); diag.HasErrors(diags) {
		t.Fatalf("v2 rejected an object-shaped authored config: %v", diags)
	}

	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`"config"`)} {
		invalid := validV2Context()
		invalid.WorkspaceProjects = []ProjectRef{
			{ID: "/a", Name: "a", Path: "a", FullPath: "/ws/a", Config: raw},
		}
		invalid.presence.workspaceProjects = true
		assertDiagnostic(t, Validate(invalid), ErrorCodeInvalidValue, "workspaceProjects[0].config")
	}
}

// TestToolSelectionMarshalsLikeTheJobSelection is the anti-drift pin on the
// OTHER wire that carries a resolved selection.
//
// An MCP tool call has no job context, so the extension contract declares its
// own ToolSelection. Three types now describe one resolved
// answer — the CLI's ResolvedSelection, this package's Selection, and the
// extension contract's ToolSelection — and they are three because a protocol
// package cannot import the CLI and this package is already imported BY the
// extension contract, so the third could not import either of the other two.
//
// What holds them together is the bytes. A member added to one and forgotten on
// another would let the same narrowing read differently depending on which
// surface answered, which is the one failure a shared resolver exists to
// prevent. The comparison is over the marshaled document — member names, member
// ORDER and omitempty behavior — because that is the whole contract.
func TestToolSelectionMarshalsLikeTheJobSelection(t *testing.T) {
	for _, sample := range []struct {
		name string
		job  Selection
		tool ext.ToolSelection
	}{
		{
			name: "the unscoped whole-workspace projection",
			job:  Selection{Mode: SelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
			tool: ext.ToolSelection{Mode: ext.ToolSelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
		},
		{
			name: "an explicit selector",
			job:  Selection{Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"}},
			tool: ext.ToolSelection{Mode: ext.ToolSelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"}},
		},
		{
			name: "impacted, with the baseline it resolved to",
			job: Selection{
				Mode: SelectionModeImpacted, Scoped: true,
				Baseline: "origin/main", BaselineSource: "trunk",
				ProjectIDs: []string{}, EmptyImpact: true,
			},
			tool: ext.ToolSelection{
				Mode: ext.ToolSelectionModeImpacted, Scoped: true,
				Baseline: "origin/main", BaselineSource: "trunk",
				ProjectIDs: []string{}, EmptyImpact: true,
			},
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			jobBytes, err := json.Marshal(sample.job)
			if err != nil {
				t.Fatalf("marshal job selection: %v", err)
			}
			toolBytes, err := json.Marshal(sample.tool)
			if err != nil {
				t.Fatalf("marshal tool selection: %v", err)
			}
			if string(jobBytes) != string(toolBytes) {
				t.Errorf("the two selection contracts no longer marshal identically:\n  job:  %s\n  tool: %s",
					jobBytes, toolBytes)
			}
		})
	}

	// The vocabularies are the same closed set, in the same canonical order, so
	// a consumer of either reads one mode table.
	if len(SelectionModes) != len(ext.ToolSelectionModes) {
		t.Fatalf("selection modes = %v, tool selection modes = %v", SelectionModes, ext.ToolSelectionModes)
	}
	for i := range SelectionModes {
		if SelectionModes[i] != ext.ToolSelectionModes[i] {
			t.Errorf("selection mode %d = %q, tool contract says %q", i, SelectionModes[i], ext.ToolSelectionModes[i])
		}
	}
}

// TestValidate_V2UserScope pins the member that marks a job run outside any
// workspace. A consumer starts from callerDir, so a present member must name an
// absolute directory, and a null member is not an absent one.
func TestValidate_V2UserScope(t *testing.T) {
	cases := []struct {
		name  string
		scope *UserScope
		code  string
		field string
	}{
		{name: "an absolute caller directory is accepted", scope: &UserScope{CallerDir: "/home/u/src"}},
		{name: "a null member is not an absent one", scope: nil, code: ErrorCodeInvalidValue, field: "userScope"},
		{name: "the caller directory is required", scope: &UserScope{}, code: ErrorCodeMissingField, field: "userScope.callerDir"},
		{name: "a relative caller directory resolves against nothing", scope: &UserScope{CallerDir: "src"}, code: ErrorCodeInvalidValue, field: "userScope.callerDir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.UserScope = tc.scope
			ctx.presence.userScope = true
			diags := Validate(ctx)
			if tc.code == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("expected acceptance, got %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}

	if diags := Validate(validV2Context()); diag.HasErrors(diags) {
		t.Fatalf("a context without userScope was rejected: %v", diags)
	}

	v1 := validV2Context()
	v1.ProtocolVersion = 0
	v1.Identity = nil
	v1.UserScope = &UserScope{CallerDir: "/home/u/src"}
	assertDiagnostic(t, Validate(v1), ErrorCodeUnexpectedField, "userScope")
}

// TestParse_UserScopeIsIgnoredByAnOlderShape pins the lenient consumer path: a
// v2 document carrying userScope parses with Parse, and the member reaches a
// consumer that reads it.
func TestParse_UserScopeIsIgnoredByAnOlderShape(t *testing.T) {
	data := readFixture(t, "fixtures/v2/valid/user-scope.json")
	ctx, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.UserScope == nil || ctx.UserScope.CallerDir != "/home/u/src/repository" {
		t.Fatalf("userScope = %+v, want the caller directory", ctx.UserScope)
	}
	var older struct {
		WorkspaceRoot string `json:"workspaceRoot"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.WorkspaceRoot == "" {
		t.Fatalf("a consumer without the member cannot read the document: %v", err)
	}
}
