package store

// Regressions of the first review of the input record (tooling/cli ADR 0113):
// each records a digest, changes the tree, and compares what the next run
// answers with a fresh collection.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	gitutil "go.putnami.dev/tooling/cli/internal/git"
)

// recordTwoRuns records the digest of patterns in one run, applies change, and
// compares what a second run answers with a fresh collection.
func recordTwoRuns(t *testing.T, root, project string, patterns []string, change func()) {
	t.Helper()
	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	change()
	next := useRecordAt(t, root)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := hashFiles(project, patterns, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

func TestTheInputRecordRegressionDanglingLinkTargetCreated(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.txt": "a"})
	must(t, os.MkdirAll(filepath.Join(root, "shared"), 0o755))
	must(t, os.Symlink(filepath.Join(root, "shared", "b.txt"), filepath.Join(project, "src", "b.txt")))
	recordTwoRuns(t, root, project, []string{"src/**"}, func() {
		writeRecordTree(t, root, map[string]string{"shared/b.txt": "b"})
	})
}

func TestTheInputRecordRegressionLinkChainRetargeted(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.txt": "a"})
	writeRecordTree(t, root, map[string]string{"shared/one/v.txt": "one", "shared/two/v.txt": "two"})
	// p/src/b.txt -> shared/cur/v.txt, shared/cur -> one
	must(t, os.Symlink("one", filepath.Join(root, "shared", "cur")))
	must(t, os.Symlink(filepath.Join(root, "shared", "cur", "v.txt"), filepath.Join(project, "src", "b.txt")))
	recordTwoRuns(t, root, project, []string{"src/**"}, func() {
		cur := filepath.Join(root, "shared", "cur")
		tmp := cur + ".tmp"
		must(t, os.Symlink("two", tmp))
		must(t, os.Rename(tmp, cur))
	})
}

func TestTheInputRecordRegressionLinkToDirectoryBecomesFile(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.txt": "a"})
	writeRecordTree(t, root, map[string]string{"shared/t/inner.txt": "i"})
	must(t, os.Symlink(filepath.Join(root, "shared", "t"), filepath.Join(project, "src", "t.txt")))
	recordTwoRuns(t, root, project, []string{"src/*.txt"}, func() {
		must(t, os.RemoveAll(filepath.Join(root, "shared", "t")))
		writeRecordTree(t, root, map[string]string{"shared/t": "now a file"})
	})
}

func TestTheInputRecordRegressionGlobParentLinkChain(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, root, map[string]string{"shared/one/v.txt": "one", "shared/two/w.txt": "two"})
	must(t, os.MkdirAll(project, 0o755))
	must(t, os.Symlink("one", filepath.Join(root, "shared", "cur")))
	// p/src -> shared/cur -> one
	must(t, os.Symlink(filepath.Join(root, "shared", "cur"), filepath.Join(project, "src")))
	recordTwoRuns(t, root, project, []string{"src/*.txt"}, func() {
		cur := filepath.Join(root, "shared", "cur")
		tmp := cur + ".tmp"
		must(t, os.Symlink("two", tmp))
		must(t, os.Rename(tmp, cur))
	})
}

func TestTheInputRecordRegressionGitLinkedProjectRetargeted(t *testing.T) {
	root := t.TempDir()
	writeRecordTree(t, root, map[string]string{
		"vendored/one/src/a.go": "package one", "vendored/two/src/b.go": "package two",
		".gitignore": ".putnami/\n",
	})
	must(t, os.MkdirAll(filepath.Join(root, "libs"), 0o755))
	must(t, os.Symlink(filepath.Join("..", "vendored", "one"), filepath.Join(root, "libs", "foo")))
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "init")
	time.Sleep(1100 * time.Millisecond)
	project := filepath.Join(root, "libs", "foo")
	patterns := []string{"git:src/**"}

	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	_, statErr := os.Stat(filepath.Join(root, ".putnami", "input-record"))
	t.Logf("recorded file: %v", statErr)
	time.Sleep(20 * time.Millisecond)
	link := filepath.Join(root, "libs", "foo")
	must(t, os.Symlink(filepath.Join("..", "vendored", "two"), link+".tmp"))
	must(t, os.Rename(link+".tmp", link))

	next := useRecordAt(t, root)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	want, err := hashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

func TestTheInputRecordRegressionCaseFoldedHeldStat(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"a.txt": "a"})
	upper, lower := []string{"Docs/**"}, []string{"docs/**"}
	record := useRecordAt(t, root)
	cm := NewCacheManager(nil)
	first, err := cm.HashFiles(project, upper, nil, ProjectConfigScope{})
	must(t, err)
	_, err = cm.HashFiles(project, lower, nil, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	writeRecordTree(t, project, map[string]string{"Docs/x.md": "x"})

	next := useRecordAt(t, root)
	cm = NewCacheManager(nil)
	_, err = cm.HashFiles(project, lower, nil, ProjectConfigScope{})
	must(t, err)
	got, err := cm.HashFiles(project, upper, nil, ProjectConfigScope{})
	must(t, err)
	want, _ := hashFiles(project, upper, nil, ProjectConfigScope{})
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

func TestTheInputRecordRegressionGitExclusionHidesIndex(t *testing.T) {
	root := t.TempDir()
	writeRecordTree(t, root, map[string]string{
		"a.go": "package a", "gen/pinned.go": "package gen", ".gitignore": ".putnami/\ngen/\n",
	})
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")
	runGit(t, root, "add", "-f", "gen/pinned.go")
	runGit(t, root, "commit", "-q", "-m", "init")
	time.Sleep(1100 * time.Millisecond)
	patterns := []string{"git:**", "!**/.git/**"}

	record := useRecordAt(t, root)
	cm := NewCacheManager(nil)
	cm.SetCandidateSource(inProcessCut(t, root))
	first, err := cm.HashFiles(root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	runGit(t, root, "rm", "-q", "--cached", "gen/pinned.go")

	next := useRecordAt(t, root)
	cm = NewCacheManager(nil)
	cm.SetCandidateSource(inProcessCut(t, root))
	got, err := cm.HashFiles(root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	want, err := hashFilesWith(inProcessCut(t, root), root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

func TestTheInputRecordRegressionGitExclusionControl(t *testing.T) {
	root := t.TempDir()
	writeRecordTree(t, root, map[string]string{
		"a.go": "package a", "gen/pinned.go": "package gen", ".gitignore": ".putnami/\ngen/\n",
	})
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")
	runGit(t, root, "add", "-f", "gen/pinned.go")
	runGit(t, root, "commit", "-q", "-m", "init")
	time.Sleep(1100 * time.Millisecond)
	patterns := []string{"git:**"}

	record := useRecordAt(t, root)
	cm := NewCacheManager(nil)
	cm.SetCandidateSource(inProcessCut(t, root))
	first, err := cm.HashFiles(root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	runGit(t, root, "rm", "-q", "--cached", "gen/pinned.go")

	next := useRecordAt(t, root)
	cm = NewCacheManager(nil)
	cm.SetCandidateSource(inProcessCut(t, root))
	got, err := cm.HashFiles(root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	want, err := hashFilesWith(inProcessCut(t, root), root, patterns, nil, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

type linkExpander struct{}

func (linkExpander) ExpandInput(dir, selector string) (InputExpansion, error) {
	return InputExpansion{Files: []string{"src/gen.go"}, Basis: wsproto.InputBasis{Listing: [][]string{{"src/**"}}}}, nil
}
func (linkExpander) Identity() string       { return "link" }
func (linkExpander) RecordIdentity() string { return "link\x00v1" }

func TestTheInputRecordRegressionSelectorLinkedFile(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, root, map[string]string{"shared/gen.go": "package one"})
	must(t, os.MkdirAll(filepath.Join(project, "src"), 0o755))
	must(t, os.Symlink(filepath.Join(root, "shared", "gen.go"), filepath.Join(project, "src", "gen.go")))
	patterns := []string{"go:sources"}
	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	must(t, os.WriteFile(filepath.Join(root, "shared", "gen.go"), []byte("package two"), 0o644))
	next := useRecordAt(t, root)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	want, err := hashFiles(project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("a false hit: the record answered %s, a fresh collection gives %s", got, want)
	}
}

func TestTheInputRecordRegressionSelectorLinkedFileRecorded(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, root, map[string]string{"shared/gen.go": "package one"})
	must(t, os.MkdirAll(filepath.Join(project, "src"), 0o755))
	must(t, os.Symlink(filepath.Join(root, "shared", "gen.go"), filepath.Join(project, "src", "gen.go")))
	patterns := []string{"go:sources"}
	record := useRecordAt(t, root)
	_, err := NewCacheManager(nil).HashFiles(project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	key, ok := recordKey("x", patterns, linkExpander{})
	t.Logf("recordKey ok=%v %q", ok, key)
	_, witness, err := witnessedHashFiles(nil, project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	t.Logf("witness=%+v record=%+v", witness != nil, witness.record)
	must(t, record.Save())
	next := useRecordAt(t, root)
	_, err = NewCacheManager(nil).HashFiles(project, patterns, linkExpander{}, ProjectConfigScope{})
	must(t, err)
	t.Logf("answered unchanged=%d", next.Answered())
}

func TestTheInputRecordRegressionFallbackCase(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a", "README.md": "r", "x.json": "{}"})
	runGit(t, project, "init", "-q")
	paths, witness, err := gitutil.CandidatePathsWitnessed(project)
	t.Logf("immediately: paths=%d witness=%d err=%v", len(paths), len(witness), err)
	time.Sleep(1100 * time.Millisecond)
	paths, witness, err = gitutil.CandidatePathsWitnessed(project)
	t.Logf("after 1.1s: paths=%d witness=%d err=%v", len(paths), len(witness), err)
	paths, witness, err = gitutil.CandidatePathsWitnessed(project)
	t.Logf("again: paths=%d witness=%d err=%v", len(paths), len(witness), err)
}

func probeGitPrefix(t *testing.T, patterns []string, setup map[string]string, change func(root string)) {
	root := t.TempDir()
	writeRecordTree(t, root, setup)
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "init")
	time.Sleep(1100 * time.Millisecond)
	project := filepath.Join(root, "p")
	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	change(root)
	next := useRecordAt(t, root)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	want, err := hashFiles(project, patterns, nil, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("FALSE HIT")
	}
}

func TestTheInputRecordRegressionGitMissingPrefixCreated(t *testing.T) {
	probeGitPrefix(t, []string{"git:missing/deep/**"}, map[string]string{"p/a.go": "a", ".gitignore": ".putnami/\n"}, func(root string) {
		writeRecordTree(t, root, map[string]string{"p/missing/deep/x.go": "x"})
	})
}

func TestTheInputRecordRegressionGitPrefixNestedGitignoreAbove(t *testing.T) {
	// A .gitignore in a directory above the prefix, created after the record.
	probeGitPrefix(t, []string{"git:src/**"}, map[string]string{"p/src/a.go": "a", "p/src/u.txt": "u", ".gitignore": ".putnami/\n"}, func(root string) {
		writeRecordTree(t, root, map[string]string{"p/.gitignore": "*.txt\n"})
	})
}

func TestTheInputRecordRegressionGitPrefixUntrackedIgnoredDirAbove(t *testing.T) {
	probeGitPrefix(t, []string{"git:src/**"}, map[string]string{"p/src/a.go": "a", ".gitignore": ".putnami/\n"}, func(root string) {
		writeRecordTree(t, root, map[string]string{"p/src/new.txt": "n"})
	})
}

func TestTheInputRecordRegressionSelectorBasisUnderExcludedDirectory(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "package a", "src/gen/g.go": "package gen"})
	patterns := []string{"go:sources", "!**/gen/**"}
	expander := basisExpander{identity: "ext"}
	record := useRecordAt(t, root)
	first, err := NewCacheManager(nil).HashFiles(project, patterns, expander, ProjectConfigScope{})
	must(t, err)
	must(t, record.Save())
	time.Sleep(20 * time.Millisecond)
	writeRecordTree(t, project, map[string]string{"src/gen/new.go": "package gen"})
	next := useRecordAt(t, root)
	got, err := NewCacheManager(nil).HashFiles(project, patterns, expander, ProjectConfigScope{})
	must(t, err)
	want, err := hashFiles(project, patterns, expander, ProjectConfigScope{})
	must(t, err)
	t.Logf("first=%s answered=%d got=%s fresh=%s", first[:12], next.Answered(), got[:12], want[:12])
	if got != want {
		t.Errorf("FALSE HIT")
	}
}
