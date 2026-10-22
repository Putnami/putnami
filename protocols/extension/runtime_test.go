package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const lifecycleFixturePath = "fixtures/valid/lifecycle-v3.json"

func TestValidateRuntime_AcceptedShapes(t *testing.T) {
	cases := map[string]RuntimeDefinition{
		"published extension ships its executable": {
			Executable: "bin/putnami-sample",
		},
		"prepared from the extension's own tree": {
			Executable: "bin/putnami-sample",
			Prepare: &RuntimePrepare{
				Command: "{extensionRoot}/bin/prepare",
				Args:    []string{"--output", "{runtimeOutput}"},
				Inputs:  []string{"cmd/**", "go.mod"},
			},
		},
		"bare program name on PATH": {
			Executable: "bin/putnami-sample",
			Prepare: &RuntimePrepare{
				Command: "go",
				Args:    []string{"build", "-o", "{runtimeOutput}/bin/putnami-sample", "./cmd/putnami-sample"},
				Inputs:  []string{"cmd/**"},
			},
		},
		"nested executable path": {Executable: "dist/darwin-arm64/putnami-sample"},
	}
	for name, runtime := range cases {
		t.Run(name, func(t *testing.T) {
			definition := runtime
			if diags := ValidateRuntime(&Manifest{Runtime: &definition}); len(diags) != 0 {
				t.Fatalf("expected no diagnostics, got %v", diags)
			}
		})
	}
}

// TestValidateRuntime_Rules pins one diagnostic code per rule so slice C1 keys
// on the code rather than on message text.
func TestValidateRuntime_Rules(t *testing.T) {
	cases := []struct {
		name    string
		runtime RuntimeDefinition
		code    string
		field   string
	}{
		{
			name:    "executable is required",
			runtime: RuntimeDefinition{},
			code:    "required-field",
			field:   "runtime.executable",
		},
		{
			name:    "executable must not escape the extension root",
			runtime: RuntimeDefinition{Executable: "../outside/bin/x"},
			code:    "invalid-runtime-path",
			field:   "runtime.executable",
		},
		{
			name:    "executable must be relative",
			runtime: RuntimeDefinition{Executable: "/usr/local/bin/x"},
			code:    "invalid-runtime-path",
			field:   "runtime.executable",
		},
		{
			name:    "executable must not be drive-qualified",
			runtime: RuntimeDefinition{Executable: "C:/tools/x.exe"},
			code:    "invalid-runtime-path",
			field:   "runtime.executable",
		},
		{
			name:    "executable must be concrete",
			runtime: RuntimeDefinition{Executable: "bin/*"},
			code:    "invalid-runtime-path",
			field:   "runtime.executable",
		},
		{
			name: "prepare command is required",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Inputs: []string{"cmd/**"}},
			},
			code:  "required-field",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not be absolute",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "/usr/bin/make", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not escape its root",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "../tools/prepare", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not escape its root mid-path",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "{extensionRoot}/../tools/prepare", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not use ambient cwd",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "bin/prepare", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not end at a parent segment",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "{extensionRoot}/bin/..", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must not be drive-absolute",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "C:/tools/make.exe", Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare command must use slash separators",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: `bin\prepare`, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.command",
		},
		{
			name: "prepare inputs are required",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make"},
			},
			code:  "required-field",
			field: "runtime.prepare.inputs",
		},
		{
			name: "prepare input must be relative",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"/etc/passwd"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.inputs[0]",
		},
		{
			name: "prepare input must not be drive-qualified",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"C:/workspace/go.mod"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.inputs[0]",
		},
		{
			name: "prepare input must not escape the extension root",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"../shared/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.inputs[0]",
		},
		{
			name: "prepare input must not be declared twice",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"cmd/**", "./cmd/**"}},
			},
			code:  "duplicate-value",
			field: "runtime.prepare.inputs[1]",
		},
		{
			name: "prepare input must use valid glob syntax",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"cmd/[abc"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.inputs[0]",
		},
		{
			name: "prepare argument must not be empty",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{"  "}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-value",
			field: "runtime.prepare.args[0]",
		},
		{
			name: "prepare argument must not be absolute",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{"/tmp/config"}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.args[0]",
		},
		{
			name: "prepare option value must not be absolute",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{"--config=/tmp/config"}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.args[0]",
		},
		{
			name: "prepare argument must not escape",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{"../../config"}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.args[0]",
		},
		{
			name: "prepare argument must not end at parent",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{"config/.."}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.args[0]",
		},
		{
			name: "prepare argument must not use backslash separators",
			runtime: RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "make", Args: []string{`foo\bar`}, Inputs: []string{"cmd/**"}},
			},
			code:  "invalid-runtime-path",
			field: "runtime.prepare.args[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition := tc.runtime
			diags := ValidateRuntime(&Manifest{Runtime: &definition})
			if !diag.HasErrors(diags) {
				t.Fatalf("expected an error diagnostic, got %v", diags)
			}
			assertDiag(t, diags, tc.code, tc.field)
		})
	}
}

// TestValidateRuntime_PreparationIsWorkspaceIndependent pins the declaration
// half of prepare isolation: a manifest may name the extension tree and the
// CLI-assigned output directory, but no workspace/project location. The
// executor still owns process isolation for an arbitrary program.
func TestValidateRuntime_PreparationIsWorkspaceIndependent(t *testing.T) {
	forbidden := []string{
		"workspaceRoot", "projectRoot", "outputRoot", "cacheRoot",
		"selectedProjects", "selectedProjectIDs", "selectedProjectPaths", "selectedProjectRoots",
		"extensionRuntime", "invocationArtifactRoot", "typo",
	}
	for _, name := range forbidden {
		t.Run(name, func(t *testing.T) {
			inCommand := &Manifest{Runtime: &RuntimeDefinition{
				Executable: "bin/x",
				Prepare:    &RuntimePrepare{Command: "{" + name + "}/bin/prepare", Inputs: []string{"cmd/**"}},
			}}
			assertDiag(t, ValidateRuntime(inCommand), "invalid-template-var", "runtime.prepare.command")

			inArg := &Manifest{Runtime: &RuntimeDefinition{
				Executable: "bin/x",
				Prepare: &RuntimePrepare{
					Command: "make",
					Args:    []string{"--root", "{" + name + "}"},
					Inputs:  []string{"cmd/**"},
				},
			}}
			assertDiag(t, ValidateRuntime(inArg), "invalid-template-var", "runtime.prepare.args[1]")
		})
	}

	for _, name := range ValidPrepareTemplateVars {
		t.Run("allowed "+name, func(t *testing.T) {
			m := &Manifest{Runtime: &RuntimeDefinition{
				Executable: "bin/x",
				Prepare: &RuntimePrepare{
					Command: "make",
					Args:    []string{"{" + name + "}"},
					Inputs:  []string{"cmd/**"},
				},
			}}
			if diags := ValidateRuntime(m); len(diags) != 0 {
				t.Fatalf("{%s} must be available during preparation, got %v", name, diags)
			}
		})
	}
}

func TestTemplateVarNames(t *testing.T) {
	cases := map[string][]string{
		"":                                 nil,
		"make":                             nil,
		"{extensionRoot}/bin/prepare":      {"extensionRoot"},
		"{a}{b}":                           {"a", "b"},
		"--out={runtimeOutput}/bin":        {"runtimeOutput"},
		"{unterminated":                    nil,
		"prefix{a}middle{b}suffix":         {"a", "b"},
		"{}":                               {""},
		"literal } before {extensionRoot}": {"extensionRoot"},
	}
	for input, want := range cases {
		if got := templateVarNames(input); !reflect.DeepEqual(got, want) {
			t.Errorf("templateVarNames(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestRuntimeAccessors(t *testing.T) {
	var nilManifest *Manifest
	if nilManifest.DeclaresRuntime() {
		t.Error("a nil manifest declares no runtime")
	}
	if (&Manifest{}).DeclaresRuntime() {
		t.Error("a manifest without a runtime section declares no runtime")
	}
	m := loadFixtureManifest(t, lifecycleFixturePath)
	if !m.DeclaresRuntime() {
		t.Fatal("the lifecycle fixture declares a runtime")
	}
	if !m.Runtime.RequiresPreparation() {
		t.Error("the lifecycle fixture's runtime declares a prepare")
	}

	var nilRuntime *RuntimeDefinition
	if nilRuntime.RequiresPreparation() {
		t.Error("a nil runtime requires no preparation")
	}
	if (&RuntimeDefinition{Executable: "bin/x"}).RequiresPreparation() {
		t.Error("a published runtime requires no preparation")
	}
}

// TestNormalizeRuntime_Canonical pins the canonical form the C1 artifact digest
// will be taken over: cleaned executable, cleaned and sorted inputs, args left
// exactly as authored because their order is the build.
func TestNormalizeRuntime_Canonical(t *testing.T) {
	m := &Manifest{Runtime: &RuntimeDefinition{
		Executable: "./bin/../bin/sample",
		Prepare: &RuntimePrepare{
			Command: "{extensionRoot}/bin/prepare",
			Args:    []string{"--output", "{runtimeOutput}"},
			Inputs:  []string{"internal/**", "./cmd/**", "go.mod"},
		},
	}}
	NormalizeRuntime(m)

	if got := m.Runtime.Executable; got != "bin/sample" {
		t.Errorf("executable = %q, want cleaned %q", got, "bin/sample")
	}
	wantInputs := []string{"cmd/**", "go.mod", "internal/**"}
	if got := m.Runtime.Prepare.Inputs; !reflect.DeepEqual(got, wantInputs) {
		t.Errorf("inputs = %v, want cleaned and sorted %v", got, wantInputs)
	}
	wantArgs := []string{"--output", "{runtimeOutput}"}
	if got := m.Runtime.Prepare.Args; !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("args = %v, want them left as authored %v", got, wantArgs)
	}

	// An unnormalizable value is left for validation to report rather than
	// silently rewritten into something the author never wrote.
	bad := &Manifest{Runtime: &RuntimeDefinition{
		Executable: "../escape",
		Prepare:    &RuntimePrepare{Command: "make", Inputs: []string{"../escape/**"}},
	}}
	NormalizeRuntime(bad)
	if bad.Runtime.Executable != "../escape" {
		t.Errorf("invalid executable = %q, want it left untouched", bad.Runtime.Executable)
	}
	if bad.Runtime.Prepare.Inputs[0] != "../escape/**" {
		t.Errorf("invalid input = %q, want it left untouched", bad.Runtime.Prepare.Inputs[0])
	}

	// A runtime without a prepare normalizes without reaching for one.
	published := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x"}}
	NormalizeRuntime(published)
	if published.Runtime.Prepare != nil {
		t.Error("normalization invented a prepare")
	}
}

// TestNormalizeRuntime_Deterministic pins that the normalized declaration
// serializes byte-identically across runs, which is what makes the artifact
// digest a function of the declaration rather than of authoring order.
func TestNormalizeRuntime_Deterministic(t *testing.T) {
	build := func() *Manifest {
		return &Manifest{Runtime: &RuntimeDefinition{
			Executable: "bin/sample",
			Prepare: &RuntimePrepare{
				Command: "{extensionRoot}/bin/prepare",
				Args:    []string{"--output", "{runtimeOutput}"},
				Inputs:  []string{"internal/**", "cmd/**", "go.sum", "go.mod", "bin/**"},
			},
		}}
	}
	canonical := build()
	NormalizeRuntime(canonical)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		other := build()
		NormalizeRuntime(other)
		got, err := json.Marshal(other)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(encoded) {
			t.Fatalf("iteration %d: serialization is not canonical\n want %s\n  got %s", i, encoded, got)
		}
	}
}

func TestValidateRuntimeToolchains(t *testing.T) {
	valid := &Manifest{
		Runtime: &RuntimeDefinition{
			Executable: "compiled/example",
			Toolchains: map[string]RuntimeToolchain{
				"compiler": {
					Lock: "compiler",
					Candidates: []RuntimeToolchainCandidate{
						{From: RuntimeToolchainCandidatePath, Path: "compiler"},
						{From: RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/compiler/compiler-{version}/bin/compiler"},
					},
					Probe: RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}", Unset: []string{"FALSE_ROOT"}},
					Environment: map[string]RuntimeToolchainEnvironment{
						"COMPILER_ROOT": {From: RuntimeToolchainEnvironmentAncestor, Levels: 2},
					},
					PrependPath: true,
				},
			},
			RunToolchains: []string{"compiler"},
			Resolves:      []string{"compiler", "linter"},
			Prepare:       &RuntimePrepare{Command: "make", Inputs: []string{"cmd/**"}, Toolchains: []string{"compiler"}},
		},
		Tasks: map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}", Toolchains: []string{"compiler"}}},
	}
	if diags := ValidateRuntime(valid); len(diags) != 0 {
		t.Fatalf("valid runtime toolchain diagnostics = %v", diags)
	}

	cases := []struct {
		name  string
		edit  func(*Manifest)
		code  string
		field string
	}{
		{"unknown reference", func(m *Manifest) {
			m.Tasks["build"] = TaskDefinition{Kind: "command", Command: "x", Toolchains: []string{"missing"}}
		}, "unknown-reference", "tasks.build.toolchains[0]"},
		{"non ASCII alias", func(m *Manifest) { m.Runtime.RunToolchains = []string{"compilér"} }, "invalid-value", "runtime.runToolchains[0]"},
		{"unknown probe token", func(m *Manifest) {
			r := m.Runtime.Toolchains["compiler"]
			r.Probe.Expect = "{version}-{platform}"
			m.Runtime.Toolchains["compiler"] = r
		}, "invalid-value", "runtime.toolchains.compiler.probe.expect"},
		{"duplicate unset", func(m *Manifest) {
			r := m.Runtime.Toolchains["compiler"]
			r.Probe.Unset = []string{"ROOT", "ROOT"}
			m.Runtime.Toolchains["compiler"] = r
		}, "duplicate-value", "runtime.toolchains.compiler.probe.unset"},
		{"escaping candidate", func(m *Manifest) {
			r := m.Runtime.Toolchains["compiler"]
			r.Candidates[1].Path = "../compiler"
			m.Runtime.Toolchains["compiler"] = r
		}, "invalid-runtime-path", "runtime.toolchains.compiler.candidates[1].path"},
		{"too many environment bindings", func(m *Manifest) {
			r := m.Runtime.Toolchains["compiler"]
			r.Environment = map[string]RuntimeToolchainEnvironment{}
			for i := 0; i < 33; i++ {
				r.Environment[fmt.Sprintf("BINDING_%d", i)] = RuntimeToolchainEnvironment{From: RuntimeToolchainEnvironmentLiteral, Value: "value"}
			}
			m.Runtime.Toolchains["compiler"] = r
		}, "limit-exceeded", "runtime.toolchains.compiler.environment"},
		{"resolves repeat", func(m *Manifest) { m.Runtime.Resolves = []string{"go", "go"} }, "duplicate-value", "runtime.resolves[1]"},
		{"resolves leading digit", func(m *Manifest) { m.Runtime.Resolves = []string{"1go"} }, "invalid-value", "runtime.resolves[0]"},
		{"resolves non ASCII identity", func(m *Manifest) { m.Runtime.Resolves = []string{"go", "gö"} }, "invalid-value", "runtime.resolves[1]"},
		{"resolves empty identity", func(m *Manifest) { m.Runtime.Resolves = []string{""} }, "invalid-value", "runtime.resolves[0]"},
		{"resolves 129-byte identity", func(m *Manifest) {
			m.Runtime.Resolves = []string{strings.Repeat("a", 129)}
		}, "invalid-value", "runtime.resolves[0]"},
		{"resolves 17 identities", func(m *Manifest) {
			m.Runtime.Resolves = nil
			for i := 0; i < 17; i++ {
				m.Runtime.Resolves = append(m.Runtime.Resolves, fmt.Sprintf("tool%d", i))
			}
		}, "limit-exceeded", "runtime.resolves"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := *valid
			runtimeCopy := *valid.Runtime
			requirementCopy := valid.Runtime.Toolchains["compiler"]
			requirementCopy.Candidates = append([]RuntimeToolchainCandidate(nil), requirementCopy.Candidates...)
			runtimeCopy.Toolchains = map[string]RuntimeToolchain{"compiler": requirementCopy}
			copy.Runtime = &runtimeCopy
			copy.Tasks = map[string]TaskDefinition{"build": valid.Tasks["build"]}
			tc.edit(&copy)
			assertDiag(t, ValidateRuntime(&copy), tc.code, tc.field)
		})
	}
}

// runtime.resolves names workspace lock identities, not the runtime's own
// toolchains: an extension may answer pins for a toolchain another extension
// runs. Sixteen identities of 128 bytes are within bounds.
func TestValidateRuntimeResolves_AcceptsAnyLockIdentityWithinBounds(t *testing.T) {
	m := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x"}}
	for i := 0; i < 15; i++ {
		m.Runtime.Resolves = append(m.Runtime.Resolves, fmt.Sprintf("tool-%d.x_y", i))
	}
	m.Runtime.Resolves = append(m.Runtime.Resolves, strings.Repeat("a", 128))
	if diags := ValidateRuntime(m); len(diags) != 0 {
		t.Fatalf("resolves within bounds diagnostics = %v", diags)
	}
}

func TestNormalizeRuntime_SortsResolves(t *testing.T) {
	authored := []string{"uv", "bun", "uv", "golangci-lint"}
	m := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x", Resolves: authored}}
	NormalizeRuntime(m)
	if want := []string{"bun", "golangci-lint", "uv"}; !reflect.DeepEqual(m.Runtime.Resolves, want) {
		t.Errorf("resolves = %v, want sorted without repeats %v", m.Runtime.Resolves, want)
	}
	if want := []string{"uv", "bun", "uv", "golangci-lint"}; !reflect.DeepEqual(authored, want) {
		t.Errorf("normalization rewrote the authored slice to %v", authored)
	}
}

// runtime.resolves is additive vocabulary inside the runtime section: it adds
// no contract rung, so adding it alone leaves a manifest's contract as it is,
// and a core that predates the member ignores it.
func TestRuntimeResolvesKeepsTheManifestContract(t *testing.T) {
	base := &Manifest{
		Name:    "@putnami/go",
		Tasks:   map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}"}},
		Runtime: &RuntimeDefinition{Executable: "bin/x"},
	}
	withResolves := *base
	runtime := *base.Runtime
	runtime.Resolves = []string{"go", "golangci-lint"}
	withResolves.Runtime = &runtime
	if got, want := RequiredLoadContract(&withResolves), RequiredLoadContract(base); got != want {
		t.Errorf("RequiredLoadContract = %d with resolves, %d without", got, want)
	}
	if got, want := RequiredCLIContract(&withResolves), RequiredCLIContract(base); got != want {
		t.Errorf("RequiredCLIContract = %d with resolves, %d without", got, want)
	}
	if got, want := ManifestProtocolVersion(&withResolves), ManifestProtocolVersion(base); got != want {
		t.Errorf("ManifestProtocolVersion = %d with resolves, %d without", got, want)
	}

	withResolves.CLIContract = RequiredLoadContract(base)
	data, err := json.Marshal(&withResolves)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := NegotiateManifest(ManifestFilename, data)
	if err != nil {
		t.Fatalf("NegotiateManifest refused a manifest stamped at its contract: %v", err)
	}
	if want := []string{"go", "golangci-lint"}; !reflect.DeepEqual(loaded.Runtime.Resolves, want) {
		t.Errorf("loaded resolves = %v, want %v", loaded.Runtime.Resolves, want)
	}

	var older struct {
		Runtime struct {
			Executable string `json:"executable"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.Runtime.Executable != "bin/x" {
		t.Errorf("a reader that predates resolves cannot read the runtime: %v", err)
	}
}

func TestValidateRuntimeInputSelectors(t *testing.T) {
	tooMany := make([]string, 0, maxRuntimeInputSelectors+1)
	for i := 0; i <= maxRuntimeInputSelectors; i++ {
		tooMany = append(tooMany, fmt.Sprintf("scheme:name-%d", i))
	}
	cases := []struct {
		name      string
		selectors []string
		code      string
		field     string
	}{
		{"repeat", []string{"go-embed:build", "go-embed:build"}, "duplicate-value", "runtime.inputSelectors[1]"},
		{"reserved git scheme", []string{"git:build"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"uppercase scheme", []string{"go-embed:build", "Go-embed:test"}, "invalid-value", "runtime.inputSelectors[1]"},
		{"underscore scheme", []string{"go_embed:build"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"no scheme", []string{"build"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"empty name", []string{"go-embed:"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"leading hyphen name", []string{"go-embed:-build"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"glob name", []string{"go-embed:**"}, "invalid-value", "runtime.inputSelectors[0]"},
		{"empty selector", []string{""}, "invalid-value", "runtime.inputSelectors[0]"},
		{"129-byte selector", []string{"s:" + strings.Repeat("n", 127)}, "invalid-value", "runtime.inputSelectors[0]"},
		{"17 selectors", tooMany, "limit-exceeded", "runtime.inputSelectors"},
	}
	for _, tc := range cases {
		for _, prepare := range []*RuntimePrepare{nil, {Command: "make", Inputs: []string{"cmd/**"}}} {
			t.Run(fmt.Sprintf("%s/prepare=%v", tc.name, prepare != nil), func(t *testing.T) {
				m := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x", Prepare: prepare, InputSelectors: tc.selectors}}
				assertDiag(t, ValidateRuntime(m), tc.code, tc.field)
				assertDiag(t, ValidateManifest(m), tc.code, tc.field)
			})
		}
	}
}

// Sixteen selectors, one of them 128 bytes, are within bounds, whatever their
// scheme: the manifest names the selectors its extension expands.
func TestValidateRuntimeInputSelectors_AcceptsSelectorsWithinBounds(t *testing.T) {
	m := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x"}}
	m.Runtime.InputSelectors = []string{"go-embed:build", "go-embed:test", "gitx:0", "a1-b:c-2"}
	for i := len(m.Runtime.InputSelectors); i < maxRuntimeInputSelectors-1; i++ {
		m.Runtime.InputSelectors = append(m.Runtime.InputSelectors, fmt.Sprintf("proto:file-%d", i))
	}
	m.Runtime.InputSelectors = append(m.Runtime.InputSelectors, "s:"+strings.Repeat("n", 126))
	if diags := ValidateRuntime(m); len(diags) != 0 {
		t.Fatalf("input selectors within bounds diagnostics = %v", diags)
	}
}

func TestNormalizeRuntime_SortsInputSelectors(t *testing.T) {
	authored := []string{"go-embed:test", "go-embed:build", "go-embed:test"}
	m := &Manifest{Runtime: &RuntimeDefinition{Executable: "bin/x", InputSelectors: authored}}
	NormalizeRuntime(m)
	if want := []string{"go-embed:build", "go-embed:test"}; !reflect.DeepEqual(m.Runtime.InputSelectors, want) {
		t.Errorf("input selectors = %v, want sorted without repeats %v", m.Runtime.InputSelectors, want)
	}
	if want := []string{"go-embed:test", "go-embed:build", "go-embed:test"}; !reflect.DeepEqual(authored, want) {
		t.Errorf("normalization rewrote the authored slice to %v", authored)
	}
}

// runtime.inputSelectors is additive vocabulary inside the runtime section: it
// adds no contract rung, so adding it alone leaves a manifest's contract as it
// is, the negotiated loader keeps the go-embed rules it applies, and a core
// that predates the member ignores it.
func TestRuntimeInputSelectorsKeepTheManifestContract(t *testing.T) {
	base := &Manifest{
		Name:    "@putnami/go",
		Tasks:   map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}"}},
		Runtime: &RuntimeDefinition{Executable: "bin/x"},
	}
	withSelectors := *base
	runtime := *base.Runtime
	runtime.InputSelectors = []string{"go-embed:build", "go-embed:test"}
	withSelectors.Runtime = &runtime
	if got, want := RequiredLoadContract(&withSelectors), RequiredLoadContract(base); got != want {
		t.Errorf("RequiredLoadContract = %d with input selectors, %d without", got, want)
	}
	if got, want := RequiredCLIContract(&withSelectors), RequiredCLIContract(base); got != want {
		t.Errorf("RequiredCLIContract = %d with input selectors, %d without", got, want)
	}
	if got, want := ManifestProtocolVersion(&withSelectors), ManifestProtocolVersion(base); got != want {
		t.Errorf("ManifestProtocolVersion = %d with input selectors, %d without", got, want)
	}

	withSelectors.CLIContract = RequiredLoadContract(base)
	data, err := json.Marshal(&withSelectors)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := NegotiateManifest(ManifestFilename, data)
	if err != nil {
		t.Fatalf("NegotiateManifest refused a manifest stamped at its contract: %v", err)
	}
	if want := []string{"go-embed:build", "go-embed:test"}; !reflect.DeepEqual(loaded.Runtime.InputSelectors, want) {
		t.Errorf("loaded input selectors = %v, want %v", loaded.Runtime.InputSelectors, want)
	}

	// A listed selector does not widen what a task may name: the loader still
	// refuses a go-embed selector it does not expand.
	listed := withSelectors
	listedRuntime := runtime
	listedRuntime.InputSelectors = []string{"go-embed:other"}
	listed.Runtime = &listedRuntime
	listed.Tasks = map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}", Inputs: map[string]TaskInputPort{
		"sources": {From: TaskInputFromProject, Files: []string{"go-embed:other"}},
	}}}
	if data, err = json.Marshal(&listed); err != nil {
		t.Fatal(err)
	}
	if _, err := NegotiateManifest(ManifestFilename, data); err == nil || !strings.Contains(err.Error(), "unsupported go embed selector") {
		t.Errorf("NegotiateManifest = %v, want the unsupported go embed selector refused", err)
	}

	var older struct {
		Runtime struct {
			Executable string `json:"executable"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.Runtime.Executable != "bin/x" {
		t.Errorf("a reader that predates input selectors cannot read the runtime: %v", err)
	}
}

// schemas/extension.json states the selector grammar and the bounds the
// validator applies to runtime.inputSelectors.
func TestRuntimeInputSelectorsSchemaAgreesWithTheValidator(t *testing.T) {
	var schema struct {
		Defs struct {
			Runtime struct {
				Properties struct {
					InputSelectors struct {
						MaxItems    int  `json:"maxItems"`
						UniqueItems bool `json:"uniqueItems"`
						Items       struct {
							Pattern   string `json:"pattern"`
							MaxLength int    `json:"maxLength"`
							Not       struct {
								Pattern string `json:"pattern"`
							} `json:"not"`
						} `json:"items"`
					} `json:"inputSelectors"`
				} `json:"properties"`
			} `json:"runtimeDefinition"`
		} `json:"$defs"`
	}
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	selectors := schema.Defs.Runtime.Properties.InputSelectors
	if selectors.MaxItems != maxRuntimeInputSelectors || !selectors.UniqueItems {
		t.Errorf("schema bounds inputSelectors at %d (unique %v), the validator at %d unique entries",
			selectors.MaxItems, selectors.UniqueItems, maxRuntimeInputSelectors)
	}
	if selectors.Items.Pattern != runtimeInputSelectorPattern || selectors.Items.MaxLength != maxRuntimeInputSelectorBytes {
		t.Errorf("schema selector = %q (max %d), the validator applies %q (max %d)",
			selectors.Items.Pattern, selectors.Items.MaxLength, runtimeInputSelectorPattern, maxRuntimeInputSelectorBytes)
	}
	if want := "^" + runtimeInputSelectorReservedScheme + ":"; selectors.Items.Not.Pattern != want {
		t.Errorf("schema refuses selectors matching %q, want %q", selectors.Items.Not.Pattern, want)
	}
}

// TestRuntimeSectionMakesAManifestV3 pins that the runtime vocabulary is
// self-identifying: a manifest carrying it is a v3 manifest even when no task
// declares a contract.
func TestRuntimeSectionMakesAManifestV3(t *testing.T) {
	m := &Manifest{
		Tasks:   map[string]TaskDefinition{"build": {Kind: "command", Command: "echo"}},
		Runtime: &RuntimeDefinition{Executable: "bin/x"},
	}
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV3 {
		t.Errorf("a manifest with a runtime section reports v%d, want v%d", got, ProtocolVersionV3)
	}
	m.Runtime = nil
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV2 {
		t.Errorf("without the section the same manifest reports v%d, want v%d", got, ProtocolVersionV2)
	}
}

// TestLifecycleFixtureRoundTrip pins that the two manifest-level sections
// survive parse → validate → normalize → marshal → parse unchanged. The
// declaration a digest covers must be the declaration a consumer reads, and
// normalization now rewrites both sections, so the round trip is where an
// asymmetry between the two would show.
func TestLifecycleFixtureRoundTrip(t *testing.T) {
	data, err := os.ReadFile(lifecycleFixturePath)
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
	if !reflect.DeepEqual(first.Runtime, second.Runtime) {
		t.Errorf("the runtime section did not survive a round trip:\n first %+v\n  got %+v",
			first.Runtime, second.Runtime)
	}
	if !reflect.DeepEqual(first.Workspace, second.Workspace) {
		t.Errorf("the workspace adapter did not survive a round trip:\n first %+v\n  got %+v",
			first.Workspace, second.Workspace)
	}
	if !reflect.DeepEqual(first.Commands, second.Commands) {
		t.Error("pipeline run conditions did not survive a round trip")
	}
	if !reflect.DeepEqual(first.Tasks, second.Tasks) {
		t.Error("task declarations did not survive a round trip")
	}
	// The fixture is authored in canonical form, so normalization is a no-op on
	// it — which is what makes it a usable example for extension authors.
	if !reflect.DeepEqual(first.Runtime.Prepare.Inputs, normalizePatternList(first.Runtime.Prepare.Inputs)) {
		t.Error("the fixture's prepare inputs are not authored in canonical order")
	}
}

// assertDiag fails unless diags carries the exact {code, field} pair.
func assertDiag(t *testing.T, diags []diag.Diagnostic, code, field string) {
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
