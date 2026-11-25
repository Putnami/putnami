package workspaceclient

import (
	"os"
	"path/filepath"
	"testing"

	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
)

// A guard whose context carries the workspace's cut reads it without Git: a
// file the cut lists is read, and one it does not list is absent.
func TestTheCommittedViewReadsTheCutFromTheContextWithoutGit(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{"provider/putnami.json": "{}\n", "provider/.gen/out.json": "{}\n"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	ctx := &pctx.Context{WorkspaceRoot: root, Tree: &job.Tree{Directory: ".", CandidateDirectories: []string{"."},
		CandidateFiles: []job.CandidateFile{{Path: "provider/putnami.json", Mode: job.CandidateModeRegular}}}}
	view, err := committedViewFrom(ctx, root, []string{"provider"})
	if err != nil || view.tree == nil {
		t.Fatalf("committedViewFrom = %+v, %v", view, err)
	}
	if _, err := view.Read("provider/putnami.json"); err != nil {
		t.Errorf("read a candidate: %v", err)
	}
	if _, err := view.Read("provider/.gen/out.json"); err == nil {
		t.Error("read a file outside the cut")
	}
}
