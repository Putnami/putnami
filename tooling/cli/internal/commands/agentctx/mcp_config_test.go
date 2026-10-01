package agentctx

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// readMCPServers parses .mcp.json in dir and returns the mcpServers object.
func readMCPServers(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if err != nil {
		t.Fatalf("read %s: %v", mcpConfigPath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", mcpConfigPath, err)
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or not an object: %v", root)
	}
	return servers
}

func assertPutnamiEntry(t *testing.T, servers map[string]any) {
	t.Helper()
	entry, ok := servers["putnami"].(map[string]any)
	if !ok {
		t.Fatalf("putnami entry missing: %v", servers)
	}
	if entry["command"] != "putnami" {
		t.Errorf("command = %v, want putnami", entry["command"])
	}
	args, _ := entry["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", entry["args"])
	}
	env, err := json.Marshal(entry["env"])
	if err != nil || string(env) != `{"PUTNAMI_AGENT_WORKSPACE":"${CLAUDE_PROJECT_DIR:-.}"}` {
		t.Errorf("agent workspace env = %s (err %v), want Claude session root expansion", env, err)
	}
}

func TestEnsureMCPConfigCreatesFile(t *testing.T) {
	dir := t.TempDir()
	status, err := ensureMCPConfig(dir, mcpRepairDivergedEntry)
	if err != nil {
		t.Fatalf("ensureMCPConfig: %v", err)
	}
	if status != mcpConfigCreated {
		t.Errorf("status = %v, want created", status)
	}
	assertPutnamiEntry(t, readMCPServers(t, dir))
}

func TestEnsureMCPConfigMergesIntoExistingFile(t *testing.T) {
	dir := t.TempDir()
	existing := `{"mcpServers":{"other":{"command":"other-bin","args":["serve"]}},"customKey":42}`
	if err := os.WriteFile(filepath.Join(dir, mcpConfigPath), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := ensureMCPConfig(dir, mcpRepairDivergedEntry)
	if err != nil {
		t.Fatalf("ensureMCPConfig: %v", err)
	}
	if status != mcpConfigAdded {
		t.Errorf("status = %v, want added", status)
	}

	servers := readMCPServers(t, dir)
	assertPutnamiEntry(t, servers)
	other, ok := servers["other"].(map[string]any)
	if !ok || other["command"] != "other-bin" {
		t.Errorf("existing server was not preserved: %v", servers)
	}

	data, _ := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse rewritten file: %v", err)
	}
	if root["customKey"].(float64) != 42 {
		t.Errorf("unknown top-level key was not preserved: %v", root)
	}
}

// `putnami mcp install` is an explicit request, so it repairs a diverged
// putnami entry instead of reporting it — and keeps every other server.
func TestEnsureMCPConfigRewritesADivergedEntryAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	custom := `{"mcpServers":{"putnami":{"command":"/stale/path/putnami","args":["mcp"]},"other":{"command":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, mcpConfigPath), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := ensureMCPConfig(dir, mcpRepairDivergedEntry)
	if err != nil {
		t.Fatalf("ensureMCPConfig: %v", err)
	}
	if status != mcpConfigUpdated {
		t.Errorf("status = %v, want updated", status)
	}
	servers := readMCPServers(t, dir)
	assertPutnamiEntry(t, servers)
	if _, ok := servers["other"]; !ok {
		t.Error("other server was dropped by the rewrite")
	}
}

func TestEnsureMCPConfigIsIdempotentOnCanonicalEntry(t *testing.T) {
	dir := t.TempDir()
	if _, err := ensureMCPConfig(dir, mcpRepairDivergedEntry); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, mcpConfigPath))

	status, err := ensureMCPConfig(dir, mcpRepairDivergedEntry)
	if err != nil {
		t.Fatalf("ensureMCPConfig: %v", err)
	}
	if status != mcpConfigUnchanged {
		t.Errorf("status = %v, want unchanged for canonical entry", status)
	}
	after, _ := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if string(before) != string(after) {
		t.Error("canonical file was rewritten")
	}
}

func TestEnsureMCPConfigLeavesMalformedFileUntouched(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"invalid json", `{not json`},
		{"mcpServers wrong type", `{"mcpServers":["array"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, mcpConfigPath)
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureMCPConfig(dir, mcpRepairDivergedEntry); err == nil {
				t.Fatal("expected an error for a malformed .mcp.json")
			}
			data, _ := os.ReadFile(path)
			if string(data) != tc.content {
				t.Errorf("malformed file was modified:\nwant %s\ngot  %s", tc.content, data)
			}
		})
	}
}

// Acceptance criterion: `putnami context generate` neither creates
// .mcp.json nor touches an existing one, in either direction.
func TestContextGenerateNeverWritesMCPConfig(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "generated-context", "context-generation-writes-no-mcp-registration")
	dir := t.TempDir()
	wsConfig := `{"name":"t","extensions":["@putnami/go"]}`
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(wsConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := wsproto.Load(dir)

	if err := ContextGenerate(dir, cfg, nil); err != nil {
		t.Fatalf("ContextGenerate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, mcpConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("context generation created %s: stat err %v", mcpConfigPath, err)
	}

	// A pre-existing registration written by an older release is user data now:
	// it is neither migrated nor rewritten.
	legacy := `{"mcpServers":{"putnami":{"args":["mcp"],"command":"putnami"},"other":{"command":"other-server"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, mcpConfigPath), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ContextGenerate(dir, cfg, nil); err != nil {
		t.Fatalf("ContextGenerate over an existing registration: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if err != nil || string(after) != legacy {
		t.Fatalf("context generation rewrote %s:\n%s\n(%v)", mcpConfigPath, after, err)
	}
}

// `putnami mcp install` is the documented, explicit path an agent IDE reaches
// the server through, and it is merge-aware over a human's own servers.
func TestMCPInstallIsTheExplicitRegistrationPath(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "mcp-registration-is-explicit-and-merge-aware")
	dir := t.TempDir()
	before := `{
  "mcpServers": {
    "other": {
      "command": "other-server"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, mcpConfigPath), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MCPInstall(dir); err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}
	servers := readMCPServers(t, dir)
	assertPutnamiEntry(t, servers)
	if encoded, err := canonicalJSON(servers["other"]); err != nil || encoded != `{"command":"other-server"}` {
		t.Fatalf("unrelated server changed: %s (%v)", encoded, err)
	}

	// Idempotent: a second explicit install leaves the file byte-identical.
	current, err := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := MCPInstall(dir); err != nil {
		t.Fatalf("second MCPInstall: %v", err)
	}
	again, err := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if err != nil || string(again) != string(current) {
		t.Fatalf("second MCPInstall rewrote the file:\n%s\n(%v)", again, err)
	}
}

// The implicit registration `init`, `install` and `upgrade` run (ADR 0040)
// makes a fresh workspace reachable by an agent session with no manual step:
// a missing file is created, and an existing one gains the entry through the
// same merge `putnami mcp install` uses, keeping every other server and key.
func TestRegisterMCPServerAddsAMissingEntry(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "a-missing-entry-is-added-and-everything-else-is-kept")
	dir := t.TempDir()
	if !RegisterMCPServer(dir, nil) {
		t.Fatal("RegisterMCPServer reported no change for a missing file")
	}
	assertPutnamiEntry(t, readMCPServers(t, dir))

	merged := t.TempDir()
	existing := `{"mcpServers":{"other":{"command":"other-bin","args":["serve"]}},"customKey":42}`
	if err := os.WriteFile(filepath.Join(merged, mcpConfigPath), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RegisterMCPServer(merged, nil) {
		t.Fatal("RegisterMCPServer reported no change for a file without the entry")
	}
	servers := readMCPServers(t, merged)
	assertPutnamiEntry(t, servers)
	if encoded, err := canonicalJSON(servers["other"]); err != nil || encoded != `{"args":["serve"],"command":"other-bin"}` {
		t.Fatalf("unrelated server changed: %s (%v)", encoded, err)
	}
	data, err := os.ReadFile(filepath.Join(merged, mcpConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		CustomKey float64 `json:"customKey"`
	}
	if err := json.Unmarshal(data, &root); err != nil || root.CustomKey != 42 {
		t.Fatalf("unknown top-level key was not preserved: %s (%v)", data, err)
	}

	// Idempotent: the canonical entry is already there, so nothing is written.
	if RegisterMCPServer(merged, nil) {
		t.Fatal("RegisterMCPServer reported a change over the canonical entry")
	}
	again, err := os.ReadFile(filepath.Join(merged, mcpConfigPath))
	if err != nil || string(again) != string(data) {
		t.Fatalf("a repeated registration rewrote the file:\n%s\n(%v)", again, err)
	}
}

// Nobody asked the implicit path to change a registration someone wrote on
// purpose: a diverged putnami entry is kept byte for byte, which is the one
// place it differs from `putnami mcp install`.
func TestRegisterMCPServerKeepsADivergedEntry(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "a-diverged-entry-is-kept-byte-for-byte")
	dir := t.TempDir()
	custom := `{"mcpServers":{"putnami":{"command":"./putnamiw","args":["mcp"]},"other":{"command":"x"}}}` + "\n"
	path := filepath.Join(dir, mcpConfigPath)
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	if RegisterMCPServer(dir, &warn) {
		t.Fatal("RegisterMCPServer reported a change over a diverged entry")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != custom {
		t.Fatalf("the implicit path rewrote a diverged entry:\n%s\n(%v)", got, err)
	}
	if warn.Len() != 0 {
		t.Fatalf("a kept entry is not a problem to warn about, got %q", warn.String())
	}
	status, err := ensureMCPConfig(dir, mcpKeepDivergedEntry)
	if err != nil || status != mcpConfigKept {
		t.Fatalf("status = %v (%v), want kept", status, err)
	}
}

// A file the implicit path cannot parse is a warning, never a failed command,
// and it is left exactly as it was.
func TestRegisterMCPServerWarnsAndLeavesAnUnparseableFile(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "an-unparseable-file-is-a-warning-and-stays-untouched")
	for _, content := range []string{`{not json`, `{"mcpServers":["array"]}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, mcpConfigPath)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		if RegisterMCPServer(dir, &warn) {
			t.Fatalf("RegisterMCPServer reported a change over %q", content)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != content {
			t.Fatalf("an unparseable file was modified:\nwant %s\ngot  %s (%v)", content, got, err)
		}
		if !strings.Contains(warn.String(), "warning") || !strings.Contains(warn.String(), mcpConfigPath) || !strings.Contains(warn.String(), "putnami mcp install") {
			t.Fatalf("warning = %q, want it to name the file and the explicit repair", warn.String())
		}
	}
}
