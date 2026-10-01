package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pack lists a project's assistant docs by the names Putnami writes and
// reads: AGENTS.md carries the guidance block, and the singular AGENT.md is not
// a convention the pack follows.
func TestDocCandidatePathsListsAgentsNotAgent(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"README.md", "AI.md", "AGENTS.md", "AGENT.md", "CLAUDE.md", "doc/b.md", "doc/a.md", "docs/c.md", "notes.md"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# "+rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := docCandidatePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "README.md,AI.md,AGENTS.md,CLAUDE.md,doc/a.md,doc/b.md,docs/c.md"
	if strings.Join(got, ",") != want {
		t.Fatalf("doc candidates = %v, want %s", got, want)
	}
}
