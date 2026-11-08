package rust

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
)

// stubRustup installs what a rustup publishes: a script that, like rustup,
// creates the toolchain directory, with a rustc that prints the stated
// release and a cargo that writes the reports of the test task. It returns
// the log of its calls and the count of its downloads.
func stubRustup(t *testing.T, root, version string) (string, *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}
	log := filepath.Join(t.TempDir(), "rustup.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
dir="$RUSTUP_HOME/toolchains/$3-` + hostTriples[hostPlatform] + `/bin"
/bin/mkdir -p "$dir"
cat_rustc='#!/bin/sh
echo "rustc ` + version + ` (stub 2026-10-13)"'
echo "$cat_rustc" > "$dir/rustc"
/bin/cat <<'CARGO' > "$dir/cargo"
#!/bin/sh
case "$*" in *nextest*)
  /bin/mkdir -p "` + root + `/target/nextest/ci"
  /bin/cat > "` + root + `/target/nextest/ci/junit.xml" <<'XML'
` + junitPass + `
XML
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--output-path" ]; then printf 'SF:src/lib.rs\nDA:1,1\nend_of_record\n' > "$a"; fi
    prev="$a"
  done;;
esac
CARGO
/bin/chmod +x "$dir/rustc" "$dir/cargo"
`
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte(script))
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256([]byte(script))
	restoreURL, restoreDigest := rustupURL, rustupDigests[hostPlatform]
	rustupURL = func(string, string) string { return server.URL + "/rustup-init" }
	rustupDigests[hostPlatform] = hex.EncodeToString(digest[:])
	t.Cleanup(func() { rustupURL, rustupDigests[hostPlatform] = restoreURL, restoreDigest })
	t.Setenv(putnamihome.Env, t.TempDir())
	return log, &downloads
}

func TestProvideInstallsTheStatedReleaseOnce(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	log, downloads := stubRustup(t, t.TempDir(), "1.82.0")
	expected := stacks.Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}
	first, err := Provide(context.Background(), expected, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(first.Executable, filepath.Join("toolchains", "1.82.0-"+hostTriples[hostPlatform], "bin", "rustc")) {
		t.Errorf("executable = %s", first.Executable)
	}
	if !strings.Contains(strings.Join(first.Env, " "), "RUSTUP_TOOLCHAIN=1.82.0-"+hostTriples[hostPlatform]) {
		t.Errorf("env = %v", first.Env)
	}
	second, err := Provide(context.Background(), expected, func(string) {})
	if err != nil || second.Executable != first.Executable {
		t.Fatalf("second = %+v %v", second, err)
	}
	if downloads.Load() != 1 {
		t.Errorf("downloads = %d", downloads.Load())
	}
	ran, _ := os.ReadFile(log)
	want := "toolchain install 1.82.0 --profile minimal -c rustfmt -c clippy -c llvm-tools-preview --no-self-update\n"
	if string(ran) != want {
		t.Errorf("rustup ran %q, want %q once", ran, want)
	}
}

func TestProvideInstallsTheDefaultReleaseWhenNoneIsStated(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	log, _ := stubRustup(t, t.TempDir(), defaultChannel)
	if _, err := Provide(context.Background(), stacks.Expected{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadFile(log); !strings.Contains(string(ran), "install "+defaultChannel+" ") {
		t.Errorf("rustup ran %q", ran)
	}
}

func TestProvideRefusesAChannelThatIsNoChannel(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	stubRustup(t, t.TempDir(), "1.82.0")
	for _, channel := range []string{"--force", "../../evil", "1.82 0"} {
		if _, err := Provide(context.Background(), stacks.Expected{Version: channel, Source: "rust-toolchain"}, func(string) {}); err == nil {
			t.Errorf("channel %q was accepted", channel)
		}
	}
}

func TestProvideRefusesARustupWhoseDigestDiffers(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	log, _ := stubRustup(t, t.TempDir(), "1.82.0")
	rustupDigests[hostPlatform] = strings.Repeat("0", 64)
	if _, err := Provide(context.Background(), stacks.Expected{Version: "1.82.0"}, func(string) {}); err == nil || !strings.Contains(err.Error(), "download rustup") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a refused rustup ran")
	}
}

func TestATaskInstallsRustWhenTheMachineHasNone(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	root := fixture(t)
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	stubRustup(t, root, "1.82.0")
	stubTestTools(t)
	t.Setenv("PATH", t.TempDir())
	status, data, _, emitted, err := runJobOutput(t, root, "crates/api", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v\n%s", status, err, emitted)
	}
	if got := data["testSummary"].(map[string]any); got["passed"] != 2 {
		t.Errorf("testSummary = %v", got)
	}
}

func TestAnOfflineTaskInstallsNothing(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	root := fixture(t)
	log, _ := stubRustup(t, root, "1.82.0")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	status, _, _, emitted, _ := runJobOutput(t, root, "crates/api", stacks.TaskLint)
	if status == "OK" || !strings.Contains(emitted, stacks.CodeToolchainMissing) {
		t.Errorf("status %s:\n%s", status, emitted)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("rustup ran offline")
	}
}

func TestARustOnPathThatSatisfiesTheStatementIsKept(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-task-installs-the-stated-rust-when-the-machine-has-none")
	root := fixture(t)
	log, _ := stubRustup(t, root, "1.82.0")
	stubTools(t, root, "1.82.0", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, _, _, err := runJob(t, root, "crates/api", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint: %s %v", status, err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("rustup ran although the machine holds the release")
	}
}

// stubTestTools publishes each test tool as a local archive that holds a stub
// binary, and returns the count of their downloads.
func stubTestTools(t *testing.T) *atomic.Int32 {
	t.Helper()
	var downloads atomic.Int32
	restore := testTools
	t.Cleanup(func() { testTools = restore })
	tools := make([]tool, len(restore))
	for i, current := range restore {
		archive := tarGzip(t, current.binary, "#!/bin/sh\nexit 0\n")
		digest := sha256.Sum256(archive)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			downloads.Add(1)
			_, _ = w.Write(archive)
		}))
		t.Cleanup(server.Close)
		current.url = func(string) string { return server.URL + "/tool.tar.gz" }
		current.digests = map[string]string{hostPlatform: hex.EncodeToString(digest[:])}
		tools[i] = current
	}
	testTools = tools
	if os.Getenv(putnamihome.Env) == "" {
		t.Setenv(putnamihome.Env, t.TempDir())
	}
	return &downloads
}

func tarGzip(t *testing.T, name, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestTheTestTaskInstallsTheToolsTheMachineDoesNotHold(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-test-tools", "the-test-task-installs-the-tools-the-machine-does-not-hold")
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}
	downloads := stubTestTools(t)
	t.Setenv(putnamihome.Env, t.TempDir())
	held := t.TempDir()
	writeFile(t, held, "cargo-nextest", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(held, "cargo-nextest"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + held}
	if dirs, err := Tools(context.Background(), stacks.TaskLint, stacks.Metadata{}, env, func(string) {}); err != nil || len(dirs) != 0 {
		t.Errorf("lint installs no tool: %v %v", dirs, err)
	}
	if downloads.Load() != 0 {
		t.Errorf("lint downloaded %d", downloads.Load())
	}
	dirs, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, env, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || filepath.Base(dirs[0]) != "cargo-llvm-cov-"+llvmCovVersion {
		t.Fatalf("dirs = %v, want cargo-llvm-cov alone", dirs)
	}
	if info, err := os.Stat(filepath.Join(dirs[0], "cargo-llvm-cov")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary: %v %v", info, err)
	}
	if _, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, env, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 {
		t.Errorf("downloads = %d, want 1", downloads.Load())
	}
}

func TestARustWithNoLLVMToolsRunsTheTestsWithoutACoverageReport(t *testing.T) {
	spectest.Proves(t, feature, "coverage-is-optional", "a-rust-with-no-llvm-tools-runs-the-tests-without-a-coverage-report")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	bin := filepath.Dir(log)
	original, err := os.ReadFile(filepath.Join(bin, "cargo"))
	if err != nil {
		t.Fatal(err)
	}
	shim := strings.Replace(string(original), "#!/bin/sh\n", "#!/bin/sh\ncase \"$*\" in *show-env*) exit 1;; esac\n", 1)
	writeFile(t, bin, "cargo", shim)
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, data, _, emitted, err := runJobOutput(t, root, "crates/api", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v\n%s", status, err, emitted)
	}
	if !strings.Contains(emitted, stacks.CodeCoverageUnavailable) {
		t.Errorf("no warning:\n%s", emitted)
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("coverage read: %v", data["coverageSummary"])
	}
	ran, _ := os.ReadFile(log)
	if !strings.Contains(string(ran), "cargo nextest run --package api --profile ci") || strings.Contains(string(ran), "llvm-cov nextest") {
		t.Errorf("steps:\n%s", ran)
	}
}
