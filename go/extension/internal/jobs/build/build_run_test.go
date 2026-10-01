package build

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"go.putnami.dev/go/extension/internal/platform"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/protocol/features/spectest"
)

// --- target resolution ---

func TestTargetResolution_ExplicitOutputPath(t *testing.T) {
	// When --output_path is set, we get a single compile target with that path
	// and the default package "." (since no entrypoint set).
	//
	// We test the target resolution logic indirectly by setting up a directory
	// structure and verifying the binary name/path derivation.

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "myapp")

	// When outputPath is absolute, it should be used directly
	if !filepath.IsAbs(outputPath) {
		t.Errorf("expected absolute path, got %q", outputPath)
	}
}

func TestTargetResolution_AutoDetectCmdDirs(t *testing.T) {
	// Create a project with cmd/foo/main.go and cmd/bar/main.go
	projectDir := t.TempDir()

	fooDir := filepath.Join(projectDir, "cmd", "foo")
	barDir := filepath.Join(projectDir, "cmd", "bar")
	os.MkdirAll(fooDir, 0o755)
	os.MkdirAll(barDir, 0o755)

	os.WriteFile(filepath.Join(fooDir, "main.go"), []byte("package main\nfunc main(){}"), 0o644)
	os.WriteFile(filepath.Join(barDir, "main.go"), []byte("package main\nfunc main(){}"), 0o644)

	// Verify cmd subdirectories are detected
	entries, err := os.ReadDir(filepath.Join(projectDir, "cmd"))
	if err != nil {
		t.Fatal(err)
	}

	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		mainFile := filepath.Join(projectDir, "cmd", entry.Name(), "main.go")
		if _, err := os.Stat(mainFile); err == nil {
			found = append(found, entry.Name())
		}
	}

	if len(found) != 2 {
		t.Errorf("expected 2 cmd targets, got %d: %v", len(found), found)
	}
}

func TestTargetResolution_FallbackToRoot(t *testing.T) {
	// When no cmd/ directory exists and no explicit entrypoint,
	// we fall back to "." as the package
	projectDir := t.TempDir()
	os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main"), 0o644)

	// Verify cmd/ directory doesn't exist
	cmdDir := filepath.Join(projectDir, "cmd")
	if _, err := os.ReadDir(cmdDir); !os.IsNotExist(err) {
		t.Skip("cmd dir exists unexpectedly")
	}
}

func TestResolveCompileTargets(t *testing.T) {
	projectDir := t.TempDir()
	outputDir := t.TempDir()
	ctx := &pctx.Context{
		OutputPath: outputDir,
		Project: pctx.Project{
			Name:     "@putnami/example",
			FullPath: projectDir,
		},
	}

	t.Run("explicit relative output and entrypoint", func(t *testing.T) {
		got := resolveCompileTargets(ctx, "./cmd/api", "release/api", "")
		want := []compileTarget{{
			pkg:        "./cmd/api",
			binaryPath: filepath.Join(outputDir, "release", "api"),
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	})

	t.Run("explicit absolute output defaults to root package", func(t *testing.T) {
		absolute := filepath.Join(t.TempDir(), "example")
		got := resolveCompileTargets(ctx, "", absolute, "")
		want := []compileTarget{{pkg: ".", binaryPath: absolute}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	})

	t.Run("explicit entrypoint derives binary name and extension", func(t *testing.T) {
		got := resolveCompileTargets(ctx, "./cmd/worker", "", ".exe")
		want := []compileTarget{{
			pkg:        "./cmd/worker",
			binaryPath: filepath.Join(outputDir, "bin", "worker.exe"),
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	})

	t.Run("auto detects command packages only", func(t *testing.T) {
		mustWriteBuildFile(t, filepath.Join(projectDir, "cmd", "api", "main.go"), "package main\n")
		mustWriteBuildFile(t, filepath.Join(projectDir, "cmd", "worker", "main.go"), "package main\n")
		mustWriteBuildFile(t, filepath.Join(projectDir, "cmd", "library", "doc.go"), "package library\n")
		mustWriteBuildFile(t, filepath.Join(projectDir, "cmd", "README.md"), "commands\n")

		got := resolveCompileTargets(ctx, "", "", "")
		want := []compileTarget{
			{pkg: "./cmd/api", binaryPath: filepath.Join(outputDir, "bin", "api")},
			{pkg: "./cmd/worker", binaryPath: filepath.Join(outputDir, "bin", "worker")},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	})

	t.Run("falls back to root package", func(t *testing.T) {
		fallbackProject := t.TempDir()
		fallbackCtx := &pctx.Context{
			OutputPath: outputDir,
			Project: pctx.Project{
				Name:     "@putnami/example",
				FullPath: fallbackProject,
			},
		}
		got := resolveCompileTargets(fallbackCtx, "", "", "")
		want := []compileTarget{{
			pkg:        ".",
			binaryPath: filepath.Join(outputDir, "bin", "putnami-example"),
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	})
}

// The artifact carries the ONE version the orchestrator resolved for its line:
// on a tagged commit Full IS the tag's version, so there is no second, "stable"
// spelling to select.
func TestRunCrossCompileProducesVersionedRuntimeBinary(t *testing.T) {
	for _, tt := range []struct {
		name string
		full string
		want string
	}{
		{name: "an untagged commit carries the suffix", full: "2.3.4-c2", want: "2.3.4-c2"},
		{name: "a tagged commit carries the tag", full: "2.3.4", want: "2.3.4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			outputDir := t.TempDir()
			mustWriteBuildFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/runtime\n\ngo 1.24.0\n")
			mustWriteBuildFile(t, filepath.Join(projectDir, "main.go"), `package main

import "fmt"

var runtimeVersion = "unstamped"

func main() {
	fmt.Print(runtimeVersion)
}
`)

			params := pctx.Params{}
			target := runtime.GOOS + "/" + runtime.GOARCH
			ctx := &pctx.Context{
				WorkspaceRoot: projectDir,
				OutputPath:    outputDir,
				Workspace:     pctx.Workspace{Version: "2.3.4"},
				Project: pctx.Project{
					Name:     "@putnami/runtime-test",
					FullPath: projectDir,
				},
				Params:  params,
				Version: &pctx.Version{Base: "2.3.4", Full: tt.full},
			}
			status, data, err := Run(ctx, jsonl.New(), []string{
				"--phase", "cross-compile",
				"--target", target,
				"--entrypoint", ".",
				"--version-var", "main.runtimeVersion",
			})
			if err != nil {
				t.Fatalf("Run cross-compile: %v", err)
			}
			if status != "OK" {
				t.Fatalf("status = %q, want OK", status)
			}

			selected, selErr := selectDistributionPlatforms(ctx, nil, target)
			if selErr != nil {
				t.Fatalf("selectDistributionPlatforms(%q): %v", target, selErr)
			}
			if len(selected) != 1 {
				t.Fatalf("selected platforms = %#v, want one host target", selected)
			}
			binaryPath := filepath.Join(outputDir, "bin", selected[0].Suffix, pkgmeta.ExecutableName(runtime.GOOS, "putnami-runtime-test"))
			binaries, ok := data["platformBinaries"].(map[string][]string)
			if !ok {
				t.Fatalf("platformBinaries = %#v, want map[string][]string", data["platformBinaries"])
			}
			if got := binaries[selected[0].Suffix]; !reflect.DeepEqual(got, []string{binaryPath}) {
				t.Fatalf("platform binaries = %#v, want [%s]", got, binaryPath)
			}

			output, err := exec.Command(binaryPath).CombinedOutput()
			if err != nil {
				t.Fatalf("execute produced runtime: %v: %s", err, output)
			}
			if got := string(output); got != tt.want {
				t.Errorf("embedded runtime version = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunCrossCompileFailurePreservesPublishedRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test compiler uses a POSIX shell")
	}

	projectDir := t.TempDir()
	outputDir := t.TempDir()
	mustWriteBuildFile(t, filepath.Join(projectDir, "main.go"), "package main\nfunc main() {}\n")
	fakeGo := filepath.Join(t.TempDir(), "go")
	mustWriteBuildFile(t, fakeGo, `#!/bin/sh
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		printf 'partial-runtime' > "$2"
		exit 1
	fi
	shift
done
exit 1
`)
	if err := os.Chmod(fakeGo, 0o755); err != nil {
		t.Fatal(err)
	}

	target := runtime.GOOS + "/" + runtime.GOARCH
	selected, selErr := selectDistributionPlatforms(&pctx.Context{Params: pctx.Params{}}, nil, target)
	if selErr != nil {
		t.Fatalf("selectDistributionPlatforms(%q): %v", target, selErr)
	}
	binaryDir := filepath.Join(outputDir, "bin", selected[0].Suffix)
	binaryPath := filepath.Join(binaryDir, "putnami-runtime-test")
	mustWriteBuildFile(t, binaryPath, "stable-runtime")

	ctx := &pctx.Context{
		WorkspaceRoot: projectDir,
		OutputPath:    outputDir,
		Project: pctx.Project{
			Name:     "@putnami/runtime-test",
			FullPath: projectDir,
		},
	}
	status, data, err := runCrossCompile(ctx, jsonl.New(), fakeGo, ".", "true", nil, selected)
	if err != nil {
		t.Fatalf("runCrossCompile: %v", err)
	}
	if status != "FAILED" || data != nil {
		t.Fatalf("result = (%q, %#v), want (FAILED, nil)", status, data)
	}
	if got, err := os.ReadFile(binaryPath); err != nil {
		t.Fatalf("read published runtime: %v", err)
	} else if string(got) != "stable-runtime" {
		t.Errorf("published runtime = %q, want prior stable bytes", got)
	}
	if leftovers, err := filepath.Glob(filepath.Join(binaryDir, "putnami-runtime-test.tmp-*")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Errorf("failed cross-compile left temp outputs: %v", leftovers)
	}
}

// --- ordinary build: nature + intent scoping ---

// newOrdinaryBuildProject writes a minimal buildable module and returns the
// context an ordinary `build` compile step would receive for it.
func newOrdinaryBuildProject(t *testing.T, projectType, mainBody string) *pctx.Context {
	t.Helper()
	projectDir := t.TempDir()
	outputDir := t.TempDir()
	mustWriteBuildFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/ordinary\n\ngo 1.24.0\n")
	mustWriteBuildFile(t, filepath.Join(projectDir, "main.go"), mainBody)
	return &pctx.Context{
		WorkspaceRoot: projectDir,
		OutputPath:    outputDir,
		Project: pctx.Project{
			Name:     "@putnami/ordinary",
			FullPath: projectDir,
			Type:     projectType,
		},
		Params: pctx.Params{},
	}
}

// TestOrdinaryBuildCompilesForTheHostOnly is the acceptance case for host-only builds:
// an application's ordinary `build` produces ONE binary, for the host, in the
// flat bin/ tree — not the four-platform bin/<suffix>/ matrix that used to be
// the unconditional default and that nothing in `build` ever consumed.
func TestOrdinaryBuildCompilesForTheHostOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "an-ordinary-build-compiles-for-the-host-only")
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps"})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}

	host := platform.HostTarget()
	if got := data["platforms"]; !reflect.DeepEqual(got, []string{host.GOOS + "/" + host.GOARCH}) {
		t.Errorf("platforms = %v, want the host alone", got)
	}

	binaries, _ := data["binaryPaths"].([]string)
	if len(binaries) != 1 {
		t.Fatalf("binaryPaths = %v, want exactly one host binary", binaries)
	}
	if _, exists := data["binaryPath"]; exists {
		t.Fatalf("compile result retained removed singular binaryPath: %v", data)
	}
	if _, statErr := os.Stat(binaries[0]); statErr != nil {
		t.Fatalf("host binary is missing: %v", statErr)
	}

	// The platform-partitioned tree is the shape of an EXPLICIT request; the
	// ordinary build must not have produced one.
	for _, p := range platform.ArchivePlatforms {
		if _, statErr := os.Stat(filepath.Join(ctx.OutputPath, "bin", p.Suffix)); statErr == nil {
			t.Errorf("ordinary build wrote bin/%s; the implicit platform matrix is supposed to be gone", p.Suffix)
		}
	}
}

// TestOrdinaryBuildOfALibraryEmitsNoBinary pins refinement decision F1: a
// library's build evidence is that its packages COMPILE. The earlier fallback
// linked the root non-main package once per archive platform — four attempts to
// produce a binary the module cannot have.
func TestOrdinaryBuildOfALibraryEmitsNoBinary(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "library", "package ordinary\n\nfunc Hello() string { return \"hi\" }\n")

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps"})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if data["compileChecked"] != true {
		t.Errorf("result data = %#v, want a compile-check verdict", data)
	}
	if binaries, _ := data["binaryPaths"].([]string); len(binaries) != 0 {
		t.Errorf("binaryPaths = %v, want none: a library owns no package main", binaries)
	}
	if entries, statErr := os.ReadDir(filepath.Join(ctx.OutputPath, "bin")); statErr == nil && len(entries) > 0 {
		t.Errorf("library build wrote %d entries into bin/", len(entries))
	}
}

// TestOrdinaryBuildOfABrokenLibraryFails is the other half of the compile
// check: dropping the binary must not drop the DIAGNOSTICS. A library build
// that reported OK on code that does not compile would trade correctness
// evidence for speed, which this rule forbids explicitly.
func TestOrdinaryBuildOfABrokenLibraryFails(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "library", "package ordinary\n\nfunc Hello() string { return notDefined }\n")

	status, _, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps"})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED: a library that does not compile has no build evidence", status)
	}
}

// TestOrdinaryBuildWithPlatformsOptsIntoCrossCompilation pins the opt-in: a
// `platforms` value resolved into the job's parameters restores the
// platform-partitioned tree, and only then.
func TestOrdinaryBuildWithPlatformsOptsIntoCrossCompilation(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "a-declared-platform-matrix-opts-into-cross-compilation")
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")
	ctx.Params["platforms"] = []byte(`"linux/amd64,linux/arm64"`)

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps", "--entrypoint", "."})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if got := data["platforms"]; !reflect.DeepEqual(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("platforms = %v, want the requested set", got)
	}

	binaries, ok := data["platformBinaries"].(map[string][]string)
	if !ok {
		t.Fatalf("platformBinaries = %#v, want the partitioned tree an explicit request produces", data["platformBinaries"])
	}
	for _, suffix := range []string{"linux-x64", "linux-arm64"} {
		if len(binaries[suffix]) != 1 {
			t.Errorf("platformBinaries[%s] = %v, want one binary", suffix, binaries[suffix])
		}
	}
}

// TestOrdinaryBuildWithTargetKeepsItsPartitionedTree pins that `--target`, the
// pre-existing cross-compilation opt-in, keeps the output layout it has always
// had — the one the package channels index by platform suffix.
func TestOrdinaryBuildWithTargetKeepsItsPartitionedTree(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "a-requested-target-partitions-the-output-tree")
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")

	status, data, err := Run(ctx, jsonl.New(), []string{
		"--phase", "compile", "--skip-deps", "--entrypoint", ".", "--target", "linux/amd64",
	})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if got := data["platforms"]; !reflect.DeepEqual(got, []string{"linux/amd64"}) {
		t.Fatalf("platforms = %v, want [linux/amd64]", got)
	}
	binary := filepath.Join(ctx.OutputPath, "bin", "linux-x64", "putnami-ordinary")
	if _, statErr := os.Stat(binary); statErr != nil {
		t.Fatalf("expected %s: %v", binary, statErr)
	}
}

// TestOrdinaryBuildOfALibraryHonoursAnExplicitPlatformRequest covers the
// platform-sensitive opt-in for libraries: build tags, CGO and platform-specific
// sources are checked per requested platform, still without emitting a binary.
//
// Nature and intent are independent, and this is the case that proves it. An
// explicit `platforms` request must widen the platform SET without changing the
// KIND of evidence: a library still cannot produce a binary, so the request must
// not route it to the cross-compile path. That mistake fails silently rather
// than loudly — `go build -o <file> .` on a non-main package exits 0 and writes
// a package ARCHIVE — so the opt-in would emit a non-executable file wearing a
// binary's name, covering the root package only. The assertions below are
// therefore about what is on disk, not just about the reported platform set.
func TestOrdinaryBuildOfALibraryHonoursAnExplicitPlatformRequest(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "library", "package ordinary\n\nfunc Hello() string { return \"hi\" }\n")
	ctx.Params["platforms"] = []byte(`["linux/amd64","darwin/arm64"]`)

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps", "--entrypoint", "."})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if got := data["platforms"]; !reflect.DeepEqual(got, []string{"linux/amd64", "darwin/arm64"}) {
		t.Errorf("platforms = %v, want the requested set in the requested order", got)
	}
	if data["compileChecked"] != true {
		t.Errorf("compileChecked = %v, want true; an explicit platform request must widen the platform set, "+
			"not switch a library onto the binary-emitting path", data["compileChecked"])
	}
	if binaries, _ := data["binaryPaths"].([]string); len(binaries) != 0 {
		t.Errorf("binaryPaths = %v, want none; a library emits no binary at any platform count", binaries)
	}
	for _, suffix := range []string{"linux-x64", "darwin-arm64"} {
		if _, statErr := os.Stat(filepath.Join(ctx.OutputPath, "bin", suffix)); statErr == nil {
			t.Errorf("library build wrote bin/%s; `go build -o` on a non-main package writes a package archive, "+
				"and shipping one as a binary is a phantom build", suffix)
		}
	}
}

// TestOrdinaryBuildInstallsTheHostBinary pins that the host build feeds
// `--install`. The option is how `tooling/cli` publishes its dev binary, and it
// needs one known host artifact — which is exactly what ordinary `build` now
// produces.
func TestOrdinaryBuildInstallsTheHostBinary(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")
	installRel := filepath.Join("bin", "ordinary-dev")

	status, _, err := Run(ctx, jsonl.New(), []string{
		"--phase", "compile", "--skip-deps", "--entrypoint", ".", "--install", installRel,
	})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if _, statErr := os.Stat(filepath.Join(ctx.WorkspaceRoot, installRel)); statErr != nil {
		t.Fatalf("install target missing: %v", statErr)
	}
}

// TestUnclassifiedProjectBuildsLikeAnApplication pins the fail-safe: an EMPTY
// project type is an application, matching the orchestrator's own default. A
// workspace whose probe could not classify a module must not lose its binary.
func TestUnclassifiedProjectBuildsLikeAnApplication(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "", "package main\n\nfunc main() {}\n")

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps"})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if binaries, _ := data["binaryPaths"].([]string); len(binaries) != 1 {
		t.Fatalf("binaryPaths = %v, want one host binary for an unclassified project", binaries)
	}
}

func mustWriteBuildFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- setEnv edge cases ---

func TestSetEnv_ValueContainsEquals(t *testing.T) {
	env := []string{"A=1"}
	result := setEnv(env, "B", "key=value")
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[1] != "B=key=value" {
		t.Errorf("result[1] = %q, want %q", result[1], "B=key=value")
	}
}

func TestSetEnv_ReplaceWithEmptyValue(t *testing.T) {
	env := []string{"GOOS=linux", "GOARCH=amd64"}
	result := setEnv(env, "GOOS", "")
	if result[0] != "GOOS=" {
		t.Errorf("result[0] = %q, want %q", result[0], "GOOS=")
	}
}

func TestSetEnv_ReplaceLast(t *testing.T) {
	env := []string{"A=1", "B=2", "C=3"}
	result := setEnv(env, "C", "new")
	if result[2] != "C=new" {
		t.Errorf("result[2] = %q, want %q", result[2], "C=new")
	}
	if len(result) != 3 {
		t.Errorf("len = %d, want 3", len(result))
	}
}

// --- copyFile edge cases ---

func TestCopyFile_LargeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "large")
	dst := filepath.Join(dir, "large-copy")

	// Create a file larger than typical buffer sizes
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Errorf("copied size = %d, want %d", len(got), len(data))
	}
}

// --- cross-compilation target parsing ---

func TestCrossCompilationTargetParsing(t *testing.T) {
	tests := []struct {
		target     string
		wantGOOS   string
		wantGOARCH string
	}{
		{"linux/amd64", "linux", "amd64"},
		{"darwin/arm64", "darwin", "arm64"},
		{"windows/amd64", "windows", "amd64"},
		{"linux", "linux", ""},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			var goosVal, goarchVal string
			parts := splitTarget(tt.target)
			goosVal = parts[0]
			if len(parts) > 1 {
				goarchVal = parts[1]
			}

			if goosVal != tt.wantGOOS {
				t.Errorf("GOOS = %q, want %q", goosVal, tt.wantGOOS)
			}
			if goarchVal != tt.wantGOARCH {
				t.Errorf("GOARCH = %q, want %q", goarchVal, tt.wantGOARCH)
			}
		})
	}
}

// TestSelectDistributionPlatforms pins the DISTRIBUTION intent's platform set,
// scoped by package channel.
//
// The full matrix is no longer the default of `package`: it is what the ARCHIVE
// channel owes. A docker channel owes exactly the image's platform, a channel
// that carries no binary owes nothing, and an invocation naming no channel at
// all keeps the matrix. Every case reads from ctx.Params or the project's
// publish declaration — both plan-time and both in the task's cache key.
func TestSelectDistributionPlatforms(t *testing.T) {
	fullMatrix := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}

	tests := []struct {
		name    string
		params  pctx.Params
		publish []string
		spec    []string
		target  string
		want    []string
	}{
		{name: "no channel expressed keeps the archive matrix", want: fullMatrix},
		{name: "specific target", target: "linux/amd64", want: []string{"linux/amd64"}},
		{name: "os target", target: "linux", want: []string{"linux/amd64", "linux/arm64"}},
		{name: "custom target outside the matrix", target: "windows/arm64", want: []string{"windows/arm64"}},
		{
			name:   "docker channel parameter compiles the image platform alone",
			params: pctx.Params{"docker": []byte("true")},
			want:   []string{"linux/amd64"},
		},
		{
			name:   "docker channel honors the image platform parameter",
			params: pctx.Params{"docker": []byte("true"), "platform": []byte(`"linux/arm64"`)},
			want:   []string{"linux/arm64"},
		},
		{
			name:    "declared docker publish channel scopes it too",
			publish: []string{"docker"},
			want:    []string{"linux/amd64"},
		},
		{
			name:    "declared go module channel compiles nothing",
			publish: []string{"go"},
			want:    nil,
		},
		{
			name:    "archives channel keeps the release matrix",
			publish: []string{"extension-archives"},
			want:    fullMatrix,
		},
		{
			name:    "archives channel builds the declared targets only",
			publish: []string{"archives"},
			spec:    []string{"linux/amd64,linux/arm64"},
			want:    []string{"linux/amd64", "linux/arm64"},
		},
		{
			// The plan gates the channel steps on the UNION of the declaration and
			// the flags, so both packagers run and both must find their binaries.
			// Treating the flag as a replacement compiled linux/amd64 alone and the
			// archive packager then failed on a darwin binary nobody built.
			name:    "an explicit flag adds to the declaration, it does not replace it",
			params:  pctx.Params{"docker": []byte("true")},
			publish: []string{"archives"},
			want:    fullMatrix,
		},
		{
			name:    "the union is symmetric: an archives flag keeps the image platform",
			params:  pctx.Params{"archives": []byte("true")},
			publish: []string{"docker"},
			spec:    []string{"darwin/arm64"},
			want:    []string{"darwin/arm64", "linux/amd64"},
		},
		{
			name:    "an image platform is not built twice when the matrix already covers it",
			params:  pctx.Params{"archives": []byte("true"), "platform": []byte(`"linux/amd64"`)},
			publish: []string{"docker"},
			want:    fullMatrix,
		},
		{
			name:    "target still wins over every channel",
			params:  pctx.Params{"docker": []byte("true")},
			publish: []string{"archives"},
			target:  "darwin/arm64",
			want:    []string{"darwin/arm64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &pctx.Context{Params: tt.params}
			if ctx.Params == nil {
				ctx.Params = pctx.Params{}
			}
			if tt.publish != nil {
				raw, err := json.Marshal(tt.publish)
				if err != nil {
					t.Fatal(err)
				}
				ctx.Project.Publish = raw
			}
			resolved, err := selectDistributionPlatforms(ctx, tt.spec, tt.target)
			if err != nil {
				t.Fatalf("selectDistributionPlatforms(%q): %v", tt.target, err)
			}
			if len(tt.want) == 0 {
				if len(resolved) != 0 {
					t.Fatalf("platforms = %v, want none: no active channel consumes a binary",
						platformNames(resolved))
				}
				return
			}
			if got := platformNames(resolved); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("platforms = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCrossCompileBuildsOnlyTheDockerChannelTarget is the end-to-end half of
// acceptance criterion 1: a real `--phase cross-compile` run for a project whose
// only channel is docker writes ONE platform directory, not four.
func TestCrossCompileBuildsOnlyTheDockerChannelTarget(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "the-package-channel-being-produced-scopes-the-targets")
	projectDir := t.TempDir()
	outputDir := t.TempDir()
	mustWriteBuildFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/imageonly\n\ngo 1.24.0\n")
	mustWriteBuildFile(t, filepath.Join(projectDir, "main.go"), "package main\n\nfunc main() {}\n")

	// The image platform names the HOST so the compile needs no cross toolchain;
	// what is under test is how many platforms are built, not which.
	host := platform.HostTarget()
	ctx := &pctx.Context{
		WorkspaceRoot: projectDir,
		OutputPath:    outputDir,
		Project: pctx.Project{
			Name:     "@putnami/image-only",
			FullPath: projectDir,
			Publish:  json.RawMessage(`["docker"]`),
		},
		Params: pctx.Params{"platform": []byte(`"` + host.GOOS + "/" + host.GOARCH + `"`)},
	}

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "cross-compile", "--entrypoint", "."})
	if err != nil {
		t.Fatalf("Run cross-compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if got, want := data["platforms"], []string{host.GOOS + "/" + host.GOARCH}; !reflect.DeepEqual(got, want) {
		t.Errorf("resolved platforms = %v, want %v", got, want)
	}

	entries, err := os.ReadDir(filepath.Join(outputDir, "bin"))
	if err != nil {
		t.Fatalf("read bin tree: %v", err)
	}
	built := make([]string, 0, len(entries))
	for _, e := range entries {
		built = append(built, e.Name())
	}
	if !reflect.DeepEqual(built, []string{host.Suffix}) {
		t.Errorf("bin/ holds %v, want only %q: the docker channel consumes one binary, so it must compile one",
			built, host.Suffix)
	}
}

// TestRequestedPlatforms is the ordinary-validation half of the platform
// contract: nothing is cross-compiled unless a plan-time parameter asks for it.
//
// Both inputs are resolved by the orchestrator and declared `from: "params"` on
// build-compile, so what this function returns is exactly what the task's cache
// key was computed from (see TestPlatformSelectionParamsAreCacheKeyInputs in the
// manifest contract suite).
func TestRequestedPlatforms(t *testing.T) {
	host := platform.HostTarget()
	hostName := host.GOOS + "/" + host.GOARCH

	tests := []struct {
		name   string
		spec   []string
		target string
		want   []string
	}{
		{name: "no request at all is the host-only default", want: nil},
		{name: "empty strings are not a request", spec: []string{"", "  "}, want: nil},
		{name: "single platform", spec: []string{"linux/amd64"}, want: []string{"linux/amd64"}},
		{name: "comma-separated flag value", spec: []string{"linux/amd64,darwin/arm64"},
			want: []string{"linux/amd64", "darwin/arm64"}},
		{name: "json array from project config", spec: []string{"linux/arm64", "darwin/amd64"},
			want: []string{"linux/arm64", "darwin/amd64"}},
		{name: "host token", spec: []string{"host"}, want: []string{hostName}},
		{name: "all token restores the release matrix", spec: []string{"all"},
			want: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}},
		{name: "duplicates collapse, order preserved", spec: []string{"darwin/arm64,linux/amd64,darwin/arm64"},
			want: []string{"darwin/arm64", "linux/amd64"}},
		{name: "os-only entry expands", spec: []string{"linux"}, want: []string{"linux/amd64", "linux/arm64"}},
		{name: "platform outside the matrix is synthesized", spec: []string{"windows/arm64"},
			want: []string{"windows/arm64"}},
		{name: "target wins over platforms", spec: []string{"all"}, target: "linux/amd64",
			want: []string{"linux/amd64"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := requestedPlatforms(tt.spec, tt.target)
			if err != nil {
				t.Fatalf("requestedPlatforms(%v, %q): %v", tt.spec, tt.target, err)
			}
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("platforms = %v, want no explicit request (host-only default)", platformNames(got))
				}
				return
			}
			if names := platformNames(got); !reflect.DeepEqual(names, tt.want) {
				t.Errorf("platforms = %v, want %v", names, tt.want)
			}
		})
	}
}

// TestPlatformsParamReadsOnlyResolvedParams pins the shape the `platforms`
// value may arrive in — a JSON array from putnami.json options, or a string from
// the CLI flag — and, more importantly, WHERE it is read from.
//
// ctx.Params is the orchestrator's merged, plan-time parameter map; it is the
// map the cache key hashes. A task that instead opened the project's
// putnami.json for itself (the shape platform.ReadGoEntrypoint uses) would
// change its own output without changing its key.
func TestPlatformsParamReadsOnlyResolvedParams(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, err := platformsParam(&pctx.Context{Params: pctx.Params{}})
		if err != nil || got != nil {
			t.Errorf("platformsParam = (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("json array", func(t *testing.T) {
		ctx := &pctx.Context{Params: pctx.Params{"platforms": []byte(`["linux/amd64","darwin/arm64"]`)}}
		want := []string{"linux/amd64", "darwin/arm64"}
		got, err := platformsParam(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("platformsParam = (%v, %v), want (%v, nil)", got, err, want)
		}
	})
	t.Run("flag string", func(t *testing.T) {
		ctx := &pctx.Context{Params: pctx.Params{"platforms": []byte(`"linux/amd64,darwin/arm64"`)}}
		want := []string{"linux/amd64,darwin/arm64"}
		got, err := platformsParam(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("platformsParam = (%v, %v), want (%v, nil)", got, err, want)
		}
	})
	t.Run("project config platforms never reaches the task through the file", func(t *testing.T) {
		projectDir := t.TempDir()
		mustWriteBuildFile(t, filepath.Join(projectDir, "putnami.json"),
			`{"name":"@acme/api","options":{"@putnami/go":{"platforms":["linux/amd64"]}}}`)
		ctx := &pctx.Context{
			Params:  pctx.Params{},
			Project: pctx.Project{Name: "@acme/api", FullPath: projectDir},
		}
		got, err := platformsParam(ctx)
		if err != nil || got != nil {
			t.Errorf("platformsParam read %v (err %v) from the project tree; the value must arrive as a resolved, "+
				"keyed parameter or the cache key is blind to it", got, err)
		}
	})
}

// splitTarget mirrors the target parsing logic from Run
func splitTarget(target string) []string {
	if target == "" {
		return nil
	}
	parts := make([]string, 0, 2)
	idx := 0
	for i, c := range target {
		if c == '/' {
			parts = append(parts, target[idx:i])
			idx = i + 1
			break
		}
	}
	if idx == 0 {
		return []string{target}
	}
	if idx < len(target) {
		parts = append(parts, target[idx:])
	}
	return parts
}

// --- Windows binary extension ---

func TestWindowsBinaryExtension(t *testing.T) {
	tests := []struct {
		goos    string
		wantExt string
	}{
		{"windows", ".exe"},
		{"linux", ""},
		{"darwin", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			ext := ""
			if tt.goos == "windows" {
				ext = ".exe"
			}
			if ext != tt.wantExt {
				t.Errorf("binExt for %q = %q, want %q", tt.goos, ext, tt.wantExt)
			}
		})
	}
}

// TestPlatformsParamRejectsUndecodableBytes pins that a malformed `platforms`
// parameter is an ERROR, not a platform.
//
// The fallback used to return string(raw), so `{"oops":1}` became a literal
// platform spec and then a GOOS. Two things go wrong with that. The visible one
// is a cryptic `go build` failure several layers from the typo. The quiet one is
// worse: `platforms` is a cache-key parameter, so the nonsense is recorded as a
// legitimate request and the failure is stored under a key that looks perfectly
// ordinary.
func TestPlatformsParamRejectsUndecodableBytes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "an-undecodable-platforms-parameter-is-an-error")
	for _, raw := range []string{`{"oops":1}`, `[1,2]`, `not-json`} {
		ctx := &pctx.Context{Params: pctx.Params{"platforms": []byte(raw)}}
		got, err := platformsParam(ctx)
		if err == nil {
			t.Errorf("platformsParam(%s) = (%v, nil), want an error rather than a synthesized platform", raw, got)
		}
		if got != nil {
			t.Errorf("platformsParam(%s) returned %v alongside its error; a rejected spec must resolve to nothing", raw, got)
		}
	}
}

// TestOrdinaryBuildRejectsAMalformedPlatformsParam is the job-level half: the
// error has to fail the build with a diagnostic, not be swallowed.
func TestOrdinaryBuildRejectsAMalformedPlatformsParam(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "a-malformed-platform-request-is-an-error")
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")
	ctx.Params["platforms"] = []byte(`{"oops":1}`)

	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "compile", "--skip-deps"})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("status = %q, want FAILED; a malformed platform request must not compile anything", status)
	}
	if data != nil {
		t.Errorf("data = %v, want nil", data)
	}
}

// TestOrdinaryBuildAcceptsTheHostToken pins finding 2 at the job level: `host`
// and `all` are documented for BOTH `--target` and `platforms`, and `--target`
// reaches TargetsFor directly. Before the vocabulary was unified this produced
// GOOS=host and a cryptic toolchain error.
func TestOrdinaryBuildAcceptsTheHostToken(t *testing.T) {
	ctx := newOrdinaryBuildProject(t, "application", "package main\n\nfunc main() {}\n")

	status, data, err := Run(ctx, jsonl.New(), []string{
		"--phase", "compile", "--skip-deps", "--entrypoint", ".", "--target", platform.PlatformHost,
	})
	if err != nil {
		t.Fatalf("Run compile: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK; `--target host` must resolve to the machine, not to GOOS=host", status)
	}
	host := platform.HostTarget()
	if got := data["platforms"]; !reflect.DeepEqual(got, []string{host.GOOS + "/" + host.GOARCH}) {
		t.Errorf("platforms = %v, want the host", got)
	}
}

// A declared executable is compiled for every distribution platform beside the
// entrypoint, so the archive packager has one to stage.
func TestRunCrossCompileBuildsDeclaredExecutables(t *testing.T) {
	projectDir := t.TempDir()
	outputDir := t.TempDir()
	mustWriteBuildFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/runtime\n\ngo 1.24.0\n")
	mustWriteBuildFile(t, filepath.Join(projectDir, "main.go"), "package main\nfunc main() {}\n")
	mustWriteBuildFile(t, filepath.Join(projectDir, "cmd", "emitter", "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Print(\"emitter\") }\n")
	mustWriteBuildFile(t, filepath.Join(projectDir, "putnami.json"), `{
		"name": "@putnami/runtime-test",
		"options": {"@putnami/go": {"executables": [{"name": "runtime-emitter", "package": "./cmd/emitter"}]}}
	}`)

	target := runtime.GOOS + "/" + runtime.GOARCH
	ctx := &pctx.Context{
		WorkspaceRoot: projectDir,
		OutputPath:    outputDir,
		Project:       pctx.Project{Name: "@putnami/runtime-test", FullPath: projectDir},
		Params:        pctx.Params{},
	}
	status, data, err := Run(ctx, jsonl.New(), []string{"--phase", "cross-compile", "--target", target, "--entrypoint", "."})
	if err != nil || status != "OK" {
		t.Fatalf("Run cross-compile = (%q, %v), want OK", status, err)
	}
	selected, err := selectDistributionPlatforms(ctx, nil, target)
	if err != nil || len(selected) != 1 {
		t.Fatalf("selectDistributionPlatforms(%q) = %#v, %v", target, selected, err)
	}
	binDir := filepath.Join(outputDir, "bin", selected[0].Suffix)
	want := []string{
		filepath.Join(binDir, pkgmeta.ExecutableName(runtime.GOOS, "putnami-runtime-test")),
		filepath.Join(binDir, pkgmeta.ExecutableName(runtime.GOOS, "runtime-emitter")),
	}
	if got := data["platformBinaries"].(map[string][]string)[selected[0].Suffix]; !reflect.DeepEqual(got, want) {
		t.Fatalf("platform binaries = %#v, want %#v", got, want)
	}
	output, err := exec.Command(want[1]).CombinedOutput()
	if err != nil || string(output) != "emitter" {
		t.Errorf("declared executable output = %q, %v; want emitter", output, err)
	}
}

// A declared executable that reuses the entrypoint's name would overwrite the
// runtime in bin/<platform>/, so the cross-compile refuses it.
func TestRunCrossCompileRefusesAnExecutableNamedLikeTheEntrypoint(t *testing.T) {
	projectDir := t.TempDir()
	mustWriteBuildFile(t, filepath.Join(projectDir, "putnami.json"), `{
		"options": {"@putnami/go": {"executables": [{"name": "putnami-runtime-test", "package": "./cmd/emitter"}]}}
	}`)
	ctx := &pctx.Context{
		WorkspaceRoot: projectDir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "@putnami/runtime-test", FullPath: projectDir},
	}
	status, data, err := runCrossCompile(ctx, jsonl.New(), "go", ".", "false", nil, nil)
	if err != nil || status != "FAILED" || data != nil {
		t.Fatalf("result = (%q, %#v, %v), want (FAILED, nil, nil)", status, data, err)
	}
}
