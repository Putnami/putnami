package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestLocalGoVersion_ReportsInstalledToolchain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found on PATH")
	}
	v := localGoVersion(CurrentGoBinary())
	if !strings.HasPrefix(v, "go1.") {
		t.Errorf("localGoVersion = %q, want a go1.x version", v)
	}
}

func TestToolBuildMatchesCurrentGoVersion_SelfMatches(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found on PATH")
	}
	// The test binary is compiled with the local Go toolchain, so its build
	// version must match what localGoVersion reports for that same toolchain.
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve test binary path")
	}
	if !toolBuildMatchesCurrentGoVersion(self) {
		t.Errorf("a binary built with the local Go toolchain must match; got false for %s", self)
	}
}

func TestToolBuildMatchesCurrentGoVersion_UnreadableBinaryIsLenient(t *testing.T) {
	// A path with no readable Go build info must not read as a mismatch, so a
	// tool whose provenance can't be determined is not reinstalled spuriously.
	if !toolBuildMatchesCurrentGoVersion(filepath.Join(t.TempDir(), "nonexistent")) {
		t.Error("unreadable binary should be treated as matching (lenient)")
	}
}

func TestResolveGo_Found(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found on PATH, skipping")
	}

	got, err := ResolveGo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty path for go")
	}
}

func TestGoBinaryResolutionUsesSystemPath(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	pathDir := t.TempDir()
	goBinary := writeExecutable(t, filepath.Join(pathDir, goBinaryName()))
	t.Setenv("PATH", pathDir)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != goBinary {
		t.Errorf("ResolveGo = %q, want PATH binary %q", resolved, goBinary)
	}
	if current := CurrentGoBinary(); current != goBinary {
		t.Errorf("CurrentGoBinary = %q, want PATH binary %q", current, goBinary)
	}
}

func TestGoBinaryResolutionPrefersExplicitGoRoot(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	goRoot := t.TempDir()
	goRootBinary := writeExecutable(t, filepath.Join(goRoot, "bin", goBinaryName()))
	pathBinary := writeExecutable(t, filepath.Join(t.TempDir(), goBinaryName()))
	t.Setenv("GOROOT", goRoot)
	t.Setenv("PATH", filepath.Dir(pathBinary))

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != goRootBinary {
		t.Errorf("ResolveGo = %q, want explicit GOROOT binary %q", resolved, goRootBinary)
	}
}

func TestGoBinaryResolutionPrefersPathBeforeManagedWorkspace(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	pathBinary := writeExecutable(t, filepath.Join(t.TempDir(), goBinaryName()))
	writeExecutable(t, legacyManagedGoPath(workspace))
	t.Setenv("PATH", filepath.Dir(pathBinary))
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != pathBinary {
		t.Errorf("ResolveGo = %q, want PATH binary %q", resolved, pathBinary)
	}
}

func TestGoBinaryResolutionSkipsNonExecutableGoRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files carry no execute permission: every regular file is a candidate")
	}

	clearManagedGoResolutionEnv(t)
	goRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(goRoot, "bin", goBinaryName()),
		[]byte("not executable"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	pathBinary := writeExecutable(t, filepath.Join(t.TempDir(), goBinaryName()))
	t.Setenv("GOROOT", goRoot)
	t.Setenv("PATH", filepath.Dir(pathBinary))

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != pathBinary {
		t.Errorf("ResolveGo = %q, want PATH fallback %q", resolved, pathBinary)
	}
	env := GoCommandEnv(os.Environ(), resolved)
	if stale := envValue(env, "GOROOT"); stale != "" {
		t.Errorf("child GOROOT = %q, want stale non-executable GOROOT removed", stale)
	}
}

func TestGoBinaryResolutionUsesLegacyManagedWorkspaceWithoutPath(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	managed := writeExecutable(t, legacyManagedGoPath(workspace))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != managed {
		t.Errorf("ResolveGo = %q, want legacy managed binary %q", resolved, managed)
	}
	if current := CurrentGoBinary(); current != managed {
		t.Errorf("CurrentGoBinary = %q, want resolved managed binary %q", current, managed)
	}
}

func TestGoBinaryResolutionUsesStableExtensionRoot(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	extensionRoot := t.TempDir()
	managed := writeExecutable(t, filepath.Join(extensionRoot, "bin", goBinaryName()))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PUTNAMI_EXTENSION_ROOT", extensionRoot)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != managed {
		t.Errorf("ResolveGo = %q, want extension-root binary %q", resolved, managed)
	}
}

func TestGoBinaryResolutionUsesWorkspaceStableExtensionRoot(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	managed := writeExecutable(
		t,
		filepath.Join(extRoot(workspace), "bin", goBinaryName()),
	)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != managed {
		t.Errorf("ResolveGo = %q, want workspace stable binary %q", resolved, managed)
	}
}

func TestResolveGoRequirementUsesGoWorkBeforeProjectGoMod(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "the-go-requirement-comes-from-go-work-before-the-project-go-mod")
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	writeTextFile(t, filepath.Join(workspace, "go.work"), "go 1.25.0\ntoolchain go1.26.1\n")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/project\ngo 1.27.0\n")

	clearManagedGoResolutionEnv(t)
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_PATH", project)

	got, err := resolveGoRequirementFor(goResolutionFromEnv())
	if err != nil {
		t.Fatalf("resolveGoRequirement: %v", err)
	}
	if got.version != "go1.26.1" {
		t.Errorf("requirement version = %q, want %q", got.version, "go1.26.1")
	}
	if want := filepath.Join(workspace, "go.work"); got.source != want {
		t.Errorf("requirement source = %q, want %q", got.source, want)
	}
}

func TestResolveGoRequirementFallsBackToProjectGoMod(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "an-absent-go-work-requirement-falls-back-to-the-project-go-mod")
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	writeTextFile(
		t,
		filepath.Join(project, "go.mod"),
		"module example.com/project\ngo 1.25.7\ntoolchain go1.26.1\n",
	)

	clearManagedGoResolutionEnv(t)
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_ROOT", project)

	got, err := resolveGoRequirementFor(goResolutionFromEnv())
	if err != nil {
		t.Fatalf("resolveGoRequirement: %v", err)
	}
	if got.version != "go1.26.1" {
		t.Errorf("requirement version = %q, want %q", got.version, "go1.26.1")
	}
	if want := filepath.Join(project, "go.mod"); got.source != want {
		t.Errorf("requirement source = %q, want %q", got.source, want)
	}
}

func TestResolveGoRequirementEmptyGoWorkFallsBackToProjectGoMod(t *testing.T) {
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	writeTextFile(t, filepath.Join(workspace, "go.work"), "// no version requirement\n")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/project\ngo 1.25.7\n")

	clearManagedGoResolutionEnv(t)
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_PATH", project)

	got, err := resolveGoRequirementFor(goResolutionFromEnv())
	if err != nil {
		t.Fatalf("resolveGoRequirement: %v", err)
	}
	if got.version != "go1.25.7" {
		t.Errorf("requirement version = %q, want %q", got.version, "go1.25.7")
	}
	if want := filepath.Join(project, "go.mod"); got.source != want {
		t.Errorf("requirement source = %q, want %q", got.source, want)
	}
}

func TestResolveGoRequirementMalformedGoWorkIsActionable(t *testing.T) {
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	goWork := filepath.Join(workspace, "go.work")
	writeTextFile(t, goWork, "go not-a-version\n")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/project\ngo 1.25.7\n")

	clearManagedGoResolutionEnv(t)
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_PATH", project)

	_, err := resolveGoRequirementFor(goResolutionFromEnv())
	if err == nil {
		t.Fatal("resolveGoRequirement accepted a malformed governing go.work")
	}
	for _, want := range []string{goWork, "invalid go version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resolveGoRequirement error = %q, want detail %q", err, want)
		}
	}
}

func TestEffectiveGoRequirementDirectiveSemantics(t *testing.T) {
	tests := []struct {
		name      string
		goVersion string
		toolchain string
		want      string
	}{
		{
			name:      "newer toolchain raises requirement",
			goVersion: "1.25.7",
			toolchain: "go1.26.1",
			want:      "go1.26.1",
		},
		{
			name:      "older toolchain does not lower go requirement",
			goVersion: "1.26.1",
			toolchain: "go1.25.7",
			want:      "go1.26.1",
		},
		{
			name:      "default keeps go requirement",
			goVersion: "1.26.1",
			toolchain: "default",
			want:      "go1.26.1",
		},
		{
			name:      "language version permits patch release",
			goVersion: "1.26",
			want:      "go1.26",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveGoRequirement(tt.goVersion, tt.toolchain)
			if err != nil {
				t.Fatalf("effectiveGoRequirement: %v", err)
			}
			if got != tt.want {
				t.Errorf("effectiveGoRequirement(%q, %q) = %q, want %q", tt.goVersion, tt.toolchain, got, tt.want)
			}
		})
	}
}

func TestGoBinaryResolutionSkipsIncompatibleGoRootAndPathForManagedGo(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	writeTextFile(t, filepath.Join(workspace, "go.work"), "go 1.26.1\n")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/project\ngo 1.25.0\n")

	goRoot := t.TempDir()
	writeFakeGoCompiler(t, filepath.Join(goRoot, "bin", goBinaryName()), "go1.24.9")
	pathBinary := writeFakeGoCompiler(t, filepath.Join(t.TempDir(), goBinaryName()), "go1.25.7")
	managed := writeFakeGoCompiler(t, legacyManagedGoPath(workspace), "go1.26.1")

	t.Setenv("GOROOT", goRoot)
	t.Setenv("PATH", filepath.Dir(pathBinary))
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_PATH", project)

	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if resolved != managed {
		t.Errorf("ResolveGo = %q, want compatible managed binary %q", resolved, managed)
	}
}

func TestGoBinaryResolutionIncompatibleCandidatesAreActionable(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	goWork := filepath.Join(workspace, "go.work")
	writeTextFile(t, goWork, "go 1.26.1\n")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/project\ngo 1.25.0\n")
	pathBinary := writeFakeGoCompiler(t, filepath.Join(t.TempDir(), goBinaryName()), "go1.25.7")

	t.Setenv("PATH", filepath.Dir(pathBinary))
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PUTNAMI_PROJECT_PATH", project)

	_, err := ResolveGo()
	if err == nil {
		t.Fatal("ResolveGo accepted an incompatible PATH compiler")
	}
	for _, want := range []string{
		"none satisfies go1.26.1",
		goWork,
		pathBinary,
		"go1.25.7",
		"putnami install",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveGo error = %q, want actionable detail %q", err, want)
		}
	}
}

func TestGoBinaryResolutionMissingIsActionable(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	t.Setenv("PATH", t.TempDir())

	_, err := ResolveGo()
	if err == nil {
		t.Fatal("ResolveGo succeeded without a Go binary on PATH")
	}
	for _, want := range []string{"PATH", "putnami install", "https://go.dev/dl/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveGo error = %q, want actionable detail %q", err, want)
		}
	}
	if current := CurrentGoBinary(); current != "go" {
		t.Errorf("CurrentGoBinary = %q, want fallback %q", current, "go")
	}
}

func clearManagedGoResolutionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOROOT", "")
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", "")
	t.Setenv("PUTNAMI_EXTENSION_ROOT", "")
	t.Setenv("PUTNAMI_PROJECT_PATH", "")
	t.Setenv("PUTNAMI_PROJECT_ROOT", "")
}

func legacyManagedGoPath(workspace string) string {
	return filepath.Join(
		workspace,
		".putnami",
		"extensions",
		"@putnami-go",
		"bin",
		goBinaryName(),
	)
}

// writeExecutable places a program at path that exits 0 for any arguments.
func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	return writeFakeProgram(t, path, fakeProgram{})
}

// writeFakeGoCompiler places a go command at path that reports version for
// `go env GOVERSION` under GOTOOLCHAIN=local and fails for anything else.
func writeFakeGoCompiler(t *testing.T, path, version string) string {
	t.Helper()
	return writeFakeProgram(t, path, fakeProgram{
		RequireEnv: map[string]string{"GOTOOLCHAIN": "local"},
		Answers:    []fakeAnswer{{Args: []string{"env", "GOVERSION"}, Stdout: version + "\n"}},
		Otherwise:  fakeAnswer{Stderr: "unexpected arguments\n", Exit: 41},
	})
}

func writeTextFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLockFileReleasesCleanly(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "tool.lock")
	unlock, err := lockFile(lockPath)
	if err != nil {
		t.Fatalf("lockFile: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	unlockAgain, err := lockFile(lockPath)
	if err != nil {
		t.Fatalf("reacquire lockFile: %v", err)
	}
	if err := unlockAgain(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
}

func TestExtRoot(t *testing.T) {
	got := extRoot("/workspace")
	want := filepath.Join("/workspace", ".putnami", "bin", "extensions", "putnami-go")
	if got != want {
		t.Errorf("extRoot = %q, want %q", got, want)
	}
}

func TestExtRoot_NestedPath(t *testing.T) {
	got := extRoot("/home/user/my-project")
	want := filepath.Join("/home/user/my-project", ".putnami", "bin", "extensions", "putnami-go")
	if got != want {
		t.Errorf("extRoot = %q, want %q", got, want)
	}
}

func TestResolveGolangciConfig_Explicit(t *testing.T) {
	got := ResolveGolangciConfig("/proj", "/ws", "/ext", "/custom/config.yml")
	if got != "/custom/config.yml" {
		t.Errorf("ResolveGolangciConfig explicit = %q, want %q", got, "/custom/config.yml")
	}
}

func TestResolveGolangciConfig_InProjectDir(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(projDir, ".golangci.yml")
	if err := os.WriteFile(configPath, []byte("# config"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveGolangciConfig(projDir, dir, "/ext", "")
	if got != configPath {
		t.Errorf("ResolveGolangciConfig = %q, want %q", got, configPath)
	}
}

func TestResolveGolangciConfig_InParentDir(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "nested", "project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Config exists in workspace root, not in project dir
	configPath := filepath.Join(dir, ".golangci.yml")
	if err := os.WriteFile(configPath, []byte("# config"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveGolangciConfig(projDir, dir, "/ext", "")
	if got != configPath {
		t.Errorf("ResolveGolangciConfig = %q, want %q", got, configPath)
	}
}

func TestResolveGolangciConfig_YAMLExtension(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use .yaml extension instead of .yml
	configPath := filepath.Join(projDir, ".golangci.yaml")
	if err := os.WriteFile(configPath, []byte("# config"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveGolangciConfig(projDir, dir, "/ext", "")
	if got != configPath {
		t.Errorf("ResolveGolangciConfig = %q, want %q", got, configPath)
	}
}

func TestResolveGolangciConfig_FallbackToExtension(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "project")
	extDir := filepath.Join(dir, "ext")
	configDir := filepath.Join(extDir, "config")

	for _, d := range []string{projDir, configDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	configPath := filepath.Join(configDir, ".golangci.yml")
	if err := os.WriteFile(configPath, []byte("# config"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveGolangciConfig(projDir, dir, extDir, "")
	if got != configPath {
		t.Errorf("ResolveGolangciConfig = %q, want %q", got, configPath)
	}
}

func TestResolveGolangciConfig_NoConfig(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "project")
	extDir := filepath.Join(dir, "ext")

	for _, d := range []string{projDir, extDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := ResolveGolangciConfig(projDir, dir, extDir, "")
	if got != "" {
		t.Errorf("ResolveGolangciConfig = %q, want empty", got)
	}
}

func TestResolveGolangciConfig_PreferYML(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Both .yml and .yaml exist; .yml should be preferred (iterated first)
	ymlPath := filepath.Join(projDir, ".golangci.yml")
	yamlPath := filepath.Join(projDir, ".golangci.yaml")
	for _, p := range []string{ymlPath, yamlPath} {
		if err := os.WriteFile(p, []byte("# config"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := ResolveGolangciConfig(projDir, dir, "/ext", "")
	if got != ymlPath {
		t.Errorf("ResolveGolangciConfig = %q, want %q (prefer .yml)", got, ymlPath)
	}
}

// isolateToolHome points the machine tool home at an empty directory.
//
// Without it these cases would read the developer's (or the CI runner's) real
// ~/.putnami/tools/go, which `putnami install` fills with the very versions they
// pin — so a machine that had run install would resolve the machine copy and a
// fresh one would not, and the assertion would depend on the host rather than on
// the code.
func isolateToolHome(t *testing.T) {
	t.Helper()
	t.Setenv(PutnamiHomeEnv, t.TempDir())
}

func TestResolvePinnedToolBinary_RejectsStalePATHAndUsesManagedPin(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "pinned-lint-tools", "a-stale-path-binary-is-replaced-by-the-managed-pin")
	isolateToolHome(t)
	workspace := t.TempDir()
	pathDir := filepath.Join(workspace, "path")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTool := func(path, version string) {
		t.Helper()
		writeFakeProgram(t, path, fakeProgram{
			Answers: []fakeAnswer{{Args: []string{"version"}, Stdout: version + "\n"}},
		})
	}

	writeTool(filepath.Join(pathDir, toolBinaryName("golangci-lint")), "2.9.0")
	managed := ToolHome("golangci-lint")
	writeTool(managed, "2.10.1")
	t.Setenv("PATH", pathDir)

	got, err := ResolvePinnedToolBinary("golangci-lint", workspace)
	if err != nil {
		t.Fatalf("ResolvePinnedToolBinary: %v", err)
	}
	if got != managed {
		t.Errorf("ResolvePinnedToolBinary = %q, want managed pin %q", got, managed)
	}
}

func TestResolvePinnedToolBinary_UsesGolangciLintV2ShortFlag(t *testing.T) {
	isolateToolHome(t)
	workspace := t.TempDir()
	pathDir := filepath.Join(workspace, "path")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tool := writeFakeProgram(t, filepath.Join(pathDir, toolBinaryName("golangci-lint")), fakeProgram{
		Answers:   []fakeAnswer{{Args: []string{"version", "--short"}, Exact: true, Stdout: "2.10.1\n"}},
		Otherwise: fakeAnswer{Stderr: "unsupported arguments\n", Exit: 3},
	})
	t.Setenv("PATH", pathDir)

	got, err := ResolvePinnedToolBinary("golangci-lint", workspace)
	if err != nil {
		t.Fatalf("ResolvePinnedToolBinary: %v", err)
	}
	if got != tool {
		t.Errorf("ResolvePinnedToolBinary = %q, want %q", got, tool)
	}
}

func TestResolvePinnedToolBinary_ReportsExpectedVersion(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "pinned-lint-tools", "the-resolver-reports-the-pinned-version-it-resolved")
	isolateToolHome(t)
	workspace := t.TempDir()
	pathDir := filepath.Join(workspace, "path")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeProgram(t, filepath.Join(pathDir, toolBinaryName("staticcheck")), printing("staticcheck 2025.1.1 (0.6.1)\n"))
	t.Setenv("PATH", pathDir)

	_, err := ResolvePinnedToolBinary("staticcheck", workspace)
	if err == nil {
		t.Fatal("ResolvePinnedToolBinary should reject a stale version")
	}
	if !strings.Contains(err.Error(), "expected v0.7.0") {
		t.Errorf("error = %q, want expected version", err)
	}
}

func TestInstallTool_UnknownTool(t *testing.T) {
	_, err := InstallTool("unknown-tool", "go", "/workspace")
	if err == nil {
		t.Error("InstallTool unknown tool should return error")
	}
	if err.Error() != "unknown tool: unknown-tool" {
		t.Errorf("error = %q, want %q", err.Error(), "unknown tool: unknown-tool")
	}
}

func TestIsExecutable(t *testing.T) {
	dir := t.TempDir()

	// Regular file with execute permission
	execFile := filepath.Join(dir, "exec")
	if err := os.WriteFile(execFile, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutable(execFile) {
		t.Error("file with 0755 should be executable")
	}

	// Regular file without execute permission: Windows records none, so there
	// every regular file is executable.
	noExecFile := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noExecFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := isExecutable(noExecFile), runtime.GOOS == "windows"; got != want {
		t.Errorf("isExecutable(file with 0644) = %v, want %v", got, want)
	}

	// Directory should not be executable
	if isExecutable(dir) {
		t.Error("directory should not be executable")
	}

	// Non-existent file
	if isExecutable(filepath.Join(dir, "nonexistent")) {
		t.Error("nonexistent file should not be executable")
	}
}

func TestGoMajorMinor(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{version: "go1.25.7", want: "1.25"},
		{version: "1.26.1", want: "1.26"},
		{version: "go1.27", want: "1.27"},
		{version: "devel", want: "devel"},
	}

	for _, tt := range tests {
		if got := goMajorMinor(tt.version); got != tt.want {
			t.Errorf("goMajorMinor(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !fileExists(file) {
		t.Error("existing file should return true")
	}
	if fileExists(filepath.Join(dir, "nonexistent")) {
		t.Error("nonexistent file should return false")
	}
	if fileExists(dir) {
		t.Error("directory should return false")
	}
}

func TestAppendEnv_NewKey(t *testing.T) {
	env := []string{"A=1", "B=2"}
	result := appendEnv(env, "C=3")
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3", len(result))
	}
	if result[2] != "C=3" {
		t.Errorf("result[2] = %q, want %q", result[2], "C=3")
	}
}

func TestAppendEnv_ReplaceKey(t *testing.T) {
	env := []string{"A=1", "B=2", "C=3"}
	result := appendEnv(env, "B=99")
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3", len(result))
	}
	if result[1] != "B=99" {
		t.Errorf("result[1] = %q, want %q", result[1], "B=99")
	}
}

func TestUnknownToolError(t *testing.T) {
	err := &unknownToolError{"my-tool"}
	if err.Error() != "unknown tool: my-tool" {
		t.Errorf("Error = %q, want %q", err.Error(), "unknown tool: my-tool")
	}
}

func TestInstallFailedError(t *testing.T) {
	err := &installFailedError{"my-tool"}
	if err.Error() != "failed to install my-tool" {
		t.Errorf("Error = %q, want %q", err.Error(), "failed to install my-tool")
	}
}

func TestResolveGoForUsesExplicitRootsAndTheRequestedProjectRequirement(t *testing.T) {
	// An MCP tool call carries its roots in the request and exports no
	// PUTNAMI_PROJECT_PATH, so resolution must work from explicit inputs alone.
	clearManagedGoResolutionEnv(t)
	workspace := t.TempDir()
	project := filepath.Join(workspace, "api")
	writeTextFile(t, filepath.Join(project, "go.mod"), "module example.com/api\ngo 1.25.7\n")

	stale := writeFakeGoCompiler(t, filepath.Join(t.TempDir(), goBinaryName()), "go1.20.0")
	t.Setenv("PATH", filepath.Dir(stale))
	extensionRoot := t.TempDir()
	managed := writeFakeGoCompiler(t, filepath.Join(extensionRoot, "bin", goBinaryName()), "go1.25.7")

	resolved, err := ResolveGoFor(GoResolution{
		WorkspaceRoot: workspace, ExtensionRoot: extensionRoot, ProjectPath: project,
	})
	if err != nil {
		t.Fatalf("ResolveGoFor: %v", err)
	}
	if resolved != managed {
		t.Fatalf("ResolveGoFor = %q, want the workspace-managed toolchain %q", resolved, managed)
	}

	// The same inputs through the job environment would have found nothing:
	// without the project path there is no requirement, and the stale PATH
	// binary would win.
	if resolved, err := ResolveGo(); err != nil || resolved != stale {
		t.Fatalf("env-only ResolveGo = %q (%v), want the stale PATH binary %q", resolved, err, stale)
	}
}

// Windows has no bin/go link to the last managed install, so the resolver
// takes the managed installs under libs/ themselves, newest release first,
// each judged against the workspace requirement like any other candidate.
// Every other OS keeps resolving through the link alone.
func TestResolveGoOnWindowsFindsTheManagedInstallsWithoutALink(t *testing.T) {
	clearManagedGoResolutionEnv(t)
	t.Setenv("PATH", t.TempDir())
	workspace := t.TempDir()
	writeTextFile(t, filepath.Join(workspace, "go.work"), "go 1.26.1\n")
	libs := managedGoLibs(workspace)
	install := func(dir, name, version string) string {
		return writeFakeGoCompiler(t, filepath.Join(libs, dir, "go", "bin", name), version)
	}
	install("go-1.25.0", "go.exe", "go1.25.0")
	current := install("go-1.26.1", "go.exe", "go1.26.1")
	newest := install("go-1.27.0", "go.exe", "go1.27.0")
	install(".go-1.28.0.stage-1", "go.exe", "go1.28.0")
	install("go-next", "go.exe", "go1.28.0")
	writeTextFile(t, filepath.Join(libs, "go-1.28.0.lock"), "")
	in := GoResolution{WorkspaceRoot: workspace}

	if resolved, err := resolveGoOn("windows", in); err != nil || resolved != newest {
		t.Fatalf("resolveGoOn(windows) = %q (%v), want the newest managed install %q", resolved, err, newest)
	}
	env := []string{workspaceRootEnvVar + "=" + workspace}
	goRoot, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(newest)))
	if err != nil {
		t.Fatal(err)
	}
	if root := managedGoRootOn("windows", newest, env); root != goRoot {
		t.Fatalf("managedGoRootOn(windows) = %q, want the install's GOROOT %q", root, goRoot)
	}

	if err := os.RemoveAll(filepath.Join(libs, "go-1.27.0")); err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveGoOn("windows", in); err != nil || resolved != current {
		t.Fatalf("resolveGoOn(windows) = %q (%v), want the install satisfying go1.26.1 %q", resolved, err, current)
	}

	writeTextFile(t, filepath.Join(workspace, "go.work"), "go 1.26.2\n")
	if _, err := resolveGoOn("windows", in); err == nil || !strings.Contains(err.Error(), current) {
		t.Fatalf("resolveGoOn(windows) = %v, want an error naming the too-old install %q", err, current)
	}

	writeTextFile(t, filepath.Join(workspace, "go.work"), "go 1.25.0\n")
	unix := install("go-1.26.1", "go", "go1.26.1")
	if resolved, err := resolveGoOn("linux", in); err == nil {
		t.Fatalf("resolveGoOn(linux) = %q, want no managed install without the bin/go link", resolved)
	}
	if root := managedGoRootOn("linux", unix, env); root != "" {
		t.Fatalf("managedGoRootOn(linux) = %q, want a managed install outside the candidates", root)
	}
}
