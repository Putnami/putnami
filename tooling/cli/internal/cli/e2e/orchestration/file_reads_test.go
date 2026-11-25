package orchestration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// TestAnUnchangedRunReadsNoFileContent holds that a run of the unchanged
// workspace answers every digest from the file index, and that a rewrite of
// the same size, with its modification time set back, is read by the next
// run. The rewrite moves the status-change time; the same-tick rule itself is
// held by the fileindex tests on a coarse clock.
func TestAnUnchangedRunReadsNoFileContent(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	root := clitest.CountsFixture(t, 3, false)

	// The first run reads every file the fixture just wrote: they are not a
	// tick older than its clock. Once they are, the next run records them.
	cold := fileindex.ContentReads()
	clitest.RunCounts(t, root, "lint,test,build", "--all")
	if fileindex.ContentReads() == cold {
		t.Fatal("the first run read no file content: the count counts nothing")
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	clitest.RunCounts(t, root, "lint,test,build", "--all")
	time.Sleep(fileindex.Margin + 100*time.Millisecond)

	before := fileindex.ContentReads()
	unchanged := clitest.RunCounts(t, root, "lint,test,build", "--all")
	if reads := fileindex.ContentReads() - before; reads != 0 {
		t.Errorf("a run of the unchanged workspace read %d file contents, want 0\n%s", reads, countsHint)
	}
	if unchanged.Engine["job"] != 0 {
		t.Fatalf("a run of the unchanged workspace executed %d tasks, want 0", unchanged.Engine["job"])
	}

	// Same size, same modification time: the status-change time moved.
	source := filepath.Join(root, "apps", "p001", "src.txt")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("q001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed := clitest.RunCounts(t, root, "lint,test,build", "--all")
	if changed.Engine["job"] != 3 {
		t.Fatalf("after a same-size rewrite of one project's source, the run executed %d tasks, want 3", changed.Engine["job"])
	}
}
