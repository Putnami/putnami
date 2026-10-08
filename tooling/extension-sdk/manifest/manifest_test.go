package manifest

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// validBuilder is the authoring-time analog of the protocol's valid v3
// fixture: it exercises every declaration feature — both kinds, all three
// roots, literal and port-supplied paths, optional-empty outputs, cacheable and
// external effects, and source mutation — and must build cleanly. Every
// rejection case below is one compiling mutation of this manifest, so a case
// that fails proves the rule fired and not that the base was already broken.
func validBuilder() *Builder {
	return New("sample/sdk-authored", "1.0.0").
		Command("build", proto.CommandDefinition{
			Description: "Build the project",
			Run: []proto.PipelineStep{
				{ID: "generate", Task: "build-generate"},
				{ID: "compile", Task: "build-compile", DependsOn: []string{"generate"}},
			},
		}).
		Command("lint", proto.CommandDefinition{
			Description: "Lint and fix sources",
			Run:         []proto.PipelineStep{{ID: "fix", Task: "lint-fix"}},
		}).
		Command("publish", proto.CommandDefinition{
			Description: "Publish to the registry",
			Run:         []proto.PipelineStep{{ID: "push", Task: "publish-registry"}},
		}).
		Task("build-generate", proto.TaskDefinition{
			Kind:    "command",
			Command: "echo",
			Declares: Declares(
				Output("gen", Directory(".gen", InProject(), Describe("Generated specs and loaders."))),
			),
		}).
		Task("build-compile", proto.TaskDefinition{
			Kind:    "command",
			Command: "echo",
			Outputs: map[string]proto.TaskOutputPort{
				"clientOutput": {Description: "configured client dir"},
				"reportPath":   {Description: "configured report file"},
			},
			Declares: Declares(
				Output("binary", File("bin/app", InCommandOutput())),
				Output("coverage", File("lcov.info", InCommandOutput(), OptionalEmpty())),
				Output("lock", File("bun.lock", InWorkspace())),
				Output("clients", DirectoryFromPort("clientOutput", OptionalEmpty())),
				Output("report", FileFromPort("reportPath", InCommandOutput())),
				Effects(proto.EffectToolchainCache, proto.EffectNetwork),
			),
		}).
		Task("lint-fix", proto.TaskDefinition{
			Kind:     "command",
			Command:  "echo",
			Writes:   []proto.ResourceRef{SourcesWrite()},
			Cache:    &proto.TaskCachePolicy{NoOutput: true},
			Declares: Declares(MutatesSources()),
		}).
		Task("publish-registry", proto.TaskDefinition{
			Kind:     "command",
			Command:  "echo",
			Cache:    NoCache(),
			Declares: Declares(Effects(proto.EffectRegistry, proto.EffectNetwork)),
		})
}

func TestBuild_ValidManifest(t *testing.T) {
	built, err := validBuilder().Build()
	if err != nil {
		t.Fatalf("valid manifest was rejected at authoring time: %v", err)
	}
	if got := proto.ManifestProtocolVersion(built); got != proto.ProtocolVersionV3 {
		t.Fatalf("built manifest reports v%d, want v%d", got, proto.ProtocolVersionV3)
	}

	// What the SDK emits must be what a consumer accepts: the manifest travels
	// through the protocol's own strict parser, not just its validator.
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := proto.ParseAndValidateManifest(encoded)
	if diag.HasErrors(diags) {
		t.Fatalf("SDK-built manifest failed the protocol's strict parse: %v", diags)
	}
	if !reflect.DeepEqual(parsed.Tasks["build-compile"].Declares.Outputs,
		built.Tasks["build-compile"].Declares.Outputs) {
		t.Error("declared outputs did not survive the round trip to a consumer")
	}
}

func TestBuild_GoEmbedSelectorsMatchLoaderValidation(t *testing.T) {
	build := func(origin, selector string) (*proto.Manifest, error) {
		return New("sample/go-embed", "1.0.0").
			Command("build", proto.CommandDefinition{Run: []proto.PipelineStep{{ID: "build", Task: "build"}}}).
			Task("build", proto.TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]proto.TaskInputPort{
				"sources": {From: origin, Files: []string{"**/*.go", selector}},
			}}).Build()
	}
	if _, err := build(proto.TaskInputFromProject, "go-embed:build"); err != nil {
		t.Fatalf("valid project selector rejected: %v", err)
	}
	for _, test := range []struct {
		name, origin, selector, want string
	}{
		{"workspace", proto.TaskInputFromWorkspace, "go-embed:test", "only valid in project task inputs"},
		{"unknown", proto.TaskInputFromProject, "go-embed:unknown", "unsupported go embed selector"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := build(test.origin, test.selector); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("builder accepted %s selector: %v", test.name, err)
			}
		})
	}
}

// TestBuild_DeclarationFeatures pins what the constructors actually produce, so
// an option that silently stopped applying could not hide behind a manifest
// that still validates.
func TestBuild_DeclarationFeatures(t *testing.T) {
	built, err := validBuilder().Build()
	if err != nil {
		t.Fatal(err)
	}
	outputs := built.Tasks["build-compile"].Declares.Outputs

	cases := map[string]proto.DeclaredOutput{
		"binary":   {Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, Path: "bin/app"},
		"coverage": {Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, Path: "lcov.info", OptionalEmpty: true},
		"lock":     {Kind: proto.OutputKindFile, Root: proto.OutputRootWorkspace, Path: "bun.lock"},
		"clients":  {Kind: proto.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
		"report":   {Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, PathFrom: "reportPath"},
	}
	for id, want := range cases {
		if got := outputs[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("output %q = %+v, want %+v", id, got, want)
		}
	}

	gen := built.Tasks["build-generate"].Declares.Outputs["gen"]
	if gen.Kind != proto.OutputKindDirectory || gen.Path != ".gen" ||
		gen.EffectiveRoot() != proto.OutputRootProject || gen.Description == "" {
		t.Errorf("project-rooted directory output = %+v", gen)
	}
	if effects := built.Tasks["build-compile"].Declares.Effects; len(effects) != 2 {
		t.Errorf("effects = %v, want both declared effects", effects)
	}
	if !built.Tasks["lint-fix"].Declares.MutatesSources {
		t.Error("MutatesSources did not reach the declaration")
	}
	if built.Tasks["publish-registry"].Cache.IsEnabled() {
		t.Error("NoCache did not disable caching")
	}
}

// TestBuild_RejectsContractViolations is the heart of "fails at authoring, not
// at plan time": each case is one compiling mutation of the valid manifest that
// breaks exactly one invariant, and each must be rejected with the code a
// consumer keys on.
func TestBuild_RejectsContractViolations(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "manifest-validated-at-build-time", "building-refuses-a-task-contract-violation")
	cases := []struct {
		name   string
		mutate func(*Builder)
		code   string
	}{
		{
			name: "EXACTNESS: glob path",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", Directory("dist/**/*.js"))),
				})
			},
			code: "invalid-output-path",
		},
		{
			name: "EXACTNESS: template variable",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", Directory("{projectRoot}/.gen"))),
				})
			},
			code: "invalid-output-path",
		},
		{
			name: "EXACTNESS: escape above the root",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", Directory("../shared/.gen"))),
				})
			},
			code: "invalid-output-path",
		},
		{
			name: "EXACTNESS: the root itself is not an output",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", Directory("."))),
				})
			},
			code: "invalid-output-path",
		},
		{
			name: "EXACTNESS: pathFrom must name a port of the same task",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", DirectoryFromPort("missingPort"))),
				})
			},
			code: "unresolved-output-port",
		},
		{
			name: "ONE OWNER: a file inside another task's declared subtree",
			mutate: func(b *Builder) {
				b.Task("build-describe", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("openapi", File(".gen/api/openapi.json"))),
				})
			},
			code: "output-overlap",
		},
		{
			name: "ONE OWNER: the same path in two tasks",
			mutate: func(b *Builder) {
				b.Task("build-describe", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Output("gen", Directory("./.gen/"))),
				})
			},
			code: "output-overlap",
		},
		{
			name: "ONE OWNER: two outputs of one task",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(
						Output("all", Directory("dist")),
						Output("esm", Directory("dist/esm")),
					),
				})
			},
			code: "output-overlap",
		},
		{
			name: "HONEST EFFECTS: external effect on a cacheable task",
			mutate: func(b *Builder) {
				b.Task("publish-registry", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Effects(proto.EffectRegistry)),
				})
			},
			code: "effect-conflict",
		},
		{
			name: "HONEST EFFECTS: unknown effect",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Effects("telepathy")),
				})
			},
			code: "invalid-enum",
		},
		{
			name: "HONEST EFFECTS: duplicate effect",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(Effects(proto.EffectNetwork), Effects(proto.EffectNetwork)),
				})
			},
			code: "duplicate-effect",
		},
		{
			name: "HONEST EFFECTS: source mutation without the sources write resource",
			mutate: func(b *Builder) {
				b.Task("lint-fix", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Declares: Declares(MutatesSources()),
				})
			},
			code: "effect-conflict",
		},
		{
			name: "HONEST EFFECTS: declared outputs contradict cache.noOutput",
			mutate: func(b *Builder) {
				b.Task("build-generate", proto.TaskDefinition{
					Kind: "command", Command: "echo",
					Cache:    &proto.TaskCachePolicy{NoOutput: true},
					Declares: Declares(Output("gen", Directory(".gen"))),
				})
			},
			code: "effect-conflict",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := validBuilder()
			tc.mutate(builder)

			built, err := builder.Build()
			if err == nil {
				t.Fatalf("authoring accepted a manifest that violates the contract: %+v", built.Tasks)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error is %T, want *ValidationError: %v", err, err)
			}
			if !hasCode(verr.Codes(), tc.code) {
				t.Fatalf("codes = %v, want %q (%v)", verr.Codes(), tc.code, err)
			}
			if !strings.Contains(verr.Error(), tc.code) {
				t.Errorf("message does not name the rule that fired: %v", verr)
			}
		})
	}
}

// TestBuild_V2TaskStillBuilds pins the additive promise on the authoring side:
// a task with no declaration is a v2 task and is accepted unchanged.
func TestBuild_V2TaskStillBuilds(t *testing.T) {
	built, err := New("sample/v2", "1.0.0").
		Command("build", proto.CommandDefinition{Run: []proto.PipelineStep{{ID: "build", Task: "build-exec"}}}).
		Task("build-exec", proto.TaskDefinition{Kind: "command", Command: "echo"}).
		Build()
	if err != nil {
		t.Fatalf("a v2 manifest must still build: %v", err)
	}
	if got := proto.ManifestProtocolVersion(built); got != proto.ProtocolVersionV2 {
		t.Fatalf("v2 manifest reports v%d", got)
	}
	if built.Tasks["build-exec"].Declares != nil {
		t.Error("authoring attached a declaration to a v2 task")
	}
}

// TestBuild_RejectsStructuralDefects pins that the v3 gate did not replace the
// structural one: an author still cannot emit a manifest with no surface or a
// task missing its command.
func TestBuild_RejectsStructuralDefects(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "manifest-validated-at-build-time", "building-refuses-a-structurally-defective-manifest")
	if _, err := New("sample/empty", "1.0.0").Build(); err == nil {
		t.Error("a manifest with no command or tool must be rejected")
	}
	_, err := New("sample/broken", "1.0.0").
		Command("build", proto.CommandDefinition{Run: []proto.PipelineStep{{ID: "build", Task: "build-exec"}}}).
		Task("build-exec", proto.TaskDefinition{Kind: "command"}).
		Build()
	if err == nil {
		t.Error("a task without a command must be rejected")
	}
}

// TestBuild_DoesNotAliasTheBuilder pins that a built manifest is independent:
// mutating it must not reach back into the builder, or a generator that edits
// its result would corrupt what it writes next.
func TestBuild_DoesNotAliasTheBuilder(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "builder-does-not-alias", "a-later-builder-mutation-cannot-change-a-built-manifest")
	builder := validBuilder()
	first, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	first.Tasks["build-generate"].Declares.Outputs["gen"] = Directory("elsewhere")

	second, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Tasks["build-generate"].Declares.Outputs["gen"].Path; got != ".gen" {
		t.Fatalf("editing a built manifest reached the builder: path = %q", got)
	}
}

// TestBuild_TaskAndCommandReplace pins last-write-wins, which is what lets a
// generator compose a manifest without tracking what it already emitted.
func TestBuild_TaskAndCommandReplace(t *testing.T) {
	builder := validBuilder().
		Command("build", proto.CommandDefinition{Run: []proto.PipelineStep{{ID: "only", Task: "build-generate"}}}).
		Task("build-compile", proto.TaskDefinition{Kind: "command", Command: "replaced"})
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if steps := built.Commands["build"].Run; len(steps) != 1 || steps[0].ID != "only" {
		t.Errorf("command was not replaced: %+v", steps)
	}
	if got := built.Tasks["build-compile"].Command; got != "replaced" {
		t.Errorf("task command = %q, want replaced", got)
	}
}

func TestBuilder_Tool(t *testing.T) {
	readOnly := true
	built, err := New("sample/tools", "1.0.0").
		Tool("sample.search", proto.ToolDefinition{
			Description: "Search the sample index",
			Command:     "sample mcp-tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &proto.ToolAnnotations{
				ReadOnlyHint:    &readOnly,
				DestructiveHint: boolPtr(false),
				IdempotentHint:  &readOnly,
				OpenWorldHint:   boolPtr(false),
			},
			Meta: map[string]any{"putnami.dev/contract": map[string]any{
				"access": "read", "readOnly": true, "supportsDryRun": false,
			}},
		}).
		Build()
	if err != nil {
		t.Fatalf("tool-only manifest was rejected: %v", err)
	}
	if _, ok := built.Tools["sample.search"]; !ok {
		t.Error("tool did not reach the built manifest")
	}
}

// A content-only manifest builds without a command or a tool, and its stamp is
// the additive contract its vocabulary requires — the one stamp a reader that
// predates agent content refuses instead of dropping the content.
func TestBuilder_AgentContentStampsTheAdditiveContract(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "the-manifest-builder-stamps-the-additive-contract")
	path := filepath.Join(t.TempDir(), proto.ManifestFilename)
	builder := New("@sample/contributor", "1.0.0").
		AgentContent(proto.AgentContentContribution{Path: "agent-content", Source: "agent-src"})
	if err := builder.WriteFile(path); err != nil {
		t.Fatalf("a content-only manifest was rejected: %v", err)
	}
	m, err := proto.LoadManifest(path)
	if err != nil {
		t.Fatalf("written manifest did not load: %v", err)
	}
	if m.CLIContract != protocolcli.AgentContentContract {
		t.Fatalf("cliContract = %d, want the agent-content contract %d", m.CLIContract, protocolcli.AgentContentContract)
	}
	if m.AgentContent == nil || m.AgentContent.Source != "agent-src" {
		t.Fatalf("agent content did not reach the written manifest: %+v", m.AgentContent)
	}

	if _, err := New("@sample/empty", "1.0.0").AgentContent(proto.AgentContentContribution{}).Build(); err == nil {
		t.Fatal("an empty agent-content contribution must not build")
	}
}

func TestWriteFile_WritesCanonicalJSON(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "manifest-validated-at-build-time", "writing-emits-canonical-json")
	path := filepath.Join(t.TempDir(), proto.ManifestFilename)
	if err := validBuilder().WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(first), "}\n") {
		t.Error("written manifest does not end in a newline")
	}
	// Deterministic bytes: a generator re-run must produce no diff.
	for i := 0; i < 5; i++ {
		if err := validBuilder().WriteFile(path); err != nil {
			t.Fatal(err)
		}
		again, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d produced different bytes", i)
		}
	}

	// The written file is what a consumer loads.
	m, err := proto.LoadManifest(path)
	if err != nil {
		t.Fatalf("written manifest did not load: %v", err)
	}
	if proto.ManifestProtocolVersion(m) != proto.ProtocolVersionV3 {
		t.Error("written manifest lost its v3 declarations")
	}
	// The stamp is what makes it loadable at all since CLI contract 3: Build
	// earned it by running the same gate the packager runs, so an SDK-authored
	// extension is usable in a workspace without being published first.
	if m.CLIContract != protocolcli.CurrentContract {
		t.Errorf("written manifest cliContract = %d, want %d", m.CLIContract, protocolcli.CurrentContract)
	}
}

// TestWriteFile_RefusesInvalidManifest pins that a rejected manifest leaves no
// file behind for a packager to pick up.
func TestWriteFile_RefusesInvalidManifest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "manifest-validated-at-build-time", "writing-refuses-an-invalid-manifest")
	builder := validBuilder().Task("build-describe", proto.TaskDefinition{
		Kind: "command", Command: "echo",
		Declares: Declares(Output("openapi", File(".gen/api/openapi.json"))),
	})
	path := filepath.Join(t.TempDir(), proto.ManifestFilename)
	if err := builder.WriteFile(path); err == nil {
		t.Fatal("WriteFile accepted a manifest with overlapping outputs")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a rejected manifest was written to disk: %v", err)
	}
}

// TestWriteFile_ReportsIOFailure pins that a valid manifest which cannot be
// written is an error and not a silent success.
func TestWriteFile_ReportsIOFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", proto.ManifestFilename)
	err := validBuilder().WriteFile(path)
	if err == nil {
		t.Fatal("WriteFile to a missing directory must fail")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path: %v", err)
	}
}

// TestValidate_StandaloneManifest pins the exported entry for authors who
// assemble a manifest without the builder.
func TestValidate_StandaloneManifest(t *testing.T) {
	m := &proto.Manifest{
		Commands: map[string]proto.CommandDefinition{
			"build": {Run: []proto.PipelineStep{{ID: "build", Task: "build-exec"}}},
		},
		Tasks: map[string]proto.TaskDefinition{
			"build-exec": {Kind: "command", Command: "echo", Declares: &proto.TaskDeclaration{
				Outputs: map[string]proto.DeclaredOutput{
					"gen": {Kind: proto.OutputKindDirectory, Path: ".gen"},
				},
			}},
		},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	m.Tasks["build-exec"].Declares.Outputs["glob"] = proto.DeclaredOutput{
		Kind: proto.OutputKindFile, Path: "dist/**/*.js",
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("control: a glob path must be rejected")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) || !hasCode(verr.Codes(), "invalid-output-path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidationError_Message(t *testing.T) {
	err := &ValidationError{Diagnostics: []diag.Diagnostic{
		diag.Errorf("output-overlap", "tasks.a.declares.outputs.gen", "overlaps tasks.b"),
		diag.Errorf("effect-conflict", "tasks.b.declares.effects", "must disable caching"),
	}}
	message := err.Error()
	for _, want := range []string{"2 protocol violation(s)", "tasks.a.declares.outputs.gen", "effect-conflict"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not mention %q", message, want)
		}
	}
	if got := err.Codes(); !reflect.DeepEqual(got, []string{"output-overlap", "effect-conflict"}) {
		t.Errorf("Codes() = %v", got)
	}
}

func boolPtr(v bool) *bool { return &v }

func hasCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
