package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestResolveConfig_FoundInStartDir(t *testing.T) {
	root := t.TempDir()
	subDir := filepath.Join(root, "a", "b")
	os.MkdirAll(subDir, 0755)

	configPath := filepath.Join(subDir, "biome.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	result := ResolveConfig("biome.json", subDir, root, "fallback")
	if result != configPath {
		t.Errorf("expected %q, got %q", configPath, result)
	}
}

func TestResolveConfig_FoundInParentDir(t *testing.T) {
	root := t.TempDir()
	parentDir := filepath.Join(root, "a")
	subDir := filepath.Join(parentDir, "b")
	os.MkdirAll(subDir, 0755)

	configPath := filepath.Join(parentDir, "biome.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	result := ResolveConfig("biome.json", subDir, root, "fallback")
	if result != configPath {
		t.Errorf("expected %q, got %q", configPath, result)
	}
}

func TestResolveConfig_FoundAtStopDir(t *testing.T) {
	root := t.TempDir()
	subDir := filepath.Join(root, "a")
	os.MkdirAll(subDir, 0755)

	configPath := filepath.Join(root, "biome.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	result := ResolveConfig("biome.json", subDir, root, "fallback")
	if result != configPath {
		t.Errorf("expected %q, got %q", configPath, result)
	}
}

func TestResolveConfig_NotFound_ReturnsFallback(t *testing.T) {
	root := t.TempDir()
	subDir := filepath.Join(root, "a", "b")
	os.MkdirAll(subDir, 0755)

	result := ResolveConfig("biome.json", subDir, root, "/default/biome.json")
	if result != "/default/biome.json" {
		t.Errorf("expected fallback '/default/biome.json', got %q", result)
	}
}

func TestResolveConfig_EmptyFallback(t *testing.T) {
	root := t.TempDir()

	result := ResolveConfig("biome.json", root, root, "")
	if result != "" {
		t.Errorf("expected empty fallback, got %q", result)
	}
}

func writeShim(t *testing.T, dir, name string) string {
	t.Helper()
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveInNodeModules_BareNameOutsideWindows(t *testing.T) {
	root := t.TempDir()
	want := writeShim(t, root, "biome")
	writeShim(t, root, "biome.exe")

	for _, goos := range []string{"linux", "darwin"} {
		if got := resolveInNodeModulesFor("biome", root, root, platform{goos: goos, goarch: "amd64"}); got != want {
			t.Errorf("%s: resolveInNodeModulesFor() = %q, want %q", goos, got, want)
		}
	}
}

func TestResolveInNodeModules_WindowsPrefersExeShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	writeShim(t, root, "biome.cmd")
	want := writeShim(t, root, "biome.exe")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsCmdShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	want := writeShim(t, root, "biome.cmd")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsSkipsShellScript(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "amd64"}); got != "" {
		t.Errorf("resolveInNodeModulesFor() = %q, want empty: Windows cannot start the extensionless shell shim", got)
	}
}

func TestResolveInNodeModules_WindowsWalksUp(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeShim(t, root, "biome.exe")

	if got := resolveInNodeModulesFor("biome", project, root, platform{goos: "windows", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

// writeNativeBiome writes the native executable of the npm platform package
// @biomejs/<pkg> under dir/node_modules, as a hoisted install lays it out.
func writeNativeBiome(t *testing.T, dir, pkg string) string {
	t.Helper()
	return writeFileAt(t, filepath.Join(dir, "node_modules", "@biomejs", pkg), nativeBiomeName(pkg))
}

// nativeBiomeName is the file name of the native executable in the platform
// package @biomejs/<pkg>.
func nativeBiomeName(pkg string) string {
	if strings.HasPrefix(pkg, "cli-win32-") {
		return "biome.exe"
	}
	return "biome"
}

func writeFileAt(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// biomePlatformPackages names, per host, the npm platform package whose native
// executable starts biome there.
var biomePlatformPackages = []struct {
	host platform
	pkg  string
}{
	{platform{goos: "linux", goarch: "amd64"}, "cli-linux-x64"},
	{platform{goos: "linux", goarch: "arm64"}, "cli-linux-arm64"},
	{platform{goos: "linux", goarch: "amd64", musl: true}, "cli-linux-x64-musl"},
	{platform{goos: "linux", goarch: "arm64", musl: true}, "cli-linux-arm64-musl"},
	{platform{goos: "darwin", goarch: "amd64"}, "cli-darwin-x64"},
	{platform{goos: "darwin", goarch: "arm64"}, "cli-darwin-arm64"},
	{platform{goos: "windows", goarch: "amd64"}, "cli-win32-x64"},
	{platform{goos: "windows", goarch: "arm64"}, "cli-win32-arm64"},
}

// hostName names a platform in a subtest.
func hostName(p platform) string {
	name := p.goos + "-" + p.goarch
	if p.musl {
		name += "-musl"
	}
	return name
}

// An isolated install links a platform package only next to the real directory
// of @biomejs/biome, which node_modules/@biomejs/biome is a symbolic link to.
func TestResolveInNodeModules_NativeBiomeOfAnIsolatedInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", "lint-starts-the-native-biome-of-the-platform-package")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, root, "biome")
	store := filepath.Join(root, "node_modules", ".bun", "@biomejs+biome@2.5.3", "node_modules", "@biomejs")
	writeFileAt(t, filepath.Join(store, "biome"), "package.json")
	want := writeFileAt(t, filepath.Join(store, "cli-linux-arm64"), "biome")
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@biomejs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "biome"), filepath.Join(root, "node_modules", "@biomejs", "biome")); err != nil {
		t.Skipf("this host cannot create a symbolic link: %v", err)
	}

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "linux", goarch: "arm64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want the native executable %q", got, want)
	}
}

// A host whose platform package is not installed starts the launcher, whatever
// other platform package is: a glibc executable does not start on a musl host,
// nor a musl one on a glibc host.
func TestResolveInNodeModules_LauncherWithoutTheHostPlatformPackage(t *testing.T) {
	cases := []struct {
		host      platform
		installed []string
	}{
		{platform{goos: "linux", goarch: "amd64"}, nil},
		{platform{goos: "darwin", goarch: "arm64"}, nil},
		{platform{goos: "linux", goarch: "amd64"}, []string{"cli-linux-x64-musl", "cli-linux-arm64", "cli-darwin-x64", "cli-win32-x64"}},
		{platform{goos: "linux", goarch: "arm64", musl: true}, []string{"cli-linux-arm64", "cli-linux-x64-musl"}},
		{platform{goos: "darwin", goarch: "arm64"}, []string{"cli-darwin-x64", "cli-linux-arm64"}},
	}
	for _, c := range cases {
		t.Run(hostName(c.host)+"-with-"+strings.Join(c.installed, "+"), func(t *testing.T) {
			root := t.TempDir()
			want := writeShim(t, root, "biome")
			for _, pkg := range c.installed {
				writeNativeBiome(t, root, pkg)
			}
			if got := resolveInNodeModulesFor("biome", root, root, c.host); got != want {
				t.Errorf("resolveInNodeModulesFor() = %q, want the launcher %q", got, want)
			}
		})
	}
}

// The nearest node_modules directory wins over a native executable further up.
func TestResolveInNodeModules_NearestNodeModulesWinsOverNativeBiome(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "packages", "app")
	want := writeShim(t, project, "biome")
	writeNativeBiome(t, root, "cli-linux-x64")

	if got := resolveInNodeModulesFor("biome", project, root, platform{goos: "linux", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

// An npm install on Windows writes biome.cmd and the native executable, and no
// .exe shim: the native executable keeps cmd.exe out of the arguments.
func TestResolveInNodeModules_WindowsPrefersNativeBiomeOverCmdShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	writeShim(t, root, "biome.cmd")
	want := writeNativeBiome(t, root, "cli-win32-x64")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want the native executable %q", got, want)
	}
}

// A bun install on Windows writes a biome.exe shim, which starts the
// JavaScript launcher: the native executable starts without it.
func TestResolveInNodeModules_WindowsPrefersNativeBiomeOverExeShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome.exe")
	want := writeNativeBiome(t, root, "cli-win32-x64")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "amd64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want the native executable %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsNativeBiomeFollowsTheArchitecture(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome.cmd")
	writeNativeBiome(t, root, "cli-win32-x64")
	want := writeNativeBiome(t, root, "cli-win32-arm64")

	if got := resolveInNodeModulesFor("biome", root, root, platform{goos: "windows", goarch: "arm64"}); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}
