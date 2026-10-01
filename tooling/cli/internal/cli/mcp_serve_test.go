package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

type testJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type testJSONRPCEnvelope struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      int               `json:"id"`
	Result  json.RawMessage   `json:"result"`
	Error   *testJSONRPCError `json:"error"`
}

func TestMCPAgentIdentityEnabled(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"1", "t", "TRUE", "true", "True"} {
		if !mcpAgentIdentityEnabled(raw) {
			t.Errorf("mcpAgentIdentityEnabled(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{"", "0", "false", "yes", "invalid"} {
		if mcpAgentIdentityEnabled(raw) {
			t.Errorf("mcpAgentIdentityEnabled(%q) = true, want false", raw)
		}
	}
}

func TestNewMCPServerPreparesAndConsumesExactLockedGuidance(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "live-extension-guidance", "the-cli-and-mcp-consume-the-same-lock-faithful-ai-bytes")
	const (
		name    = "@putnami/cloud"
		version = "0.1.0-locked"
		guide   = "# Cloud exact\n\nLocked MCP routing.  \n"
		digest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	root := t.TempDir()
	store := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
	artifact := sharedtest.WriteContextTestExtension(t, store, digest, name, version, guide)
	manifestHash, err := lockfile.HashFile(filepath.Join(artifact, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{
		Version: version, ManifestHash: manifestHash,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): digest},
	})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
	// Discovery may load the workspace while constructing the server. Keep its
	// authored root minimal; exact guidance comes from the supplied config and
	// lock, not a generated file.
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(`{"extensions":{"@putnami/cloud":"stable"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "stable"}}}
	srv := newMCPServerWithContext(context.Background(), root, cfg, "test")

	var out bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	if err := srv.Serve(context.Background(), strings.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Result.Instructions, guide) {
		t.Fatalf("initialize did not consume exact locked guide: %q", response.Result.Instructions)
	}
}

func TestColdMCPPreparesProviderIndexWithoutInstall(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-initialization", "graph-readiness", "cold-mcp-prepares-the-provider-index-without-install")
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename,
		`{"name":"cold","includes":["app"],"extensions":["/provider"]}`, 0o644)
	write("app/package.json", `{"name":"@acme/app"}`, 0o644)
	write("provider/putnami.extension.json", `{
  "name": "@putnami/test-provider",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {"executable": "runtime"},
  "workspace": {"markers": ["package.json"], "inputs": ["package.json"]}
}`, 0o644)
	write("provider/runtime", fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' '{"extension":"@putnami/test-provider","version":"1.0.0","platform":"%s/%s","cliContract":4,"runtimeProtocol":2,"runtimeABI":1}'
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' '{"version":1,"extension":"@putnami/test-provider","projects":[{"path":"app","sourceName":"@acme/app"}]}'
  exit 0
fi
exit 2
`, runtime.GOOS, runtime.GOARCH), 0o755)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	srv := newMCPServerWithContext(context.Background(), root, wsproto.Load(root), "test")
	if _, err := os.Stat(workspace.SnapshotPath(root)); !os.IsNotExist(err) {
		t.Fatalf("constructing the server eagerly created an index: %v", err)
	}

	var stdout bytes.Buffer
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`,
		"",
	}, "\n")
	if err := srv.Serve(context.Background(), strings.NewReader(requests), &stdout); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("stdout contains non-JSON-RPC preparation output: %q", stdout.String())
	}
	frames := make([]testJSONRPCEnvelope, len(lines))
	for i, line := range lines {
		frame := &frames[i]
		if err := json.Unmarshal(line, frame); err != nil {
			t.Fatalf("stdout frame is not JSON: %q: %v", line, err)
		}
		if frame.JSONRPC != "2.0" || frame.ID != i+1 || frame.Error != nil || len(frame.Result) == 0 {
			t.Fatalf("stdout frame is not a successful JSON-RPC envelope: %+v", frame)
		}
	}
	var toolResult extproto.ToolCallResult
	if err := json.Unmarshal(frames[1].Result, &toolResult); err != nil {
		t.Fatalf("decode list_projects result: %v", err)
	}
	if toolResult.IsError || len(toolResult.Content) != 1 || !strings.Contains(toolResult.Content[0].Text, "@acme/app") {
		t.Fatalf("cold graph tool did not answer from the provider index: %+v", toolResult)
	}
	snapshot, err := workspace.LoadSnapshot(root)
	if err != nil || snapshot == nil || len(snapshot.Providers) != 1 {
		t.Fatalf("provider index = %+v, err=%v", snapshot, err)
	}
	for _, forbidden := range []string{".putnami/install-state.json", "CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(root, forbidden)); !os.IsNotExist(err) {
			t.Errorf("minimal graph preparation wrote %s: %v", forbidden, err)
		}
	}
}

func TestColdMCPRetriesAfterExactProviderBecomesAvailable(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-initialization", "graph-readiness", "cold-mcp-prepares-the-provider-index-without-install")
	root, cfg := coldRegistryProviderWorkspace(t)
	srv := newMCPServerWithContext(context.Background(), root, cfg, "test")

	first := callMCPListProjects(t, srv)
	if !first.IsError ||
		!mcpToolResultContains(first, "exact workspace extension preparation is incomplete", "putnami install") {
		t.Fatalf("missing exact provider refusal is not actionable: %+v", first)
	}
	if _, err := os.Stat(workspace.SnapshotPath(root)); !os.IsNotExist(err) {
		t.Fatalf("failed exact preparation published a core-only index: %v", err)
	}

	writeLockedMCPProvider(t, root, 4)
	second := callMCPListProjects(t, srv)
	if second.IsError || !mcpToolResultContains(second, "@acme/app") {
		t.Fatalf("repaired exact provider did not succeed on retry: %+v", second)
	}
	snapshot, err := workspace.LoadSnapshot(root)
	if err != nil || snapshot == nil || len(snapshot.Providers) != 1 ||
		snapshot.Providers[0].Extension != "@putnami/test-provider" {
		t.Fatalf("retried provider index = %+v, err=%v", snapshot, err)
	}
}

func TestColdMCPRetriesAfterExactProviderDiscoveryIsRepaired(t *testing.T) {
	t.Parallel()
	root, cfg := coldRegistryProviderWorkspace(t)
	writeLockedMCPProvider(t, root, 999)
	srv := newMCPServerWithContext(context.Background(), root, cfg, "test")

	first := callMCPListProjects(t, srv)
	if !first.IsError || !mcpToolResultContains(first, "workspace provider discovery is incomplete") {
		t.Fatalf("skipped exact provider refusal is not actionable: %+v", first)
	}
	if _, err := os.Stat(workspace.SnapshotPath(root)); !os.IsNotExist(err) {
		t.Fatalf("incomplete provider discovery published a core-only index: %v", err)
	}

	writeLockedMCPProvider(t, root, 4)
	second := callMCPListProjects(t, srv)
	if second.IsError || !mcpToolResultContains(second, "@acme/app") {
		t.Fatalf("repaired provider discovery did not succeed on retry: %+v", second)
	}
}

func coldRegistryProviderWorkspace(t *testing.T) (string, *wsproto.Config) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(
		`{"name":"cold-registry","includes":["app"],"extensions":{"@putnami/test-provider":"stable"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "package.json"), []byte(`{"name":"@acme/app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root, wsproto.Load(root)
}

func writeLockedMCPProvider(t *testing.T, root string, cliContract int) {
	t.Helper()
	const (
		name    = "@putnami/test-provider"
		version = "1.0.0"
	)
	artifact := layout.ArtifactDir(root, layout.Extensions, name, version)
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "runtime": {"executable": "runtime"},
  "workspace": {"markers": ["package.json"], "inputs": ["package.json"]}
}`, name, version, cliContract)
	manifestPath := filepath.Join(artifact, "putnami.extension.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' '{"extension":"@putnami/test-provider","version":"1.0.0","platform":"%s/%s","cliContract":4,"runtimeProtocol":2,"runtimeABI":1}'
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' '{"version":1,"extension":"@putnami/test-provider","projects":[{"path":"app","sourceName":"@acme/app"}]}'
  exit 0
fi
exit 2
`, runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(artifact, "runtime"), []byte(runtimeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifact(root, layout.Extensions, name, version); err != nil {
		t.Fatal(err)
	}
	manifestHash, err := lockfile.HashFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{Version: version, ManifestHash: manifestHash})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
}

func callMCPListProjects(t *testing.T, srv interface {
	Serve(context.Context, io.Reader, io.Writer) error
}) extproto.ToolCallResult {
	t.Helper()
	var stdout bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}` + "\n"
	if err := srv.Serve(context.Background(), strings.NewReader(request), &stdout); err != nil {
		t.Fatal(err)
	}
	var frame testJSONRPCEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &frame); err != nil {
		t.Fatalf("decode JSON-RPC response: %v", err)
	}
	if frame.JSONRPC != "2.0" || frame.ID != 1 || frame.Error != nil || len(frame.Result) == 0 {
		t.Fatalf("list_projects response is not a successful JSON-RPC envelope: %+v", frame)
	}
	var result extproto.ToolCallResult
	if err := json.Unmarshal(frame.Result, &result); err != nil {
		t.Fatalf("decode list_projects result: %v", err)
	}
	return result
}

func mcpToolResultContains(result extproto.ToolCallResult, fragments ...string) bool {
	for _, fragment := range fragments {
		found := false
		for _, content := range result.Content {
			if strings.Contains(content.Text, fragment) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// A host starts the MCP server when an agent session starts, so the
// agent-workflow reconcile runs there before the first request is answered
// (ADR 0040). It is bounded, a failure is a warning on stderr rather than a
// refused session, and stdout carries nothing but the protocol.
func TestMCPServeReconcilesAgentWorkflowsBeforeServing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "session-start-reconcile", "the-mcp-server-reconciles-before-serving-and-starts-on-failure")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(`{"name":"session"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n")
	var out, stderr bytes.Buffer
	calls := 0
	reconcile := func(ctx context.Context, wsRoot string, _ *wsproto.Config) error {
		calls++
		if wsRoot != root {
			t.Errorf("reconciled %s, want the served workspace %s", wsRoot, root)
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Error("the session-start reconcile must run under a deadline")
		}
		if out.Len() != 0 {
			t.Errorf("the server answered before the reconcile ran: %q", out.String())
		}
		return errors.New("the locked version is not in the local store")
	}

	if err := serveMCPSession(context.Background(), root, wsproto.Load(root), "test", reconcile, in, &out, &stderr); err != nil {
		t.Fatalf("serveMCPSession: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reconcile calls = %d, want exactly one at session start", calls)
	}
	var response testJSONRPCEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil || response.ID != 1 || response.Error != nil {
		t.Fatalf("stdout is not one initialize response: %q (%v)", out.String(), err)
	}
	warning := stderr.String()
	if !strings.Contains(warning, "warning") || !strings.Contains(warning, "not in the local store") || !strings.Contains(warning, "putnami install") {
		t.Fatalf("stderr = %q, want a warning naming the failure and the remedy", warning)
	}
}
