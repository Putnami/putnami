package store

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
	gitutil "go.putnami.dev/tooling/cli/internal/git"
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
	{"../shared/**/*.txt"},
	{"src/*/l*"},
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
			writeRecordTree(t, root, map[string]string{"shared/a/s.txt": "s"})
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
					// A write above the project reaches its "../" patterns.
					if rng.Intn(4) == 0 {
						mutateTree(t, rng, filepath.Join(root, "shared"))
					} else {
						mutateTree(t, rng, project)
					}
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
	switch op := rng.Intn(11); {
	case op == 10: // point a link somewhere else
		var links []string
		_ = filepath.WalkDir(project, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Type()&os.ModeSymlink != 0 {
				links = append(links, path)
			}
			return nil
		})
		if len(links) > 0 {
			link := pick(links)
			must(t, os.Remove(link))
			target := pick(dirs)
			if len(files) > 0 && rng.Intn(2) == 0 {
				target = pick(files)
			}
			must(t, os.Symlink(target, link))
		}
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

// git runs git in dir, skipping the test where git cannot run.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git %v: %v %s", args, err, out)
	}
}

// inProcessCut is a candidate source that reads the cut of root in-process
// with its witness, as the run's tree facts do.
func inProcessCut(t *testing.T, root string) CandidateSource {
	t.Helper()
	files, witness, err := gitutil.CandidateFilesWitnessed(root)
	if err != nil {
		t.Fatal(err)
	}
	cut := CandidateCut{Modes: map[string]string{}, Unmerged: map[string]bool{}, Witness: witness}
	for _, file := range files {
		cut.Paths = append(cut.Paths, file.Path)
		switch file.Mode {
		case "untracked":
		case "unmerged":
			cut.Unmerged[file.Path] = true
		default:
			cut.Modes[file.Path] = file.Mode
		}
	}
	return func(dir string) (CandidateCut, bool) { return cut, filepath.Clean(dir) == filepath.Clean(root) }
}

var gitRecordPatternSets = [][]string{
	{"git:**"},
	{"git:**/*.go"},
	{"git:**", "!git:**/*.txt"},
}

// TestTheInputRecordServesNoFalseHitOfGitCandidates is the randomized test of
// digests of Git candidates read in-process: it stages, unstages and removes
// paths, rewrites ignore rules and the index, and writes the work tree between
// runs (ADR 0113 §3).
func TestTheInputRecordServesNoFalseHitOfGitCandidates(t *testing.T) {
	answered := int64(0)
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible test sequence
			root := t.TempDir()
			writeRecordTree(t, root, map[string]string{
				"a/x.go": "package a", "a/n.txt": "n", "b/y.go": "package b", ".gitignore": ".putnami/\nignored/\n*.log\n",
				"ignored/z.go": "package z", "c/d/w.go": "package w", "top.txt": "t",
			})
			runGit(t, root, "init", "-q")
			runGit(t, root, "add", "-A")
			runGit(t, root, "commit", "-q", "-m", "init")
			// The reader keeps a configuration read more than a second after
			// its files were written, and only a kept one gives a witness.
			time.Sleep(1100 * time.Millisecond)
			for round := range 20 {
				record := useRecordAt(t, root)
				cm := NewCacheManager(nil)
				source := inProcessCut(t, root)
				cm.SetCandidateSource(source)
				for _, patterns := range gitRecordPatternSets {
					// A nested repository is an error of the collection, with
					// or without the record: both must fail alike.
					got, gotErr := cm.HashFiles(root, patterns, nil, ProjectConfigScope{})
					want, wantErr := hashFilesWith(inProcessCut(t, root), root, patterns, nil, ProjectConfigScope{})
					if (gotErr == nil) != (wantErr == nil) {
						t.Fatalf("round %d, patterns %v: the run answered %s, %v; a fresh collection %s, %v", round, patterns, got, gotErr, want, wantErr)
					}
					if got != want {
						t.Fatalf("round %d, patterns %v: the run read %s, a fresh collection %s (a false hit)\ntree:\n%s",
							round, patterns, got, want, listTree(t, root))
					}
				}
				answered += record.Answered()
				if err := record.Save(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(15 * time.Millisecond)
				for range 1 + rng.Intn(2) {
					mutateGitTree(t, rng, root)
				}
			}
		})
	}
	if answered == 0 {
		t.Fatal("the record answered no digest of Git candidates: the test exercises nothing")
	}
}

// mutateGitTree makes one random change to the repository at root.
func mutateGitTree(t *testing.T, rng *rand.Rand, root string) {
	t.Helper()
	dirs := []string{"a", "b", "c/d", "ignored", "e"}
	dir := dirs[rng.Intn(len(dirs))]
	name := fmt.Sprintf("f%d.%s", rng.Intn(3), []string{"go", "txt", "log"}[rng.Intn(3)])
	path := filepath.Join(root, dir, name)
	switch rng.Intn(11) {
	case 0: // an untracked file
		writeRecordTree(t, root, map[string]string{dir + "/" + name: "new"})
	case 1: // stage everything
		runGit(t, root, "add", "-A")
	case 2: // remove a tracked path from the index only
		runGit(t, root, "rm", "-q", "--cached", "-r", "--ignore-unmatch", dir)
	case 3: // delete from the work tree
		_ = os.RemoveAll(path)
	case 4: // ignore a directory, or stop ignoring them
		// The workspace's .putnami, where the record lives, stays ignored, as
		// a workspace ignores it.
		rules := []string{".putnami/\nignored/\n*.log\n", ".putnami/\nignored/\n*.log\nb/\n", ".putnami/\n*.log\n", ".putnami/\n"}
		writeRecordTree(t, root, map[string]string{".gitignore": rules[rng.Intn(len(rules))]})
	case 5: // a nested .gitignore
		writeRecordTree(t, root, map[string]string{dir + "/.gitignore": "*.go\n"})
	case 6: // the repository's own excludes
		writeRecordTree(t, root, map[string]string{".git/info/exclude": []string{"*.txt\n", ""}[rng.Intn(2)]})
	case 7: // a nested repository
		nested := filepath.Join(root, "e", "nested")
		writeRecordTree(t, nested, map[string]string{"n.go": "package n"})
		runGit(t, nested, "init", "-q")
		runGit(t, nested, "add", "-A")
		runGit(t, nested, "commit", "-q", "--allow-empty", "-m", "nested")
	case 8: // commit
		runGit(t, root, "add", "-A")
		runGit(t, root, "commit", "-q", "--allow-empty", "-m", "c")
	case 9: // same size, modification time set back
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			content, _ := os.ReadFile(path)
			if len(content) > 0 {
				content[0] ^= 0x01
			}
			must(t, os.WriteFile(path, content, info.Mode()))
			must(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
		}
	default: // the exec bit, which the index records
		if _, err := os.Stat(path); err == nil {
			must(t, os.Chmod(path, 0o755))
			runGit(t, root, "add", "-A")
		}
	}
}
