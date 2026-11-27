package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// A machine worker forgets the loaded workspace before each run
// (ForgetProcessMemos): a configuration edited between two runs moves no
// index, and the next run loads what is on disk (CLI ADR 0116 §4).
func TestForgetProcessMemosDropsTheLoadedWorkspace(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "putnami.workspace.json")
	if err := os.WriteFile(config, []byte(`{"name":"first"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateLoadCache(dir)
	t.Cleanup(func() { InvalidateLoadCache(dir) })
	if ws, err := Load(dir); err != nil || ws.Name != "first" {
		t.Fatalf("Load = %v, %v; want the workspace named first", ws, err)
	}
	if err := os.WriteFile(config, []byte(`{"name":"second"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ForgetProcessMemos()
	ws, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Name != "second" {
		t.Errorf("the next run loaded the workspace named %q, want the edited configuration's %q", ws.Name, "second")
	}
}
