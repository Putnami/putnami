package treefingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The golden fixture's commit. Its object id follows from the four committed
// files, the fixed identity and the fixed date below, hashed as git objects
// without git.
const goldenHead = "9c84963d9c87f949beeabb30a83ed54b4102a860"

// goldenPreimage is the canonical stream of the golden fixture, written from
// the documented format: the version, HEAD, one modified and one deleted
// tracked path, and two untracked files in git's sorted order. The ignored
// build.log is absent. Each content digest is the SHA-256 of the bytes the
// fixture leaves at that path: "after\n", nothing, "nested\n" and
// "untracked\n".
const goldenPreimage = "putnami-tree-fingerprint/2\n" +
	"head " + goldenHead + "\n" +
	"tracked 2\n" +
	"file 7b9a72466d3960eb2aacccfc848939453490db0678bd4725def3f789b891c919 10\nedited.txt\n" +
	"gone e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 11\nremoved.txt\n" +
	"untracked 2\n" +
	"file 370a8c04b8a65bb4494275eec227f1b694db04c76da6b0b8ae88ed1ab19790a3 20\nnested/untracked.txt\n" +
	"file 528eee7ba2c0adea12842fd3eab1088d4d8402d115182154b0baef559f2164f2 12\nnew file.txt\n"

// goldenFingerprint is the SHA-256 of goldenPreimage. Sessions, qualify
// verdicts and tree verify dossiers compare this digest across hosts and
// releases, so it moves only with treeFingerprintPreimageVersion.
const goldenFingerprint = "c69d763284805d5771b16083822e7fd93eb3e30b9a63ffc11c781a538e7f4635"

// goldenGit runs git for the fixture with no system or global configuration,
// no inherited GIT_* variable, and a fixed author, committer and date, so the
// commit has the same object id on every host.
func goldenGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	emptyConfig := filepath.Join(t.TempDir(), "gitconfig")
	write(t, emptyConfig, "")
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + emptyConfig,
		"GIT_AUTHOR_NAME=Putnami Fixture",
		"GIT_AUTHOR_EMAIL=fixture@putnami.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=Putnami Fixture",
		"GIT_COMMITTER_EMAIL=fixture@putnami.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			env = append(env, entry)
		}
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// goldenFixture builds the golden tree. The repository's own configuration
// turns off line-ending conversion and points core.excludesFile at an empty
// file, so a reader's global configuration cannot change what git lists. It
// writes no executable bit and no symbolic link, so the tree is the same on a
// host that stores neither.
func goldenFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	excludes := filepath.Join(t.TempDir(), "excludes")
	write(t, excludes, "")
	goldenGit(t, repo, "init", "-q", "--object-format=sha1", "--template=")
	goldenGit(t, repo, "config", "core.autocrlf", "false")
	goldenGit(t, repo, "config", "core.excludesFile", excludes)
	write(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	write(t, filepath.Join(repo, "README.md"), "fixture\n")
	write(t, filepath.Join(repo, "edited.txt"), "before\n")
	write(t, filepath.Join(repo, "removed.txt"), "removed\n")
	goldenGit(t, repo, "add", ".gitignore", "README.md", "edited.txt", "removed.txt")
	goldenGit(t, repo, "commit", "-q", "-m", "fixture")

	write(t, filepath.Join(repo, "edited.txt"), "after\n")
	if err := os.Remove(filepath.Join(repo, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "nested", "untracked.txt"), "nested\n")
	write(t, filepath.Join(repo, "new file.txt"), "untracked\n")
	write(t, filepath.Join(repo, "build.log"), "noise\n")
	return repo
}

// TestComputeMatchesTheGoldenPreimageAndDigest pins the encoding. The expected
// values are literals computed outside this package: the commit id from git's
// object format, and the preimage from the format Compute documents. The
// preimage is pinned through its digest: Compute returns the SHA-256 of the
// stream it wrote, so equal digests mean equal streams.
func TestComputeMatchesTheGoldenPreimageAndDigest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"the-preimage-is-the-documented-stream")
	sum := sha256.Sum256([]byte(goldenPreimage))
	if got := hex.EncodeToString(sum[:]); got != goldenFingerprint {
		t.Fatalf("the golden literals disagree: sha256(goldenPreimage) = %s, goldenFingerprint = %s", got, goldenFingerprint)
	}

	repo := goldenFixture(t)
	tree, err := Compute(repo)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if tree.HeadSHA != goldenHead {
		t.Fatalf("the fixture is wrong: HEAD = %s, want %s", tree.HeadSHA, goldenHead)
	}
	if !tree.Dirty {
		t.Errorf("the golden tree reported clean")
	}
	if tree.Fingerprint != goldenFingerprint {
		t.Errorf("Compute = %s, want %s: the canonical stream is no longer\n%s", tree.Fingerprint, goldenFingerprint, goldenPreimage)
	}
	if !strings.HasPrefix(goldenPreimage, treeFingerprintPreimageVersion+"\n") {
		t.Errorf("the golden preimage does not open with %q", treeFingerprintPreimageVersion)
	}
}
