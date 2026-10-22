package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
)

// toolchainResolveRequestJSON asks @putnami/python, offline, for the uv pin of
// a linux/amd64 host.
const toolchainResolveRequestJSON = `{"version":1,"extension":"@putnami/python","identities":["uv"],"platform":"linux/amd64","offline":true}`

// runToolchainResolve runs the extension's entry point with the reserved
// toolchain-resolve argv and request on stdin, and fails the test if the call
// reaches ordinary subcommand dispatch.
func runToolchainResolve(t *testing.T, request string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runEntrypoint(wsproto.ToolchainResolveControlArgs(), strings.NewReader(request), &stdout, func(map[string]cli.JobFunc) {
		t.Fatal("the toolchain-resolve call reached ordinary subcommand dispatch")
	})
	return stdout.String(), err
}

// The reserved control call answers on stdout with one result document that
// passes the toolchain-resolve contract, on its own and against its request.
func TestToolchainResolve_ServesTheControlCall(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "uv-answers-its-toolchain-pin",
		"the-reserved-call-reads-one-request-and-writes-one-answer")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "pyproject.toml"),
		"[project]\nname = \"ws\"\n\n[tool.uv]\nrequired-version = \"=="+toolchain.DefaultUVVersion+"\"\n")
	t.Chdir(root)

	stdout, err := runToolchainResolve(t, toolchainResolveRequestJSON)
	if err != nil {
		t.Fatalf("toolchain-resolve: %v", err)
	}
	request, diags := wsproto.ParseAndValidateToolchainResolveRequest([]byte(toolchainResolveRequestJSON))
	if diag.HasErrors(diags) {
		t.Fatalf("request: %v", diags)
	}
	result, diags := wsproto.ParseAndValidateToolchainResolveResult([]byte(stdout))
	if diag.HasErrors(diags) {
		t.Fatalf("answer %q: %v", stdout, diags)
	}
	if diags := wsproto.ValidateToolchainResolveAnswer(request, result); diag.HasErrors(diags) {
		t.Fatalf("answer %q does not answer its request: %v", stdout, diags)
	}
	pin := result.Toolchains[toolchain.UVLockIdentity].Pin
	if result.Extension != pythonExtensionName || pin == nil || pin.Version != toolchain.DefaultUVVersion {
		t.Fatalf("answer = %+v, want the uv %s pin attributed to %s", result, toolchain.DefaultUVVersion, pythonExtensionName)
	}
	if got := pin.Archives["linux/amd64"].Executable; got != "uv-x86_64-unknown-linux-musl/uv" {
		t.Fatalf("linux/amd64 executable = %q, want uv-x86_64-unknown-linux-musl/uv", got)
	}

	var other bytes.Buffer
	if handled, _ := handleToolchainResolve([]string{"test"}, strings.NewReader(""), &other); handled || other.Len() != 0 {
		t.Error("the toolchain-resolve call claimed an ordinary subcommand's argv")
	}
}

// A workspace the extension cannot read is no answer: the call fails and
// writes nothing to stdout.
func TestToolchainResolve_WritesNothingWhenItCannotAnswer(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "pyproject.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	stdout, err := runToolchainResolve(t, toolchainResolveRequestJSON)
	if err == nil || !strings.HasPrefix(err.Error(), "toolchain-resolve: ") {
		t.Fatalf("error = %v, want a toolchain-resolve failure", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

// An asked identity other than uv is answered as a problem, so the core never
// reads an omission as "the lock keeps no pin" for a toolchain the extension
// does not pin.
func TestToolchainResolve_AnswersOnlyForUV(t *testing.T) {
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  pythonExtensionName,
		Identities: []string{"go", toolchain.UVLockIdentity},
		Platform:   "darwin/arm64",
		Offline:    true,
	}
	result, err := answerToolchainResolve(t.TempDir(), request)
	if err != nil {
		t.Fatalf("answerToolchainResolve: %v", err)
	}
	diags := append(wsproto.ValidateToolchainResolveResult(&result), wsproto.ValidateToolchainResolveAnswer(&request, &result)...)
	if diag.HasErrors(diags) {
		t.Fatalf("answer %+v breaks the contract: %v", result, diags)
	}
	if problem := result.Toolchains["go"].Problem; problem == nil || !strings.Contains(problem.Message, `not "go"`) {
		t.Fatalf("go = %+v, want a problem", result.Toolchains["go"])
	}
	if result.Toolchains[toolchain.UVLockIdentity].Pin == nil {
		t.Fatalf("uv = %+v, want a pin", result.Toolchains[toolchain.UVLockIdentity])
	}
}
