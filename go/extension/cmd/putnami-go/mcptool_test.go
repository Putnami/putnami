package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

func callDocsTool(t *testing.T, request proto.ToolCallRequest) proto.ToolCallResult {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runMCPTool(context.Background(), bytes.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	var result proto.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", out.String(), err)
	}
	return result
}

func TestGoDocsToolReturnsUnavailableAsSuccessfulJSON(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "api")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module example.test/api\n\nrequire example.test/missing v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := callDocsTool(t, proto.ToolCallRequest{
		Name: toolGoDocs, WorkspaceRoot: root,
		Arguments: json.RawMessage(`{"reference":"example.test/missing","project":"api"}`),
	})
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("tool result = %+v", result)
	}
	if !strings.Contains(result.Content[0].Text, `"status": "unavailable"`) ||
		!strings.Contains(result.Content[0].Text, `"reason": "offline_missing"`) {
		t.Fatalf("tool content = %s", result.Content[0].Text)
	}
}

func TestGoDocsToolStrictArgumentsAndManifestStayInSync(t *testing.T) {
	result := callDocsTool(t, proto.ToolCallRequest{
		Name: toolGoDocs, WorkspaceRoot: t.TempDir(),
		Arguments: json.RawMessage(`{"reference":"example.test/dep","workspace":"wrong"}`),
	})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "unknown field") {
		t.Fatalf("tool result = %+v", result)
	}

	manifest := loadExtensionManifest(t)
	if len(manifest.Tools) != 1 {
		t.Fatalf("manifest tools = %v", manifest.Tools)
	}
	if _, ok := manifest.Tools[toolGoDocs]; !ok {
		t.Fatalf("manifest does not declare %s", toolGoDocs)
	}
	if _, ok := mcpToolHandlers()[toolGoDocs]; !ok {
		t.Fatalf("handler does not implement %s", toolGoDocs)
	}
}
