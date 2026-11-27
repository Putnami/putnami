package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// settledTree writes a tree of Go files and waits past the file index's
// margin, so a witness of it can be recorded.
func settledTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if !fileindex.TrustsStats(root) {
		t.Skip("the temporary directory's file system is not one the index trusts")
	}
	for _, file := range []string{"a/x.go", "a/b/y.go", "c/z.go", "c/readme.md"} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	return root
}

func TestARecursiveGlobInputIsAnsweredByItsWitnessUntilAPathMoves(t *testing.T) {
	root := settledTree(t)
	first := globInput(root, "**/*.go")
	walks := globWalks.Load()
	if again := globInput(root, "**/*.go"); again.Digest != first.Digest {
		t.Fatalf("an unchanged tree changed the digest: %s against %s", again.Digest, first.Digest)
	}
	if globWalks.Load() != walks {
		t.Fatal("an unchanged tree was walked again despite a holding witness")
	}

	for _, change := range []struct {
		name  string
		write func()
	}{
		{"a file rewritten with its size kept", func() {
			_ = os.WriteFile(filepath.Join(root, "a", "x.go"), []byte("a/X.go"), 0o644)
		}},
		{"a file created in a subdirectory", func() {
			_ = os.WriteFile(filepath.Join(root, "a", "b", "new.go"), []byte("new"), 0o644)
		}},
		{"a directory created", func() {
			_ = os.MkdirAll(filepath.Join(root, "d", "e"), 0o755)
			_ = os.WriteFile(filepath.Join(root, "d", "e", "w.go"), []byte("w"), 0o644)
		}},
		{"a file removed", func() { _ = os.Remove(filepath.Join(root, "c", "z.go")) }},
	} {
		before := globInput(root, "**/*.go")
		change.write()
		after := globInput(root, "**/*.go")
		if after.Digest == before.Digest {
			t.Errorf("%s: the digest did not move", change.name)
		}
		// A fresh collection agrees with what the witness answered.
		globWitnesses.Delete(root + "\x00**/*.go")
		if fresh := globInput(root, "**/*.go"); fresh.Digest != after.Digest {
			t.Errorf("%s: the witnessed answer %s differs from a fresh walk %s", change.name, after.Digest, fresh.Digest)
		}
	}
}

func TestAFileUnderAnUnwatchedPatternDoesNotMoveTheGoDigest(t *testing.T) {
	root := settledTree(t)
	first := globInput(root, "**/*.go")
	// A file the pattern does not select, written in a walked directory,
	// moves the directory's stat: the input is collected again, and keeps its
	// digest.
	if err := os.WriteFile(filepath.Join(root, "c", "notes.md"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	walks := globWalks.Load()
	if again := globInput(root, "**/*.go"); again.Digest != first.Digest {
		t.Errorf("a file the pattern does not select moved the digest")
	}
	if globWalks.Load() == walks {
		t.Error("a moved directory stat did not collect the input again")
	}
}
