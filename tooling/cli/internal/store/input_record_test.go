package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// The input record's digests (tooling/cli ADR 0113). These tests set the
// process's current record, so none of them runs in parallel.

// useRecordAt makes a record of the workspace at root current, with a clock an
// hour ahead so every part the test wrote is recordable, and returns it.
func useRecordAt(t *testing.T, root string) *fileindex.Record {
	t.Helper()
	record := fileindex.OpenRecordAt(fileindex.RecordPath(root), "test", time.Now().Add(time.Hour).UnixNano())
	restore := fileindex.UseRecord(record)
	t.Cleanup(restore)
	if fileindex.CurrentRecord() == nil {
		t.Skip("this platform or filesystem keeps no input record")
	}
	return record
}

func writeRecordTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestARecordedInputDigestIsAnsweredWithoutCollecting(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a", "src/b/c.go": "c", "README.md": "r"})
	patterns := []string{"src/**/*.go", "README.md"}

	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	next := useRecordAt(t, root)
	second, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if second != first || next.Answered() != 1 {
		t.Fatalf("the second run computed %s (answered %d); want the recorded %s answered once", second, next.Answered(), first)
	}

	// A file created where the walk listed moves its directory: collected.
	writeRecordTree(t, project, map[string]string{"src/b/d.go": "d"})
	third := useRecordAt(t, root)
	moved, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := hashFiles(project, patterns, nil, ProjectConfigScope{})
	if moved != fresh || moved == first || third.Answered() != 0 {
		t.Fatalf("after a file was created, the digest was %s (answered %d); want the fresh %s", moved, third.Answered(), fresh)
	}
}

func TestAWriteOfTheRunMovesARecordedInputDigest(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a"})
	patterns := []string{"src/**/*.go"}

	record := useRecordAt(t, root)
	if _, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	useRecordAt(t, root)
	cm := NewCacheManager(nil)
	if _, err := cm.HashFiles(project, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	writeRecordTree(t, project, map[string]string{"src/new.go": "n"})
	cm.InvalidateFileHashesUnder([]string{filepath.Join(project, "src", "new.go")})
	got, err := cm.HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := hashFiles(project, patterns, nil, ProjectConfigScope{}); got != want {
		t.Fatalf("after a write of the run the digest was %s; want %s", got, want)
	}
}

func TestADigestWrittenWithinTheMarginIsNotRecorded(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"a.txt": "a"})
	record := fileindex.OpenRecord(fileindex.RecordPath(root), "test")
	restore := fileindex.UseRecord(record)
	defer restore()
	if fileindex.CurrentRecord() == nil {
		t.Skip("this platform or filesystem keeps no input record")
	}
	if _, err := NewCacheManager(nil).HashFiles(project, []string{"*.txt"}, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fileindex.RecordPath(root)); !os.IsNotExist(err) {
		t.Fatalf("a digest of files written within the margin was recorded (stat: %v)", err)
	}
}

// recordPatternSets are the selections the randomized test keys on: walks,
// globs, literal names, exclusions, a missing prefix, and sets the record
// never answers (a glob before the last segment, a character class).
var recordPatternSets = [][]string{
	{"**/*.go"},
	{"src/**"},
	{"*.txt"},
	{"src/a/*.json"},
	{"docs/readme.md"},
	{"**/*.go", "!**/*_test.go"},
	{"missing/**/*.go"},
	{"src/*/x.txt"},
	{"src/[ab]/*.json"},
	{"src/a/deep/**/*.txt", "notes.txt"},
}

// TestTheInputRecordServesNoFalseHit writes the tree at random between runs
// and compares each digest a run answers or collects with a collection made
// with no record and no memo. A difference is a false hit. Git candidate
// patterns are not recorded yet (ADR 0113 §3), so the writes are of the work
// tree alone.
func TestTheInputRecordServesNoFalseHit(t *testing.T) {
	answered := int64(0)
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible test sequence
			root := t.TempDir()
			project := filepath.Join(root, "p")
			writeRecordTree(t, project, map[string]string{
				"src/a/x.go": "package a", "src/a/x_test.go": "package a", "src/a/c.json": "{}",
				"src/b/y.go": "package b", "src/b/x.txt": "x", "src/a/deep/z/w.txt": "w",
				"notes.txt": "n", "docs/readme.md": "# r", "top.go": "package top",
			})
			for round := range 25 {
				record := useRecordAt(t, root)
				cm := NewCacheManager(nil)
				for _, patterns := range recordPatternSets {
					got, err := cm.HashFiles(project, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					want, err := hashFiles(project, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Fatalf("round %d, patterns %v: the run read %s, a fresh collection %s (a false hit)\ntree:\n%s",
							round, patterns, got, want, listTree(t, project))
					}
				}
				answered += record.Answered()
				if err := record.Save(); err != nil {
					t.Fatal(err)
				}
				// Past the coarsest status-change tick, so a write lands at a
				// later time than any stat the record took.
				time.Sleep(15 * time.Millisecond)
				for range 1 + rng.Intn(3) {
					mutateTree(t, rng, project)
				}
			}
		})
	}
	if answered == 0 {
		t.Fatal("the record answered no digest: the test exercises nothing")
	}
}

// mutateTree makes one random write under project.
func mutateTree(t *testing.T, rng *rand.Rand, project string) {
	t.Helper()
	files, dirs := treeEntries(t, project)
	pick := func(list []string) string { return list[rng.Intn(len(list))] }
	newName := func() string {
		names := []string{"n.go", "n_test.go", "n.json", "n.txt", "x.txt", "readme.md", "w.txt"}
		return fmt.Sprintf("%d%s", rng.Intn(3), names[rng.Intn(len(names))])
	}
	switch op := rng.Intn(10); {
	case op == 0 && len(files) > 0: // remove a file
		must(t, os.Remove(pick(files)))
	case op == 1: // create a file
		must(t, os.WriteFile(filepath.Join(pick(dirs), newName()), []byte("new"), 0o644))
	case op == 2 && len(files) > 0: // rename a file within or across directories
		must(t, os.Rename(pick(files), filepath.Join(pick(dirs), newName())))
	case op == 3 && len(files) > 0: // same size, modification time set back
		file := pick(files)
		info, err := os.Stat(file)
		must(t, err)
		content, err := os.ReadFile(file)
		must(t, err)
		if len(content) > 0 {
			content[0] ^= 0x01
		}
		must(t, os.WriteFile(file, content, info.Mode()))
		must(t, os.Chtimes(file, info.ModTime(), info.ModTime()))
	case op == 4 && len(files) > 0: // change the mode
		must(t, os.Chmod(pick(files), 0o755))
	case op == 5: // create a directory, sometimes with a file
		dir := filepath.Join(pick(dirs), fmt.Sprintf("d%d", rng.Intn(3)))
		must(t, os.MkdirAll(dir, 0o755))
		if rng.Intn(2) == 0 {
			must(t, os.WriteFile(filepath.Join(dir, newName()), []byte("in"), 0o644))
		}
	case op == 6 && len(dirs) > 1: // remove a directory and what it holds
		if dir := pick(dirs[1:]); dir != project {
			must(t, os.RemoveAll(dir))
		}
	case op == 7 && len(files) > 0: // replace a file by a rename over it
		file := pick(files)
		temp := file + ".tmp"
		must(t, os.WriteFile(temp, []byte("replaced"), 0o644))
		must(t, os.Rename(temp, file))
	case op == 8: // a link, to a file or a directory
		target := pick(dirs)
		if len(files) > 0 && rng.Intn(2) == 0 {
			target = pick(files)
		}
		_ = os.Symlink(target, filepath.Join(pick(dirs), "l"+newName()))
	default: // create the missing prefix, or a file in it
		must(t, os.MkdirAll(filepath.Join(project, "missing", "m"), 0o755))
		must(t, os.WriteFile(filepath.Join(project, "missing", "m", newName()), []byte("m"), 0o644))
	}
}

func treeEntries(t *testing.T, project string) (files, dirs []string) {
	t.Helper()
	err := filepath.WalkDir(project, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, path)
		case d.Type().IsRegular():
			files = append(files, path)
		}
		return nil
	})
	must(t, err)
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs
}

func listTree(t *testing.T, project string) string {
	t.Helper()
	files, dirs := treeEntries(t, project)
	return strings.Join(append(dirs, files...), "\n")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
