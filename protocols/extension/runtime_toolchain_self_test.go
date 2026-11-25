package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// selfManifest returns a manifest whose one toolchain declares self.
func selfManifest(self *RuntimeToolchainProbeSelf) *Manifest {
	return &Manifest{
		Name:     "sample/probe-self",
		Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{{ID: "build", Task: "build"}}}},
		Runtime: &RuntimeDefinition{
			Executable: "compiled/example",
			Toolchains: map[string]RuntimeToolchain{
				"compiler": {
					Lock:       "compiler",
					Candidates: []RuntimeToolchainCandidate{{From: RuntimeToolchainCandidatePath, Path: "compiler"}},
					Probe:      RuntimeToolchainProbe{Args: []string{"version"}, Expect: "{version}", Self: self},
				},
			},
			RunToolchains: []string{"compiler"},
		},
		Tasks: map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}"}},
	}
}

func TestValidateRuntimeToolchains_ProbeSelf(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "a-toolchain-probe-may-name-its-own-executable", "a-probe-self-is-validated")
	for _, self := range []*RuntimeToolchainProbeSelf{
		{Args: []string{"env", "ROOT"}, Path: "bin/compiler"},
		{Args: []string{"--print", "process.execPath"}},
		{Args: []string{"-e", "print 1"}, Path: "compiler.exe"},
	} {
		if diags := ValidateRuntime(selfManifest(self)); len(diags) != 0 {
			t.Errorf("self %+v: diagnostics %v, want none", *self, diags)
		}
	}
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = "arg"
	}
	for _, tc := range []struct {
		name  string
		self  RuntimeToolchainProbeSelf
		code  string
		field string
	}{
		{"no argument", RuntimeToolchainProbeSelf{Path: "bin/compiler"}, "required-field", "args"},
		{"too many arguments", RuntimeToolchainProbeSelf{Args: tooMany}, "limit-exceeded", "args"},
		{"absolute path", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "/bin/compiler"}, "invalid-value", "path"},
		{"escaping path", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "../compiler"}, "invalid-value", "path"},
		{"dot segment", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "bin/./compiler"}, "invalid-value", "path"},
		{"empty segment", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "bin//compiler"}, "invalid-value", "path"},
		{"trailing slash", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "bin/"}, "invalid-value", "path"},
		{"backslash", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: `bin\compiler`}, "invalid-value", "path"},
		{"drive", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "C:compiler"}, "invalid-value", "path"},
		{"token", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: "bin/{version}"}, "invalid-value", "path"},
		{"too long", RuntimeToolchainProbeSelf{Args: []string{"root"}, Path: strings.Repeat("a", 257)}, "invalid-value", "path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			self := tc.self
			requireDiagnostic(t, ValidateRuntime(selfManifest(&self)), tc.code, "runtime.toolchains.compiler.probe.self."+tc.field)
		})
	}
}

func TestParseManifest_ProbeSelfIsStrict(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "a-toolchain-probe-may-name-its-own-executable", "a-probe-self-is-validated")
	const manifest = `{"name": "sample/probe-self", "cliContract": 4,
 "commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
 "runtime": {"executable": "compiled/example",
  "toolchains": {"compiler": {"lock": "compiler", "candidates": [{"from": "path", "path": "compiler"}],
   "probe": {"args": ["version"], "expect": "{version}", "self": {"args": ["env", "ROOT"], "path": "bin/compiler"%s}}}},
  "runToolchains": ["compiler"]},
 "tasks": {"build": {"kind": "command", "command": "{extensionRuntime}"}}}`
	m, diags := ParseAndValidateManifest([]byte(strings.Replace(manifest, "%s", "", 1)))
	if diag.HasErrors(diags) {
		t.Fatalf("a probe with self: %v", diags)
	}
	self := m.Runtime.Toolchains["compiler"].Probe.Self
	if self == nil || strings.Join(self.Args, " ") != "env ROOT" || self.Path != "bin/compiler" {
		t.Fatalf("parsed self = %+v, want args [env ROOT] and path bin/compiler", self)
	}
	if _, diags := ParseAndValidateManifest([]byte(strings.Replace(manifest, "%s", `, "root": "x"`, 1))); !diag.HasErrors(diags) {
		t.Fatal("a self with an unknown member parsed without an error")
	}
}

func TestRequiredCLIContract_ProbeSelfRaisesNoFloor(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "a-toolchain-probe-may-name-its-own-executable", "a-probe-self-raises-no-contract-floor")
	without := RequiredCLIContract(selfManifest(nil))
	with := selfManifest(&RuntimeToolchainProbeSelf{Args: []string{"env", "ROOT"}, Path: "bin/compiler"})
	if got := RequiredCLIContract(with); got != without {
		t.Fatalf("RequiredCLIContract with probe self = %d, want %d as without it", got, without)
	}
	if without >= protocolcli.ToolchainProbeSelfContract {
		t.Fatalf("a manifest without self requires %d, want below %d", without, protocolcli.ToolchainProbeSelfContract)
	}
	// A reader below the rung parses the manifest loosely and drops the member;
	// one at the rung reads it. Both load a manifest stamped at its floor.
	with.CLIContract = without
	data := marshalManifest(t, with)
	loaded, err := NegotiateManifest("putnami.extension.json", data)
	if err != nil {
		t.Fatalf("a manifest stamped %d with probe self does not load: %v", without, err)
	}
	if loaded.Runtime.Toolchains["compiler"].Probe.Self == nil {
		t.Fatal("the loaded manifest dropped probe self")
	}
}
