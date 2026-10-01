package toolchain

import (
	"os"
	"path/filepath"
	"testing"
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
		if got := resolveInNodeModulesFor("biome", root, root, goos, "amd64"); got != want {
			t.Errorf("%s: resolveInNodeModulesFor() = %q, want %q", goos, got, want)
		}
	}
}

func TestResolveInNodeModules_WindowsPrefersExeShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	writeShim(t, root, "biome.cmd")
	want := writeShim(t, root, "biome.exe")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "amd64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsCmdShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	want := writeShim(t, root, "biome.cmd")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "amd64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsSkipsShellScript(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "amd64"); got != "" {
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

	if got := resolveInNodeModulesFor("biome", project, root, "windows", "amd64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

// writeNativeBiome writes the native executable npm installs for biome on
// windows/arch under dir/node_modules.
func writeNativeBiome(t *testing.T, dir, arch string) string {
	t.Helper()
	packageDir := filepath.Join(dir, "node_modules", "@biomejs", "cli-win32-"+arch)
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(packageDir, "biome.exe")
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// An npm install on Windows writes biome.cmd and the native executable, and no
// .exe shim: the native executable keeps cmd.exe out of the arguments.
func TestResolveInNodeModules_WindowsPrefersNativeBiomeOverCmdShim(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome")
	writeShim(t, root, "biome.cmd")
	want := writeNativeBiome(t, root, "x64")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "amd64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want the native executable %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsPrefersExeShimOverNativeBiome(t *testing.T) {
	root := t.TempDir()
	writeNativeBiome(t, root, "x64")
	want := writeShim(t, root, "biome.exe")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "amd64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_WindowsNativeBiomeFollowsTheArchitecture(t *testing.T) {
	root := t.TempDir()
	writeShim(t, root, "biome.cmd")
	writeNativeBiome(t, root, "x64")
	want := writeNativeBiome(t, root, "arm64")

	if got := resolveInNodeModulesFor("biome", root, root, "windows", "arm64"); got != want {
		t.Errorf("resolveInNodeModulesFor() = %q, want %q", got, want)
	}
}

func TestResolveInNodeModules_NativeBiomeOnlyOnWindows(t *testing.T) {
	root := t.TempDir()
	writeNativeBiome(t, root, "x64")

	for _, goos := range []string{"linux", "darwin"} {
		if got := resolveInNodeModulesFor("biome", root, root, goos, "amd64"); got != "" {
			t.Errorf("%s: resolveInNodeModulesFor() = %q, want empty", goos, got)
		}
	}
}
