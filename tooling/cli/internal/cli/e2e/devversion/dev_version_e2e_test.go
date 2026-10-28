// Package devversion drives `putnami version get` through the real CLI in a
// workspace that declares the real @putnami/dev extension by path. The
// extension's runtime is built from source by its bin/prepare with the Go
// release the workspace lock pins. These tests swap the process streams, the
// working directory and the environment, so they run in their own test
// binary.
package devversion

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// bootstrappedEnv is the variable the workspace bootstrap sets before it runs
// the implicit install.
const bootstrappedEnv = "PUTNAMI_WORKSPACE_BOOTSTRAPPED"

// noAutoInstallEnv turns the workspace bootstrap off. A CI host may set it, so
// the test clears it: the command must run behind the bootstrap.
const noAutoInstallEnv = "PUTNAMI_NO_AUTO_INSTALL"

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// repositoryRoot is the workspace root above the test's working directory.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json above the test's working directory")
		}
		dir = parent
	}
}

// versionLine is a line `version get` prints for the fixture's one version
// line, the workspace root: two spaces, its name, and a version with the
// ordered suffix of an untagged commit. The implicit install writes the
// assistant guidance files into the fixture, so the tree is dirty and the
// suffix ends with a hash of the changes.
var versionLine = regexp.MustCompile(`(?m)^  \(workspace root\) \d+\.\d+\.\d+-\d{14}-[0-9a-f]{7}(-[0-9a-f]{7})?$`)

// TestVersionGetRunsThroughTheDevExtensionBuiltFromSource runs `putnami
// version get` in a fixture workspace whose only extension is /tooling/dev,
// declared by path. The core no longer serves the command, so the answer is
// the executable's. The run covers extension discovery, the removed-root
// refusal that a declared command group lifts, the implicit install, the
// build of the runtime by bin/prepare, the cache trust the command resolves
// and the host RPC channel version.lines is answered over. It then reads the
// build information of the runtime the store holds: the release
// putnami.lock.json pins built it, without cgo and with trimmed paths.
func TestVersionGetRunsThroughTheDevExtensionBuiltFromSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bin/prepare is a bash script")
	}
	repoRoot := repositoryRoot(t)
	locked, err := lockfile.ReadLockFile(repoRoot)
	if err != nil || locked == nil {
		t.Fatalf("read putnami.lock.json: %v", err)
	}
	pin, ok := locked.GetToolchain("go")
	if !ok || pin.Version == "" {
		t.Fatal("putnami.lock.json pins no Go release")
	}
	wantGo := "go" + pin.Version
	if runtime.Version() != wantGo {
		t.Skipf("this test binary was built by %s, not by %s, the release putnami.lock.json pins", runtime.Version(), wantGo)
	}

	root := t.TempDir()
	hometest.Temp(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	artifactDir := filepath.Join(t.TempDir(), "artifacts")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)
	t.Setenv(bootstrappedEnv, "")
	t.Setenv(noAutoInstallEnv, "")

	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"dev-version-fixture","extensions":["/tooling/dev"]}`+"\n")
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami/\n")
	clitest.WriteFile(t, filepath.Join(root, "README.md"), "# Dev version fixture\n")
	// The extension's go.mod replaces the modules it builds with by relative
	// paths, so it builds in place: the workspace reaches it through a link.
	if err := os.MkdirAll(filepath.Join(root, "tooling"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repoRoot, "tooling", "dev"), filepath.Join(root, "tooling", "dev")); err != nil {
		t.Skipf("the platform cannot link the extension into the fixture: %v", err)
	}
	// The lock carries the Go release the runtime builds with, at the lock
	// format the repository records it in, and nothing else of this
	// repository's.
	lock := lockfile.NewLockFile()
	lock.Version = locked.Version
	lock.SetToolchain("go", pin)
	if err := lockfile.WriteLockFile(root, lock); err != nil {
		t.Fatal(err)
	}
	clitest.InitGitRepo(t, root)

	code, output := clitest.RunGateArgs(t, root, "version", "get")
	if code != 0 {
		t.Fatalf("putnami version get exit = %d, want 0:\n%s", code, output)
	}
	if !versionLine.MatchString(output) {
		t.Fatalf("putnami version get printed no version line for the workspace root:\n%s", output)
	}
	if strings.Contains(output, "is no longer a core command") {
		t.Fatalf("the core refused version although the extension declares it:\n%s", output)
	}
	if os.Getenv(bootstrappedEnv) == "" {
		t.Fatalf("%s is unset after the command: the implicit install did not start", bootstrappedEnv)
	}

	// The artifact store keeps a prepared runtime under
	// sha256/<digest prefix>/<digest>/runtime, and the extension's manifest
	// names the executable compiled/putnami-dev in it. The fixture's store is
	// fresh, so it holds this one build.
	built, err := filepath.Glob(filepath.Join(artifactDir, "sha256", "*", "*", "runtime", "compiled", "putnami-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("the artifact store holds %d compiled putnami-dev runtimes, want 1: %v", len(built), built)
	}
	info, err := buildinfo.ReadFile(built[0])
	if err != nil {
		t.Fatalf("read the runtime's build information: %v", err)
	}
	if info.GoVersion != wantGo {
		t.Errorf("the runtime was built by %s, want %s, the release putnami.lock.json pins", info.GoVersion, wantGo)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" {
		t.Errorf("the runtime was built with CGO_ENABLED=%q and -trimpath=%q, want 0 and true",
			settings["CGO_ENABLED"], settings["-trimpath"])
	}
}

// TestTheContributorPrepareScriptIsTheDevOneUnderItsOwnNames reads the
// bin/prepare of @putnami/dev and of @putnami/contributor. The host runs both
// the same way, and the test above runs the dev one end to end. The
// contributor one is the same script with its own names: the PUTNAMI_CONTRIBUTOR_
// prefix of the variables the host passes, and the putnami-contributor
// executable. With those names swapped back, the two files and their
// permissions are equal, so the run above proves the contributor build too.
func TestTheContributorPrepareScriptIsTheDevOneUnderItsOwnNames(t *testing.T) {
	repoRoot := repositoryRoot(t)
	devPath := filepath.Join(repoRoot, "tooling", "dev", "bin", "prepare")
	contributorPath := filepath.Join(repoRoot, "tooling", "contributor", "bin", "prepare")
	dev, err := os.ReadFile(devPath)
	if err != nil {
		t.Fatal(err)
	}
	contributor, err := os.ReadFile(contributorPath)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.NewReplacer(
		"PUTNAMI_CONTRIBUTOR_", "PUTNAMI_DEV_",
		"putnami-contributor", "putnami-dev",
	).Replace(string(contributor))
	if renamed != string(dev) {
		t.Errorf("%s differs from %s beyond the extension's names; change both scripts together", contributorPath, devPath)
	}

	devInfo, err := os.Stat(devPath)
	if err != nil {
		t.Fatal(err)
	}
	contributorInfo, err := os.Stat(contributorPath)
	if err != nil {
		t.Fatal(err)
	}
	if devInfo.Mode().Perm() != contributorInfo.Mode().Perm() {
		t.Errorf("%s has mode %v and %s has mode %v, want the same",
			contributorPath, contributorInfo.Mode().Perm(), devPath, devInfo.Mode().Perm())
	}
}
