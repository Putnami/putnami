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
	{"go:sources", "*.txt"},
	{"**/*.go", "!**/gen/**"},
	{"go:sources", "!**/gen/**"},
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
				"src/a/gen/g.go": "package gen",
			})
			writeRecordTree(t, root, map[string]string{"shared/a/s.txt": "s"})
			for round := range 25 {
				record := useRecordAt(t, root)
				cm := NewCacheManager(nil)
				expander := basisExpander{identity: "ext"}
				for _, patterns := range recordPatternSets {
					got, err := cm.HashFiles(project, patterns, expander, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					want, err := hashFiles(project, patterns, expander, ProjectConfigScope{})
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
	// A random write that does not apply, such as a directory created where
	// a file now stands, is skipped: the tree changes all the same.
	skip := func(error) {}
	files, dirs := treeEntries(t, project)
	pick := func(list []string) string { return list[rng.Intn(len(list))] }
	newName := func() string {
		names := []string{"n.go", "n_test.go", "n.json", "n.txt", "x.txt", "readme.md", "w.txt"}
		return fmt.Sprintf("%d%s", rng.Intn(3), names[rng.Intn(len(names))])
	}
	shared := filepath.Join(filepath.Dir(project), "shared")
	if filepath.Base(project) == "shared" {
		shared = filepath.Join(filepath.Dir(project), "elsewhere")
	}
	switch op := rng.Intn(14); {
	case op == 11: // a link to a path that may not exist yet, outside the tree
		_ = os.Symlink(filepath.Join(shared, fmt.Sprintf("t%d", rng.Intn(3))), filepath.Join(pick(dirs), "dl"+newName()))
	case op == 12: // create, retarget or replace what such links lead to
		target := filepath.Join(shared, fmt.Sprintf("t%d", rng.Intn(3)))
		switch rng.Intn(4) {
		case 0:
			_ = os.RemoveAll(target)
			skip(os.MkdirAll(filepath.Dir(target), 0o755))
			skip(os.WriteFile(target, []byte(fmt.Sprint("file", rng.Intn(9))), 0o644))
		case 1:
			_ = os.RemoveAll(target)
			skip(os.MkdirAll(filepath.Join(target, "in"), 0o755))
		case 2: // a link chain: the target is itself a link, retargeted
			_ = os.RemoveAll(target)
			skip(os.MkdirAll(shared, 0o755))
			skip(os.Symlink(fmt.Sprintf("t%d", rng.Intn(3)), target))
		default:
			_ = os.RemoveAll(target)
		}
	case op == 13 && len(dirs) > 1: // a directory replaced by a file of its name
		dir := pick(dirs[1:])
		skip(os.RemoveAll(dir))
		skip(os.WriteFile(dir, []byte("was a directory"), 0o644))
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
			skip(os.Remove(link))
			target := pick(dirs)
			if len(files) > 0 && rng.Intn(2) == 0 {
				target = pick(files)
			}
			skip(os.Symlink(target, link))
		}
	case op == 0 && len(files) > 0: // remove a file
		skip(os.Remove(pick(files)))
	case op == 1: // create a file
		skip(os.WriteFile(filepath.Join(pick(dirs), newName()), []byte("new"), 0o644))
	case op == 2 && len(files) > 0: // rename a file within or across directories
		skip(os.Rename(pick(files), filepath.Join(pick(dirs), newName())))
	case op == 3 && len(files) > 0: // same size, modification time set back
		file := pick(files)
		info, err := os.Stat(file)
		skip(err)
		content, err := os.ReadFile(file)
		skip(err)
		if len(content) > 0 {
			content[0] ^= 0x01
		}
		skip(os.WriteFile(file, content, info.Mode()))
		skip(os.Chtimes(file, info.ModTime(), info.ModTime()))
	case op == 4 && len(files) > 0: // change the mode
		skip(os.Chmod(pick(files), 0o755))
	case op == 5: // create a directory, sometimes with a file
		dir := filepath.Join(pick(dirs), fmt.Sprintf("d%d", rng.Intn(3)))
		skip(os.MkdirAll(dir, 0o755))
		if rng.Intn(2) == 0 {
			skip(os.WriteFile(filepath.Join(dir, newName()), []byte("in"), 0o644))
		}
	case op == 6 && len(dirs) > 1: // remove a directory and what it holds
		if dir := pick(dirs[1:]); dir != project {
			skip(os.RemoveAll(dir))
		}
	case op == 7 && len(files) > 0: // replace a file by a rename over it
		file := pick(files)
		temp := file + ".tmp"
		skip(os.WriteFile(temp, []byte("replaced"), 0o644))
		skip(os.Rename(temp, file))
	case op == 8: // a link, to a file or a directory
		target := pick(dirs)
		if len(files) > 0 && rng.Intn(2) == 0 {
			target = pick(files)
		}
		_ = os.Symlink(target, filepath.Join(pick(dirs), "l"+newName()))
	default: // create the missing prefix, or a file in it
		// A directory replaced by a file may stand in the way: then nothing.
		if os.MkdirAll(filepath.Join(project, "missing", "m"), 0o755) == nil {
			skip(os.WriteFile(filepath.Join(project, "missing", "m", newName()), []byte("m"), 0o644))
		}
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

// tryGit runs git in dir, and ignores a refusal: a random step may not apply.
func tryGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	_ = cmd.Run()
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
	{"git:a/**"},
	{"git:**/*.go", "c/**/*.txt", "*.txt"},
	{"git:**", "!**/.git/**"},
}

// TestTheInputRecordServesNoFalseHitOfGitCandidates is the randomized test of
// digests of Git candidates read in-process: it stages, unstages and removes
// paths, rewrites ignore rules and the index, and writes the work tree between
// runs (ADR 0113 §3).
func TestTheInputRecordServesNoFalseHitOfGitCandidates(t *testing.T) {
	answered := int64(0)
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible test sequence
			root := t.TempDir()
			writeRecordTree(t, root, map[string]string{
				"a/x.go": "package a", "a/n.txt": "n", "b/y.go": "package b", ".gitignore": ".putnami/\nignored/\n*.log\n",
				"ignored/z.go": "package z", "c/d/w.go": "package w", "top.txt": "t",
			})
			runGit(t, root, "init", "-q", "-b", "master")
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
	switch rng.Intn(13) {
	case 11: // switch to another branch with other files, and back
		branch := fmt.Sprintf("b%d", rng.Intn(2))
		tryGit(t, root, "checkout", "-q", "-f", "-B", branch)
		writeRecordTree(t, root, map[string]string{"a/" + branch + ".go": "package a"})
		tryGit(t, root, "add", "-A")
		tryGit(t, root, "commit", "-q", "--allow-empty", "-m", branch)
		if rng.Intn(2) == 0 {
			tryGit(t, root, "checkout", "-q", "-f", "master")
		}
		// The reader keeps no configuration read within a second of a change
		// to HEAD, and without one the cut has no witness.
		time.Sleep(1100 * time.Millisecond)
	case 12: // a configured excludes file, rewritten
		excludes := filepath.Join(filepath.Dir(root), "excludes")
		must(t, os.WriteFile(excludes, []byte([]string{"*.txt\n", "a/\n", ""}[rng.Intn(3)]), 0o644))
		tryGit(t, root, "config", "core.excludesFile", excludes)
		time.Sleep(1100 * time.Millisecond)
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

// TestASampledAnswerThatDiffersDiscardsTheRecord plants a wrong value in the
// record under a witness that holds, as an unknown false-hit class would: the
// run checks the answer, returns the collected digest, discards the record,
// and its save leaves no entry (ADR 0113 §1).
func TestASampledAnswerThatDiffersDiscardsTheRecord(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a"})
	patterns := []string{"src/**/*.go"}

	record := useRecordAt(t, root)
	want, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := recordKey(project+"|"+strings.Join(patterns, ",")+"||", patterns, nil)
	_, parts, ok := record.Answer(InputDigestTable, key, record.Hold)
	if !ok {
		t.Fatal("the digest was not recorded")
	}
	if !record.Put(InputDigestTable, key, []byte("planted\x00"+fmt.Sprint(1)+"\x00memoized"), parts) {
		t.Fatal("the planted value was refused")
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	next := useRecordAt(t, root)
	next.SetSampleRate(1)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("the run answered %s; want the collected %s", got, want)
	}
	if next.Discarded() == "" || !strings.Contains(next.Discarded(), "planted") {
		t.Fatalf("the record was not discarded (%q)", next.Discarded())
	}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	after := useRecordAt(t, root)
	if _, _, ok := after.Answer(InputDigestTable, key, after.Hold); ok {
		t.Fatal("a discarded record kept an entry")
	}
}

// TestAWriteInsideTheMarginIsNotAnsweredLater runs the racy rule end to end
// with the filesystem's clock (ADR 0113 §2): a digest of files written just
// now is collected but not recorded, so a rewrite in the same tick of the
// clock, which can keep the file's stat, is read by the next run.
func TestAWriteInsideTheMarginIsNotAnsweredLater(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a"})
	patterns := []string{"src/**/*.go"}
	record := fileindex.OpenRecord(fileindex.RecordPath(root), "test")
	restore := fileindex.UseRecord(record)
	defer restore()
	if fileindex.CurrentRecord() == nil {
		t.Skip("this platform or filesystem keeps no input record")
	}
	if _, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	key, _ := recordKey(project+"|"+strings.Join(patterns, ",")+"||", patterns, nil)
	saved := fileindex.OpenRecord(fileindex.RecordPath(root), "test")
	if _, _, ok := saved.Answer(InputDigestTable, key, func([]fileindex.RecordPart) bool { return true }); ok || record.Computed() == 0 {
		t.Fatalf("a digest of files written inside the margin was recorded (offered %d)", record.Computed())
	}
	info, err := os.Stat(filepath.Join(project, "src", "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	writeRecordTree(t, project, map[string]string{"src/a.go": "b"})
	must(t, os.Chtimes(filepath.Join(project, "src", "a.go"), info.ModTime(), info.ModTime()))

	next := fileindex.OpenRecord(fileindex.RecordPath(root), "test")
	restoreNext := fileindex.UseRecord(next)
	defer restoreNext()
	got, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hashFiles(project, patterns, nil, ProjectConfigScope{})
	if got != want || next.Answered() != 0 {
		t.Fatalf("a digest of files written inside the margin was answered (%d): %s, want %s", next.Answered(), got, want)
	}
}
