package treefingerprint

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

var lowercaseSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func fingerprint(t *testing.T, dir string) Tree {
	t.Helper()
	tree, err := Compute(dir)
	if err != nil {
		t.Fatalf("Compute(%s): %v", dir, err)
	}
	if !lowercaseSHA256.MatchString(tree.Fingerprint) {
		t.Fatalf("fingerprint %q is not a lowercase hex sha256", tree.Fingerprint)
	}
	return tree
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// initGitRepo creates a repository on branch main whose one commit holds
// README.md.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "checkout", "-b", "main")
	write(t, filepath.Join(dir, "README.md"), "# Test")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestFingerprintTreeIsDeterministicAndCwdIndependent pins the property every
// other use rests on: the digest describes the TREE, not the reader. Two agents
// run this from wherever they happen to be, so a cwd-dependent digest would be
// useless.
func TestFingerprintTreeIsDeterministicAndCwdIndependent(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	nested := filepath.Join(repo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	clean := fingerprint(t, repo)
	if clean.Dirty {
		t.Errorf("a freshly committed tree reports dirty")
	}
	if again := fingerprint(t, repo); again != clean {
		t.Errorf("reading the same tree twice gave %+v then %+v", clean, again)
	}
	if fromSubdirectory := fingerprint(t, nested); fromSubdirectory != clean {
		t.Errorf("reading from a subdirectory gave %+v, want %+v", fromSubdirectory, clean)
	}
}

// TestFingerprintTreeSeesContentBehindAnUnchangedStatus is the defect the digest
// exists for: a tracked file that is ALREADY dirty gets new content, so the
// `git status` output is byte-for-byte identical across the two states and a
// path-only check cannot tell them apart.
func TestFingerprintTreeSeesContentBehindAnUnchangedStatus(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	tracked := filepath.Join(repo, "README.md")

	write(t, tracked, "gated")
	gated := fingerprint(t, repo)
	if !gated.Dirty {
		t.Errorf("an edited tracked file did not report dirty")
	}
	statusGated := runGit(t, repo, "status", "--porcelain")

	write(t, tracked, "edited after the gate")
	edited := fingerprint(t, repo)
	if statusEdited := runGit(t, repo, "status", "--porcelain"); statusEdited != statusGated {
		t.Fatalf("the scenario is wrong, git status already differs:\n%q\n%q", statusGated, statusEdited)
	}
	if edited.Fingerprint == gated.Fingerprint {
		t.Errorf("a content change behind an unchanged status did not move the digest")
	}

	// Restoring the exact bytes restores the exact digest: the check accepts an
	// unchanged tree rather than merely rejecting everything.
	write(t, tracked, "gated")
	if restored := fingerprint(t, repo); restored != gated {
		t.Errorf("restoring the bytes gave %+v, want %+v", restored, gated)
	}
}

func TestFingerprintTreeCoversUntrackedContentAndIgnoresIgnoredFiles(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	base := fingerprint(t, repo)

	untracked := filepath.Join(repo, "untracked.txt")
	write(t, untracked, "one")
	one := fingerprint(t, repo)
	if one.Fingerprint == base.Fingerprint {
		t.Errorf("an untracked file did not move the digest")
	}
	if !one.Dirty {
		t.Errorf("an untracked file did not report dirty")
	}

	// Same path, different bytes: the path list is identical, the digest is not.
	write(t, untracked, "two")
	if two := fingerprint(t, repo); two.Fingerprint == one.Fingerprint {
		t.Errorf("untracked content changed without moving the digest")
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatalf("remove untracked: %v", err)
	}
	if restored := fingerprint(t, repo); restored != base {
		t.Errorf("removing the untracked file gave %+v, want %+v", restored, base)
	}

	// An ignored file is not part of the tree a gate reasons about.
	write(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "ignore")
	withRule := fingerprint(t, repo)
	write(t, filepath.Join(repo, "ignored.txt"), "noise")
	if withIgnored := fingerprint(t, repo); withIgnored != withRule {
		t.Errorf("an ignored file moved the digest: %+v, want %+v", withIgnored, withRule)
	}
}

// TestFingerprintTreeFoldsInHead keeps the delta meaningful: a diff is only
// meaningful against the commit it was taken from, so a moved HEAD must read as
// a different tree instead of canceling out against an identical diff.
func TestFingerprintTreeFoldsInHead(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	before := fingerprint(t, repo)

	write(t, filepath.Join(repo, "README.md"), "landed")
	runGit(t, repo, "commit", "-am", "land the change")
	after := fingerprint(t, repo)

	if after.Dirty {
		t.Errorf("a committed tree reports dirty")
	}
	if after.HeadSHA == before.HeadSHA {
		t.Fatalf("the scenario is wrong, HEAD did not move")
	}
	if after.Fingerprint == before.Fingerprint {
		t.Errorf("a moved HEAD produced the same digest with an empty delta both times")
	}
}

// TestFingerprintTreeDistinguishesTheExecutableBit covers the one change that
// alters what a tree DOES while leaving every byte it contains identical.
func TestFingerprintTreeDistinguishesTheExecutableBit(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	script := filepath.Join(repo, "script.sh")
	write(t, script, "#!/bin/sh\n")
	if !hostStatsExecBit {
		// Windows stores no executable bit on disk; git's index carries it
		// there, so the bit is set on a committed file through the index.
		runGit(t, repo, "add", "script.sh")
		runGit(t, repo, "commit", "-m", "add script")
	}
	plain := fingerprint(t, repo)

	if hostStatsExecBit {
		if err := os.Chmod(script, 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	} else {
		runGit(t, repo, "update-index", "--chmod=+x", "script.sh")
	}
	if executable := fingerprint(t, repo); executable.Fingerprint == plain.Fingerprint {
		t.Errorf("the executable bit did not move the digest")
	}
}

// TestFingerprintTreeReadsAnUntrackedNestedRepository pins the one entry git
// refuses to hash. `ls-files --others` collapses a nested repository to
// "<path>/", which the shell implementation this replaced could not hash at all
// — it aborted with "Unable to hash". The path enters the digest; the nested
// repository's contents do not, because git reports nothing about them either.
func TestFingerprintTreeReadsAnUntrackedNestedRepository(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	base := fingerprint(t, repo)

	nested := initGitRepo(t)
	moved := filepath.Join(repo, "vendored")
	if err := os.Rename(nested, moved); err != nil {
		t.Skipf("cannot relocate a nested repository across filesystems: %v", err)
	}
	if listing := runGit(t, repo, "ls-files", "--others", "--exclude-standard"); !strings.Contains(listing, "vendored/") {
		t.Fatalf("the scenario is wrong, git did not collapse the nested repository: %q", listing)
	}

	nestedTree, err := Compute(repo)
	if err != nil {
		t.Fatalf("Compute over a nested repository: %v", err)
	}
	if nestedTree.Fingerprint == base.Fingerprint {
		t.Errorf("an untracked nested repository did not move the digest")
	}
}

func TestFingerprintTreeRefusesWhatItCannotIdentify(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"a-tree-that-cannot-be-identified-has-no-fingerprint")
	t.Run("outside a git worktree", func(t *testing.T) {
		if _, err := Compute(t.TempDir()); err == nil {
			t.Fatal("Compute succeeded outside a git worktree")
		} else if !strings.Contains(err.Error(), "not inside a git worktree") {
			t.Errorf("error = %v, want it to name the missing worktree", err)
		}
	})
	t.Run("a repository with no commit", func(t *testing.T) {
		empty := t.TempDir()
		runGit(t, empty, "init", "-b", "main")
		if _, err := Compute(empty); err == nil {
			t.Fatal("Compute succeeded on a repository with no commit")
		} else if !strings.Contains(err.Error(), "has no commit") {
			t.Errorf("error = %v, want it to name the missing commit", err)
		}
	})
}

// addSubmodule wires child into parent at name and commits the gitlink. Local
// file transports need protocol.file.allow: git refuses them by default since
// CVE-2022-39253.
func addSubmodule(t *testing.T, parent, child, name string) {
	t.Helper()
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", child, name)
	runGit(t, parent, "commit", "-m", "add "+name)
}

// TestFingerprintTreeSeesContentInsideADirtySubmodule is the regression for the
// defect that took the digest to version 2.
//
// Version 1 hashed `git diff HEAD`, which renders a dirty submodule as
// `Subproject commit <sha>-dirty` — one word, whatever is actually inside it.
// Once a submodule was dirty, every further edit within it left the parent's
// rendered diff byte-for-byte identical, so an agent could gate a tree and then
// rewrite a submodule's source without moving the fingerprint the finalizer
// compares. It is the same "same status, different bytes" defect this mechanism
// exists to catch, one level down.
func TestFingerprintTreeSeesContentInsideADirtySubmodule(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	child := initGitRepo(t)
	write(t, filepath.Join(child, "source.txt"), "v1")
	runGit(t, child, "add", "source.txt")
	runGit(t, child, "commit", "-m", "source")

	parent := initGitRepo(t)
	addSubmodule(t, parent, child, "vendored")
	clean := fingerprint(t, parent)
	if clean.Dirty {
		t.Fatalf("a freshly committed submodule reported dirty")
	}

	source := filepath.Join(parent, "vendored", "source.txt")
	write(t, source, "v2")
	first := fingerprint(t, parent)
	if first.Fingerprint == clean.Fingerprint {
		t.Fatalf("dirtying a submodule did not move the parent digest")
	}
	if !first.Dirty {
		t.Errorf("a dirty submodule did not report the parent dirty")
	}

	// The defect: the submodule is ALREADY dirty, and its content changes again.
	// `git status` on the parent says exactly what it said a moment ago.
	write(t, source, "v3-entirely-different")
	second := fingerprint(t, parent)
	if second.Fingerprint == first.Fingerprint {
		t.Errorf("content changed inside an already-dirty submodule without moving the digest")
	}

	// And it accepts an unchanged tree rather than merely rejecting everything.
	write(t, source, "v2")
	if restored := fingerprint(t, parent); restored != first {
		t.Errorf("restoring the submodule's bytes gave %+v, want %+v", restored, first)
	}

	// An untracked file inside the submodule is part of that submodule's tree,
	// so it is part of this one's.
	write(t, filepath.Join(parent, "vendored", "scratch.txt"), "noise")
	if withUntracked := fingerprint(t, parent); withUntracked.Fingerprint == first.Fingerprint {
		t.Errorf("an untracked file inside a submodule did not move the parent digest")
	}
}

// TestFingerprintTreeIgnoresDiffRenderingConfiguration is the second regression
// that took the digest to version 2.
//
// A fingerprint is a join key two parties compare as strings. Version 1 hashed
// rendered `git diff` output, so settings that change only how git DISPLAYS a
// diff changed the digest of identical bytes: `--binary` does not imply
// `--full-index` for a text diff, so `core.abbrev` alone made two machines
// disagree about the same tree. Each knob below moved the version-1 digest.
func TestFingerprintTreeIgnoresDiffRenderingConfiguration(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	write(t, tracked, "line1\nline2\nline3\n")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-m", "tracked")

	write(t, tracked, "line1\nCHANGED\nline3\n")
	write(t, filepath.Join(repo, "untracked.txt"), "also here")
	want := fingerprint(t, repo)

	for _, setting := range [][2]string{
		{"core.abbrev", "4"},
		{"core.abbrev", "20"},
		{"diff.mnemonicPrefix", "true"},
		{"diff.noprefix", "true"},
		{"diff.srcPrefix", "custom/"},
		{"diff.dstPrefix", "custom/"},
		{"diff.context", "7"},
		{"diff.algorithm", "patience"},
		{"diff.indentHeuristic", "false"},
		{"diff.submodule", "log"},
		{"diff.ignoreSubmodules", "all"},
		{"status.renames", "false"},
	} {
		runGit(t, repo, "config", setting[0], setting[1])
		if got := fingerprint(t, repo); got != want {
			t.Errorf("%s=%s moved the digest: %+v, want %+v", setting[0], setting[1], got, want)
		}
		runGit(t, repo, "config", "--unset", setting[0])
	}
}

// TestFingerprintTreeRecordsADeletedTrackedFile keeps a removal distinct from an
// emptying. Both leave nothing to read at the path, and a digest that could not
// tell them apart would accept one for the other.
func TestFingerprintTreeRecordsADeletedTrackedFile(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	repo := initGitRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	write(t, tracked, "content")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-m", "tracked")
	committed := fingerprint(t, repo)

	if err := os.Remove(tracked); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}
	deleted := fingerprint(t, repo)
	if deleted.Fingerprint == committed.Fingerprint {
		t.Fatalf("deleting a tracked file did not move the digest")
	}
	if !deleted.Dirty {
		t.Errorf("a deleted tracked file did not report dirty")
	}

	write(t, tracked, "")
	if emptied := fingerprint(t, repo); emptied.Fingerprint == deleted.Fingerprint {
		t.Errorf("an emptied file and a deleted one produced the same digest")
	}

	write(t, tracked, "content")
	if restored := fingerprint(t, repo); restored != committed {
		t.Errorf("restoring the file gave %+v, want %+v", restored, committed)
	}
}

// TestFingerprintTreeSurvivesAnUninitializedSubmodule guards the recursion.
//
// An uninitialized submodule is an EMPTY DIRECTORY inside the parent's worktree,
// so `rev-parse --show-toplevel` run inside it answers with the parent's own
// root. Recursing on that would fingerprint the parent from within itself, once
// per level, until the depth guard fired — so the checkout test has to come
// before the recursion, not after it as a fallback.
func TestFingerprintTreeSurvivesAnUninitializedSubmodule(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"one-digest-covers-head-the-tracked-diff-and-untracked-content")
	child := initGitRepo(t)
	parent := initGitRepo(t)
	addSubmodule(t, parent, child, "vendored")

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "-c", "protocol.file.allow=always", "clone", "--no-recurse-submodules", parent, clone)

	tree, err := Compute(clone)
	if err != nil {
		t.Fatalf("Compute on a clone with an uninitialized submodule: %v", err)
	}
	if !lowercaseSHA256.MatchString(tree.Fingerprint) {
		t.Errorf("fingerprint %q is not a lowercase hex sha256", tree.Fingerprint)
	}
	if tree.Dirty {
		t.Errorf("a clone with an uninitialized submodule reported dirty")
	}
}

// A host that stats the executable bit keeps the disk's kind. On a host that
// does not, a regular file's kind follows git's worktree mode, and links and
// directories keep their kind.
func TestTrackedKind(t *testing.T) {
	for _, tc := range []struct {
		diskKind, worktreeMode string
		statsExecBit           bool
		want                   string
	}{
		{treeEntryFile, executableMode, true, treeEntryFile},
		{treeEntryExec, "100644", true, treeEntryExec},
		{treeEntryFile, executableMode, false, treeEntryExec},
		{treeEntryExec, "100644", false, treeEntryFile},
		{treeEntryFile, "100644", false, treeEntryFile},
		{treeEntryLink, "120000", false, treeEntryLink},
		{treeEntryDirectory, executableMode, false, treeEntryDirectory},
	} {
		if got := trackedKind(tc.diskKind, tc.worktreeMode, tc.statsExecBit); got != tc.want {
			t.Errorf("trackedKind(%q, %q, %v) = %q, want %q", tc.diskKind, tc.worktreeMode, tc.statsExecBit, got, tc.want)
		}
	}
}

// indexExecutableRepo commits tool.sh and plain.txt at 0644 with
// core.fileMode=false, the setting Git for Windows gives every clone, then marks
// tool.sh executable in the index only and edits both files, so git lists both
// as changed while the disk still carries no executable bit.
func indexExecutableRepo(t *testing.T) string {
	t.Helper()
	repo := initGitRepo(t)
	runGit(t, repo, "config", "core.fileMode", "false")
	tool, plain := filepath.Join(repo, "tool.sh"), filepath.Join(repo, "plain.txt")
	write(t, tool, "#!/bin/sh\n")
	write(t, plain, "plain\n")
	runGit(t, repo, "add", "tool.sh", "plain.txt")
	runGit(t, repo, "commit", "-m", "add files")
	runGit(t, repo, "update-index", "--chmod=+x", "tool.sh")
	write(t, tool, "#!/bin/sh\nexit 0\n")
	write(t, plain, "plain, edited\n")
	for _, p := range []string{tool, plain} {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// Without a host executable bit, the fingerprint classifies a tracked file from
// the worktree mode `git status --porcelain=v2` reports, which mirrors the index
// under core.fileMode=false. With one, the disk decides, as it always does on
// Linux and macOS.
func TestTrackedChangesTakesTheExecutableBitFromGitWithoutAHostExecBit(t *testing.T) {
	repo := indexExecutableRepo(t)
	kinds := func(statsExecBit bool) map[string]string {
		entries, err := trackedChanges(repo, 0, statsExecBit)
		if err != nil {
			t.Fatalf("trackedChanges: %v", err)
		}
		got := map[string]string{}
		for _, entry := range entries {
			got[entry.path] = entry.kind
		}
		return got
	}

	if got := kinds(false); got["tool.sh"] != treeEntryExec || got["plain.txt"] != treeEntryFile {
		t.Errorf("without a host executable bit: kinds = %v, want tool.sh exec and plain.txt file", got)
	}
	if got := kinds(true); got["tool.sh"] != treeEntryFile || got["plain.txt"] != treeEntryFile {
		t.Errorf("with a host executable bit: kinds = %v, want both file, as the disk says", got)
	}
}

// git's environment loses every variable that selects another repository or
// injects configuration, and keeps transport credentials, tracing and the
// remaining settings.
func TestRepositoryEnvironmentKeepsTransportAndRemovesForeignRepository(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"the-environment-cannot-redirect-the-repository")
	selectors := []string{"GIT_DIR=/tmp/foreign.git", "GIT_WORK_TREE=/tmp/foreign", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/tmp/foreign", "GIT_CONFIG_GLOBAL=/tmp/gitconfig",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_INDEX_FILE=/tmp/foreign.index", "GIT_COMMON_DIR=/tmp/foreign.git",
		"GIT_OBJECT_DIRECTORY=/tmp/objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/alternates", "GIT_PREFIX=sub/",
		"GIT_CEILING_DIRECTORIES=/tmp", "GIT_DISCOVERY_ACROSS_FILESYSTEM=1", "GIT_NAMESPACE=other"}
	kept := []string{"GIT_SSH_COMMAND=ssh -i /tmp/key", "GIT_ASKPASS=/tmp/askpass", "GIT_LITERAL_PATHSPECS=1", "PATH=/bin"}
	got := repositoryEnvironment(append(slices.Clone(selectors), kept...))
	for _, selector := range selectors {
		if slices.Contains(got, selector) {
			t.Errorf("selector survived: %q", selector)
		}
	}
	if !slices.Equal(got, kept) {
		t.Errorf("repositoryEnvironment kept %q, want %q", got, kept)
	}
}

// A fingerprint taken where git exports GIT_DIR, as it does to a hook, or
// where the environment injects configuration, still describes the worktree
// holding dir.
func TestComputeIgnoresRepositorySelectorsInTheEnvironment(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"the-environment-cannot-redirect-the-repository")
	repo := initGitRepo(t)
	write(t, filepath.Join(repo, "untracked.txt"), "untracked")
	want := fingerprint(t, repo)

	foreign := initGitRepo(t)
	write(t, filepath.Join(foreign, "README.md"), "foreign edit")
	excludes := filepath.Join(t.TempDir(), "excludes")
	write(t, excludes, "untracked.txt\n")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", excludes)

	if got := fingerprint(t, repo); got != want {
		t.Errorf("the environment redirected the fingerprint: %+v, want %+v", got, want)
	}
}
