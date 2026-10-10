package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/dirlink"
)

func gitInputWrite(t *testing.T, root, path, data string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitInputRun(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitInputKey(t *testing.T, root string, patterns ...string) string {
	t.Helper()
	key := &CacheKey{ProjectRoot: root, FilePatterns: patterns}
	digest, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func gitInputPhysicalRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(initStoreGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGitInputsTrackCandidateBytesAndMembership(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-bytes-and-membership-determine-the-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, ".gitignore", ".context/\nworktrees/\n.putnami/\nignored.txt\n")
	gitInputWrite(t, root, "ignored.txt", "tracked despite ignore")
	gitInputRun(t, root, "add", "-f", "ignored.txt", ".gitignore")
	project := filepath.Join(root, "tooling", "gate")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	key := func() string { return gitInputKey(t, project, "git:**") }
	before := key()
	for _, path := range []string{".context/local.json", "worktrees/other/putnami.json", ".putnami/cache/output"} {
		gitInputWrite(t, root, path, "ignored noise")
	}
	if key() != before {
		t.Fatal("ignored local state changed the candidate key")
	}
	candidates := []string{"ignored.txt", "tooling/clientgen-extension/doc/new.md", "name with spaces.txt", "name\nwith-newline.txt"}
	if runtime.GOOS == "windows" {
		// A Windows file name cannot contain a newline.
		candidates = candidates[:len(candidates)-1]
	}
	for _, path := range candidates {
		before = key()
		gitInputWrite(t, root, path, "new candidate bytes")
		if key() == before {
			t.Fatalf("candidate change at %q did not invalidate the key", path)
		}
	}
	before = key()
	gitInputRun(t, root, "add", ".")
	if key() != before {
		t.Fatal("staging identical candidate bytes changed the key")
	}
	if err := os.Rename(filepath.Join(root, "name with spaces.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if key() == before {
		t.Fatal("rename did not change the candidate key")
	}
	before = key()
	if err := os.Remove(filepath.Join(root, "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	if key() == before {
		t.Fatal("unstaged tracked deletion did not change the candidate key")
	}
	before = key()
	gitInputRun(t, root, "add", "-u")
	if key() != before {
		t.Fatal("staging a deletion changed the candidate key")
	}
}

func TestGitInputsUseRawBytesAndSymlinkTargets(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-bytes-and-membership-determine-the-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "putnami.json", `{"tasks":{"test":{"timeout":"1s"}}}`)
	gitInputWrite(t, root, ".gen/version.json", `{"buildTime":"first"}`)
	patterns := []string{"putnami.json", ".gen/version.json", "git:**"}
	key := func() string { return gitInputKey(t, root, patterns...) }
	before := key()
	gitInputWrite(t, root, "putnami.json", `{"tasks":{"test":{"timeout":"2s"}}}`)
	if key() == before {
		t.Fatal("raw project config bytes were normalized away")
	}
	before = key()
	gitInputWrite(t, root, ".gen/version.json", `{"buildTime":"second"}`)
	if key() == before {
		t.Fatal("raw version stamp bytes were normalized away")
	}
	before = key()
	patterns = []string{"git:**", ".gen/version.json", "putnami.json", "git:**"}
	if key() != before {
		t.Fatal("pattern order or duplicate changed the raw candidate digest")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("missing-target", link); err != nil {
		t.Fatal(err)
	}
	before = key()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	gitInputWrite(t, root, "link", "missing-target")
	if key() == before {
		t.Fatal("symlink and regular file with the same bytes shared a key")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	gitInputWrite(t, filepath.Dir(external), "external", "outside bytes")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	before = key()
	gitInputWrite(t, filepath.Dir(external), "external", "changed outside bytes")
	if key() != before {
		t.Fatal("candidate hashing followed a symlink outside the repository")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("another-missing-target", link); err != nil {
		t.Fatal(err)
	}
	if key() == before {
		t.Fatal("symlink target text did not change the key")
	}
}

func TestGitInputsFilterProjectRelativePathsAndFailClosed(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-enumeration-failure-never-produces-a-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "tools/gate/own.txt", "own")
	gitInputWrite(t, root, "tools/other/doc/a.md", "included")
	gitInputWrite(t, root, "tools/other/doc/excluded.md", "excluded")
	project := filepath.Join(root, "tools/gate")
	files, err := CollectKeyFiles(project, []string{"git:../other/doc/**", "!git:../other/doc/excluded.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "tools/other/doc/a.md")}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("candidate projection = %v, want %v", files, want)
	}
	// A Windows file name cannot contain a colon: there "git:literal.txt"
	// names an alternate data stream of a file called "git", so the literal
	// prefix case runs on Unix only.
	if runtime.GOOS != "windows" {
		gitInputWrite(t, root, "git:literal.txt", "literal prefix")
		files, err = CollectKeyFiles(root, []string{"git:git:literal.txt"})
		if err != nil || !reflect.DeepEqual(files, []string{filepath.Join(root, "git:literal.txt")}) {
			t.Fatalf("literal git: filename selected %v, error %v", files, err)
		}
	}
	key := &CacheKey{ProjectRoot: t.TempDir(), FilePatterns: []string{"git:**"}}
	if digest, err := key.ComputeHashUsing(NewCacheManager(nil)); err == nil || digest != "" {
		t.Fatalf("non-Git input returned digest %q, error %v", digest, err)
	}
	if err := os.Mkdir(filepath.Join(root, "unsupported"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitInputWrite(t, root, "unsupported/file", "tracked")
	gitInputRun(t, root, "add", "unsupported/file")
	if err := os.Remove(filepath.Join(root, "unsupported/file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unsupported/file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectKeyFiles(root, []string{"git:**"}); err == nil {
		t.Fatal("tracked non-regular input was silently omitted")
	}
}

// A project directory reached through a directory link, a symbolic link on
// Unix and a junction on Windows, keys its Git inputs as the directory itself:
// Git reports the physical repository root either way.
func TestGitInputsResolveProjectDirectoryAliasesBeforeParentPaths(t *testing.T) {
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "tools/gate/own.txt", "own")
	gitInputWrite(t, root, "README.md", "repository root")
	project := filepath.Join(root, "tools/gate")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := dirlink.Create(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, patterns := range [][]string{{"git:**"}, {"git:../../README.md"}, {"git:**", "own.txt"}} {
		if direct, linked := gitInputKey(t, project, patterns...), gitInputKey(t, alias, patterns...); direct != linked {
			t.Fatalf("alias changed Git input key for %v: %s != %s", patterns, direct, linked)
		}
		files, err := CollectKeyFiles(alias, patterns)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("alias selected no candidates for %v", patterns)
		}
	}
	before := gitInputKey(t, alias, "git:**")
	gitInputWrite(t, root, "README.md", "changed root")
	if gitInputKey(t, alias, "git:**") == before {
		t.Fatal("repository edit above an alias did not invalidate its key")
	}
}

func TestGitInputsBatchGuardUsesTheCandidateSet(t *testing.T) {
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, ".gitignore", "tools/gate/ignored.md\n")
	gitInputWrite(t, root, "tools/gate/ignored.md", "ignored")
	project := filepath.Join(root, "tools/gate")
	if matched, err := HasMatchingFiles(project, []string{"git:**/*.md"}); err != nil || matched {
		t.Fatalf("ignored-only Git input matched=%v, error=%v", matched, err)
	}
	if matched, err := HasMatchingFiles(project, []string{"**/*.md"}); err != nil || !matched {
		t.Fatalf("ordinary pattern behavior changed: matched=%v, error=%v", matched, err)
	}
	if err := os.Remove(filepath.Join(project, "ignored.md")); err != nil {
		t.Fatal(err)
	}
	gitInputWrite(t, root, "tools/reader.md", "candidate outside the project")
	if matched, err := HasMatchingFiles(project, []string{"git:**/*.md"}); err != nil || !matched {
		t.Fatalf("external candidate input matched=%v, error=%v", matched, err)
	}
}

// A source binding records each regular file's executable bit, so a task that
// compares one reads the bit, and the key must move with it: chmod +x of a
// candidate is a change, on the host's own reading of the bit.
func TestGitInputsHoldTheExecutableBit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-bytes-and-membership-determine-the-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "tool.sh", "#!/bin/sh\n")
	gitInputWrite(t, root, "untracked.sh", "#!/bin/sh\n")
	gitInputRun(t, root, "add", "tool.sh")
	key := func() string { return gitInputKey(t, root, "git:**") }
	before := key()
	if runtime.GOOS == "windows" {
		// Windows stores no executable bit; a tracked file takes it from the
		// index, as a source binding does.
		gitInputRun(t, root, "update-index", "--chmod=+x", "tool.sh")
	} else if err := os.Chmod(filepath.Join(root, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := key()
	if executable == before {
		t.Fatal("making a tracked candidate executable kept the key")
	}
	if runtime.GOOS == "windows" {
		gitInputRun(t, root, "update-index", "--chmod=-x", "tool.sh")
	} else if err := os.Chmod(filepath.Join(root, "tool.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if key() != before {
		t.Fatal("restoring the mode did not restore the key: the bit is not all the change held")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(root, "untracked.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		if key() == before {
			t.Fatal("making an untracked candidate executable kept the key")
		}
	}
}

// fixedModeInfo is a file's stat with a chosen mode, so both readings of the
// executable bit are tested on any host.
type fixedModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (info fixedModeInfo) Mode() os.FileMode { return info.mode }

// The digest reads the bit where the binding reads it: from the stat where the
// host stores it, from the index mode where it does not, and an untracked file
// is regular there. Its preimage is not the one earlier keys used, so a key
// over a regular candidate cannot land on an entry written before the bit was
// keyed (ADR 0061).
func TestGitCandidateDigestReadsTheBitWhereTheBindingDoes(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-bytes-and-membership-determine-the-key")
	dir := t.TempDir()
	gitInputWrite(t, dir, "tool.sh", "#!/bin/sh\n")
	path := filepath.Join(dir, "tool.sh")
	stat, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(perm os.FileMode, indexMode string, statsExecBit bool) string {
		entry := fileEntry{path: path, info: fixedModeInfo{FileInfo: stat, mode: perm}, gitCandidate: true, indexMode: indexMode}
		sum := gitCandidateDigest(entry, make([]byte, 64), statsExecBit)
		if sum == nil {
			t.Fatal("digest of a readable candidate is nil")
		}
		return hex.EncodeToString(sum)
	}
	regular, executable := digest(0o644, "100644", true), digest(0o755, "100644", true)
	if regular == executable {
		t.Fatal("a stat that stores the bit: the executable bit did not change the digest")
	}
	if digest(0o644, "100755", true) != regular {
		t.Fatal("a stat that stores the bit: the index mode changed the digest")
	}
	if digest(0o644, "100755", false) != executable {
		t.Fatal("a stat without the bit: an executable index mode is not the executable digest")
	}
	if digest(0o755, "100644", false) != regular || digest(0o755, "", false) != regular {
		t.Fatal("a stat without the bit: the stat's bit changed the digest of a regular or untracked file")
	}
	previous := sha256.New()
	writeField(previous, "git-file")
	previous.Write([]byte("#!/bin/sh\n"))
	if hex.EncodeToString(previous.Sum(nil)) == regular {
		t.Fatal("the regular digest kept the preimage keys used before the bit was keyed; an old entry could be served")
	}
}

// An unmerged path has no single mode, and a source binding refuses one, so a
// selected unmerged candidate produces no key. A submodule is a directory
// candidate: no key holds its checked-out commit, so none is produced either,
// and a task keyed on the cut runs uncached instead of replaying a verdict
// about another commit.
func TestGitInputsRefuseUnmergedAndSubmoduleCandidates(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "git-candidate-inputs", "candidate-enumeration-failure-never-produces-a-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "conflict.txt", "base\n")
	gitInputRun(t, root, "add", "conflict.txt")
	gitInputRun(t, root, "commit", "-m", "base")
	gitInputRun(t, root, "checkout", "-b", "other")
	gitInputWrite(t, root, "conflict.txt", "other\n")
	gitInputRun(t, root, "commit", "-am", "other")
	gitInputRun(t, root, "checkout", "main")
	gitInputWrite(t, root, "conflict.txt", "main\n")
	gitInputRun(t, root, "commit", "-am", "main")
	merge := exec.Command("git", "merge", "other")
	merge.Dir = root
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("the merge did not conflict:\n%s", out)
	}
	if _, err := CollectKeyFiles(root, []string{"git:**"}); err == nil {
		t.Fatal("an unmerged candidate produced a key")
	}
	if files, err := CollectKeyFiles(root, []string{"git:f"}); err != nil || len(files) != 1 {
		t.Fatalf("an unmerged path outside the pattern blocked its key: files %v, error %v", files, err)
	}
	gitInputWrite(t, root, "conflict.txt", "resolved\n")
	gitInputRun(t, root, "add", "conflict.txt")
	if _, err := CollectKeyFiles(root, []string{"git:**"}); err != nil {
		t.Fatalf("the resolved candidate still produced no key: %v", err)
	}
	gitInputRun(t, root, "commit", "-m", "resolve")

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@t.com"}, {"config", "user.name", "T"}, {"config", "commit.gpgsign", "false"},
	} {
		gitInputRun(t, sub, args...)
	}
	gitInputWrite(t, sub, "inner.txt", "inner")
	gitInputRun(t, sub, "add", "inner.txt")
	gitInputRun(t, sub, "commit", "-m", "inner")
	gitInputRun(t, root, "-c", "advice.addEmbeddedRepo=false", "add", "sub")
	if _, err := CollectKeyFiles(root, []string{"git:**"}); err == nil {
		t.Fatal("a submodule candidate produced a key that cannot hold its commit")
	}
	if digest, err := (&CacheKey{ProjectRoot: root, FilePatterns: []string{"git:**"}}).ComputeHashUsing(NewCacheManager(nil)); err == nil || digest != "" {
		t.Fatalf("a submodule candidate returned digest %q, error %v", digest, err)
	}
}
