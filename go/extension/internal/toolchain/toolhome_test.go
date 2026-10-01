package toolchain

import (
	"archive/zip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveGoToolHomeRoot_Precedence(t *testing.T) {
	env := map[string]string{}
	lookup := func(key string) string { return env[key] }

	if got := ResolveGoToolHomeRoot(lookup); got != "" {
		t.Fatalf("no home at all = %q, want the empty root that forces the legacy workspace path", got)
	}

	env[ExtensionCacheRootEnv] = "/machine/extensions/go-ext"
	if got, want := ResolveGoToolHomeRoot(lookup), filepath.Join("/machine/extensions/go-ext", "go", "tools"); got != want {
		t.Errorf("contract cache root = %q, want %q", got, want)
	}

	env[homeEnvName()] = "/users/dev"
	if got, want := ResolveGoToolHomeRoot(lookup), filepath.Join("/users/dev", ".putnami", "tools", "go"); got != want {
		t.Errorf("HOME root = %q, want %q", got, want)
	}

	env[PutnamiHomeEnv] = "/relocated/putnami"
	if got, want := ResolveGoToolHomeRoot(lookup), filepath.Join("/relocated/putnami", "tools", "go"); got != want {
		t.Errorf("PUTNAMI_HOME root = %q, want %q", got, want)
	}
}

// Both machine roots follow os.UserHomeDir, which reads USERPROFILE on Windows
// and HOME elsewhere, so the extension and the CLI agree on one home.
func TestMachineRootsFollowUserHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv(PutnamiHomeEnv, "")
	t.Setenv(GoCacheDirEnv, "")
	t.Setenv(homeEnvName(), home)
	if runtime.GOOS == "windows" {
		t.Setenv("HOME", "")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ResolveGoToolHomeRoot(os.Getenv), filepath.Join(userHome, ".putnami", "tools", "go"); got != want {
		t.Errorf("tool home root = %q, want %q", got, want)
	}
	if got, want := ResolveGoCacheRoot(os.Getenv), filepath.Join(userHome, ".putnami", "cache", "go"); got != want {
		t.Errorf("go cache root = %q, want %q", got, want)
	}
}

// TestResolveGoToolHomeRoot_IgnoresTheGoCacheOverride pins the deliberate split
// between the two machine roots this extension owns. PUTNAMI_GO_CACHE_DIR moves
// the Go BUILD and MODULE caches, which `putnami cache clean` empties on
// purpose; folding the tool home into it would make a cache clean silently
// un-warm the machine and hand the next lint a 40 s rebuild.
func TestResolveGoToolHomeRoot_IgnoresTheGoCacheOverride(t *testing.T) {
	env := map[string]string{
		GoCacheDirEnv: "/somewhere/else/cache",
		homeEnvName(): "/users/dev",
	}
	lookup := func(key string) string { return env[key] }

	want := filepath.Join("/users/dev", ".putnami", "tools", "go")
	if got := ResolveGoToolHomeRoot(lookup); got != want {
		t.Fatalf("tool home = %q, want %q — the Go cache override must not relocate managed tool binaries", got, want)
	}
}

// TestGoToolHomePath_KeyIsTheWholeArtifactIdentity pins the layout both
// implementations build. The path IS the compatibility check: two workspaces on
// two Go minors keep two copies instead of evicting each other on every switch.
// The file name follows the key's platform on every host.
func TestGoToolHomePath_KeyIsTheWholeArtifactIdentity(t *testing.T) {
	got := goToolHomePath("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "1.26", "linux", "amd64")
	want := filepath.Join("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "go1.26", "linux-amd64", "golangci-lint")
	if got != want {
		t.Errorf("goToolHomePath = %q, want %q", got, want)
	}
	got = goToolHomePath("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "1.26", "windows", "amd64")
	want = filepath.Join("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "go1.26", "windows-amd64", "golangci-lint.exe")
	if got != want {
		t.Errorf("goToolHomePath = %q, want %q: the key's platform names the file, not the host", got, want)
	}
	got = goToolHomePath("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "1.26", "darwin", "arm64")
	want = filepath.Join("/home/.putnami/tools/go", "golangci-lint", "v2.10.1", "go1.26", "darwin-arm64", "golangci-lint")
	if got != want {
		t.Errorf("goToolHomePath = %q, want %q: the architecture is part of the key", got, want)
	}
	if goToolHomePath("", "golangci-lint", "v2.10.1", "1.26", "linux", "amd64") != "" {
		t.Error("an unresolved root must yield no path rather than a relative one")
	}
}

// TestResolvePinnedToolBinary_PrefersTheMachineToolHome is the regression guard
// for the defect this change removes: `putnami install` wrote one
// workspace-local directory while `putnami lint` read another, so install
// compiled a tool lint never found and lint compiled it again. Both now agree on
// the machine tool home.
func TestResolvePinnedToolBinary_PrefersTheMachineToolHome(t *testing.T) {
	putnamiHome := t.TempDir()
	t.Setenv(PutnamiHomeEnv, putnamiHome)
	workspace := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	home := ToolHome("golangci-lint")
	if home == "" {
		t.Fatal("ToolHome resolved nothing despite PUTNAMI_HOME")
	}
	if !strings.HasPrefix(home, putnamiHome) {
		t.Fatalf("ToolHome = %q, want it under the relocated home %q", home, putnamiHome)
	}
	writeFakeProgram(t, home, printing("2.10.1\n"))

	got, err := ResolvePinnedToolBinary("golangci-lint", workspace)
	if err != nil {
		t.Fatalf("ResolvePinnedToolBinary: %v", err)
	}
	if got != home {
		t.Errorf("ResolvePinnedToolBinary = %q, want the machine copy %q", got, home)
	}
}

func TestResolvePinnedToolBinary_ReadsLegacyWorkspaceTools(t *testing.T) {
	t.Setenv(PutnamiHomeEnv, t.TempDir())
	t.Setenv("PATH", t.TempDir())
	workspace := t.TempDir()
	paths := LegacyManagedToolPaths("staticcheck", workspace)
	if len(paths) != 2 {
		t.Fatalf("legacy paths = %v, want two", paths)
	}
	for i, path := range paths {
		version := "0.7.0"
		if i == 0 {
			version = "0.6.1"
		}
		writeFakeProgram(t, path, printing("staticcheck "+version+"\n"))
	}
	got, err := ResolvePinnedToolBinary("staticcheck", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got != paths[1] {
		t.Errorf("resolved = %q, want matching legacy path %q", got, paths[1])
	}
	if _, err := os.Stat(ToolHome("staticcheck")); !os.IsNotExist(err) {
		t.Errorf("legacy read wrote machine home: %v", err)
	}
}

// TestInstallToolFromArtifact_RestoresOnlyTheExactPinnedBuild proves the
// prebuilt path: a binary whose EMBEDDED build information says it is the pinned
// build is copied into the tool home, and one that says anything else is refused
// with a reason the caller logs before compiling instead.
//
// The artifact is a real Go binary installed from a file:// module proxy, so the
// check reads the same debug/buildinfo record a released tool carries — a
// hand-written file would prove nothing about the format.
func TestInstallToolFromArtifact_RestoresOnlyTheExactPinnedBuild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	artifact := buildProbeToolFromProxy(t, "example.com/probe", "v1.2.3")

	dest := filepath.Join(t.TempDir(), "tools", "probe")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installToolFromArtifact(artifact, dest, "probe", "v1.2.3"); err != nil {
		t.Fatalf("installToolFromArtifact: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the restored tool is missing: %v", err)
	}
	if !isExecutable(dest) {
		t.Errorf("the restored tool at %s is not executable", dest)
	}
	original, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(restored) {
		t.Error("the restored tool is not byte-identical to the artifact")
	}

	err = installToolFromArtifact(artifact, dest, "probe", "v9.9.9")
	if err == nil {
		t.Fatal("a version mismatch was accepted; a silent downgrade would reach every machine")
	}
	if !strings.Contains(err.Error(), "v1.2.3") || !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("mismatch reason = %q, want both the embedded and the pinned version", err)
	}

	if err := installToolFromArtifact(filepath.Join(t.TempDir(), "absent"), dest, "probe", "v1.2.3"); err == nil {
		t.Error("a missing artifact was accepted; a source checkout must fall back to compiling")
	}
}

// TestInstallTool_NeverWritesInsideTheExtensionRoot pins the ownership rule. For
// a registry-installed extension PUTNAMI_EXTENSION_ROOT is a symlink into the
// content-addressed artifact store, shared by every workspace on the machine, so
// a write there mutates an entry meant to be immutable.
func TestInstallTool_NeverWritesInsideTheExtensionRoot(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	putnamiHome := t.TempDir()
	extensionRoot := t.TempDir()
	t.Setenv(PutnamiHomeEnv, putnamiHome)
	t.Setenv(extensionRootEnvVar, extensionRoot)

	artifact := buildProbeToolFromProxy(t, "example.com/probe", "v1.2.3")
	stagedTool := filepath.Join(extensionRoot, "compiled", "tools", toolBinaryName("probe"))
	if err := os.MkdirAll(filepath.Dir(stagedTool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(artifact, stagedTool); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, extensionRoot)

	if got := ExtensionArtifactToolPath("probe"); got != stagedTool {
		t.Fatalf("ExtensionArtifactToolPath = %q, want %q", got, stagedTool)
	}

	dest := filepath.Join(putnamiHome, "tools", "go", "probe", "v1.2.3", "probe")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installToolFromArtifact(stagedTool, dest, "probe", "v1.2.3"); err != nil {
		t.Fatalf("installToolFromArtifact: %v", err)
	}

	if after := snapshotTree(t, extensionRoot); after != before {
		t.Errorf("the extension root changed during a restore:\nbefore %s\nafter  %s", before, after)
	}
}

// snapshotTree renders a directory as a stable path+size listing.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		size := int64(0)
		if !info.IsDir() {
			size = info.Size()
		}
		lines = append(lines, rel+":"+strings.TrimSpace(strings.Join([]string{info.Mode().String()}, ""))+":"+itoa(size))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// fileProxyURL is the file:// URL of a local directory, as GOPROXY accepts it:
// a Windows path such as C:\proxy becomes file:///C:/proxy.
func fileProxyURL(dir string) string {
	slashed := filepath.ToSlash(dir)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}

// buildProbeToolFromProxy installs a one-file main module from a file:// module
// proxy and returns the resulting binary. `go install pkg@version` is the exact
// command InstallTool runs, so the binary carries the same buildinfo shape a
// released tool does — including the module version the pin is compared against.
func buildProbeToolFromProxy(t *testing.T, modulePath, version string) string {
	t.Helper()
	proxy := t.TempDir()
	versionDir := filepath.Join(proxy, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := "module " + modulePath + "\n\ngo 1.22\n"
	// The version list is what `go` reads before the .info: without it the
	// download fails on the deprecation lookup rather than on the module.
	if err := os.WriteFile(filepath.Join(versionDir, "list"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, version+".info"),
		[]byte(`{"Version":"`+version+`","Time":"2026-09-11T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, version+".mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(versionDir, version+".zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(zipFile)
	root := modulePath + "@" + version
	for _, entry := range []struct{ name, content string }{
		{path.Join(root, "go.mod"), goMod},
		{path.Join(root, "main.go"), "package main\n\nfunc main() {}\n"},
	} {
		file, err := archive.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatal(err)
	}

	gopath := t.TempDir()
	// The module cache is written read-only by design, so the temp directory
	// cannot be removed until the write bit is restored.
	t.Cleanup(func() {
		_ = filepath.Walk(gopath, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, info.Mode()|0o200)
			}
			return nil
		})
	})
	cmd := exec.Command("go", "install", modulePath+"@"+version)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"GOPATH="+gopath,
		"GOMODCACHE="+filepath.Join(gopath, "pkg", "mod"),
		"GOPROXY="+fileProxyURL(proxy),
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		// The fixture proxy is the only source; nothing may route around it and
		// no checksum database can know these modules. GOENV=off also keeps a
		// developer's persisted `go env -w GOPRIVATE` from rerouting the fetch.
		"GOSUMDB=off",
		"GONOSUMDB=",
		"GONOPROXY=",
		"GOPRIVATE=",
		"GOENV=off",
		"GOFLAGS=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot install the probe module from a file proxy: %v\n%s", err, out)
	}

	binary := filepath.Join(gopath, "bin", "probe")
	if runtime.GOOS == "windows" {
		binary = filepath.Join(gopath, "bin", "probe.exe")
	}
	if !isExecutable(binary) {
		t.Fatalf("probe binary not found at %s", binary)
	}
	return binary
}
