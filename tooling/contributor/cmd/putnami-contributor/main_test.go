package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// extensionRoot is the directory of the extension this command is the
// runtime of.
var extensionRoot = filepath.Join("..", "..")

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(extensionRoot, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	return manifest
}

// TestTheManifestDeclaresThisRuntime holds the shipped manifest to strict
// validation and to this command. The runtime it declares is the executable
// bin/prepare builds from this directory with the lock's Go toolchain, whose
// path and pinned version the host passes to bin/prepare, the agent content
// stays declared, and the manifest claims the contract the
// packager stamps on it, with no extension dependency.
func TestTheManifestDeclaresThisRuntime(t *testing.T) {
	t.Parallel()
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	if manifest.Runtime == nil || manifest.Runtime.Prepare == nil {
		t.Fatal("the manifest declares no prepared runtime")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := manifest.Runtime.Executable, "compiled/"+filepath.Base(wd); got != want {
		t.Errorf("runtime executable %q, want %q, the program this directory builds", got, want)
	}
	prepare := manifest.Runtime.Prepare
	if prepare.Command != "{extensionRoot}/bin/prepare" || !slices.Equal(prepare.Args, []string{"--output", "{runtimeOutput}"}) {
		t.Errorf("prepare runs %s %v, want bin/prepare --output {runtimeOutput}", prepare.Command, prepare.Args)
	}
	for _, input := range []string{"bin/prepare", "cmd/**", "internal/**", "go.mod", "go.sum"} {
		if !slices.Contains(prepare.Inputs, input) {
			t.Errorf("prepare inputs %v omit %s, which the build reads", prepare.Inputs, input)
		}
	}
	if !slices.Equal(prepare.Toolchains, []string{"runtimeCompiler"}) {
		t.Fatalf("prepare toolchains %v, want [runtimeCompiler]", prepare.Toolchains)
	}
	compiler := manifest.Runtime.Toolchains["runtimeCompiler"]
	if compiler.Lock != "go" || compiler.Optional || compiler.Probe.Expect != "go{version}" {
		t.Errorf("runtimeCompiler %+v does not resolve the Go release putnami.lock.json pins", compiler)
	}
	if binding := compiler.Environment[lockedCompilerEnvironment]; binding.From != extension.RuntimeToolchainEnvironmentExecutable {
		t.Errorf("runtimeCompiler publishes %s as %+v, want the resolved executable", lockedCompilerEnvironment, binding)
	}
	if binding := compiler.Environment[lockedVersionEnvironment]; binding.From != extension.RuntimeToolchainEnvironmentLiteral || binding.Value != compiler.Probe.Expect {
		t.Errorf("runtimeCompiler publishes %s as %+v, want the literal %q the probe expects", lockedVersionEnvironment, binding, compiler.Probe.Expect)
	}
	if manifest.AgentContent == nil || manifest.AgentContent.Source != "src" || manifest.AgentContent.Path != "content" {
		t.Errorf("agentContent %+v, want source src packaged at content", manifest.AgentContent)
	}
	if len(manifest.ExtensionDeps.List) != 0 {
		t.Errorf("the manifest names extension dependencies %v", manifest.ExtensionDeps.List)
	}
	if got, want := manifest.CLIContract, extension.RequiredCLIContract(manifest); got != want {
		t.Errorf("cliContract %d, want %d, the contract the packager stamps", got, want)
	}
}

// TestRunAnswersTheRuntimeHandshake pins the descriptor the host compares
// with the manifest before it runs anything from this executable.
func TestRunAnswersTheRuntimeHandshake(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		t.Fatalf("runtime-info is not one descriptor: %v", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("runtime-info carries trailing data: %v", err)
	}
	want := runtimeproto.Info{
		Extension:       readManifest(t).Name,
		Version:         runtimeVersion,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	}
	if info != want {
		t.Errorf("runtime-info %+v, want %+v", info, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("runtime-info wrote to stderr: %q", stderr.String())
	}
}

// TestRunRefusesEveryOtherInvocation pins that the runtime does nothing
// beyond the handshake: any other argument list exits 2 and writes nothing
// to stdout.
func TestRunRefusesEveryOtherInvocation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"verify"},
		{"__putnami"},
		{"__putnami", "runtime-info", "extra"},
	} {
		t.Run(path.Join(append([]string{"args"}, args...)...), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), "unknown invocation") {
				t.Errorf("stderr %q does not name the refusal", stderr.String())
			}
		})
	}
}

// TestRunReportsAHandshakeItCannotWrite pins that a runtime-info descriptor
// the runtime cannot write is a failure the host sees: exit 1 and the write
// error on stderr, never a silent success.
func TestRunReportsAHandshakeItCannotWrite(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, failingWriter{}, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), errClosedStdout.Error()) {
		t.Errorf("stderr %q does not name the write error", stderr.String())
	}
}

var errClosedStdout = errors.New("stdout is closed")

// failingWriter refuses every write, as a closed stdout does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errClosedStdout }
