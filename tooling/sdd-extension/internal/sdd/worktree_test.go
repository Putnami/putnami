package sdd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// candidateWorkspace is a fixture workspace that is also a Git work tree, so
// the steps read its candidate cut: the tracked files and the untracked files
// no ignore rule excludes.
func candidateWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := decisionsWorkspace(t)
	gitIn(t, root, "init", "-q")
	return root
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func symlinkIn(t *testing.T, root, target, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links are not available: %v", err)
		}
		t.Fatal(err)
	}
}

func removeIn(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// cutFingerprint digests the candidate cut as the task input `git:**` keys
// it: two equal fingerprints are one cache key.
func cutFingerprint(t *testing.T, root string) string {
	t.Helper()
	tree, err := gitcandidate.Open(root)
	if err != nil || tree == nil {
		t.Fatalf("open the candidate cut: %v, %v", tree, err)
	}
	fingerprint, err := tree.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

// cutStep is one change to a fixture work tree, whether it moves the `git:**`
// key, and the verdict the step reads after it.
type cutStep struct {
	name    string
	apply   func()
	moves   bool
	verdict string
}

// assertVerdictFollowsTheKey applies each step and checks the two halves of a
// sound cache key: the key moves exactly when the step says it does, and a
// verdict never changes while the key stays put, which is what a replayed
// entry would get wrong.
func assertVerdictFollowsTheKey(t *testing.T, root string, verdict func() string, initial string, steps []cutStep) {
	t.Helper()
	key, got := cutFingerprint(t, root), verdict()
	if got != initial {
		t.Fatalf("initial verdict = %q, want %q", got, initial)
	}
	for _, step := range steps {
		step.apply()
		nextKey, next := cutFingerprint(t, root), verdict()
		if moved := nextKey != key; moved != step.moves {
			t.Errorf("%s: key moved = %v, want %v", step.name, moved, step.moves)
		}
		if nextKey == key && next != got {
			t.Errorf("%s: verdict moved from %q to %q while the key stayed put", step.name, got, next)
		}
		if next != step.verdict {
			t.Errorf("%s: verdict = %q, want %q", step.name, next, step.verdict)
		}
		key, got = nextKey, next
	}
}

func TestWorktreeReadsOnlyTheCandidateCut(t *testing.T) {
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "ignored/\n*.log\n")
	writeFixtureFile(t, filepath.Join(root, "doc", "guide.md"), "# guide\n")
	writeFixtureFile(t, filepath.Join(root, "doc", "debug.log"), "noise\n")
	writeFixtureFile(t, filepath.Join(root, "ignored", "secret.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, "node_modules", "pkg", "decisions.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, ".hidden", "decisions.json"), "{}")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkIn(t, root, "doc/guide.md", "link.md")
	symlinkIn(t, root, "doc", "doc-link")
	symlinkIn(t, root, "ignored/secret.json", "to-ignored.json")
	symlinkIn(t, root, "../outside.md", "escape.md")
	tree, err := openWorktree(root)
	if err != nil || tree.tree == nil {
		t.Fatalf("openWorktree = %+v, %v; want the candidate cut of a Git work tree", tree, err)
	}

	if data, err := tree.readRegular("doc/guide.md"); err != nil || string(data) != "# guide\n" {
		t.Errorf("readRegular(doc/guide.md) = %q, %v", data, err)
	}
	for _, rel := range []string{"doc/debug.log", "ignored/secret.json", "missing.md"} {
		if _, err := tree.readRegular(rel); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("readRegular(%s) error = %v, want fs.ErrNotExist: it is not a candidate", rel, err)
		}
	}
	if _, err := tree.readRegular("link.md"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readRegular(link.md) error = %v, want a refusal of the symbolic link", err)
	}

	for rel, want := range map[string]bool{"doc/guide.md": true, "link.md": false, "doc": false, "doc/debug.log": false} {
		if got := tree.isRegular(rel); got != want {
			t.Errorf("isRegular(%s) = %v, want %v", rel, got, want)
		}
	}
	for rel, want := range map[string]bool{"doc": true, "empty": false, "ignored": false, "doc-link": false, "doc/guide.md": false} {
		if got := tree.isDir(rel); got != want {
			t.Errorf("isDir(%s) = %v, want %v", rel, got, want)
		}
	}

	if data, err := tree.readFollowing("link.md"); err != nil || string(data) != "# guide\n" {
		t.Errorf("readFollowing(link.md) = %q, %v", data, err)
	}
	if data, err := tree.readFollowing("doc-link/guide.md"); err != nil || string(data) != "# guide\n" {
		t.Errorf("readFollowing(doc-link/guide.md) = %q, %v", data, err)
	}
	if _, err := tree.readFollowing("to-ignored.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readFollowing(to-ignored.json) error = %v, want fs.ErrNotExist", err)
	}
	if _, err := tree.readFollowing("escape.md"); !errors.Is(err, errLeavesWorkspace) {
		t.Errorf("readFollowing(escape.md) error = %v, want errLeavesWorkspace", err)
	}
	if _, err := tree.readFollowing("doc-link"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readFollowing(doc-link) error = %v, want a refusal of the directory", err)
	}

	for rel, want := range map[string]struct {
		exists bool
		fails  bool
	}{
		"link.md":         {exists: true},
		"doc/guide.md":    {exists: true},
		"to-ignored.json": {},
		"doc/debug.log":   {},
		"escape.md":       {fails: true},
		"doc":             {fails: true},
	} {
		exists, err := tree.fileExists(rel)
		if exists != want.exists || (err != nil) != want.fails {
			t.Errorf("fileExists(%s) = %v, %v; want %v, failure %v", rel, exists, err, want.exists, want.fails)
		}
	}

	want := []string{".gitignore", "doc/guide.md"}
	if got := tree.candidateRegularFiles(excludedDecisionDirectory); !reflect.DeepEqual(got, want) {
		t.Errorf("candidateRegularFiles = %v, want %v: regular candidates outside excluded directories", got, want)
	}
}

func TestWorktreeOutsideAGitWorkTreeReadsTheDisk(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := decisionsWorkspace(t)
	if err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Run(); err == nil {
		t.Skip("the temporary directory lies inside a Git work tree")
	}
	writeFixtureFile(t, filepath.Join(root, "doc", "guide.md"), "# guide\n")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree, err := openWorktree(root)
	if err != nil || tree.tree != nil {
		t.Fatalf("openWorktree = %+v, %v; want the disk outside a Git work tree", tree, err)
	}
	if !tree.isDir("empty") || !tree.isRegular("doc/guide.md") {
		t.Error("the disk reader does not see the directory and the file the disk holds")
	}
	if exists, err := tree.fileExists("doc/guide.md"); !exists || err != nil {
		t.Errorf("fileExists(doc/guide.md) = %v, %v", exists, err)
	}
	if exists, err := tree.fileExists("missing.md"); exists || err != nil {
		t.Errorf("fileExists(missing.md) = %v, %v", exists, err)
	}
	if _, err := tree.fileExists("doc"); err == nil {
		t.Error("fileExists(doc) accepted a directory")
	}
}

// TestRecipesVerdictFollowsTheGitKey proves recipes-validate's key: a sample
// directory exists when it holds a candidate, so emptying or renaming it moves
// both the key and the verdict, and an ignored file moves neither.
func TestRecipesVerdictFollowsTheGitKey(t *testing.T) {
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "*.log\n")
	writeFixtureFile(t, filepath.Join(root, "go", "samples", RecipeIndexFilename), migrationRecipeIndex)
	sample := filepath.Join(root, "go", "samples", "migrations-feature")
	writeFixtureFile(t, filepath.Join(sample, "main.go"), "package main\n")
	verdict := func() string {
		report, err := BuildRecipesResult(fixtureWorkspace("w", root))
		if err == nil {
			return "ok"
		}
		codes := make([]string, 0, len(report.Diagnostics))
		for _, finding := range report.Diagnostics {
			codes = append(codes, finding.Code)
		}
		return strings.Join(codes, ",")
	}
	missing := ErrorCodeRecipeSampleMissing
	assertVerdictFollowsTheKey(t, root, verdict, "ok", []cutStep{
		{"an ignored file appears in the sample", func() { writeFixtureFile(t, filepath.Join(sample, "debug.log"), "x") }, false, "ok"},
		{"the cut is staged", func() { gitIn(t, root, "add", "-A") }, false, "ok"},
		{"the sample keeps only an ignored file", func() { removeIn(t, root, "go/samples/migrations-feature/main.go") }, true, missing},
		{"the sample gets its file back", func() { writeFixtureFile(t, filepath.Join(sample, "main.go"), "package main\n") }, true, "ok"},
		{"the sample is renamed", func() {
			if err := os.Rename(sample, filepath.Join(root, "go", "samples", "migrations")); err != nil {
				t.Fatal(err)
			}
		}, true, missing},
	})
}

// TestDecisionsVerdictFollowsTheGitKey proves decisions-validate's key: a
// file a check's glob matches is read when it is a candidate, so adding or
// deleting one moves both the key and the verdict, and an ignored one moves
// neither.
func TestDecisionsVerdictFollowsTheGitKey(t *testing.T) {
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "local/\n")
	committedDecisions(t, root, scaleToZeroRegistry)
	writeFixtureFile(t, filepath.Join(root, "services", "api", "infra", "requirements.json"),
		`{"protocolVersion":2,"scaling":{"minInstances":0}}`)
	verdict := func() string {
		report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
		if err == nil {
			return fmt.Sprintf("ok, %d checked", report.Summary.FilesChecked)
		}
		paths := make([]string, 0, len(report.Findings))
		for _, finding := range report.Findings {
			paths = append(paths, finding.Decision+" "+finding.Path)
		}
		return strings.Join(paths, ",")
	}
	violating := `{"protocolVersion":2,"scaling":{"minInstances":1}}`
	assertVerdictFollowsTheKey(t, root, verdict, "ok, 1 checked", []cutStep{
		{"an ignored matched file violates", func() {
			writeFixtureFile(t, filepath.Join(root, "local", "infra", "requirements.json"), violating)
		}, false, "ok, 1 checked"},
		{"a candidate matched file violates", func() {
			writeFixtureFile(t, filepath.Join(root, "sites", "site", "infra", "requirements.json"), violating)
		}, true, "D-001 sites/site/infra/requirements.json"},
		{"the cut is staged", func() { gitIn(t, root, "add", "-A") }, false, "D-001 sites/site/infra/requirements.json"},
		{"the violating file is deleted", func() { removeIn(t, root, "sites") }, true, "ok, 1 checked"},
	})
}

// TestDecisionsADRResolvesInTheCandidateCut keeps an adr link to an ignored
// file dangling, as it is in a clone, and lets the ignore rule's removal,
// which moves the key, resolve it.
func TestDecisionsADRResolvesInTheCandidateCut(t *testing.T) {
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "doc/\n")
	committedDecisions(t, root, `{"protocolVersion":1,"decisions":[
		{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x",
		 "adr":"doc/adr/0001-a.md","reviewOnly":true}]}`)
	writeFixtureFile(t, filepath.Join(root, "doc", "adr", "0001-a.md"), "# ADR\n")
	verdict := func() string {
		if _, err := BuildDecisionsResult(fixtureWorkspace("w", root)); err != nil {
			return "dangling"
		}
		return "ok"
	}
	assertVerdictFollowsTheKey(t, root, verdict, "dangling", []cutStep{
		{"the ignore rule goes", func() { writeFixtureFile(t, filepath.Join(root, ".gitignore"), "") }, true, "ok"},
	})
}

// TestCodeownersVerdictFollowsTheGitKey proves codeowners-sync's key. A run
// that rewrites .github/CODEOWNERS moves the key, so the CLI never replays
// it; the next run leaves the cut as it found it, and that is the run a later
// one replays. An ignored putnami.json declares no owners.
func TestCodeownersVerdictFollowsTheGitKey(t *testing.T) {
	root, ws := codeownersWorkspace(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitIn(t, root, "init", "-q")
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "go/putnami.json\n")
	writeFixtureFile(t, filepath.Join(root, "go", "putnami.json"),
		`{"options": {"sdd": {"owners": ["@acme/go"]}}}`)

	run := func() (string, bool) {
		t.Helper()
		report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
		if err != nil {
			t.Fatalf("build: %v (%+v)", err, report.Diagnostics)
		}
		rules := make([]string, 0, len(report.Rules))
		for _, rule := range report.Rules {
			rules = append(rules, rule.Pattern+" "+strings.Join(rule.Owners, " "))
		}
		return strings.Join(rules, "; "), report.Written
	}

	before := cutFingerprint(t, root)
	rules, written := run()
	if !written || rules != "* @lead" {
		t.Fatalf("first run = %q, written %v; want the catch-all rule written, without the ignored scope's owners", rules, written)
	}
	if after := cutFingerprint(t, root); after == before {
		t.Fatal("writing CODEOWNERS left the key in place, so the CLI would replay a run that rewrote it")
	}

	before = cutFingerprint(t, root)
	if again, written := run(); written || again != rules {
		t.Fatalf("second run = %q, written %v; want the same rules and no write", again, written)
	}
	if after := cutFingerprint(t, root); after != before {
		t.Fatal("a run that wrote nothing moved the key")
	}

	writeFixtureFile(t, filepath.Join(root, "go", "putnami.json"), `{"options": {"sdd": {"owners": ["@acme/other"]}}}`)
	if after := cutFingerprint(t, root); after != before {
		t.Fatal("editing an ignored putnami.json moved the key")
	}
	if again, written := run(); written || again != rules {
		t.Fatalf("after an ignored edit: %q, written %v; want the same rules and no write", again, written)
	}

	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "")
	if after := cutFingerprint(t, root); after == before {
		t.Fatal("removing the ignore rule left the key in place")
	}
	if rules, written := run(); !written || rules != "* @lead; /go/ @acme/other" {
		t.Fatalf("after the ignore rule went: %q, written %v; want the scope rule written", rules, written)
	}
	if !strings.Contains(readCodeowners(t, root), "/go/ @acme/other") {
		t.Fatalf("CODEOWNERS = %q, want the scope rule", readCodeowners(t, root))
	}
}

// TestCodeownersSpecFloorCountsOnlyInTheCut keeps the spec-governance rules
// to a committed floor: an ignored specs.baseline.json is not one.
func TestCodeownersSpecFloorCountsOnlyInTheCut(t *testing.T) {
	root, ws := codeownersWorkspace(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitIn(t, root, "init", "-q")
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), featureproto.SpecsBaselineFilename+"\n")
	writeFixtureFile(t, filepath.Join(root, featureproto.SpecsBaselineFilename), `{}`)
	report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err != nil {
		t.Fatalf("build: %v (%+v)", err, report.Diagnostics)
	}
	if strings.Contains(readCodeowners(t, root), codeownersGovernanceComment) {
		t.Fatal("an ignored spec floor governed the spec files")
	}
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "")
	if _, err := BuildCodeownersResult(ws, ownersOptions("@lead")); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(readCodeowners(t, root), codeownersGovernanceComment) {
		t.Fatal("a candidate spec floor did not govern the spec files")
	}
}

// TestDocsLinksVerdictFollowsTheGitKey proves docs-links-validate's key: a
// link target exists when it is a candidate, so deleting it, or ignoring it,
// moves both the key and the verdict, and an ignored file moves neither.
func TestDocsLinksVerdictFollowsTheGitKey(t *testing.T) {
	root := docsWorkspace(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitIn(t, root, "init", "-q")
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "*.log\n")
	ws := fixtureWorkspace("w", root, appProject("lib", "tooling/lib"))
	verdict := func() string {
		report, _, err := BuildDocsLinksResult(ws, nil)
		if err == nil {
			return fmt.Sprintf("ok, %d documents", report.Documents)
		}
		files := make([]string, 0, len(report.Findings))
		for _, finding := range report.Findings {
			files = append(files, finding.File)
		}
		return strings.Join(files, ",")
	}
	guide := filepath.Join(root, "tooling", "doc", "guide.md")
	guideText := "# Guide\n\n## Set up\n\n[lib](../lib/README.md#usage)\n"
	assertVerdictFollowsTheKey(t, root, verdict, "ok, 3 documents", []cutStep{
		{"an ignored file appears", func() { writeFixtureFile(t, filepath.Join(root, "tooling", "doc", "debug.log"), "x") }, false, "ok, 3 documents"},
		{"the cut is staged", func() { gitIn(t, root, "add", "-A") }, false, "ok, 3 documents"},
		{"a link target is deleted", func() { removeIn(t, root, "tooling/doc/guide.md") }, true, "README.md,tooling/lib/README.md"},
		// A tracked file stays a candidate whatever the ignore rules say, so
		// the deletion is staged before the target comes back ignored.
		{"the deletion is staged", func() { gitIn(t, root, "add", "-A") }, false, "README.md,tooling/lib/README.md"},
		{"the target comes back ignored", func() {
			writeFixtureFile(t, filepath.Join(root, ".gitignore"), "*.log\ntooling/doc/guide.md\n")
			writeFixtureFile(t, guide, guideText)
		}, true, "README.md,tooling/lib/README.md"},
		{"the target is a candidate again", func() { writeFixtureFile(t, filepath.Join(root, ".gitignore"), "*.log\n") }, true, "ok, 3 documents"},
		{"a heading an anchor names changes", func() {
			writeFixtureFile(t, guide, strings.Replace(guideText, "## Set up", "## Setup", 1))
		}, true, "README.md"},
	})
}
