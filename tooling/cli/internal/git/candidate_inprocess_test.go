package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/gittest"
)

// inProcessHome gives the test a home of its own, whose global configuration
// and ignore file the test writes: the in-process read and Git read the same
// ones.
func inProcessHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// sameCut fails the test unless the cut read in-process is the one Git lists
// at root. supported says whether the in-process reader reads that state
// itself; when it does not, CandidateFiles must still give Git's listing.
func sameCut(t *testing.T, root, state string, supported bool) {
	t.Helper()
	viaGit, gitErr := CandidateFilesFromGit(root)
	local, localErr := candidateFilesInProcess(root)
	if inProcessUnsupported(localErr) == supported {
		t.Fatalf("%s: in-process read unsupported = %v (%v), want %v", state, inProcessUnsupported(localErr), localErr, !supported)
	}
	if supported {
		if (gitErr == nil) != (localErr == nil) || !reflect.DeepEqual(local, viaGit) {
			t.Fatalf("%s: in-process %v (%v)\nGit %v (%v)", state, cutDiff(local, viaGit), localErr, len(viaGit), gitErr)
		}
	}
	files, err := CandidateFiles(root)
	if (gitErr == nil) != (err == nil) || !reflect.DeepEqual(files, viaGit) {
		t.Fatalf("%s: CandidateFiles %v (%v), Git %v (%v)", state, files, err, viaGit, gitErr)
	}
	sameListings(t, root, state, supported)
}

// listingPrefixes are the bounds sameListings lists under: a directory, a
// file, a nested repository, a path in no directory, a directory Git
// ignores, and two at once.
var listingPrefixes = [][]string{
	nil, {"app"}, {"README.md"}, {"app/nested"}, {"app/nested/x.go"}, {"missing/dir"}, {"lib/dist"}, {"app", "odd"}, {"APP"},
}

// sameListings fails the test unless the source binding snapshot, the
// bounded candidate listings and the index modes read in-process are the
// ones Git gives at root.
func sameListings(t *testing.T, root, state string, supported bool) {
	t.Helper()
	snapshot, _, snapshotErr := ReadSourceBindingSnapshot(root)
	local, _, localErr := sourceBindingSnapshotInProcess(root)
	if inProcessUnsupported(localErr) == supported {
		t.Fatalf("%s: in-process snapshot unsupported = %v (%v), want %v", state, inProcessUnsupported(localErr), localErr, !supported)
	}
	viaGit, _, gitErr := sourceBindingSnapshotFromGit(root)
	if supported && localErr == nil && (!reflect.DeepEqual(local.tracked, viaGit.tracked) || !reflect.DeepEqual(local.untracked, viaGit.untracked)) {
		t.Fatalf("%s: in-process snapshot %+v, Git %+v", state, local, viaGit)
	}
	if (gitErr == nil) != (snapshotErr == nil) || (gitErr == nil && (!reflect.DeepEqual(snapshot.tracked, viaGit.tracked) || !reflect.DeepEqual(snapshot.untracked, viaGit.untracked))) {
		t.Fatalf("%s: snapshot %+v (%v), Git %+v (%v)", state, snapshot, snapshotErr, viaGit, gitErr)
	}
	for _, prefixes := range listingPrefixes {
		paths, err := CandidatePaths(root, prefixes...)
		gitPaths, gitErr := CandidatePathsFromGit(root, prefixes...)
		if (err == nil) != (gitErr == nil) || !reflect.DeepEqual(paths, gitPaths) {
			t.Fatalf("%s: CandidatePaths %v = %q (%v), Git %q (%v)", state, prefixes, paths, err, gitPaths, gitErr)
		}
		modes, err := ReadIndexModes(root, prefixes...)
		gitModes, gitErr := ReadIndexModesFromGit(root, prefixes...)
		if (err == nil) != (gitErr == nil) || !reflect.DeepEqual(modes, gitModes) {
			t.Fatalf("%s: ReadIndexModes %v = %v (%v), Git %v (%v)", state, prefixes, modes, err, gitModes, gitErr)
		}
	}
}

// cutDiff names the entries two cuts disagree on.
func cutDiff(local, viaGit []protocoljob.CandidateFile) string {
	modes := map[string]string{}
	for _, file := range viaGit {
		modes[file.Path] = file.Mode
	}
	var diff []string
	for _, file := range local {
		if mode, ok := modes[file.Path]; !ok || mode != file.Mode {
			diff = append(diff, "+"+file.Path+" "+file.Mode)
		}
		delete(modes, file.Path)
	}
	for path, mode := range modes {
		diff = append(diff, "-"+path+" "+mode)
	}
	return strings.Join(diff, ", ")
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// The cut read in-process is the one `git ls-files -t --stage --others
// --exclude-standard` lists, over every shape of the work tree the candidate
// cut covers: the ignore rules of every source, deleted, intent-to-add,
// renamed and unmerged paths, modes and links, gitlinks and nested
// repositories, sparse entries, newline and non-UTF-8 names, and each index
// format. So are the source binding snapshot, the bounded candidate listings
// and the index modes. A state the reader does not read falls back to Git.
func TestTheCutReadInProcessIsTheCutGitLists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names with a newline and executable modes are POSIX")
	}
	home := inProcessHome(t)
	root := initGitRepo(t)
	sameCut(t, root, "one commit", true)

	write(t, filepath.Join(root, ".gitignore"), "\xef\xbb\xbf# rules\n*.log\r\n/build/\n!keep.log\ndist/\n\\#hash\n\\!bang\ntrailing\\ \nspaces   \n**/deep/**/x.txt\nsrc/**/gen\n[ab]?.tmp\n*.o\n")
	write(t, filepath.Join(root, "app", ".gitignore"), "!*.o\nlocal/\n/anchored.txt\nsub/*.c\n")
	for _, rel := range []string{
		"a.log", "keep.log", "build/out", "app/build/out", "lib/dist/x.js", "#hash", "!bang", "trailing ",
		"spaces", "q/deep/r/x.txt", "deep/x.txt", "src/a/b/gen", "src/gen", "ax.tmp", "cx.tmp",
		"top.o", "app/kept.o", "app/local/x", "app/anchored.txt", "app/z/anchored.txt", "app/sub/a.c",
		"app/sub/deeper/a.c", "app/new.go", "untracked.txt", "a-b/x", "a/c",
	} {
		write(t, filepath.Join(root, filepath.FromSlash(rel)), rel+"\n")
	}
	sameCut(t, root, "the rules of .gitignore files", true)

	write(t, filepath.Join(root, ".git", "info", "exclude"), "untracked.txt\n")
	write(t, filepath.Join(home, ".config", "git", "ignore"), "app/new.go\n")
	sameCut(t, root, "info/exclude and the default global ignore file", true)
	write(t, filepath.Join(home, "rules"), "*.go\n")
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/rules\n")
	sameCut(t, root, "core.excludesFile", true)
	write(t, filepath.Join(home, ".gitconfig"), "")

	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "chore: the rules")
	runGit(t, root, "update-index", "--chmod=+x", "README.md")
	write(t, filepath.Join(root, "intent.txt"), "x\n")
	runGit(t, root, "add", "-N", "intent.txt")
	mustRemove(t, filepath.Join(root, "spaces"))
	runGit(t, root, "mv", "app/kept.o", "app/renamed.o")
	if err := os.Symlink("README.md", filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	sameCut(t, root, "modes, intent-to-add, deleted, renamed and linked paths", true)

	write(t, filepath.Join(root, "odd", "new\nline.txt"), "x\n")
	write(t, filepath.Join(root, "odd", "latin\xe9.txt"), "x\n")
	write(t, filepath.Join(root, "odd", "tracked\nname.txt"), "x\n")
	runGit(t, root, "add", "odd/tracked\nname.txt")
	sameCut(t, root, "newline and non-UTF-8 names", true)

	head := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	write(t, filepath.Join(root, "sub", "inside.txt"), "x\n")
	sameCut(t, root, "a gitlink with files below it", true)

	nested := filepath.Join(root, "app", "nested")
	write(t, filepath.Join(nested, "x.go"), "package x\n")
	gittest.Run(t, nested, "init", "-q")
	ignoredNested := filepath.Join(root, "lib", "dist", "repo")
	write(t, filepath.Join(ignoredNested, "y"), "y\n")
	gittest.Run(t, ignoredNested, "init", "-q")
	notARepository := filepath.Join(root, "fake")
	write(t, filepath.Join(notARepository, ".git", "placeholder"), "")
	write(t, filepath.Join(notARepository, "z.txt"), "z\n")
	sameCut(t, root, "nested repositories, and a .git that is none", true)
	linked := filepath.Join(root, "linkednested")
	write(t, filepath.Join(linked, ".git"), "gitdir: ../app/nested/.git\n")
	write(t, filepath.Join(linked, "w.txt"), "w\n")
	sameCut(t, root, "a nested .git file", true)

	for _, version := range []string{"2", "3", "4"} {
		runGit(t, root, "update-index", "--index-version", version)
		sameCut(t, root, "index version "+version, true)
	}
	runGit(t, root, "update-index", "--untracked-cache")
	sameCut(t, root, "an untracked cache", true)
	runGit(t, root, "update-index", "--no-untracked-cache")

	runGit(t, root, "update-index", "--split-index")
	sameCut(t, root, "a split index", true)
	write(t, filepath.Join(root, "after-split.txt"), "x\n")
	runGit(t, root, "add", "after-split.txt")
	runGit(t, root, "rm", "-q", "--cached", "app/renamed.o")
	write(t, filepath.Join(root, "README.md"), "# changed\n")
	runGit(t, root, "add", "README.md")
	sameCut(t, root, "a split index with additions, deletions and replacements", true)
	runGit(t, root, "update-index", "--no-split-index")
	sameCut(t, root, "the split index merged back", true)

	for _, dir := range []string{nested, linked, notARepository, filepath.Join(root, "sub")} {
		mustRemove(t, dir)
	}
	runGit(t, root, "update-index", "--force-remove", "sub")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "chore: the shapes")
	runGit(t, root, "checkout", "-q", "-b", "left")
	write(t, filepath.Join(root, "README.md"), "# left\n")
	runGit(t, root, "commit", "-q", "-am", "left")
	runGit(t, root, "checkout", "-q", "main")
	write(t, filepath.Join(root, "README.md"), "# right\n")
	runGit(t, root, "commit", "-q", "-am", "right")
	if err := gittest.Command(root, "merge", "-q", "left").Run(); err == nil {
		t.Fatal("the merge did not conflict")
	}
	sameCut(t, root, "an unmerged path", true)
	runGit(t, root, "merge", "--abort")

	runGit(t, root, "sparse-checkout", "set", "--no-cone", "/app/")
	sameCut(t, root, "a sparse checkout without the root's rules, which Git reads from the index", false)
	runGit(t, root, "sparse-checkout", "set", "--no-cone", "/.gitignore", "/app/")
	sameCut(t, root, "a sparse checkout", true)
	runGit(t, root, "sparse-checkout", "set", "--cone", "--sparse-index", "app")
	sameCut(t, root, "a sparse index", false)
	runGit(t, root, "sparse-checkout", "disable")
	sameCut(t, root, "the sparse checkout disabled", true)

	runGit(t, root, "config", "core.ignoreCase", "true")
	write(t, filepath.Join(root, "Readme.MD"), "x\n")
	write(t, filepath.Join(root, "APP", "Other.LOG"), "x\n")
	write(t, filepath.Join(root, "Build", "x"), "x\n")
	sameCut(t, root, "core.ignoreCase", true)
	runGit(t, root, "config", "core.ignoreCase", "false")
	sameCut(t, root, "core.ignoreCase off", true)

	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", "-q", worktree)
	write(t, filepath.Join(worktree, "only-here.txt"), "x\n")
	sameCut(t, worktree, "a linked worktree", true)

	sameCut(t, filepath.Join(root, "app"), "a root below the work tree's root", false)

	runGit(t, root, "config", "index.skipHash", "true")
	runGit(t, root, "add", "-A")
	sameCut(t, root, "index.skipHash", false)
	runGit(t, root, "config", "index.skipHash", "false")
	runGit(t, root, "update-index", "--index-version", "3")
	sameCut(t, root, "index.skipHash off", true)

	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	sameCut(t, root, "a corrupt index", false)
}

// A SHA-256 repository is read in-process with its longer object names.
func TestTheCutOfASHA256RepositoryIsReadInProcess(t *testing.T) {
	inProcessHome(t)
	root := t.TempDir()
	if out, err := gittest.Command(root, "init", "-q", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("this git does not create SHA-256 repositories: %v\n%s", err, out)
	}
	write(t, filepath.Join(root, "a.txt"), "a\n")
	write(t, filepath.Join(root, "b.txt"), "b\n")
	runGit(t, root, "add", "a.txt")
	sameCut(t, root, "a SHA-256 repository", true)
}

// Outside every repository the in-process reader steps aside and Git's
// answer, an *UnmanagedError, stands.
func TestTheCutOutsideARepositoryIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	sameCut(t, outsideRepository(t), "outside a repository", false)
}

// The cut of this repository read in-process is the one Git lists.
func TestTheCutOfThisRepositoryReadInProcessIsTheCutGitLists(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("the test does not run in a work tree: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "putnami.workspace.json")); err != nil {
		t.Skip("the test does not run in this repository's work tree")
	}
	// The work tree is live: a task of the run that tests it may add a file
	// between the two reads. A difference that holds over three pairs of
	// reads fails.
	var diff string
	for range 3 {
		viaGit, gitErr := CandidateFilesFromGit(root)
		local, localErr := candidateFilesInProcess(root)
		if gitErr != nil || localErr != nil {
			t.Fatalf("this repository: in-process %v, Git %v", localErr, gitErr)
		}
		if diff = cutDiff(local, viaGit); diff == "" {
			return
		}
	}
	t.Fatalf("this repository: the in-process cut differs from Git's: %s", diff)
}

// The work tree root found in-process is the one `git rev-parse
// --show-toplevel` answers: from the root, a directory below it, a path
// through a link, a nested repository and a linked worktree; a directory
// inside the git directory and one outside every repository are Git's answer.
func TestTheWorkTreeRootFoundInProcessIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, "app", "deep", "x.txt"), "x\n")
	nested := filepath.Join(root, "app", "nested")
	write(t, filepath.Join(nested, "y.txt"), "y\n")
	gittest.Run(t, nested, "init", "-q")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(root, "app"), link); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", "-q", worktree)
	for _, c := range []struct {
		dir       string
		supported bool
	}{
		{root, true}, {filepath.Join(root, "app", "deep"), true}, {link, true}, {nested, true},
		{worktree, true}, {filepath.Join(root, ".git", "refs"), false}, {"outside", false},
	} {
		if c.dir == "outside" {
			// Last: the ceiling it sets holds for the rest of the test.
			c.dir = outsideRepository(t)
		}
		local, localErr := workTreeRootInProcess(c.dir)
		if inProcessUnsupported(localErr) == c.supported {
			t.Fatalf("%s: in-process unsupported = %v (%v), want %v", c.dir, inProcessUnsupported(localErr), localErr, !c.supported)
		}
		viaGit, gitErr := WorkTreeRootFromGit(c.dir)
		if c.supported && (localErr != nil || gitErr != nil || local != viaGit) {
			t.Fatalf("%s: in-process %q (%v), Git %q (%v)", c.dir, local, localErr, viaGit, gitErr)
		}
		got, err := WorkTreeRoot(c.dir)
		if (err == nil) != (gitErr == nil) || got != viaGit {
			t.Fatalf("%s: WorkTreeRoot %q (%v), Git %q (%v)", c.dir, got, err, viaGit, gitErr)
		}
	}
}

// A read of the cut names the directories its walk read and the ignore files
// it read: a directory excluded as a whole, a gitlink and a nested repository
// are not read, and a directory without a .gitignore adds no rule file.
func TestTheCutNamesTheDirectoriesAndRuleFilesItRead(t *testing.T) {
	home := inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, ".gitignore"), "build/\n")
	write(t, filepath.Join(root, "app", ".gitignore"), "*.log\n")
	write(t, filepath.Join(root, "app", "deep", "x.txt"), "x\n")
	write(t, filepath.Join(root, "build", "out"), "x\n")
	write(t, filepath.Join(root, ".git", "info", "exclude"), "\n")
	write(t, filepath.Join(home, ".config", "git", "ignore"), "\n")
	nested := filepath.Join(root, "nested")
	write(t, filepath.Join(nested, "y"), "y\n")
	gittest.Run(t, nested, "init", "-q")
	repo, err := openLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := repo.readCut()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "app", "app/deep"}; !reflect.DeepEqual(cut.Directories, want) {
		t.Errorf("directories = %q, want %q", cut.Directories, want)
	}
	want := []string{
		filepath.Join(repo.commonDir, "info", "exclude"), filepath.Join(home, ".config", "git", "ignore"),
		filepath.Join(repo.root, ".gitignore"), filepath.Join(repo.root, "app", ".gitignore"),
	}
	if !reflect.DeepEqual(cut.RuleFiles, want) {
		t.Errorf("rule files = %q, want %q", cut.RuleFiles, want)
	}
}

// antedate dates files an hour back, so a reading of Git's configuration
// made from them may be kept: one made from a file changed less than a
// second before is read again.
func antedate(t *testing.T, files ...string) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	for _, file := range files {
		if err := os.Chtimes(file, past, past); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// configReads is the number of git processes an in-process open of root
// starts: one when it reads Git's configuration, none when it keeps a
// reading.
func configReads(t *testing.T, root string) int {
	t.Helper()
	repo, err := openLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	return repo.Spawned
}

// The reading of Git's configuration a process keeps follows the files it
// depends on: it is kept while they do not change, and an include that names
// a file not there yet, that file appearing with a rule source, a
// conditional include relative to its file, and another HOME each move it.
func TestTheConfigurationReadingFollowsItsIncludes(t *testing.T) {
	home := inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	write(t, filepath.Join(home, ".gitconfig"), "[include]\n\tpath = ~/included\n")
	all := func() []string {
		return []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, "included"), filepath.Join(home, "conditional"),
			filepath.Join(root, ".git", "config"), filepath.Join(root, ".git", "HEAD")}
	}
	antedate(t, all()...)
	sameCut(t, root, "an include of a file that is not there", true)
	if reads := configReads(t, root); reads != 0 {
		t.Fatalf("an unchanged configuration was read again: %d git processes", reads)
	}
	write(t, filepath.Join(home, "rules"), "a.txt\n")
	write(t, filepath.Join(home, "included"), "[core]\n\texcludesFile = ~/rules\n")
	antedate(t, filepath.Join(home, "included"))
	if reads := configReads(t, root); reads != 1 {
		t.Fatalf("the included file appeared: %d git processes, want the reading again", reads)
	}
	sameCut(t, root, "the included file now names an ignore file", true)
	write(t, filepath.Join(home, ".gitconfig"), "[includeIf \"gitdir:"+filepath.ToSlash(root)+"/\"]\n\tpath = conditional\n")
	write(t, filepath.Join(home, "conditional"), "[core]\n\texcludesFile = ~/none\n")
	antedate(t, all()...)
	sameCut(t, root, "a conditional include relative to its file", true)
	if reads := configReads(t, root); reads != 0 {
		t.Fatalf("an unchanged conditional include was read again: %d git processes", reads)
	}
	write(t, filepath.Join(home, ".gitconfig"), "[includeIf \"onbranch:main\"]\n\tpath = conditional\n")
	antedate(t, all()...)
	sameCut(t, root, "a conditional include on the branch", true)
	if reads := configReads(t, root); reads != 0 {
		t.Fatalf("an unchanged branch condition was read again: %d git processes", reads)
	}
	runGit(t, root, "checkout", "-q", "-b", "other")
	if reads := configReads(t, root); reads != 1 {
		t.Fatalf("another branch: %d git processes, want the reading again", reads)
	}
	sameCut(t, root, "another branch under a branch condition", true)
	other := t.TempDir()
	write(t, filepath.Join(other, ".gitconfig"), "[core]\n\texcludesFile = ~/rules\n")
	write(t, filepath.Join(other, "rules"), "a.txt\n")
	t.Setenv("HOME", other)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(other, ".config"))
	if reads := configReads(t, root); reads != 1 {
		t.Fatalf("another HOME: %d git processes, want the reading again", reads)
	}
	sameCut(t, root, "another HOME", true)
}

// Under GIT_CONFIG_* variables, which Git reads but the Git calls the reader
// replaces do not pass on, a bound listing is the same whether the reader
// or Git answers.
func TestABoundListingIsTheSameUnderConfigurationVariables(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	write(t, filepath.Join(root, "rules"), "a.txt\n")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(root, "rules"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "rules-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	write(t, filepath.Join(root, "rules-config"), "[core]\n\texcludesFile = "+filepath.Join(root, "rules")+"\n")
	for _, prefixes := range [][]string{nil, {"a.txt"}} {
		local, err := candidatePathsInProcess(root, prefixes)
		viaGit, gitErr := CandidatePathsFromGit(root, prefixes...)
		if err != nil || gitErr != nil || !reflect.DeepEqual(local, viaGit) {
			t.Fatalf("%v: in-process %q (%v), Git %q (%v)", prefixes, local, err, viaGit, gitErr)
		}
	}
	local, err := workTreeRootInProcess(root)
	viaGit, gitErr := WorkTreeRootFromGit(root)
	if err != nil || gitErr != nil || local != viaGit {
		t.Fatalf("work tree root: in-process %q (%v), Git %q (%v)", local, err, viaGit, gitErr)
	}
}

// A repository whose format Git refuses is Git's answer: a format version
// above 1, an unknown extension in version 1, an extension of version 1 in
// version 0. An extension Git knows in version 0 is read.
func TestARepositoryFormatGitRefusesIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	for _, c := range []struct {
		version, extension string
		supported          bool
	}{
		{"0", "", true}, {"0", "preciousObjects", true}, {"2", "", false},
		{"1", "unknownExtension", false}, {"0", "unknownExtension", false}, {"0", "noop-v1", false}, {"1", "noop-v1", true},
	} {
		// The repository's own file: Git refuses to open a repository of
		// some of these formats at all.
		config := filepath.Join(root, ".git", "config")
		runGit(t, root, "config", "--file", config, "core.repositoryFormatVersion", c.version)
		for _, extension := range []string{"preciousObjects", "unknownExtension", "noop-v1"} {
			_ = gittest.Command(root, "config", "--file", config, "--unset-all", "extensions."+extension).Run()
		}
		if c.extension != "" {
			runGit(t, root, "config", "--file", config, "extensions."+c.extension, "true")
		}
		sameCut(t, root, "format "+c.version+" with "+c.extension, c.supported)
	}
}

// A NUL in a rule line ends the rule, as Git holds the line as a C string.
func TestARuleEndsAtItsFirstNUL(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, ".gitignore"), "a.txt\x00b.txt\nc\x00\n")
	for _, name := range []string{"a.txt", "b.txt", "a.txtb.txt", "c"} {
		write(t, filepath.Join(root, name), "x\n")
	}
	sameCut(t, root, "rules with a NUL", true)
}

// Inside a bare repository nested in a work tree, Git answers no work tree
// root, and the reader steps aside.
func TestTheWorkTreeRootInsideABareRepositoryIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	bare := filepath.Join(root, "bare.git")
	runGit(t, root, "init", "-q", "--bare", bare)
	if _, err := workTreeRootInProcess(bare); !inProcessUnsupported(err) {
		t.Fatalf("in-process root of a bare repository: %v, want unsupported", err)
	}
	got, err := WorkTreeRoot(bare)
	viaGit, gitErr := WorkTreeRootFromGit(bare)
	if got != viaGit || (err == nil) != (gitErr == nil) {
		t.Fatalf("WorkTreeRoot = %q (%v), Git %q (%v)", got, err, viaGit, gitErr)
	}
}

// Under a variable that changes how Git reads a pathspec, the reader steps
// aside and a bound listing is Git's.
func TestABoundListingUnderPathspecVariablesIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, "App", "a.txt"), "a\n")
	for _, name := range []string{"GIT_LITERAL_PATHSPECS", "GIT_ICASE_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			if _, err := candidatePathsInProcess(root, []string{"app"}); !inProcessUnsupported(err) {
				t.Fatalf("in-process listing under %s: %v, want unsupported", name, err)
			}
			got, err := CandidatePaths(root, "app")
			viaGit, gitErr := CandidatePathsFromGit(root, "app")
			if (err == nil) != (gitErr == nil) || !reflect.DeepEqual(got, viaGit) {
				t.Fatalf("CandidatePaths = %q (%v), Git %q (%v)", got, err, viaGit, gitErr)
			}
		})
	}
}

// sameHead fails the test unless HEAD, the branch, the clone's depth and the
// origin URL read in-process are Git's answers at root.
func sameHead(t *testing.T, root, state string) {
	t.Helper()
	sha, shaErr := HeadSHA(root)
	gitSHA, gitSHAErr := gitread.HeadSHA(root)
	if sha != gitSHA || (shaErr == nil) != (gitSHAErr == nil) {
		t.Fatalf("%s: HeadSHA %q (%v), Git %q (%v)", state, sha, shaErr, gitSHA, gitSHAErr)
	}
	branch, branchErr := CurrentBranch(root)
	gitBranch, gitBranchErr := gitread.CurrentBranch(root)
	if branch != gitBranch || (branchErr == nil) != (gitBranchErr == nil) {
		t.Fatalf("%s: CurrentBranch %q (%v), Git %q (%v)", state, branch, branchErr, gitBranch, gitBranchErr)
	}
	shallow, shallowErr := IsShallow(root)
	gitShallow, gitShallowErr := gitread.IsShallow(root)
	if shallow != gitShallow || (shallowErr == nil) != (gitShallowErr == nil) {
		t.Fatalf("%s: IsShallow %v (%v), Git %v (%v)", state, shallow, shallowErr, gitShallow, gitShallowErr)
	}
	for _, remote := range []string{"origin", "Upper", "missing"} {
		url := RemoteURL(root, remote)
		out, _ := gitread.Run(root, "config", "--get", "remote."+remote+".url")
		if url != strings.TrimSpace(out) {
			t.Fatalf("%s: RemoteURL(%s) %q, Git %q", state, remote, url, strings.TrimSpace(out))
		}
	}
}

// HEAD, the current branch, the clone's depth and a remote's URL read
// in-process are Git's answers: a loose and a packed branch, a branch a tag
// or a git directory file makes ambiguous, a detached HEAD, an unborn branch,
// a work tree file named HEAD, a linked worktree, several URLs of a remote
// and a remote named in capitals, a shallow clone.
func TestTheHeadReadInProcessIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	runGit(t, root, "remote", "add", "origin", "https://example.invalid/a.git")
	runGit(t, root, "config", "--add", "remote.origin.url", "https://example.invalid/b.git")
	runGit(t, root, "remote", "add", "Upper", "https://example.invalid/upper.git")
	inProcess := func(state string) {
		t.Helper()
		repo, err := openLocal(root)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if _, err := repo.headObject(); err != nil {
			t.Fatalf("%s: HEAD not read in-process: %v", state, err)
		}
		if _, err := repo.currentBranch(); err != nil {
			t.Fatalf("%s: branch not read in-process: %v", state, err)
		}
	}
	sameHead(t, root, "a loose branch")
	inProcess("a loose branch")
	runGit(t, root, "pack-refs", "--all")
	sameHead(t, root, "a packed branch")
	inProcess("a packed branch")
	runGit(t, root, "tag", "main")
	sameHead(t, root, "a branch a tag makes ambiguous")
	runGit(t, root, "tag", "-d", "main")
	runGit(t, root, "checkout", "-q", "-b", "ORIG_HEAD")
	sameHead(t, root, "a branch a git directory file may make ambiguous")
	runGit(t, root, "checkout", "-q", "main")
	runGit(t, root, "checkout", "-q", "--detach")
	sameHead(t, root, "a detached HEAD")
	runGit(t, root, "checkout", "-q", "main")
	write(t, filepath.Join(root, "HEAD"), "x\n")
	sameHead(t, root, "a work tree file named HEAD")
	mustRemove(t, filepath.Join(root, "HEAD"))
	runGit(t, root, "checkout", "-q", "--orphan", "unborn")
	sameHead(t, root, "an unborn branch")
	runGit(t, root, "checkout", "-q", "-f", "main")
	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", "-q", "-b", "side", worktree)
	sameHead(t, worktree, "a linked worktree")

	runGit(t, root, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	runGit(t, root, "checkout", "-q", "alias")
	sameHead(t, root, "a symbolic branch")
	runGit(t, root, "checkout", "-q", "main")
	runGit(t, root, "update-ref", "-d", "--no-deref", "refs/heads/alias")
	runGit(t, root, "branch", "-q", "linked-ref")
	runGit(t, root, "pack-refs", "--all")
	if err := os.Symlink("main", filepath.Join(root, ".git", "refs", "heads", "linked-ref")); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "commit", "-q", "--allow-empty", "-m", "past the packed ref")
	runGit(t, root, "checkout", "-q", "linked-ref")
	sameHead(t, root, "a branch whose loose ref is a link")
	runGit(t, root, "checkout", "-q", "main")
	runGit(t, root, "checkout", "-q", "-b", "bisect/x")
	runGit(t, root, "update-ref", "refs/bisect/x", "HEAD")
	sameHead(t, root, "a branch a per-worktree ref makes ambiguous")
	runGit(t, root, "checkout", "-q", "main")
	// A recent Git refuses to create a tag named HEAD; an older clone or
	// another tool may hold one, so the test writes the ref itself.
	tagHEAD := filepath.Join(root, ".git", "refs", "tags", "HEAD")
	write(t, tagHEAD, strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))+"\n")
	runGit(t, root, "checkout", "-q", "--detach")
	sameHead(t, root, "a detached HEAD a tag named HEAD makes ambiguous")
	runGit(t, root, "checkout", "-q", "main")
	mustRemove(t, tagHEAD)
	runGit(t, root, "config", "remote.empty.url", "x")
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), append(mustRead(t, filepath.Join(root, ".git", "config")), []byte("[remote \"empty\"]\n\turl\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if url := RemoteURL(root, "empty"); url != "" {
		t.Fatalf("a remote whose last url has no value: %q, want empty", url)
	}

	shallow := filepath.Join(t.TempDir(), "shallow")
	runGit(t, root, "commit", "-q", "--allow-empty", "-m", "second")
	runGit(t, t.TempDir(), "clone", "-q", "--depth", "1", "file://"+root, shallow)
	sameHead(t, shallow, "a shallow clone")
	write(t, filepath.Join(shallow, ".git", "shallow"), "not an object\n")
	sameHead(t, shallow, "a shallow file Git refuses")
	packed := initGitRepo(t)
	runGit(t, packed, "pack-refs", "--all")
	data := mustRead(t, filepath.Join(packed, ".git", "packed-refs"))
	if err := os.WriteFile(filepath.Join(packed, ".git", "packed-refs"), bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	sameHead(t, packed, "a packed-refs file with carriage returns")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A configuration file modified less than a second before a read is too
// close to tell from a later rewrite of the same size: the reading is not
// kept, and the next read asks Git again. The file is dated in the future,
// so the test does not depend on how fast it runs.
func TestARecentConfigurationIsReadAgain(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, ".git", "config"), future, future); err != nil {
		t.Fatal(err)
	}
	if reads := configReads(t, root); reads != 1 {
		t.Fatalf("first read: %d git processes, want 1", reads)
	}
	if reads := configReads(t, root); reads != 1 {
		t.Fatalf("a configuration modified at or after the read: %d git processes, want it read again", reads)
	}
	antedate(t, filepath.Join(root, ".git", "config"), filepath.Join(root, ".git", "HEAD"))
	_ = configReads(t, root)
	if reads := configReads(t, root); reads != 0 {
		t.Fatalf("an old configuration: %d git processes, want the kept reading", reads)
	}
}

// Refs Git refuses or reads otherwise step aside: a branch name its ref
// rules refuse, a per-worktree ref that makes a linked worktree's branch
// ambiguous, a corrupt branch, a packed-refs file out of order.
func TestRefsGitReadsOtherwiseAreGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	stepsAside := func(dir, state string) {
		t.Helper()
		repo, err := openLocal(dir)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if _, err := repo.currentBranch(); !inProcessUnsupported(err) {
			t.Fatalf("%s: in-process branch %v, want unsupported", state, err)
		}
		sameHead(t, dir, state)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/a..b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepsAside(root, "a branch name Git refuses")
	runGit(t, root, "checkout", "-q", "-f", "main")
	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", "-q", "-b", "bisect/x", worktree)
	runGit(t, worktree, "update-ref", "refs/bisect/x", "HEAD")
	stepsAside(worktree, "a per-worktree ref that makes a linked worktree's branch ambiguous")
	runGit(t, root, "branch", "-q", "corrupt")
	if err := os.WriteFile(filepath.Join(root, ".git", "refs", "heads", "corrupt"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/corrupt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepsAside(root, "a corrupt branch")
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRemove(t, filepath.Join(root, ".git", "refs", "heads", "corrupt"))
	head := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	runGit(t, root, "pack-refs", "--all")
	write(t, filepath.Join(root, ".git", "packed-refs"), "# pack-refs with: peeled fully-peeled sorted \n"+head+" refs/heads/zz\n"+head+" refs/heads/main\n")
	if repo, err := openLocal(root); err != nil {
		t.Fatal(err)
	} else if _, _, err := repo.packedRef("refs/heads/main"); !inProcessUnsupported(err) {
		t.Fatalf("a packed-refs file out of order: %v, want unsupported", err)
	}
	sameHead(t, root, "a packed-refs file out of order")
}

// A NUL in a .git file ends the path it names, as Git holds it as a C
// string.
func TestANulInAGitFileEndsItsPath(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	nested := filepath.Join(root, "app", "nested")
	write(t, filepath.Join(nested, "x"), "x\n")
	gittest.Run(t, nested, "init", "-q")
	linked := filepath.Join(root, "linked")
	write(t, filepath.Join(linked, ".git"), "gitdir: ../app/nested/.git\x00garbage\n")
	write(t, filepath.Join(linked, "w"), "w\n")
	sameCut(t, root, "a .git file with a NUL", true)
}

// A rule whose meaning Git changed after 2.43, a literal prefix that does not
// end with a slash followed by "**", is left to the Git of the machine.
func TestARuleWhoseMeaningGitChangedIsGitsAnswer(t *testing.T) {
	inProcessHome(t)
	root := initGitRepo(t)
	write(t, filepath.Join(root, ".gitignore"), "foo**/bar\n")
	for _, rel := range []string{"foobar", "foo/x/bar", "foo/bar", "other.txt"} {
		write(t, filepath.Join(root, filepath.FromSlash(rel)), "x\n")
	}
	sameCut(t, root, "a rule foo**/bar", false)
	write(t, filepath.Join(root, ".gitignore"), "foo/**/bar\n")
	sameCut(t, root, "a rule foo/**/bar", true)
}
