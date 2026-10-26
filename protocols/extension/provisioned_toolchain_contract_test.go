package extension

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// provisionedManifest is a runtime whose compiler toolchain is provisioned
// from the archive the workspace lock pins for the "compiler" identity, which
// the same manifest answers for.
func provisionedManifest() *Manifest {
	return &Manifest{
		Name:     "@test/compiler",
		Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{{ID: "b", Task: "build"}}}},
		Tasks:    map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}", Toolchains: []string{"compiler"}}},
		Runtime: &RuntimeDefinition{
			Executable: "bin/compiler-ext",
			Resolves:   []string{"compiler"},
			Toolchains: map[string]RuntimeToolchain{
				"compiler": {
					Lock: "compiler",
					Candidates: []RuntimeToolchainCandidate{
						{From: RuntimeToolchainCandidatePath, Path: "compiler"},
						{From: RuntimeToolchainCandidateProvisioned},
					},
					Probe: RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}"},
				},
			},
		},
	}
}

// A reader before rung 13 neither provisions the pinned archive nor knows
// where its executable is, so a manifest with a provisioned candidate needs
// rung 13 wherever it loads from, and a lower stamp is refused with the reason.
func TestAProvisionedCandidateRequiresTheProvisionedToolchainContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	m := provisionedManifest()
	m.CLIContract = protocolcli.PublishChannelsContract
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NegotiateManifest("compiler.json", data)
	if err == nil {
		t.Fatal("a contract-12 stamp loaded a manifest with a provisioned candidate")
	}
	for _, fragment := range []string{"requires 13 for its provisioned toolchain candidate", "re-package the extension"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal %q does not name %q", err, fragment)
		}
	}

	m.CLIContract = protocolcli.ProvisionedToolchainContract
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	loaded, err := NegotiateManifest("compiler.json", data)
	if err != nil {
		t.Fatalf("contract-13 stamp refused: %v", err)
	}
	if got := loaded.Runtime.Toolchains["compiler"].Candidates[1]; got != (RuntimeToolchainCandidate{From: RuntimeToolchainCandidateProvisioned}) {
		t.Fatalf("loaded candidate = %+v, want a bare provisioned candidate", got)
	}
	if got := RequiredCLIContract(loaded); got != protocolcli.ProvisionedToolchainContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.ProvisionedToolchainContract)
	}
	if got := RequiredLoadContract(loaded); got != protocolcli.ProvisionedToolchainContract {
		t.Fatalf("RequiredLoadContract = %d, want %d: the rung binds every origin", got, protocolcli.ProvisionedToolchainContract)
	}
}

// Only a provisioned candidate raises the floor: the other sources, and
// runtime.resolves alone, leave it where the manifest's other vocabulary puts
// it.
func TestRequiredCLIContractRaisesToProvisionedOnlyForAProvisionedCandidate(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	withoutProvisioned := provisionedManifest()
	compiler := withoutProvisioned.Runtime.Toolchains["compiler"]
	compiler.Candidates = []RuntimeToolchainCandidate{{From: RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/compiler/compiler-{version}/bin/compiler"}}
	withoutProvisioned.Runtime.Toolchains["compiler"] = compiler

	beside := provisionedManifest()
	beside.Tasks["publish"] = publishTask("npm")

	for _, test := range []struct {
		name     string
		manifest *Manifest
		want     int
	}{
		{"no provisioned candidate", withoutProvisioned, protocolcli.CurrentContract},
		{"a provisioned candidate", provisionedManifest(), protocolcli.ProvisionedToolchainContract},
		{"beside a publish channel rung", beside, protocolcli.ProvisionedToolchainContract},
		{"no runtime", &Manifest{}, protocolcli.CurrentContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RequiredCLIContract(test.manifest); got != test.want {
				t.Fatalf("RequiredCLIContract = %d, want %d", got, test.want)
			}
		})
	}
}

// A provisioned candidate takes no path and no environment, a toolchain lists
// it at most once, and the same manifest answers for its lock identity. Each
// rule has its own code and field.
func TestValidateProvisionedCandidate(t *testing.T) {
	edit := func(change func(*RuntimeToolchain)) *Manifest {
		m := provisionedManifest()
		compiler := m.Runtime.Toolchains["compiler"]
		compiler.Candidates = slices.Clone(compiler.Candidates)
		change(&compiler)
		m.Runtime.Toolchains["compiler"] = compiler
		return m
	}
	const provisioned = "runtime.toolchains.compiler.candidates[1]"

	if diags := ValidateRuntime(provisionedManifest()); len(diags) != 0 {
		t.Fatalf("valid provisioned candidate diagnostics = %v", diags)
	}
	for _, test := range []struct {
		name     string
		manifest *Manifest
		code     string
		field    string
	}{
		{"a path", edit(func(r *RuntimeToolchain) { r.Candidates[1].Path = "bin/compiler" }), "invalid-value", provisioned + ".path"},
		{"an environment name", edit(func(r *RuntimeToolchain) { r.Candidates[1].Environment = "COMPILER_ROOT" }), "invalid-value", provisioned + ".environment"},
		{"two provisioned candidates", edit(func(r *RuntimeToolchain) {
			r.Candidates = append(r.Candidates, RuntimeToolchainCandidate{From: RuntimeToolchainCandidateProvisioned})
		}), "duplicate-value", "runtime.toolchains.compiler.candidates[2]"},
		{"an identity runtime.resolves omits", func() *Manifest {
			m := provisionedManifest()
			m.Runtime.Resolves = []string{"linter"}
			return m
		}(), "unknown-reference", provisioned},
		{"no runtime.resolves", func() *Manifest {
			m := provisionedManifest()
			m.Runtime.Resolves = nil
			return m
		}(), "unknown-reference", provisioned},
	} {
		t.Run(test.name, func(t *testing.T) {
			diags := ValidateRuntime(test.manifest)
			assertDiag(t, diags, test.code, test.field)
			if len(diags) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one", diags)
			}
		})
	}

	// The remedy is the part of the diagnostic an author acts on.
	m := provisionedManifest()
	m.Runtime.Resolves = nil
	if diags := ValidateRuntime(m); len(diags) != 1 || !strings.Contains(diags[0].Message, `runtime.resolves to list lock identity "compiler"`) {
		t.Fatalf("diagnostics = %v, want the runtime.resolves remedy", diags)
	}
}

// Path is omitted when empty, so a provisioned candidate marshals without a
// "path" member, and every other candidate, whose path is required, keeps the
// bytes it had: the toolchain identity hashes the marshaled declaration.
func TestProvisionedCandidateMarshalsWithoutAPath(t *testing.T) {
	for _, test := range []struct {
		candidate RuntimeToolchainCandidate
		want      string
	}{
		{RuntimeToolchainCandidate{From: RuntimeToolchainCandidateProvisioned}, `{"from":"provisioned"}`},
		{RuntimeToolchainCandidate{From: RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/go/go-{version}/go/bin/go"},
			`{"from":"putnami-home","path":"toolchains/go/go-{version}/go/bin/go"}`},
		{RuntimeToolchainCandidate{From: RuntimeToolchainCandidateEnvironment, Path: "bin/go", Environment: "GOROOT"},
			`{"from":"environment","path":"bin/go","environment":"GOROOT"}`},
	} {
		data, err := json.Marshal(test.candidate)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != test.want {
			t.Errorf("marshal %+v = %s, want %s", test.candidate, data, test.want)
		}
	}
}

// The schema and the Go validator agree on the shape of a candidate: the
// source vocabulary, and which sources require, or refuse, a path and an
// environment name. The instances avoid the path grammar, which only Go
// checks.
func TestRuntimeToolchainCandidateSchemaAgreesWithTheValidator(t *testing.T) {
	definition := readSchemaDefinition(t, "runtimeToolchainCandidate")
	properties, _ := definition["properties"].(map[string]any)
	from, _ := properties["from"].(map[string]any)
	enum, _ := from["enum"].([]any)
	schemaSources := make([]string, 0, len(enum))
	for _, source := range enum {
		schemaSources = append(schemaSources, source.(string))
	}
	goSources := []string{
		RuntimeToolchainCandidatePath, RuntimeToolchainCandidateEnvironment, RuntimeToolchainCandidateHome,
		RuntimeToolchainCandidatePutnamiHome, RuntimeToolchainCandidateProvisioned,
	}
	if !slices.Equal(schemaSources, goSources) {
		t.Fatalf("schema candidate sources = %v, Go sources = %v", schemaSources, goSources)
	}

	for _, test := range []struct {
		name     string
		instance map[string]any
		valid    bool
	}{
		{"provisioned", map[string]any{"from": "provisioned"}, true},
		{"provisioned with a path", map[string]any{"from": "provisioned", "path": "bin/compiler"}, false},
		{"provisioned with an environment", map[string]any{"from": "provisioned", "environment": "ROOT"}, false},
		{"PATH", map[string]any{"from": "path", "path": "compiler"}, true},
		{"PATH without a path", map[string]any{"from": "path"}, false},
		{"home", map[string]any{"from": "home", "path": "bin/compiler"}, true},
		{"home without a path", map[string]any{"from": "home"}, false},
		{"putnami-home", map[string]any{"from": "putnami-home", "path": "toolchains/compiler/bin/compiler"}, true},
		{"putnami-home without a path", map[string]any{"from": "putnami-home"}, false},
		{"environment", map[string]any{"from": "environment", "environment": "ROOT", "path": "bin/compiler"}, true},
		{"environment without a path", map[string]any{"from": "environment", "environment": "ROOT"}, false},
		{"an unknown source", map[string]any{"from": "download", "path": "bin/compiler"}, false},
		{"no source", map[string]any{"path": "bin/compiler"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			schemaValid := schemaSubsetMatches(definition, test.instance) && conditionalSchemaViolations(definition, test.instance) == 0
			data, err := json.Marshal(test.instance)
			if err != nil {
				t.Fatal(err)
			}
			var candidate RuntimeToolchainCandidate
			if err := json.Unmarshal(data, &candidate); err != nil {
				t.Fatal(err)
			}
			goValid := len(validateRuntimeToolchainCandidate("candidate", candidate)) == 0
			if schemaValid != test.valid || goValid != test.valid {
				t.Fatalf("schema valid = %v, Go valid = %v, want %v", schemaValid, goValid, test.valid)
			}
		})
	}
}
