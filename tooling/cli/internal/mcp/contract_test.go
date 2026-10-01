package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

type contractSnapshot struct {
	Tools     []Tool     `json:"tools"`
	Resources []Resource `json:"resources"`
}

func TestContractMatchesToolingBaseline(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-description", "the-access-contract-matches-the-baseline")
	srv := NewServer(Options{ServerVersion: "test"})
	snapshot := contractSnapshot{
		Tools:     srv.toolDefs(),
		Resources: srv.resourceDefs(),
	}
	got, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatalf("marshal contract snapshot: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "contract.json")
	if os.Getenv("UPDATE_MCP_CONTRACT") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create baseline dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MCP contract drifted from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
