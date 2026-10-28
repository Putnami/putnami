package collaboration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// echoHelperEnv makes this test binary the echo tool
// (TestEchoToolHelperProcess): the self-exec helper-process pattern call_test.go
// explains.
const echoHelperEnv = "PUTNAMI_COLLAB_ECHO_TOOL_HELPER"

// TestEchoToolHelperProcess is the echo tool. It does nothing unless a tool
// invocation spawned it.
//
// It writes the raw ToolCallRequest bytes it read on stdin back out inside a
// result envelope. The request is deliberately NOT decoded and re-encoded here:
// the tests read this text to assert what crossed the wire.
func TestEchoToolHelperProcess(t *testing.T) {
	if os.Getenv(echoHelperEnv) == "" {
		t.Skip("helper process: not invoked as an extension tool")
	}
	request, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	encoded, err := json.Marshal(proto.ToolCallResult{
		Content: []proto.ToolContent{{Type: "text", Text: string(request)}},
	})
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(encoded)
	// Exit before the testing package prints its own PASS line: a tool's stdout
	// carries ONE document and the orchestrator rejects anything after it.
	os.Exit(0)
}

func testExtensionToolDefinition(command string) extension.ToolDefinition {
	trueVal, falseVal := true, false
	return extension.ToolDefinition{
		Description: "Test extension tool.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: &extension.ToolAnnotations{
			ReadOnlyHint:    &trueVal,
			DestructiveHint: &falseVal,
			IdempotentHint:  &trueVal,
			OpenWorldHint:   &trueVal,
		},
		Meta: map[string]any{"putnami.dev/contract": map[string]any{
			"access": "read", "readOnly": true, "supportsDryRun": false,
		}},
		Command: command,
	}
}

// TestAToolMayNameThePreparedRuntime pins the resolution {extensionRuntime}
// gets on the invocation path.
//
// A manifest-declared tool names the extension's prepared runtime the same way
// every task does, and BuildTemplateVars deliberately does not carry that token
// — the orchestrator supplies it only where it owns one. Without this, a
// provider tool would run the literal string "{extensionRuntime}".
func TestAToolMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate this test binary: %v", err)
	}
	def := testExtensionToolDefinition("{" + proto.TemplateVarExtensionRuntime + "}")
	def.Args = []string{"-test.run=^TestEchoToolHelperProcess$"}
	def.Env = map[string]string{echoHelperEnv: "1"}
	candidate := extensionToolCandidate{name: "putnami.echo", def: def, ext: &extension.ExtensionDescription{
		Name: "@putnami/echo", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/echo"},
		// Already synchronized, which is what a second call sees. Preparing one
		// here would compile a Go module inside a unit test.
		RuntimeExecutable: executable,
	}}
	workspaceRoot := t.TempDir()

	result, err := invokeExtensionTool(context.Background(), workspaceRoot, candidate, proto.ToolCallRequest{Name: "putnami.echo"})
	if err != nil {
		t.Fatalf("a tool naming {extensionRuntime} did not run: %v", err)
	}
	var request proto.ToolCallRequest
	if err := json.Unmarshal([]byte(result.Content[0].Text), &request); err != nil {
		t.Fatalf("the echoed request is not a ToolCallRequest: %v\n%s", err, result.Content[0].Text)
	}
	if request.Name != "putnami.echo" {
		t.Errorf("request name = %q", request.Name)
	}

	// An extension that declares no runtime has no answer for the token: the
	// invocation fails before any process starts.
	undeclared := candidate
	undeclared.ext = &extension.ExtensionDescription{Name: "@putnami/without", Path: t.TempDir()}
	_, err = invokeExtensionTool(context.Background(), workspaceRoot, undeclared, proto.ToolCallRequest{Name: "putnami.echo"})
	var failure *extensionToolError
	if !errors.As(err, &failure) || failure.stage != stagePrepare {
		t.Fatalf("a tool naming an undeclared runtime: err = %v, want a failure before the process starts", err)
	}
}

// TestAToolEnvMayNameThePreparedRuntime extends the resolution to env values:
// a tool that names the prepared runtime only in its env gets the executable,
// not the literal "{extensionRuntime}".
func TestAToolEnvMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	token := "{" + proto.TemplateVarExtensionRuntime + "}"
	def := testExtensionToolDefinition("tool")
	def.Env = map[string]string{"TOOL_RUNTIME": token}
	executable := filepath.Join(t.TempDir(), "compiled", "tool")
	candidate := extensionToolCandidate{name: "putnami.tool", def: def, ext: &extension.ExtensionDescription{
		Name: "@putnami/tool", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/tool"},
		// Already synchronized, which is what a second call sees.
		RuntimeExecutable: executable,
	}}

	vars, err := extensionToolTemplateVars(context.Background(), t.TempDir(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := extension.ExpandTemplateVars(token, vars); got != executable {
		t.Fatalf("an env value naming {extensionRuntime} expands to %q, want the prepared runtime %q", got, executable)
	}
}
