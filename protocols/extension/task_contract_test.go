package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

const v3FixturePath = "fixtures/valid/task-contract-v3.json"

func loadFixtureManifest(t *testing.T, path string) *Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse %s: %v", path, diags)
	}
	return m
}

// diagCodes returns the diagnostic codes in order, for assertions that care
// about which rule fired rather than the exact message.
func diagCodes(diags []diag.Diagnostic) []string {
	codes := make([]string, 0, len(diags))
	for _, d := range diags {
		codes = append(codes, d.Code)
	}
	return codes
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestNormalizeOutputPath(t *testing.T) {
	valid := map[string]string{
		".gen":                  ".gen",
		"dist/":                 "dist",
		"./dist/bin":            "dist/bin",
		"dist//bin/app":         "dist/bin/app",
		"a/b/../c":              "a/c",
		"lcov.info":             "lcov.info",
		"clients/typescript/v1": "clients/typescript/v1",
	}
	for input, want := range valid {
		got, err := NormalizeOutputPath(input)
		if err != nil {
			t.Errorf("NormalizeOutputPath(%q) unexpected error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeOutputPath(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"",
		"   ",
		".",
		"./",
		"..",
		"../shared",
		"a/../..",
		"/abs/path",
		"dist\\bin",
		"dist/**/*.js",
		"dist/*.map",
		"report-[0-9].json",
		"{projectRoot}/.gen",
	}
	for _, input := range invalid {
		if got, err := NormalizeOutputPath(input); err == nil {
			t.Errorf("NormalizeOutputPath(%q) = %q, want error", input, got)
		}
	}
}

func TestOutputsOverlap(t *testing.T) {
	ref := func(root, path string) OutputRef { return OutputRef{Root: root, Path: path} }
	port := func(root, name string) OutputRef { return OutputRef{Root: root, FromPort: name} }
	cedes := func(root, path string, excludes ...string) OutputRef {
		return OutputRef{Root: root, Path: path, Excludes: excludes}
	}

	cases := []struct {
		name string
		a, b OutputRef
		want bool
	}{
		{"identical paths", ref("project", ".gen"), ref("project", ".gen"), true},
		{"case aliases are one portable owner", ref("project", "Dist/API"), ref("project", "dist/api"), true},
		{"case aliases preserve containment", ref("project", "Dist"), ref("project", "dist/API"), true},
		{"file inside declared subtree", ref("project", ".gen"), ref("project", ".gen/api/openapi.json"), true},
		{"subtree inside declared subtree", ref("project", "dist"), ref("project", "dist/esm"), true},
		{"containment is symmetric", ref("project", "dist/esm"), ref("project", "dist"), true},
		{"sibling paths", ref("project", "dist"), ref("project", "build"), false},
		{"prefix without segment boundary", ref("project", "dist"), ref("project", "dist2"), false},
		{"prefix without segment boundary, reversed", ref("project", "dist2"), ref("project", "dist"), false},
		{"different roots are not comparable", ref("project", "dist"), ref("workspace", "dist"), false},
		{"unnormalizable path is not comparable", ref("project", ""), ref("project", "dist"), false},
		{"same port under same root", port("project", "clientOutput"), port("project", "clientOutput"), true},
		{"different ports", port("project", "clientOutput"), port("project", "docsOutput"), false},
		{"port vs literal defers to runtime", port("project", "clientOutput"), ref("project", "clients"), false},
		// The carve-out: a subtree its owner cedes has one owner, not two, and
		// the answer does not depend on which side is compared first.
		{
			"ceded subpath has one owner",
			cedes("project", ".gen", ".gen/migration-bundle"),
			ref("project", ".gen/migration-bundle"),
			false,
		},
		{
			"ceded subpath is symmetric",
			ref("project", ".gen/migration-bundle"),
			cedes("project", ".gen", ".gen/migration-bundle"),
			false,
		},
		{
			"a path inside a ceded subtree is also ceded",
			cedes("project", ".gen", ".gen/migration-bundle"),
			ref("project", ".gen/migration-bundle/bundle.json"),
			false,
		},
		{
			"a cede is case-insensitive like every other ownership comparison",
			cedes("project", ".gen", ".gen/Migration-Bundle"),
			ref("project", ".gen/migration-bundle"),
			false,
		},
		{
			"ceding a subpath does not cede the tree that contains it",
			cedes("project", ".gen", ".gen/migration-bundle"),
			ref("project", ".gen"),
			true,
		},
		{
			"a sibling of the ceded subpath still collides",
			cedes("project", ".gen", ".gen/migration-bundle"),
			ref("project", ".gen/schema"),
			true,
		},
		{
			"two claims on the ceded subpath still collide",
			ref("project", ".gen/migration-bundle"),
			ref("project", ".gen/migration-bundle/bundle.json"),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutputsOverlap(tc.a, tc.b); got != tc.want {
				t.Errorf("OutputsOverlap(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestOutputsOverlap_ResolvedRefs pins that the same predicate serves plan-time
// callers that resolved each root to an absolute base: a workspace-rooted path
// that lands inside a project is caught once both refs are resolved, which is
// the cross-root case the static check deliberately leaves open.
func TestOutputsOverlap_ResolvedRefs(t *testing.T) {
	base := "/ws/packages/api"
	fromProject := OutputRef{Root: base, Path: "dist"}
	fromWorkspace := OutputRef{Root: base, Path: "dist/esm"}
	if !OutputsOverlap(fromProject, fromWorkspace) {
		t.Error("resolved refs under the same absolute base must compare as overlapping")
	}
	other := OutputRef{Root: "/ws/packages/web", Path: "dist"}
	if OutputsOverlap(fromProject, other) {
		t.Error("resolved refs under different absolute bases must not overlap")
	}
	caseAlias := OutputRef{Root: "/WS/Packages/API", Path: "DIST"}
	if !OutputsOverlap(fromProject, caseAlias) {
		t.Error("resolved root and path case aliases must overlap portably")
	}
}

func TestOutputEffectiveRootAndRef(t *testing.T) {
	if got := (DeclaredOutput{}).EffectiveRoot(); got != OutputRootProject {
		t.Errorf("EffectiveRoot() = %q, want %q", got, OutputRootProject)
	}
	if got := (DeclaredOutput{Root: OutputRootWorkspace}).EffectiveRoot(); got != OutputRootWorkspace {
		t.Errorf("EffectiveRoot() = %q, want %q", got, OutputRootWorkspace)
	}

	ref := DeclaredOutput{Kind: OutputKindDirectory, Path: "dist/"}.Ref()
	if ref.Root != OutputRootProject || ref.Path != "dist" || !ref.Resolved() {
		t.Errorf("Ref() = %+v, want normalized project-rooted resolved ref", ref)
	}
	bad := DeclaredOutput{Kind: OutputKindFile, Path: "../escape"}.Ref()
	if bad.Path != "" || bad.Resolved() {
		t.Errorf("Ref() for an invalid path = %+v, want empty unresolved path", bad)
	}
	dynamic := DeclaredOutput{Kind: OutputKindDirectory, PathFrom: "clientOutput"}.Ref()
	if dynamic.Resolved() {
		t.Errorf("Ref() for a pathFrom output must not be resolved: %+v", dynamic)
	}
}

// A carve-out reaches the ownership comparison only when the declaration makes
// it decidable. Everything else is DROPPED from the ref rather than honored, so
// a defective declaration can never buy itself an ownership exemption: the
// overlap is reported and validation names the defect.
func TestOutputRef_CarriesOnlyDecidableExcludes(t *testing.T) {
	cases := []struct {
		name   string
		output DeclaredOutput
		want   []string
	}{
		{
			name: "normalized, sorted and kept",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{
				".gen/schema/./openapi.json", ".gen/migration-bundle/",
			}},
			want: []string{".gen/migration-bundle", ".gen/schema/openapi.json"},
		},
		{
			name:   "an entry equal to the path cedes nothing",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{".gen"}},
			want:   nil,
		},
		{
			name:   "an entry outside the path cedes nothing",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{"dist/other"}},
			want:   nil,
		},
		{
			name:   "an unnormalizable entry is dropped",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{".gen/*"}},
			want:   nil,
		},
		{
			name:   "a segment prefix is not inside the path",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{".gen2/bundle"}},
			want:   nil,
		},
		{
			name:   "an unresolved path has nothing to be inside of",
			output: DeclaredOutput{Kind: OutputKindDirectory, PathFrom: "clientOutput", Excludes: []string{"clients/go"}},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.output.Ref().Excludes; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Ref().Excludes = %v, want %v", got, tc.want)
			}
		})
	}
}

// A preserve is output-relative, so it is decidable on a pathFrom output too.
// Only a directory output preserves anything, and an entry that does not
// normalize is dropped: the bytes stay captured, which is the safe direction.
func TestDecidablePreserves(t *testing.T) {
	cases := []struct {
		name   string
		output DeclaredOutput
		want   []string
	}{
		{
			name: "normalized, sorted and kept on a pathFrom output",
			output: DeclaredOutput{Kind: OutputKindDirectory, PathFrom: "clientOutput", Preserves: []string{
				"putnami.json", "./go.sum", "go.mod",
			}},
			want: []string{"go.mod", "go.sum", "putnami.json"},
		},
		{
			name:   "a file output preserves nothing",
			output: DeclaredOutput{Kind: OutputKindFile, Path: "dist/app.js", Preserves: []string{"putnami.json"}},
			want:   nil,
		},
		{
			name:   "an escaping entry is dropped",
			output: DeclaredOutput{Kind: OutputKindDirectory, Path: "clients/go", Preserves: []string{"../putnami.json"}},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecidablePreserves(tc.output); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecidablePreserves = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTaskEffectVocabulary(t *testing.T) {
	for _, effect := range ValidTaskEffects {
		if !IsValidTaskEffect(effect) {
			t.Errorf("IsValidTaskEffect(%q) = false for a member of the vocabulary", effect)
		}
	}
	if IsValidTaskEffect("sources") {
		t.Error("source mutation is the mutatesSources flag, not an effect member")
	}
	if IsValidTaskEffect("") || IsValidTaskEffect("nope") {
		t.Error("IsValidTaskEffect accepted a value outside the closed vocabulary")
	}
	for _, effect := range []string{EffectRegistry, EffectCloud, EffectProcess} {
		if !IsExternalTaskEffect(effect) {
			t.Errorf("IsExternalTaskEffect(%q) = false; a cache hit cannot reproduce it", effect)
		}
	}
	for _, effect := range []string{EffectNetwork, EffectToolchainCache, EffectWorkspaceFiles, "nope"} {
		if IsExternalTaskEffect(effect) {
			t.Errorf("IsExternalTaskEffect(%q) = true; only registry, cloud, and process are external", effect)
		}
	}
}

// TestManifestProtocolVersion pins how a loader recognizes v3: the vocabulary
// is self-identifying, and a manifest without it is a v2 manifest.
func TestManifestProtocolVersion(t *testing.T) {
	if got := ManifestProtocolVersion(nil); got != ProtocolVersionV2 {
		t.Errorf("ManifestProtocolVersion(nil) = %d, want %d", got, ProtocolVersionV2)
	}

	v2 := loadFixtureManifest(t, "fixtures/valid/full.json")
	if got := ManifestProtocolVersion(v2); got != ProtocolVersionV2 {
		t.Errorf("full.json (no declares) = v%d, want v%d", got, ProtocolVersionV2)
	}
	for name, task := range v2.Tasks {
		if task.UsesTaskContractV3() {
			t.Errorf("task %q in a v2 fixture reports the v3 contract", name)
		}
	}

	v3 := loadFixtureManifest(t, v3FixturePath)
	if got := ManifestProtocolVersion(v3); got != ProtocolVersionV3 {
		t.Errorf("task-contract-v3.json = v%d, want v%d", got, ProtocolVersionV3)
	}
	if !v3.Tasks["build-generate"].UsesTaskContractV3() {
		t.Error("build-generate declares outputs but does not report the v3 contract")
	}
	// A v3 manifest mixes contracts per task: lint-fix declares only an effect
	// flag, and a task with no declares block at all stays v2.
	if v3.Tasks["build-generate"].Declares.MutatesSources {
		t.Error("build-generate must not claim source mutation")
	}
	if !v3.Tasks["lint-fix"].Declares.MutatesSources {
		t.Error("lint-fix must declare source mutation")
	}

	finalizerOnly := &Manifest{
		Commands: map[string]CommandDefinition{
			"test": {Run: []PipelineStep{{
				ID: "teardown", Task: "teardown", RunOn: StepRunOnFinally,
				Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
			}}},
		},
	}
	if got := ManifestProtocolVersion(finalizerOnly); got != ProtocolVersionV3 {
		t.Errorf("finalizer-only manifest = v%d, want v%d", got, ProtocolVersionV3)
	}
}

func TestValidateManifest_V3FixtureIsClean(t *testing.T) {
	m := loadFixtureManifest(t, v3FixturePath)
	if diags := FullValidateManifest(m); len(diags) != 0 {
		t.Fatalf("task-contract-v3.json produced diagnostics: %v", diags)
	}
}

// TestValidateTaskDeclaration_Rules pins one diagnostic code per rule so a
// consumer (the conformance harness in B3a, the planner in B2b) can rely on the
// code rather than message text.
func TestValidateTaskDeclaration_Rules(t *testing.T) {
	cases := []struct {
		name string
		task TaskDefinition
		code string
	}{
		{
			name: "kind required",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Path: "dist"}},
			}},
			code: "required-field",
		},
		{
			name: "kind must be file or directory",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: "tree", Path: "dist"}},
			}},
			code: "invalid-enum",
		},
		{
			name: "root must be known",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile, Root: "home", Path: "dist"}},
			}},
			code: "invalid-enum",
		},
		{
			name: "path or pathFrom is required",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile}},
			}},
			code: "required-field",
		},
		{
			name: "path and pathFrom are exclusive",
			task: TaskDefinition{
				Outputs: map[string]TaskOutputPort{"port": {}},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile, Path: "dist", PathFrom: "port"}},
				},
			},
			code: "invalid-value",
		},
		{
			name: "path must be normalizable",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindDirectory, Path: "../outside"}},
			}},
			code: "invalid-output-path",
		},
		{
			name: "pathFrom must name a declared port",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindDirectory, PathFrom: "clientOutput"}},
			}},
			code: "unresolved-output-port",
		},
		{
			name: "empty output id",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"": {Kind: OutputKindFile, Path: "dist"}},
			}},
			code: "required-field",
		},
		{
			name: "an exclude must be normalizable",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {
					Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{".gen/**"},
				}},
			}},
			code: "invalid-output-path",
		},
		{
			name: "an exclude must sit strictly inside the output path",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {
					Kind: OutputKindDirectory, Path: ".gen", Excludes: []string{".gen"},
				}},
			}},
			code: "invalid-output-exclude",
		},
		{
			name: "only a directory output may cede a subpath",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {
					Kind: OutputKindFile, Path: "lcov.info", Excludes: []string{"lcov.info/part"},
				}},
			}},
			code: "invalid-output-exclude",
		},
		{
			name: "a ceding output must declare a literal path",
			task: TaskDefinition{
				Outputs: map[string]TaskOutputPort{"clientOutput": {}},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {
						Kind: OutputKindDirectory, PathFrom: "clientOutput", Excludes: []string{"clients/go"},
					}},
				},
			},
			code: "invalid-output-exclude",
		},
		{
			name: "duplicate exclude",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {
					Kind: OutputKindDirectory, Path: ".gen",
					Excludes: []string{".gen/bundle", ".gen/./bundle"},
				}},
			}},
			code: "duplicate-output-exclude",
		},
		{
			name: "unknown effect",
			task: TaskDefinition{Declares: &TaskDeclaration{Effects: []string{"telepathy"}}},
			code: "invalid-enum",
		},
		{
			name: "duplicate effect",
			task: TaskDefinition{Declares: &TaskDeclaration{Effects: []string{EffectNetwork, EffectNetwork}}},
			code: "duplicate-effect",
		},
		{
			name: "noOutput contradicts declared outputs",
			task: TaskDefinition{
				Cache: &TaskCachePolicy{NoOutput: true},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile, Path: "lcov.info"}},
				},
			},
			code: "effect-conflict",
		},
		{
			name: "external effect must disable caching",
			task: TaskDefinition{Declares: &TaskDeclaration{Effects: []string{EffectRegistry}}},
			code: "effect-conflict",
		},
		{
			name: "mutatesSources without the sources write resource",
			task: TaskDefinition{Declares: &TaskDeclaration{MutatesSources: true}},
			code: "effect-conflict",
		},
		{
			name: "sources write resource without mutatesSources",
			task: TaskDefinition{
				Writes:   []ResourceRef{{ID: ResourceIDSources}},
				Declares: &TaskDeclaration{Effects: []string{EffectToolchainCache}},
			},
			code: "effect-conflict",
		},
		{
			name: "scope must be known",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile, Scope: "session", Path: "dsn.env"}},
			}},
			code: "invalid-enum",
		},
		{
			name: "a runtime file must be invocation-scoped",
			task: TaskDefinition{
				Cache: &TaskCachePolicy{Enabled: boolPtr(false)},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindRuntimeFile, Path: "dsn.env"}},
				},
			},
			code: "invocation-scope-required",
		},
		{
			name: "a sensitive output must be invocation-scoped",
			task: TaskDefinition{
				Cache: &TaskCachePolicy{Enabled: boolPtr(false)},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {Kind: OutputKindFile, Sensitive: true, Path: "dsn.env"}},
				},
			},
			code: "invocation-scope-required",
		},
		{
			name: "an invocation-scoped output must not name a staging root",
			task: TaskDefinition{
				Cache: &TaskCachePolicy{Enabled: boolPtr(false)},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {
						Kind:  OutputKindFile,
						Scope: OutputScopeInvocation,
						Root:  OutputRootProject,
						Path:  "dsn.env",
					}},
				},
			},
			code: "invocation-scope-conflict",
		},
		{
			name: "a sensitive output must not report its path through a port",
			task: TaskDefinition{
				Cache:   &TaskCachePolicy{Enabled: boolPtr(false)},
				Outputs: map[string]TaskOutputPort{"credentialsPath": {}},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {
						Kind:      OutputKindRuntimeFile,
						Scope:     OutputScopeInvocation,
						Sensitive: true,
						PathFrom:  "credentialsPath",
					}},
				},
			},
			code: "sensitive-path-leak",
		},
		{
			name: "an invocation-scoped output cannot be replayed from cache",
			task: TaskDefinition{Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{"o": {
					Kind:  OutputKindRuntimeFile,
					Scope: OutputScopeInvocation,
					Path:  "dsn.env",
				}},
			}},
			code: "invocation-cache-conflict",
		},
		{
			name: "an invocation-scoped output must still be a safe path",
			task: TaskDefinition{
				Cache: &TaskCachePolicy{Enabled: boolPtr(false)},
				Declares: &TaskDeclaration{
					Outputs: map[string]DeclaredOutput{"o": {
						Kind:  OutputKindRuntimeFile,
						Scope: OutputScopeInvocation,
						Path:  "../../etc/passwd",
					}},
				},
			},
			code: "invalid-output-path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateTaskDeclaration("tasks.t", tc.task)
			if !diag.HasErrors(diags) {
				t.Fatalf("expected an error diagnostic, got %v", diags)
			}
			if !hasCode(diags, tc.code) {
				t.Fatalf("expected code %q, got %v", tc.code, diagCodes(diags))
			}
			for _, d := range diags {
				if !strings.HasPrefix(d.Field, "tasks.t.declares") {
					t.Errorf("diagnostic field %q must be rooted at the declaration", d.Field)
				}
			}
		})
	}
}

// TestValidateTaskDeclaration_AcceptedShapes pins that the rules above do not
// fire on the declarations they must allow.
func TestValidateTaskDeclaration_AcceptedShapes(t *testing.T) {
	cases := map[string]TaskDefinition{
		"v2 task has no declaration": {Kind: "command", Command: "echo"},
		"external effect with caching disabled": {
			Cache:    &TaskCachePolicy{Enabled: boolPtr(false)},
			Declares: &TaskDeclaration{Effects: []string{EffectCloud, EffectProcess, EffectRegistry}},
		},
		"cacheable network effect": {
			Declares: &TaskDeclaration{Effects: []string{EffectNetwork, EffectToolchainCache}},
		},
		"source mutation with the sources write resource": {
			Writes:   []ResourceRef{{ID: ResourceIDSources, Scope: ResourceScopeProject}},
			Cache:    &TaskCachePolicy{NoOutput: true},
			Declares: &TaskDeclaration{MutatesSources: true},
		},
		"noOutput with effects only": {
			Cache:    &TaskCachePolicy{NoOutput: true},
			Declares: &TaskDeclaration{Effects: []string{EffectToolchainCache}},
		},
		"optional-empty outputs at every root": {
			Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"gen":      {Kind: OutputKindDirectory, Path: ".gen", OptionalEmpty: true},
				"lock":     {Kind: OutputKindFile, Root: OutputRootWorkspace, Path: "bun.lock"},
				"coverage": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "lcov.info", OptionalEmpty: true},
			}},
		},
		"invocation-scoped artifacts on an uncacheable setup task": {
			Cache: &TaskCachePolicy{Enabled: boolPtr(false)},
			Declares: &TaskDeclaration{
				Outputs: map[string]DeclaredOutput{
					"dsn": {
						Kind:      OutputKindRuntimeFile,
						Scope:     OutputScopeInvocation,
						Sensitive: true,
						Path:      "database/dsn.env",
					},
					"lease": {Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "database/lease.json"},
				},
				Effects: []string{EffectProcess},
			},
		},
		"an explicitly durable output is the pre-scope default": {
			Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"gen": {Kind: OutputKindDirectory, Scope: OutputScopeDurable, Path: ".gen"},
			}},
		},
	}

	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			if diags := validateTaskDeclaration("tasks.t", task); len(diags) != 0 {
				t.Fatalf("expected no diagnostics, got %v", diags)
			}
		})
	}
}

// TestOutputScopeVocabulary pins the scope defaults and the derived root. An
// invocation-scoped output must never resolve to a staging root: staging is
// what gets captured, and capture is the one thing these outputs must escape.
func TestOutputScopeVocabulary(t *testing.T) {
	durable := DeclaredOutput{Kind: OutputKindDirectory, Path: ".gen"}
	if got := durable.EffectiveScope(); got != OutputScopeDurable {
		t.Errorf("absent scope = %q, want %q", got, OutputScopeDurable)
	}
	if durable.IsInvocationScoped() {
		t.Error("an output without a scope is durable")
	}
	if got := durable.EffectiveRoot(); got != OutputRootProject {
		t.Errorf("durable root = %q, want %q", got, OutputRootProject)
	}

	explicit := DeclaredOutput{Kind: OutputKindFile, Scope: OutputScopeDurable, Root: OutputRootWorkspace, Path: "bun.lock"}
	if got := explicit.EffectiveRoot(); got != OutputRootWorkspace {
		t.Errorf("explicit durable root = %q, want %q", got, OutputRootWorkspace)
	}

	invocation := DeclaredOutput{Kind: OutputKindRuntimeFile, Scope: OutputScopeInvocation, Path: "dsn.env"}
	if !invocation.IsInvocationScoped() {
		t.Error("an invocation-scoped output must report itself as one")
	}
	if got := invocation.EffectiveRoot(); got != OutputRootInvocation {
		t.Errorf("invocation root = %q, want %q", got, OutputRootInvocation)
	}
	for _, staged := range []string{OutputRootProject, OutputRootWorkspace, OutputRootCommandOutput} {
		if invocation.EffectiveRoot() == staged {
			t.Errorf("an invocation-scoped output resolved to the staging root %q", staged)
		}
	}

	// Ownership still applies inside the invocation scratch, and never crosses
	// into a staged tree.
	if !OutputsOverlap(invocation.Ref(), DeclaredOutput{
		Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "dsn.env",
	}.Ref()) {
		t.Error("two invocation-scoped outputs at one path must collide")
	}
	if OutputsOverlap(invocation.Ref(), DeclaredOutput{Kind: OutputKindFile, Path: "dsn.env"}.Ref()) {
		t.Error("an invocation-scoped output must not collide with a project-rooted one")
	}

	for _, vocabulary := range [][]string{ValidOutputKinds, ValidOutputScopes} {
		for i := 1; i < len(vocabulary); i++ {
			if vocabulary[i-1] >= vocabulary[i] {
				t.Errorf("vocabulary is not in canonical (sorted) order: %v", vocabulary)
			}
		}
	}
}

// TestSourcesWriteScope pins that only the PROJECT-scoped sources resource
// satisfies the source-mutation pairing: a workspace-scoped resource of the
// same id serializes something else.
func TestSourcesWriteScope(t *testing.T) {
	task := TaskDefinition{
		Writes:   []ResourceRef{{ID: ResourceIDSources, Scope: ResourceScopeWorkspace}},
		Declares: &TaskDeclaration{MutatesSources: true},
	}
	if diags := validateTaskDeclaration("tasks.t", task); !diag.HasErrors(diags) {
		t.Fatal("a workspace-scoped sources resource must not satisfy mutatesSources")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestValidateOutputOwnership(t *testing.T) {
	dir := func(root, path string) DeclaredOutput {
		return DeclaredOutput{Kind: OutputKindDirectory, Root: root, Path: path}
	}

	t.Run("nested project outputs in different tasks collide", func(t *testing.T) {
		m := &Manifest{Tasks: map[string]TaskDefinition{
			"generate": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"gen": dir("", ".gen")}}},
			"describe": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"openapi": {Kind: OutputKindFile, Path: ".gen/api/openapi.json"},
			}}},
		}}
		diags := ValidateOutputOwnership(m)
		if !diag.HasErrors(diags) || !hasCode(diags, "output-overlap") {
			t.Fatalf("expected an output-overlap error, got %v", diags)
		}
	})

	t.Run("two outputs of one task collide", func(t *testing.T) {
		m := &Manifest{Tasks: map[string]TaskDefinition{
			"build": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"all": dir("", "dist"),
				"esm": dir("", "dist/esm"),
			}}},
		}}
		if diags := ValidateOutputOwnership(m); !hasCode(diags, "output-overlap") {
			t.Fatalf("expected an output-overlap error, got %v", diags)
		}
	})

	t.Run("distinct paths do not collide", func(t *testing.T) {
		m := &Manifest{Tasks: map[string]TaskDefinition{
			"a": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"one": dir("", "dist")}}},
			"b": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"two": dir("", "dist2")}}},
		}}
		if diags := ValidateOutputOwnership(m); len(diags) != 0 {
			t.Fatalf("expected no diagnostics, got %v", diags)
		}
	})

	t.Run("command-output paths collide within one command", func(t *testing.T) {
		m := &Manifest{
			Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{
				{ID: "a", Task: "a"},
				{ID: "b", Task: "b"},
			}}},
			Tasks: map[string]TaskDefinition{
				"a": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
				"b": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
			},
		}
		if diags := ValidateOutputOwnership(m); !hasCode(diags, "output-overlap") {
			t.Fatalf("steps of one command share the output dir; expected overlap, got %v", diags)
		}
	})

	t.Run("command-output paths in different commands are independent", func(t *testing.T) {
		m := &Manifest{
			Commands: map[string]CommandDefinition{
				"build": {Run: []PipelineStep{{ID: "a", Task: "a"}}},
				"test":  {Run: []PipelineStep{{ID: "b", Task: "b"}}},
			},
			Tasks: map[string]TaskDefinition{
				"a": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
				"b": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
			},
		}
		if diags := ValidateOutputOwnership(m); len(diags) != 0 {
			t.Fatalf("different commands write different output dirs; expected no overlap, got %v", diags)
		}
	})

	t.Run("same runtime port under one root collides", func(t *testing.T) {
		m := &Manifest{Tasks: map[string]TaskDefinition{
			"a": {
				Outputs: map[string]TaskOutputPort{"clientOutput": {}},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"clients": {Kind: OutputKindDirectory, PathFrom: "clientOutput"},
				}},
			},
			"b": {
				Outputs: map[string]TaskOutputPort{"clientOutput": {}},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"clients": {Kind: OutputKindDirectory, PathFrom: "clientOutput"},
				}},
			},
		}}
		if diags := ValidateOutputOwnership(m); !hasCode(diags, "output-overlap") {
			t.Fatalf("expected overlap for the same port under one root, got %v", diags)
		}
	})

	t.Run("a step with no task never claims ownership", func(t *testing.T) {
		m := &Manifest{
			// A structurally invalid step (validation reports it separately)
			// must not make the ownership map attribute outputs to task "".
			Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{
				{ID: "broken"},
				{ID: "a", Task: "a"},
			}}},
			Tasks: map[string]TaskDefinition{
				"a": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
				"orphan": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"report": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "report.json"},
				}}},
			},
		}
		if diags := ValidateOutputOwnership(m); len(diags) != 0 {
			t.Fatalf("an unscheduled task shares no command output dir; got %v", diags)
		}
	})

	t.Run("nil and v2 manifests are inert", func(t *testing.T) {
		if diags := ValidateOutputOwnership(nil); diags != nil {
			t.Fatalf("nil manifest produced %v", diags)
		}
		v2 := loadFixtureManifest(t, "fixtures/valid/full.json")
		if diags := ValidateOutputOwnership(v2); len(diags) != 0 {
			t.Fatalf("v2 manifest produced %v", diags)
		}
	})
}

// TestValidateOutputOwnership_Deterministic pins that the ownership phase does
// not inherit Go map iteration order.
func TestValidateOutputOwnership_Deterministic(t *testing.T) {
	m := &Manifest{Tasks: map[string]TaskDefinition{
		"zebra":   {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"z": {Kind: OutputKindDirectory, Path: "dist"}}}},
		"alpha":   {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"a": {Kind: OutputKindFile, Path: "dist/a.js"}}}},
		"mango":   {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"m": {Kind: OutputKindFile, Path: "dist/m.js"}}}},
		"bravo":   {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"b": {Kind: OutputKindDirectory, Path: "dist/nested"}}}},
		"charlie": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"c": {Kind: OutputKindDirectory, Path: "other"}}}},
	}}

	canonical := ValidateOutputOwnership(m)
	if len(canonical) == 0 {
		t.Fatal("expected overlap diagnostics")
	}
	for i := 0; i < 100; i++ {
		if got := ValidateOutputOwnership(m); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: non-deterministic ownership diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestNormalizeTaskDeclarations_Canonical pins the canonical form the B0e
// task-contract digest is taken over: sorted effects and cleaned paths, with a
// byte-identical serialization across runs.
func TestNormalizeTaskDeclarations_Canonical(t *testing.T) {
	makeManifest := func() *Manifest {
		return &Manifest{Tasks: map[string]TaskDefinition{
			"zebra": {Kind: "command", Command: "echo", Declares: &TaskDeclaration{
				Effects: []string{EffectWorkspaceFiles, EffectNetwork, EffectToolchainCache},
				Outputs: map[string]DeclaredOutput{
					"dist": {Kind: OutputKindDirectory, Path: "./dist/"},
					"gen": {Kind: OutputKindDirectory, Path: "a/b/../.gen", Excludes: []string{
						"a/.gen/schema/./openapi.json", "a/.gen/migration-bundle/",
					}},
				},
			}},
			"alpha": {Kind: "command", Command: "echo", Declares: &TaskDeclaration{
				Effects: []string{EffectNetwork},
				Outputs: map[string]DeclaredOutput{
					"bad": {Kind: OutputKindFile, Path: "../escape"},
				},
			}},
		}}
	}

	m := makeManifest()
	NormalizeTaskDeclarations(m)

	wantEffects := []string{EffectNetwork, EffectToolchainCache, EffectWorkspaceFiles}
	if got := m.Tasks["zebra"].Declares.Effects; !reflect.DeepEqual(got, wantEffects) {
		t.Errorf("effects = %v, want sorted %v", got, wantEffects)
	}
	if got := m.Tasks["zebra"].Declares.Outputs["dist"].Path; got != "dist" {
		t.Errorf("path = %q, want cleaned %q", got, "dist")
	}
	if got := m.Tasks["zebra"].Declares.Outputs["gen"].Path; got != "a/.gen" {
		t.Errorf("path = %q, want cleaned %q", got, "a/.gen")
	}
	// Carve-outs are cleaned and sorted for the same reason effects are: the
	// digest is taken over this form, so authoring order cannot move it.
	wantExcludes := []string{"a/.gen/migration-bundle", "a/.gen/schema/openapi.json"}
	if got := m.Tasks["zebra"].Declares.Outputs["gen"].Excludes; !reflect.DeepEqual(got, wantExcludes) {
		t.Errorf("excludes = %v, want cleaned and sorted %v", got, wantExcludes)
	}
	// An unnormalizable path is left for validation to report rather than
	// silently rewritten into something the author never wrote.
	if got := m.Tasks["alpha"].Declares.Outputs["bad"].Path; got != "../escape" {
		t.Errorf("invalid path = %q, want it left untouched", got)
	}

	canonical, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 50; i++ {
		other := makeManifest()
		NormalizeTaskDeclarations(other)
		got, err := json.Marshal(other)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Equal(got, canonical) {
			t.Fatalf("iteration %d: serialization is not canonical\n want: %s\n  got: %s", i, canonical, got)
		}
	}

	NormalizeTaskDeclarations(nil)
	NormalizeTaskDeclarations(&Manifest{Tasks: map[string]TaskDefinition{"v2": {Kind: "command", Command: "echo"}}})
}

// TestV3FixtureRoundTrip pins that a v3 manifest survives parse → normalize →
// marshal → parse unchanged, so the declaration a digest covers is the
// declaration a consumer reads.
func TestV3FixtureRoundTrip(t *testing.T) {
	data, err := os.ReadFile(v3FixturePath)
	if err != nil {
		t.Fatal(err)
	}
	first, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse+validate: %v", diags)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, diags := ParseAndValidateManifest(encoded)
	if diag.HasErrors(diags) {
		t.Fatalf("re-parse: %v", diags)
	}
	if !reflect.DeepEqual(first.Tasks, second.Tasks) {
		t.Error("task declarations did not survive a round trip")
	}
}

// TestV3InvalidFixtures pins WHICH rule each v3 fixture violates through the
// full validation entry point consumers call. The shared invalid-fixture
// conformance test only requires "some diagnostic", which a fixture could
// satisfy by accident (a typo elsewhere in the file); this test makes each
// fixture certify its own rule, and the harness-level twin
// (TestConformance_TaskContracts_InvalidFixtures) proves the same codes come
// from the task contract rather than from an unrelated phase.
func TestV3InvalidFixtures(t *testing.T) {
	for name, codes := range v3InvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			diags := FullValidateManifest(m)
			if !reflect.DeepEqual(diagCodes(diags), codes) {
				t.Fatalf("codes = %v, want %v (%v)", diagCodes(diags), codes, diags)
			}
		})
	}
}

// writeFixtureAtContract writes a fixture to a temp manifest with its
// cliContract set to contract (or removed, for contract 0). Fixtures carry no
// stamp of their own — they are the VALIDATION corpus, and the stamp is earned
// at package time — so a loader test has to state which contract it is
// exercising rather than inherit one from the corpus.
func writeFixtureAtContract(t *testing.T, source []byte, contract int) string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(source, &raw); err != nil {
		t.Fatal(err)
	}
	if contract == 0 {
		delete(raw, "cliContract")
	} else {
		raw["cliContract"] = contract
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ManifestFilename)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadManifest_RecognizesV3 pins loader recognition: a v3 manifest stamped
// at the current CLI contract loads with its declarations intact.
//
// It used to assert the same at contract positions 0, 1 and 2 — the tolerate-v2
// half of B0b — because the ladder adapted anything lower. Slice B6c deleted
// that arm, so those positions moved to
// TestLoadManifest_LowerContractRejectsAV3Manifest below: the declarations are
// still intact in the file, but a loader that cannot trust the stamp must not
// act on them.
func TestLoadManifest_RecognizesV3(t *testing.T) {
	source, err := os.ReadFile(v3FixturePath)
	if err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(writeFixtureAtContract(t, source, protocolcli.CurrentContract))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV3 {
		t.Fatalf("version = v%d, want v%d", got, ProtocolVersionV3)
	}
	gen := m.Tasks["build-generate"].Declares.Outputs["gen"]
	if gen.Kind != OutputKindDirectory || gen.Path != ".gen" || gen.EffectiveRoot() != OutputRootProject {
		t.Fatalf("declared output not preserved: %+v", gen)
	}
	if !m.Tasks["lint-fix"].Declares.MutatesSources {
		t.Fatal("mutatesSources not preserved")
	}
}

// TestLoadManifest_LowerContractRejectsAV3Manifest is the converted half: a
// perfectly good v3 manifest that carries an OLDER stamp is rejected anyway.
// The stamp is the only evidence a loader has about which contract an extension
// implements; reading the declarations and ignoring the stamp would mean
// trusting the half of the document that happens to look modern.
func TestLoadManifest_LowerContractRejectsAV3Manifest(t *testing.T) {
	source, err := os.ReadFile(v3FixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for contract := 0; contract < protocolcli.CurrentContract; contract++ {
		t.Run(fmt.Sprintf("contract-%d", contract), func(t *testing.T) {
			if _, err := LoadManifest(writeFixtureAtContract(t, source, contract)); err == nil {
				t.Fatalf("contract %d loaded; it is below the required %d",
					contract, protocolcli.CurrentContract)
			}
		})
	}
}

// TestLoadManifest_TaskContractIsOrthogonalToTheCLIContract pins what the CLI
// contract does NOT decide. A manifest stamped at contract 3 whose tasks carry
// no `declares` block still loads as a task-contract-v2 manifest: requiring
// every task to declare is the slice that deletes inferred capture (B6c — B7a
// shipped without it, and the LEGACY PATH markers in the CLI's scheduler name
// B6c as the owner), not this one, and the two versions answer different
// questions.
func TestLoadManifest_TaskContractIsOrthogonalToTheCLIContract(t *testing.T) {
	source, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(writeFixtureAtContract(t, source, protocolcli.CurrentContract))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV2 {
		t.Fatalf("version = v%d, want v%d", got, ProtocolVersionV2)
	}
	for name, task := range m.Tasks {
		if task.Declares != nil {
			t.Errorf("task %q gained a v3 declaration at load time", name)
		}
	}
}

// TestLoadManifest_TolerantOfInvalidV3 pins that strictness stays at the
// authoring and packaging surfaces: a malformed declaration in an installed
// extension never drops it at load time, but FullValidateManifest rejects it.
func TestLoadManifest_TolerantOfInvalidV3(t *testing.T) {
	source, err := os.ReadFile("fixtures/invalid/output-overlap.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(writeFixtureAtContract(t, source, protocolcli.CurrentContract))
	if err != nil {
		t.Fatalf("load must tolerate a defective declaration: %v", err)
	}
	if !diag.HasErrors(FullValidateManifest(m)) {
		t.Fatal("FullValidateManifest must reject the manifest LoadManifest tolerated")
	}
}

// A carve-out cedes a subpath to ANOTHER task. A task that ceded one to ITSELF
// would declare two outputs nothing orders against each other on restore, so
// one materialize could delete the other; that declaration stays the overlap it
// was before carve-outs existed. The cross-task control keeps the rule from
// being a blanket rejection.
func TestValidateOutputOwnership_CarveOutDoesNotDivideOneTask(t *testing.T) {
	gen := DeclaredOutput{
		Kind: OutputKindDirectory, Root: OutputRootProject,
		Path: ".gen", Excludes: []string{".gen/migration-bundle"},
	}
	bundle := DeclaredOutput{
		Kind: OutputKindDirectory, Root: OutputRootProject,
		Path: ".gen/migration-bundle", OptionalEmpty: true,
	}

	oneTask := &Manifest{Tasks: map[string]TaskDefinition{
		"build-generate": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
			"gen": gen, "migrationBundle": bundle,
		}}},
	}}
	diags := ValidateOutputOwnership(oneTask)
	if len(diags) != 1 || diags[0].Code != "output-overlap" {
		t.Fatalf("a task ceding a subpath to itself produced %v, want one output-overlap", diags)
	}
	if !strings.Contains(diags[0].Message, "same task") {
		t.Errorf("diagnostic %q does not explain that the two outputs belong to one task", diags[0].Message)
	}
	// Every ownership diagnostic names the invariant it enforces, and the
	// extension harnesses that mutate a real manifest look for that name:
	// tooling/scaffold, tooling/clientgen-extension and python/extension all
	// assert "exactly one owner" on the diagnostics of a doctored manifest.
	// Dropping the phrase from this branch turns three of those harnesses red
	// in CI while the protocol's own suite stays green, so pin it here.
	if !strings.Contains(diags[0].Message, "exactly one owner") {
		t.Errorf("diagnostic %q does not name the ONE OWNER PER OUTPUT invariant the downstream "+
			"manifest harnesses assert on", diags[0].Message)
	}

	twoTasks := &Manifest{Tasks: map[string]TaskDefinition{
		"build-generate": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"gen": gen}}},
		"build-describe": {Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{"migrationBundle": bundle}}},
	}}
	if diags := ValidateOutputOwnership(twoTasks); len(diags) != 0 {
		t.Fatalf("the same pair split across two tasks was rejected: %v", diags)
	}
}

// TestValidateOutputDrift pins the shapes a drift policy may ride on. Every
// accepted row is a committed worktree output the engine can take a reference
// for; every rejected row names an output with no committed state, or a
// pathFrom output under a root nobody digests before every run.
func TestValidateOutputDrift(t *testing.T) {
	task := TaskDefinition{Outputs: map[string]TaskOutputPort{"clientOutput": {}}}
	cases := []struct {
		name   string
		output DeclaredOutput
		codes  []string
	}{
		{name: "absent policy compares nothing", output: DeclaredOutput{Kind: OutputKindDirectory, Path: "clients/go"}},
		{name: "literal file at warn", output: DeclaredOutput{Kind: OutputKindFile, Path: "schema/openapi.json", Drift: OutputDriftWarn}},
		{name: "literal workspace directory at fail", output: DeclaredOutput{Kind: OutputKindDirectory, Root: OutputRootWorkspace, Path: "docs/generated", Drift: OutputDriftFail}},
		{name: "pathFrom under the default project root", output: DeclaredOutput{Kind: OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true, Drift: OutputDriftFail}},
		{name: "pathFrom under an explicit project root", output: DeclaredOutput{Kind: OutputKindDirectory, Root: OutputRootProject, PathFrom: "clientOutput", Drift: OutputDriftFail}},
		{name: "unknown policy", output: DeclaredOutput{Kind: OutputKindFile, Path: "a.txt", Drift: "error"}, codes: []string{"invalid-enum"}},
		{name: "command-output root", output: DeclaredOutput{Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "lcov.info", Drift: OutputDriftWarn}, codes: []string{"invalid-output-drift"}},
		{name: "invocation scope", output: DeclaredOutput{Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "lease.json", Drift: OutputDriftWarn}, codes: []string{"invalid-output-drift"}},
		{name: "pathFrom under the workspace root", output: DeclaredOutput{Kind: OutputKindDirectory, Root: OutputRootWorkspace, PathFrom: "clientOutput", Drift: OutputDriftFail}, codes: []string{"invalid-output-drift"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, d := range validateDeclaredOutput("tasks.t.declares.outputs.o", tc.output, task) {
				if d.Code == "invalid-output-drift" || (d.Code == "invalid-enum" && strings.HasSuffix(d.Field, ".drift")) {
					got = append(got, d.Code)
				}
			}
			if !reflect.DeepEqual(got, tc.codes) {
				t.Fatalf("drift codes = %v, want %v", got, tc.codes)
			}
		})
	}
	if !IsValidOutputDriftPolicy(OutputDriftWarn) || !IsValidOutputDriftPolicy(OutputDriftFail) || IsValidOutputDriftPolicy("") {
		t.Fatal("the drift vocabulary is exactly warn and fail; the empty string is the absent field")
	}
}
