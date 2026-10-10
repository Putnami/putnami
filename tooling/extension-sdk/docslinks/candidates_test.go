package docslinks

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gitcandidate"
)

// workTree creates an empty Git work tree.
func workTree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func makeSymlink(t *testing.T, root, target, rel string) {
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

func remove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, root, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(from)), filepath.Join(root, filepath.FromSlash(to))); err != nil {
		t.Fatal(err)
	}
}

// fingerprint is what a `git:**` input of root keys: the candidate paths, and
// the bytes or link text of each.
func fingerprint(t *testing.T, root string) string {
	t.Helper()
	tree, err := gitcandidate.Open(root)
	if err != nil || tree == nil {
		t.Fatalf("open the candidate cut: %v, %v", tree, err)
	}
	value, err := tree.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCheckInAWorkTreeReadsTheCandidateCut(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "*.local.md\nbuild/\n")
	writeFile(t, root, "lib/doc/guide.md", "# Guide\n\n## Usage\n")
	writeFile(t, root, "lib/doc/new.md", "# New\n")
	writeFile(t, root, "lib/doc/notes.local.md", "# Notes\n\n## Private\n")
	writeFile(t, root, "lib/build/out.md", "# Out\n")
	writeFile(t, root, "lib/doc/gone.md", "# Gone\n")
	if err := os.MkdirAll(filepath.Join(root, "lib", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeSymlink(t, root, "doc/notes.local.md", "lib/ignored-link.md")
	makeSymlink(t, root, "doc/guide.md", "lib/guide-link.md")
	readme := writeFile(t, root, "lib/README.md", strings.Join([]string{
		"[tracked](doc/guide.md#usage)",
		"[untracked](doc/new.md)",
		"[directory](doc)",
		"[ignored](doc/notes.local.md)",
		"[ignored directory](build/out.md)",
		"[empty](empty)",
		"[deleted](doc/gone.md)",
		"[case](doc/Guide.md)",
		"[link](guide-link.md#usage)",
		"[link to ignored](ignored-link.md)",
	}, "\n"))
	runGit(t, root, "add", ".gitignore", "lib/doc/guide.md", "lib/doc/gone.md")
	remove(t, root, "lib/doc/gone.md")

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lib/README.md:4:11 doc/notes.local.md",
		"lib/README.md:5:21 build/out.md",
		"lib/README.md:6:9 empty",
		"lib/README.md:7:11 doc/gone.md",
		"lib/README.md:8:8 doc/Guide.md",
		"lib/README.md:10:19 ignored-link.md",
	}
	if got := messages(t, root, findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q\nwant %q", got, want)
	}
}

func TestCheckInAWorkTreeRefusesALinkThatLeavesThroughASymlink(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "# Secret\n")
	root := workTree(t)
	makeSymlink(t, root, filepath.Join(outside, "secret.md"), "lib/absolute.md")
	makeSymlink(t, root, "../../outside.md", "lib/relative.md")
	readme := writeFile(t, root, "lib/README.md", "[a](absolute.md)\n[r](relative.md)\n")
	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want both links refused", findings)
	}
	for _, finding := range findings {
		if !strings.Contains(finding.Message, "leaves the workspace") {
			t.Errorf("finding %q does not say the link leaves the workspace", finding.Message)
		}
	}
}

func TestCheckInAWorkTreeFailsOnADocumentOutsideTheCut(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "ignored.md\n")
	ignored := writeFile(t, root, "ignored.md", "# x\n")
	if _, err := Check(root, []string{ignored}); err == nil {
		t.Fatal("Check read a document that is not a candidate")
	}
	if _, err := Check(root, []string{filepath.Join(t.TempDir(), "outside.md")}); err == nil {
		t.Fatal("Check read a document outside the workspace")
	}
}

func TestProjectDocumentsInAWorkTreeReadTheCandidateCut(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "ignored/\n*.local.md\n")
	for _, rel := range []string{
		"README.md",
		"doc/guide.md",
		"doc/notes.local.md",
		"ignored/README.md",
		"nested/putnami.json",
		"nested/README.md",
		"module/go.mod",
		"module/README.md",
		"testdata/README.md",
		"_build/README.md",
		"src/README.md",
	} {
		writeFile(t, root, rel, "# x\n")
	}
	makeSymlink(t, root, "guide.md", "doc/link.md")
	files, err := ProjectDocuments(root)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(files))
	for _, file := range files {
		rel, _ := filepath.Rel(root, file)
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"README.md", "doc/guide.md", "doc/link.md", "src/README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documents = %q, want %q", got, want)
	}
}

func TestWorkspaceDocumentsInAWorkTreeGroupsTheCandidateCut(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "*.local.md\n")
	for _, rel := range []string{
		"README.md",
		"notes.local.md",
		"scope/putnami.json",
		"scope/doc/page.md",
		"lib/README.md",
		"lib/doc/guide.md",
		"lib/doc/draft.local.md",
		"lib/tool/go.mod",
		"lib/tool/README.md",
		"node_modules/pkg/README.md",
		"_hidden/app/README.md",
		"_hidden/app/doc/page.md",
	} {
		writeFile(t, root, rel, "# x\n")
	}
	lib := filepath.Join(root, "lib")
	hidden := filepath.Join(root, "_hidden", "app")
	grouped, err := WorkspaceDocuments(root, []string{lib, hidden, filepath.Join(root, "absent"), t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]string, len(grouped))
	for owner, files := range grouped {
		key, _ := filepath.Rel(root, owner)
		for _, file := range files {
			rel, _ := filepath.Rel(root, file)
			got[filepath.ToSlash(key)] = append(got[filepath.ToSlash(key)], filepath.ToSlash(rel))
		}
	}
	want := map[string][]string{
		".":           {"README.md", "scope/doc/page.md"},
		"lib":         {"lib/README.md", "lib/doc/guide.md", "lib/tool/README.md"},
		"_hidden/app": {"_hidden/app/README.md", "_hidden/app/doc/page.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documents = %q, want %q", got, want)
	}
}

func TestCheckProjectInAWorkTree(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, "lib/README.md", "[gone](gone.md) [root](../README.md)\n")
	writeFile(t, root, "README.md", "# Root\n")
	findings, err := CheckProject(root, filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if got := messages(t, root, findings); !reflect.DeepEqual(got, []string{"lib/README.md:1:8 gone.md"}) {
		t.Fatalf("findings = %q", got)
	}
	if _, err := CheckProject(root, t.TempDir()); err == nil {
		t.Fatal("CheckProject read a project outside the workspace")
	}
}

// TestTheVerdictMovesOnlyWithTheGitKey holds the cache contract of lint-docs
// and docs-links-validate: both are keyed on `git:**`, so their verdict must
// be a function of the candidate cut. Each step below changes the worktree;
// a step that changes the findings must change what the key reads, and a step
// outside the cut must change neither.
func TestTheVerdictMovesOnlyWithTheGitKey(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "*.local.md\nbuild/\n")
	writeFile(t, root, "lib/doc/guide.md", "# Guide\n\n## Usage\n")
	writeFile(t, root, "lib/doc/other.md", "# Other\n")
	writeFile(t, root, "lib/README.md", strings.Join([]string{
		"[guide](doc/guide.md#usage)",
		"[other](doc/other.md)",
		"[draft](doc/draft.local.md)",
		"[output](build/out.md)",
		"[later](doc/later.md)",
	}, "\n"))
	runGit(t, root, "add", "-A")

	verdict := func() string {
		t.Helper()
		findings, err := CheckProject(root, filepath.Join(root, "lib"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(messages(t, root, findings), "\n")
	}
	beforeVerdict, beforeKey := verdict(), fingerprint(t, root)
	for _, step := range []struct {
		name  string
		apply func()
		moves bool
	}{
		{"an ignored file appears at a link target", func() { writeFile(t, root, "lib/doc/draft.local.md", "# Draft\n") }, false},
		{"an ignored directory fills a link target", func() { writeFile(t, root, "lib/build/out.md", "# Out\n") }, false},
		{"an untracked file appears at a link target", func() { writeFile(t, root, "lib/doc/later.md", "# Later\n") }, true},
		{"a link target is renamed to another case", func() { rename(t, root, "lib/doc/later.md", "lib/doc/Later.md") }, true},
		{"the link target gets its case back", func() { rename(t, root, "lib/doc/Later.md", "lib/doc/later.md") }, true},
		{"the cut is staged", func() { runGit(t, root, "add", "-A") }, false},
		{"a link target is deleted", func() { remove(t, root, "lib/doc/other.md") }, true},
		{"the deletion is staged", func() { runGit(t, root, "add", "-A") }, false},
		{"a heading an anchor names changes", func() { writeFile(t, root, "lib/doc/guide.md", "# Guide\n\n## Use\n") }, true},
	} {
		step.apply()
		afterVerdict, afterKey := verdict(), fingerprint(t, root)
		keyMoved := afterKey != beforeKey
		if afterVerdict != beforeVerdict && !keyMoved {
			t.Errorf("%s: the verdict moved but the key did not\nbefore:\n%s\nafter:\n%s", step.name, beforeVerdict, afterVerdict)
		}
		if keyMoved != step.moves {
			t.Errorf("%s: the key moved = %v, want %v", step.name, keyMoved, step.moves)
		}
		if step.moves && afterVerdict == beforeVerdict {
			t.Errorf("%s: the verdict did not move, so this step proves nothing", step.name)
		}
		beforeVerdict, beforeKey = afterVerdict, afterKey
	}
}

func TestReaderReadFileReadsTheCutInAWorkTreeAndTheDiskOutsideOne(t *testing.T) {
	root := workTree(t)
	writeFile(t, root, ".gitignore", "ignored.json\n")
	writeFile(t, root, "putnami.json", "{}")
	writeFile(t, root, "ignored.json", "{}")
	makeSymlink(t, root, "putnami.json", "link.json")
	reader, err := NewReader(root)
	if err != nil {
		t.Fatal(err)
	}
	for rel, wantFound := range map[string]bool{"putnami.json": true, "link.json": true, "ignored.json": false, "absent.json": false} {
		_, err := reader.ReadFile(filepath.Join(root, rel))
		if found := err == nil; found != wantFound {
			t.Errorf("ReadFile(%s) error = %v, want found %v", rel, err, wantFound)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadFile(%s) error = %v, want fs.ErrNotExist", rel, err)
		}
	}
	if _, err := reader.ReadFile(filepath.Join(t.TempDir(), "x.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile outside the workspace error = %v, want fs.ErrNotExist", err)
	}

	disk := t.TempDir()
	writeFile(t, disk, "ignored.json", "{}")
	diskReader, err := NewReader(disk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := diskReader.ReadFile(filepath.Join(disk, "ignored.json")); err != nil {
		t.Errorf("ReadFile outside a work tree = %v, want the disk's bytes", err)
	}
}
