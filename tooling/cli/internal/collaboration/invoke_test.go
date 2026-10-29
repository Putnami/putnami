package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// echoHelperEnv makes this test binary the echo provider
// (TestEchoProviderHelperProcess): the self-exec helper-process pattern
// call_test.go explains.
const echoHelperEnv = "PUTNAMI_COLLAB_ECHO_PROVIDER_HELPER"

// TestEchoProviderHelperProcess is the echo provider. It does nothing unless a
// provider invocation spawned it.
//
// It writes the raw ProviderRequest bytes it read on stdin back to stdout. The
// request is deliberately NOT decoded and re-encoded here: the tests read
// these bytes to assert what crossed the wire.
func TestEchoProviderHelperProcess(t *testing.T) {
	if os.Getenv(echoHelperEnv) == "" {
		t.Skip("helper process: not invoked as a provider")
	}
	request, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(request)
	// Exit before the testing package prints its own PASS line: a provider's
	// stdout carries ONE document and the orchestrator rejects anything after
	// it.
	os.Exit(0)
}

func testProviderDeclaration(command string) extension.ProviderDeclaration {
	openWorld := true
	return extension.ProviderDeclaration{
		Version: 1,
		Command: command,
		Operations: map[string]extension.ProviderOperation{
			collab.OperationFind: {Description: "Test provider operation.", OpenWorld: &openWorld},
		},
	}
}

// TestAProviderMayNameThePreparedRuntime pins the resolution {extensionRuntime}
// gets on the invocation path.
//
// A provider declaration names the extension's prepared runtime the same way
// every task does, and BuildTemplateVars deliberately does not carry that token
// — the orchestrator supplies it only where it owns one. Without this, a
// provider would run the literal string "{extensionRuntime}".
func TestAProviderMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate this test binary: %v", err)
	}
	declaration := testProviderDeclaration("{" + proto.TemplateVarExtensionRuntime + "}")
	declaration.Args = []string{"-test.run=^TestEchoProviderHelperProcess$"}
	declaration.Env = map[string]string{echoHelperEnv: "1"}
	candidate := providerCandidate{contract: collab.ContractTasks, declaration: declaration, ext: &extension.ExtensionDescription{
		Name: "@putnami/echo", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/echo"},
		// Already synchronized, which is what a second call sees. Preparing one
		// here would compile a Go module inside a unit test.
		RuntimeExecutable: executable,
	}}
	workspaceRoot := t.TempDir()
	sent := collab.ProviderRequest{
		Arguments:     json.RawMessage(`{"page":{"size":20}}`),
		WorkspaceRoot: workspaceRoot,
		Provider:      &collab.ProviderCall{Contract: collab.ContractTasks, Version: 1, Operation: collab.OperationFind},
	}

	stdout, err := invokeProvider(context.Background(), workspaceRoot, candidate, sent)
	if err != nil {
		t.Fatalf("a provider naming {extensionRuntime} did not run: %v", err)
	}
	// The wire carries exactly the ProviderRequest members: a strict decode
	// refuses a tool name, an agent or any other member it does not declare.
	var request collab.ProviderRequest
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		t.Fatalf("the echoed request is not a ProviderRequest: %v\n%s", err, stdout)
	}
	if request.Provider == nil || request.Provider.Contract != collab.ContractTasks || request.Provider.Operation != collab.OperationFind {
		t.Errorf("provider member = %+v", request.Provider)
	}

	// An extension that declares no runtime has no answer for the token: the
	// invocation fails before any process starts.
	undeclared := candidate
	undeclared.ext = &extension.ExtensionDescription{Name: "@putnami/without", Path: t.TempDir()}
	_, err = invokeProvider(context.Background(), workspaceRoot, undeclared, sent)
	var failure *providerError
	if !errors.As(err, &failure) || failure.stage != stagePrepare {
		t.Fatalf("a provider naming an undeclared runtime: err = %v, want a failure before the process starts", err)
	}
}

// TestAProviderEnvMayNameThePreparedRuntime extends the resolution to env
// values: a provider that names the prepared runtime only in its env gets the
// executable, not the literal "{extensionRuntime}".
func TestAProviderEnvMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	token := "{" + proto.TemplateVarExtensionRuntime + "}"
	declaration := testProviderDeclaration("provider")
	declaration.Env = map[string]string{"PROVIDER_RUNTIME": token}
	executable := filepath.Join(t.TempDir(), "compiled", "provider")
	candidate := providerCandidate{contract: collab.ContractTasks, declaration: declaration, ext: &extension.ExtensionDescription{
		Name: "@putnami/provider", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/provider"},
		// Already synchronized, which is what a second call sees.
		RuntimeExecutable: executable,
	}}

	vars, err := providerTemplateVars(context.Background(), t.TempDir(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := extension.ExpandTemplateVars(token, vars); got != executable {
		t.Fatalf("an env value naming {extensionRuntime} expands to %q, want the prepared runtime %q", got, executable)
	}
}
