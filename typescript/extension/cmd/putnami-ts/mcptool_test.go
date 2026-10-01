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

func TestTypeScriptDocsToolReturnsExactInstalledBytes(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "app")
	packageDir := filepath.Join(projectDir, "node_modules", "@putnami", "web")
	for _, dir := range []string{projectDir, packageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(`{"name":"app","dependencies":{"@putnami/web":"4.1.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"@putnami/web","version":"4.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "AI.md"), []byte("# Web 4.1 docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := callDocsTool(t, proto.ToolCallRequest{
		Name: toolTypeScriptDocs, WorkspaceRoot: root,
		Arguments: json.RawMessage(`{"reference":"@putnami/web","project":"app"}`),
	})
	if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, `"version": "4.1.0"`) || !strings.Contains(result.Content[0].Text, "# Web 4.1 docs") {
		t.Fatalf("tool result = %+v", result)
	}
}

func TestTypeScriptDocsToolStrictArgumentsAndManifestStayInSync(t *testing.T) {
	result := callDocsTool(t, proto.ToolCallRequest{
		Name: toolTypeScriptDocs, WorkspaceRoot: t.TempDir(),
		Arguments: json.RawMessage(`{"reference":"@putnami/web","workspace":"wrong"}`),
	})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "unknown field") {
		t.Fatalf("tool result = %+v", result)
	}

	manifest := loadExtensionManifest(t)
	if len(manifest.Tools) != 1 {
		t.Fatalf("manifest tools = %v", manifest.Tools)
	}
	if _, ok := manifest.Tools[toolTypeScriptDocs]; !ok {
		t.Fatalf("manifest does not declare %s", toolTypeScriptDocs)
	}
	if _, ok := mcpToolHandlers()[toolTypeScriptDocs]; !ok {
		t.Fatalf("handler does not implement %s", toolTypeScriptDocs)
	}
}
