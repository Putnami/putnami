package inputresolve

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// openRunIndex opens the file index a run of the workspace at root reads
// through, as the engine does, and returns it with the function that saves it
// and restores the previous index.
func openRunIndex(t *testing.T, root string) (*fileindex.Index, func()) {
	t.Helper()
	index := fileindex.Open(fileindex.Path(root), "inputresolve-test")
	restore := fileindex.Use(index)
	return index, func() {
		t.Helper()
		restore()
		if err := index.Save(); err != nil {
			t.Fatal(err)
		}
	}
}

// A run that reads a recorded answer digests its basis through the file
// index: once the basis files are recorded, an unchanged run reads none of
// them, and a rewrite that keeps the size and the modification time is read
// and asked again.
func TestAnUnchangedRunDigestsTheBasisWithoutReadingIt(t *testing.T) {
	storeRoot := t.TempDir()
	root, app := selectorWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)

	index, done := openRunIndex(t, root)
	first, log := recordingResolver(t, storeRoot, root, "digest-a")
	resolveFiles(t, first, query(app, "src/**"))
	done()
	if index.Reads() == 0 {
		t.Fatal("the first run read no basis file: the count counts nothing")
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("the first run asked %d times, want 1", len(got))
	}

	index, done = openRunIndex(t, root)
	before := fileindex.ContentReads()
	second, log := recordingResolver(t, storeRoot, root, "digest-a")
	if got := resolveFiles(t, second, query(app, "src/**")); !slices.Equal(got, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("recorded files = %v", got)
	}
	second.PrimeInputs(nil)
	if _, err := second.ResolveInput(query(app, "src/**")); err != nil {
		t.Fatal(err)
	}
	done()
	if reads := fileindex.ContentReads() - before; reads != 0 {
		t.Fatalf("an unchanged run read %d basis files, want 0", reads)
	}
	if index.Hits() == 0 {
		t.Fatal("an unchanged run answered no digest from the index")
	}
	if got := requests(t, log); len(got) != 0 {
		t.Fatalf("an unchanged run asked %d times, want 0", len(got))
	}

	// Same size, same modification time: the status-change time moved.
	source := filepath.Join(app, "a.txt")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, source, "Z")
	if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, done = openRunIndex(t, root)
	before = fileindex.ContentReads()
	third, log := recordingResolver(t, storeRoot, root, "digest-a")
	resolveFiles(t, third, query(app, "src/**"))
	done()
	if fileindex.ContentReads() == before {
		t.Fatal("a run after a same-size rewrite read no basis file")
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("a run after a same-size rewrite asked %d times, want 1", len(got))
	}
}

// The CLI digests a basis only through the file index: a call to the
// protocol's reading digest would read every basis file on every run, which
// no count of the index's reads can see.
func TestTheCLIDigestsNoBasisWithoutTheFileIndex(t *testing.T) {
	internalDir := filepath.Join("..", "..")
	err := filepath.WalkDir(internalDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "wsproto.InputBasisDigest(") || strings.Contains(string(data), "InputBasisDigestFrom(dir, basis, nil)") {
			t.Errorf("%s digests an input basis by reading every file; use basisDigest", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
