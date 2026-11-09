package swift

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
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

// swiftlyScript is what the stub swiftly does: like swiftly, init writes the
// config of SWIFTLY_HOME_DIR and moves the swiftly it runs into
// SWIFTLY_BIN_DIR, unless it runs from there, and install writes a toolchain under
// SWIFTLY_TOOLCHAINS_DIR, 6.0 resolving to 6.0.3, whose swift prints its
// release and logs a build, beside an llvm-cov.
func swiftlyScript(log string) string {
	return `#!/bin/sh
echo "$* GNUPGHOME=$GNUPGHOME" >> "` + log + `"
case "$1" in
init) echo '{}' > "$SWIFTLY_HOME_DIR/config.json"
  if [ "$0" != "$SWIFTLY_BIN_DIR/swiftly" ]; then /bin/mv "$0" "$SWIFTLY_BIN_DIR/swiftly"; fi;;
fail) echo "Unsupported Linux platform" >&2; exit 1;;
install)
  release="$2"
  if [ "$release" = 6.0 ]; then release=6.0.3; fi
  dir="$SWIFTLY_TOOLCHAINS_DIR/$release/usr/bin"
  /bin/mkdir -p "$dir"
  printf '#!/bin/sh\ncase "$1" in --version) echo "Swift version %s (swift-%s-RELEASE)";; *) echo "$PWD: swift $*" >> "$STUB_LOG";; esac\n' "$release" "$release" > "$dir/swift"
  printf '#!/bin/sh\n' > "$dir/llvm-cov"
  /bin/chmod +x "$dir/swift" "$dir/llvm-cov"
  if [ -n "$STUB_POST_INSTALL" ]; then echo "$STUB_POST_INSTALL" > "$5"; fi;;
esac
`
}

// stubSwiftly publishes the stub swiftly on a local server, as a Linux
// archive, or as a macOS package when pkg is true, pins its SHA-256, and
// points the Putnami home at a new directory. It returns the log of its calls
// and the count of its downloads.
func stubSwiftly(t *testing.T, pkg bool) (string, *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}
	log := filepath.Join(t.TempDir(), "swiftly.log")
	script := swiftlyScript(log)
	artifact := tarGzip(t, "swiftly", script)
	if pkg {
		artifact = xarPkg(t, map[string]string{"./bin/swiftly": script, "./share/doc/swiftly/LICENSE.txt": "license"})
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(artifact)
	restore, had := swiftlyArtifacts[hostPlatform]
	swiftlyArtifacts[hostPlatform] = swiftlyArtifact{URL: server.URL + "/swiftly", SHA256: hex.EncodeToString(digest[:]), Pkg: pkg}
	t.Cleanup(func() {
		if had {
			swiftlyArtifacts[hostPlatform] = restore
		} else {
			delete(swiftlyArtifacts, hostPlatform)
		}
	})
	t.Setenv(putnamihome.Env, t.TempDir())
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "swift.log"))
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	return log, &downloads
}

func swiftToolchain(t *testing.T) stacks.Toolchain {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Profile.Variant(VariantSwiftPM)
	if err != nil {
		t.Fatal(err)
	}
	return p.Toolchain
}

func TestProvideInstallsTheStatedReleaseOnce(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	log, downloads := stubSwiftly(t, false)
	t.Setenv("SWIFTLY_HOME_DIR", "/machine/swiftly")
	t.Setenv("STUB_POST_INSTALL", "apt-get -y install libcurl4-openssl-dev")
	expected := []stacks.Expected{{Version: "6.0", Source: ".swift-version"}}
	var said []string
	first, err := Provide(context.Background(), swiftToolchain(t), expected, func(s string) { said = append(said, s) })
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(os.Getenv(putnamihome.Env), "tools", "swift")
	if first.Executable != filepath.Join(base, "toolchains", "6.0.3", "usr", "bin", "swift") {
		t.Errorf("executable = %s", first.Executable)
	}
	if !strings.Contains(strings.Join(said, "\n"), "apt-get -y install libcurl4-openssl-dev") {
		t.Errorf("the system packages swiftly names are not said: %q", said)
	}
	second, err := Provide(context.Background(), swiftToolchain(t), expected, func(string) {})
	if err != nil || second.Executable != first.Executable {
		t.Fatalf("second = %+v %v", second, err)
	}
	if downloads.Load() != 1 {
		t.Errorf("downloads = %d", downloads.Load())
	}
	ran, _ := os.ReadFile(log)
	gnupg := "GNUPGHOME=" + filepath.Join(base, "gnupg")
	want := "init --assume-yes --no-modify-profile --skip-install --quiet-shell-followup " + gnupg + "\n" +
		"install 6.0 --assume-yes --post-install-file " + filepath.Join(base, "post-install-6.0.sh") + " " + gnupg + "\n"
	if string(ran) != want {
		t.Errorf("swiftly ran\n%s\nwant once\n%s", ran, want)
	}
	// A later statement the installed release satisfies installs nothing
	// either; one it does not satisfy installs that release.
	if _, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{Version: "6.0.3", Source: ".swift-version"}}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(log); string(got) != want {
		t.Errorf("swiftly ran again:\n%s", got)
	}
	other, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{Version: "5.10.1", Source: ".swift-version"}}, func(string) {})
	if err != nil || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(other.Executable)))) != "5.10.1" {
		t.Errorf("other release = %+v %v", other, err)
	}
}

func TestProvideInstallsTheDefaultReleaseWhenNoneIsStated(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	log, _ := stubSwiftly(t, false)
	provided, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{}}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadFile(log); !strings.Contains(string(ran), "install "+defaultRelease+" ") || !strings.Contains(provided.Executable, defaultRelease) {
		t.Errorf("swiftly ran %q for %s", ran, provided.Executable)
	}
}

func TestProvideUnpacksTheMacOSPackage(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	log, _ := stubSwiftly(t, true)
	if _, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{Version: "6.0"}}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadFile(log); !strings.Contains(string(ran), "install 6.0 ") {
		t.Errorf("swiftly ran %q", ran)
	}
}

func TestNewestReadsTheMacOSToolchainNames(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	h := home(t.TempDir())
	for _, dir := range []string{"swift-6.0-RELEASE.xctoolchain", "swift-6.4.2-RELEASE.xctoolchain", "main-snapshot-2026-10-01.xctoolchain"} {
		writeExecutable(t, filepath.Join(h.toolchains, dir, "usr", "bin"), "swift", "#!/bin/sh\n")
	}
	check := swiftToolchain(t)
	for stated, want := range map[string]string{"6.0.0": "swift-6.0-RELEASE.xctoolchain", "6.0": "swift-6.0-RELEASE.xctoolchain", "6": "swift-6.4.2-RELEASE.xctoolchain", "6.4.2": "swift-6.4.2-RELEASE.xctoolchain"} {
		admitted := func(v string) bool { return check.Compare(stacks.Expected{Version: stated}, v).Code == "" }
		if got, ok := h.newest(admitted, false); !ok || got != want {
			t.Errorf("%s: %q %v, want %s", stated, got, ok, want)
		}
	}
}

func TestASwiftlyFailureSaysWhy(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	stubSwiftly(t, false)
	base := t.TempDir()
	swiftly := filepath.Join(base, "swiftly")
	writeExecutable(t, base, "swiftly", swiftlyScript(filepath.Join(base, "log")))
	h := home(base)
	if err := h.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := h.run(context.Background(), swiftly, "fail"); err == nil || !strings.Contains(err.Error(), "Unsupported Linux platform") {
		t.Errorf("err = %v", err)
	}
}

func TestProvideRefusesAStatementThatNamesNoRelease(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	log, _ := stubSwiftly(t, false)
	for _, version := range []string{"--force", "main-snapshot", "6.0 1", "../6.0"} {
		if _, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{Version: version, Source: ".swift-version"}}, func(string) {}); err == nil {
			t.Errorf("%q was accepted", version)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("swiftly ran")
	}
}

func TestProvideRefusesASwiftlyWhoseDigestDiffers(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	for _, pkg := range []bool{false, true} {
		log, _ := stubSwiftly(t, pkg)
		a := swiftlyArtifacts[hostPlatform]
		a.SHA256 = strings.Repeat("0", 64)
		swiftlyArtifacts[hostPlatform] = a
		if _, err := Provide(context.Background(), swiftToolchain(t), []stacks.Expected{{Version: "6.0"}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "download swiftly") {
			t.Fatalf("pkg %v: err = %v", pkg, err)
		}
		if _, err := os.Stat(log); err == nil {
			t.Errorf("pkg %v: a refused swiftly ran", pkg)
		}
	}
}

func TestAPackageTaskInstallsSwiftWhenTheMachineHasNone(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	root := fixture(t)
	writeFile(t, root, "Packages/Pricing/.swift-version", "6.0\n")
	log, downloads := stubSwiftly(t, false)
	t.Setenv("PATH", t.TempDir())
	for range 2 {
		if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskBuild); status != "OK" || err != nil {
			t.Fatalf("build: %s %v", status, err)
		}
	}
	if built, _ := os.ReadFile(os.Getenv("STUB_LOG")); strings.Count(string(built), "swift build --force-resolved-versions") != 2 {
		t.Errorf("the installed swift built:\n%s", built)
	}
	if ran, _ := os.ReadFile(log); strings.Count("\n"+string(ran), "\ninstall ") != 1 || downloads.Load() != 1 {
		t.Errorf("a second run installed again (%d downloads):\n%s", downloads.Load(), ran)
	}
}

func TestAnOfflinePackageTaskInstallsNothing(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	root := fixture(t)
	log, _ := stubSwiftly(t, false)
	t.Setenv("PATH", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskBuild); status == "OK" || err == nil || err.Error() != stacks.CodeToolchainMissing {
		t.Errorf("build: %s %v", status, err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("swiftly ran offline")
	}
}

func TestASwiftOnPathThatSatisfiesTheStatementIsKept(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	root := fixture(t)
	writeFile(t, root, "Packages/Pricing/.swift-version", "6.0\n")
	stubTools(t)
	log, downloads := stubSwiftly(t, false)
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	if _, err := os.Stat(log); err == nil || downloads.Load() != 0 {
		t.Error("swiftly ran although the machine holds the release")
	}
}

func TestAnXcodeProjectKeepsItsToolchainCheckOnly(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	root := fixture(t)
	_, downloads := stubSwiftly(t, false)
	t.Setenv("PATH", t.TempDir())
	if status, _, _, err := runJob(t, root, "App", stacks.TaskBuild); status == "OK" || err == nil || err.Error() != stacks.CodeToolchainMissing {
		t.Errorf("build: %s %v", status, err)
	}
	if downloads.Load() != 0 {
		t.Error("an Xcode project installed Swift")
	}
}

func TestTheProfileNamesTheProvisionedVariable(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	if got := swiftToolchain(t).Provisioned; got != ProvisionedEnv {
		t.Errorf("swiftpm provisioned = %q", got)
	}
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	xcode, err := s.Profile.Variant(VariantXcode)
	if err != nil || xcode.Toolchain.Provisioned != "" {
		t.Errorf("xcode provisioned = %q %v", xcode.Toolchain.Provisioned, err)
	}
}

func TestThePackageTestTaskFindsTheLLVMCovOfItsToolchain(t *testing.T) {
	spectest.Proves(t, feature, "provides-llvm-cov", "the-package-test-task-finds-the-llvm-cov-of-its-toolchain")
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are shell scripts")
	}
	toolchain := t.TempDir()
	usrBin := filepath.Join(toolchain, "usr", "bin")
	writeExecutable(t, usrBin, "swift", "#!/bin/sh\n")
	writeExecutable(t, usrBin, "llvm-cov", "#!/bin/sh\n")
	pkg := stacks.Metadata{Variant: VariantSwiftPM}
	ctx := context.Background()

	// A swift linked onto the PATH: its llvm-cov is beside the link's target.
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(usrBin, "swift"), filepath.Join(linked, "swift")); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + linked}
	if dirs, err := Tools(ctx, stacks.TaskTest, pkg, env, func(string) {}); err != nil || len(dirs) != 1 || dirs[0] != usrBin {
		t.Errorf("linked swift: %v %v", dirs, err)
	}
	for _, other := range []struct {
		task string
		meta stacks.Metadata
	}{{stacks.TaskBuild, pkg}, {stacks.TaskLint, pkg}, {stacks.TaskTest, stacks.Metadata{Variant: VariantXcode}}} {
		if dirs, _ := Tools(ctx, other.task, other.meta, env, func(string) {}); len(dirs) != 0 {
			t.Errorf("%s %s appends %v", other.task, other.meta.Variant, dirs)
		}
	}
	// An llvm-cov on the PATH comes first.
	held := t.TempDir()
	writeExecutable(t, held, "llvm-cov", "#!/bin/sh\n")
	if dirs, _ := Tools(ctx, stacks.TaskTest, pkg, []string{"PATH=" + linked + string(os.PathListSeparator) + held}, func(string) {}); len(dirs) != 0 {
		t.Errorf("an llvm-cov on PATH: %v", dirs)
	}
	// A proxy, such as swiftly's, names its toolchain's runtime resource path.
	proxy := t.TempDir()
	writeExecutable(t, proxy, "swift", fmt.Sprintf("#!/bin/sh\n[ \"$1\" = -print-target-info ] && echo '{\"paths\":{\"runtimeResourcePath\":\"%s\"}}'\n", filepath.Join(toolchain, "usr", "lib", "swift")))
	if dirs, err := Tools(ctx, stacks.TaskTest, pkg, []string{"PATH=" + proxy}, func(string) {}); err != nil || len(dirs) != 1 || dirs[0] != usrBin {
		t.Errorf("proxy: %v %v", dirs, err)
	}
	// No llvm-cov anywhere: swiftpm-test falls back on xcrun.
	if dirs, err := Tools(ctx, stacks.TaskTest, pkg, []string{"PATH=" + t.TempDir()}, func(string) {}); err != nil || len(dirs) != 0 {
		t.Errorf("no swift: %v %v", dirs, err)
	}
}

func TestExtractPkgFileReadsTheNestedPayload(t *testing.T) {
	spectest.Proves(t, feature, "provides-its-toolchain", "a-package-task-installs-the-stated-swift-when-the-machine-has-none")
	dir := t.TempDir()
	pkg := filepath.Join(dir, "swiftly.pkg")
	if err := os.WriteFile(pkg, xarPkg(t, map[string]string{"./bin/swiftly": "binary", "./share/x": "y"}), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "swiftly")
	if err := extractPkgFile(pkg, "bin/swiftly", dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "binary" {
		t.Errorf("content = %q", got)
	}
	if err := extractPkgFile(pkg, "bin/missing", filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was extracted")
	}
	if err := os.WriteFile(pkg, []byte("not a package"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractPkgFile(pkg, "bin/swiftly", dest); err == nil {
		t.Error("a file that is no xar archive was read")
	}
}

func writeExecutable(t *testing.T, dir, name, content string) {
	t.Helper()
	writeFile(t, dir, name, content)
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
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

// xarPkg builds a product archive as swiftly's macOS package is: a xar
// archive whose component package holds a PackageInfo and a Payload, a
// gzip-compressed odc cpio archive of files.
func xarPkg(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var cpio bytes.Buffer
	entry := func(name string, mode int, content string) {
		fmt.Fprintf(&cpio, "070707%06o%06o%06o%06o%06o%06o%06o%011o%06o%011o%s\x00%s",
			0, 1, mode, 0, 0, 1, 0, 0, len(name)+1, len(content), name, content)
	}
	entry(".", 0o40755, "")
	for name, content := range files {
		entry(name, 0o100755, content)
	}
	entry("TRAILER!!!", 0, "")
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	if _, err := gz.Write(cpio.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var info bytes.Buffer
	zw := zlib.NewWriter(&info)
	_, _ = zw.Write([]byte(`<pkg-info identifier="org.swift.swiftly"/>`))
	_ = zw.Close()
	toc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<xar><toc><checksum style="none"/>
<file id="1"><name>Distribution</name></file>
<file id="2"><name>swiftly.pkg</name><type>directory</type>
<file id="3"><name>PackageInfo</name><data><offset>0</offset><length>%d</length><size>41</size><encoding style="application/x-gzip"/></data></file>
<file id="4"><name>Payload</name><data><offset>%d</offset><length>%d</length><size>%d</size><encoding style="application/octet-stream"/></data></file>
</file></toc></xar>`, info.Len(), info.Len(), payload.Len(), payload.Len())
	var compressed bytes.Buffer
	zw = zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte(toc))
	_ = zw.Close()
	var out bytes.Buffer
	out.WriteString("xar!")
	for _, v := range []any{uint16(28), uint16(1), uint64(compressed.Len()), uint64(len(toc)), uint32(0)} {
		if err := binary.Write(&out, binary.BigEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	out.Write(compressed.Bytes())
	out.Write(info.Bytes())
	out.Write(payload.Bytes())
	return out.Bytes()
}
