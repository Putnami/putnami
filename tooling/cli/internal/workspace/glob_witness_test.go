package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
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

// forgetProcess drops what this process holds of root's witnesses, as a new
// CLI process starts without them.
func forgetProcess(root string) {
	globWitnesses.Range(func(key, _ any) bool {
		if name, _ := key.(string); len(name) > len(root) && name[:len(root)] == root {
			globWitnesses.Delete(key)
		}
		return true
	})
	witnessFiles.Delete(root)
}

func TestANewProcessAnswersAnUnchangedGlobInputFromTheWitnessFile(t *testing.T) {
	if fileindex.ExecutableIdentity() == "" {
		t.Skip("this test binary has no program identity")
	}
	root := settledTree(t)
	first := globInput(root, "**/*.go")
	SaveGlobWitnesses(root, SnapshotWritePolicy{})
	if _, err := os.Stat(filepath.Join(root, ".putnami", GlobWitnessFilename)); err != nil {
		t.Fatalf("no witness file: %v", err)
	}

	forgetProcess(root)
	walks := globWalks.Load()
	if again := globInput(root, "**/*.go"); again.Digest != first.Digest {
		t.Fatalf("the digest moved across processes: %s against %s", again.Digest, first.Digest)
	}
	if globWalks.Load() != walks {
		t.Fatal("a new process walked an unchanged tree its witness file holds")
	}
	// A run that only confirmed its witnesses writes nothing.
	info, _ := os.Stat(filepath.Join(root, ".putnami", GlobWitnessFilename))
	SaveGlobWitnesses(root, SnapshotWritePolicy{})
	if after, _ := os.Stat(filepath.Join(root, ".putnami", GlobWitnessFilename)); !after.ModTime().Equal(info.ModTime()) {
		t.Error("a run that only confirmed its witnesses rewrote the file")
	}

	// A change in a new process is seen through the file's witness.
	if err := os.WriteFile(filepath.Join(root, "c", "z.go"), []byte("c/Z.go"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgetProcess(root)
	if changed := globInput(root, "**/*.go"); changed.Digest == first.Digest {
		t.Error("a new process answered a changed tree from the witness file")
	}
}

func TestAWitnessFileOfAnotherProgramIsNotRead(t *testing.T) {
	root := settledTree(t)
	_ = globInput(root, "**/*.go")
	SaveGlobWitnesses(root, SnapshotWritePolicy{})
	path := filepath.Join(root, ".putnami", GlobWitnessFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("no witness file was written")
	}
	other := strings.Replace(string(data), `"program":"`, `"program":"another build `, 1)
	if err := os.WriteFile(path, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	forgetProcess(root)
	walks := globWalks.Load()
	_ = globInput(root, "**/*.go")
	if globWalks.Load() == walks {
		t.Error("a witness file another program wrote answered an input")
	}
}

func TestPlanAndDryRunWriteNoWitnessFile(t *testing.T) {
	root := settledTree(t)
	_ = globInput(root, "**/*.go")
	SaveGlobWitnesses(root, SnapshotWritePolicy{Plan: true})
	if _, err := os.Stat(filepath.Join(root, ".putnami", GlobWitnessFilename)); err == nil {
		t.Error("a --plan run wrote the witness file")
	}
}

// heldFor reports whether this process holds a witness of pattern in root.
func heldFor(root, pattern string) bool {
	_, held := globWitnesses.Load(root + "\x00" + pattern)
	return held
}

func TestAPrefixThatIsMissingOrNotADirectoryIsNotHeld(t *testing.T) {
	root := settledTree(t)
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"later/**/*.go", "f/**/*.go", "a/x.go/**/*.go"} {
		empty := globInput(root, pattern)
		if heldFor(root, pattern) {
			t.Errorf("%s: a walk that listed no search root was held", pattern)
		}
		dir := globSearchRoot(root, pattern)
		_ = os.Remove(dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if after := globInput(root, pattern); after.Digest == empty.Digest {
			t.Errorf("%s: a file under a prefix that came back did not move the digest", pattern)
		}
	}
}

func TestAWalkWhoseDirectoryMovedDuringItIsNotHeld(t *testing.T) {
	root := settledTree(t)
	swap := filepath.Join(root, "a", ".x.go.swp")
	globWalkHook = func(dir string) {
		if dir == filepath.Join(root, "a") {
			_ = os.WriteFile(swap, []byte("swap"), 0o644)
			_ = os.Remove(swap)
		}
	}
	t.Cleanup(func() { globWalkHook = nil })
	_ = globInput(root, "**/*.go")
	if heldFor(root, "**/*.go") {
		t.Error("a walk whose directory moved during it was held")
	}
}

func TestASymlinkedMatchIsNotHeld(t *testing.T) {
	root := settledTree(t)
	if err := os.Symlink(filepath.Join(root, "a", "x.go"), filepath.Join(root, "c", "link.go")); err != nil {
		t.Skip("no symlinks here")
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	_ = globInput(root, "**/*.go")
	if heldFor(root, "**/*.go") {
		t.Error("a match through a symlink was held: its stat does not answer for its target")
	}
}

// rewriteWitnessFile applies edit to root's witness file.
func rewriteWitnessFile(t *testing.T, root string, edit func(*persistedWitnesses)) {
	t.Helper()
	path := filepath.Join(root, ".putnami", GlobWitnessFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("no witness file was written")
	}
	var file persistedWitnesses
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	edit(&file)
	if data, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAWitnessFileThatCannotHoldItsInputsIsNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*persistedWitnesses)
	}{
		{"a stamp ahead of the clock", func(f *persistedWitnesses) {
			f.Stamp = time.Now().Add(time.Hour).UnixNano()
		}},
		{"an input without a directory", func(f *persistedWitnesses) {
			for i := range f.Inputs {
				var files []int
				for _, p := range f.Inputs[i].Paths {
					if f.Paths[p].Stat.Mode&modeType != modeDirectory {
						files = append(files, p)
					}
				}
				f.Inputs[i].Paths = files
			}
		}},
		{"an index out of range", func(f *persistedWitnesses) {
			for i := range f.Inputs {
				f.Inputs[i].Paths = append(f.Inputs[i].Paths, len(f.Paths))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := settledTree(t)
			_ = globInput(root, "**/*.go")
			SaveGlobWitnesses(root, SnapshotWritePolicy{})
			rewriteWitnessFile(t, root, tc.edit)
			forgetProcess(root)
			walks := globWalks.Load()
			_ = globInput(root, "**/*.go")
			if globWalks.Load() == walks {
				t.Error("the witness file answered an input")
			}
		})
	}
}

func TestTheWitnessFileIsTheSameForTheSameWitnesses(t *testing.T) {
	root := settledTree(t)
	for _, pattern := range []string{"**/*.go", "**/*.md", "a/**/*.go"} {
		_ = globInput(root, pattern)
	}
	path := filepath.Join(root, ".putnami", GlobWitnessFilename)
	var files [2]persistedWitnesses
	for i := range files {
		markGlobWitnessesDirty(root)
		SaveGlobWitnesses(root, SnapshotWritePolicy{})
		data, err := os.ReadFile(path)
		if err != nil {
			t.Skip("no witness file was written")
		}
		if err := json.Unmarshal(data, &files[i]); err != nil {
			t.Fatal(err)
		}
		files[i].Stamp = 0
	}
	first, _ := json.Marshal(files[0])
	second, _ := json.Marshal(files[1])
	if string(first) != string(second) {
		t.Errorf("two saves of the same witnesses differ:\n%s\n%s", first, second)
	}
}
