package git

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A git started by name runs the macOS trampoline again. Tests are left
// out: a split package may not import this one.
var directGitCall = regexp.MustCompile(`exec\.Command(Context)?\(([A-Za-z_][A-Za-z0-9_]*, )?"git"`)

// startedByName lists the files that start git by name on purpose, with how
// many times.
var startedByName = map[string]int{
	// git worktree add runs repository hooks and filters, which keep the
	// environment the macOS trampoline gives them.
	"internal/commands/cachecmd/cache_verify.go": 1,
}

func TestTheModuleRunsGitThroughExecutable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if n := len(directGitCall.FindAll(data, -1)); n != startedByName[rel] {
			found = append(found, fmt.Sprintf("%s: %d by name, %d allowed", rel, n, startedByName[rel]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("start git as exec.Command(gitread.Executable(), ...), not by name:\n  %s", strings.Join(found, "\n  "))
	}
}
