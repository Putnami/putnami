package sharedtest

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// UnindexedWorkspace writes a workspace with one member, "web", that has a
// package.json and no putnami.json, and no workspace index. Loading it resolves
// "web" from authored config alone, so the load records the provider-view
// warning. It returns the workspace root.
func UnindexedWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["web"]}`)
	writeFixture(t, filepath.Join(root, "web", "package.json"), `{"name":"@acme/web"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// RecordWorkspaceIndex writes the workspace index of an UnindexedWorkspace, with
// a provider view that names "web" as "@acme/web".
func RecordWorkspaceIndex(t *testing.T, root string) {
	t.Helper()
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: "@fixture/lang",
		Projects:  []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web"}},
	}
	wsproto.NormalizeProbeResult(&result)
	snapshot := workspace.NewSnapshotWithProviders(ws, ws.ProbeDigest(), []workspace.SnapshotProvider{{
		Extension: result.Extension,
		Digest:    wsproto.ProbeResultDigest(result),
		Result:    result,
	}})
	if err := workspace.WriteSnapshot(root, snapshot, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("write workspace index: %v", err)
	}
	workspace.InvalidateLoadCache(root)
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
