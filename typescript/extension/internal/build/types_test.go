package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

func TestDiscoverSourceFiles_FindsTSFiles(t *testing.T) {
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "a.ts"), []byte("export const a = 1;"), 0644)
	os.WriteFile(filepath.Join(srcDir, "b.tsx"), []byte("export const B = () => null;"), 0644)

	files := discoverSourceFiles(projectDir)
	sort.Strings(files)

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != filepath.Join("src", "a.ts") {
		t.Errorf("files[0] = %q, want %q", files[0], filepath.Join("src", "a.ts"))
	}
	if files[1] != filepath.Join("src", "b.tsx") {
		t.Errorf("files[1] = %q, want %q", files[1], filepath.Join("src", "b.tsx"))
	}
}

func TestDiscoverSourceFiles_ExcludesDTS(t *testing.T) {
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "a.d.ts"), []byte("declare module 'a';"), 0644)

	files := discoverSourceFiles(projectDir)
	if len(files) != 0 {
		t.Errorf("expected 0 files (d.ts excluded), got %d: %v", len(files), files)
	}
}

func TestDiscoverSourceFiles_ExcludesTestFiles(t *testing.T) {
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "a.test.ts"), []byte("test('a', () => {});"), 0644)
	os.WriteFile(filepath.Join(srcDir, "b.spec.ts"), []byte("test('b', () => {});"), 0644)

	files := discoverSourceFiles(projectDir)
	if len(files) != 0 {
		t.Errorf("expected 0 files (test/spec excluded), got %d: %v", len(files), files)
	}
}

func TestDiscoverSourceFiles_IncludesBinDir(t *testing.T) {
	projectDir := t.TempDir()
	binDir := filepath.Join(projectDir, "bin")
	os.MkdirAll(binDir, 0755)
	os.WriteFile(filepath.Join(binDir, "cli.ts"), []byte("console.log('cli');"), 0644)

	files := discoverSourceFiles(projectDir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files[0] != filepath.Join("bin", "cli.ts") {
		t.Errorf("files[0] = %q, want %q", files[0], filepath.Join("bin", "cli.ts"))
	}
}

func TestDiscoverSourceFiles_EmptyProject(t *testing.T) {
	projectDir := t.TempDir()
	// No src/ or bin/ directories

	files := discoverSourceFiles(projectDir)
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty project, got %d: %v", len(files), files)
	}
}

// ---- hasSourceFiles ----

func TestHasSourceFiles_FindsSrc(t *testing.T) {
	projectDir := t.TempDir()
	os.MkdirAll(filepath.Join(projectDir, "src"), 0755)
	os.WriteFile(filepath.Join(projectDir, "src", "a.ts"), []byte("export const a = 1;"), 0644)

	if !hasSourceFiles(projectDir) {
		t.Error("expected hasSourceFiles=true when src contains a .ts file")
	}
}

func TestHasSourceFiles_FindsBin(t *testing.T) {
	projectDir := t.TempDir()
	os.MkdirAll(filepath.Join(projectDir, "bin"), 0755)
	os.WriteFile(filepath.Join(projectDir, "bin", "cli.ts"), []byte("console.log('cli');"), 0644)

	if !hasSourceFiles(projectDir) {
		t.Error("expected hasSourceFiles=true when bin contains a .ts file")
	}
}

func TestHasSourceFiles_EmptyProject(t *testing.T) {
	projectDir := t.TempDir()
	if hasSourceFiles(projectDir) {
		t.Error("expected hasSourceFiles=false for empty project")
	}
}

func TestHasSourceFiles_IgnoresDTSAndTests(t *testing.T) {
	projectDir := t.TempDir()
	os.MkdirAll(filepath.Join(projectDir, "src"), 0755)
	os.WriteFile(filepath.Join(projectDir, "src", "a.d.ts"), []byte("declare const a: number;"), 0644)
	os.WriteFile(filepath.Join(projectDir, "src", "a.test.ts"), []byte("test('a', () => {});"), 0644)

	if hasSourceFiles(projectDir) {
		t.Error("expected hasSourceFiles=false when only .d.ts/test files are present")
	}
}

func TestHasSourceFiles_AgreesWithDiscover(t *testing.T) {
	// hasSourceFiles must report true iff discoverSourceFiles finds files.
	withSrc := t.TempDir()
	os.MkdirAll(filepath.Join(withSrc, "src"), 0755)
	os.WriteFile(filepath.Join(withSrc, "src", "a.ts"), []byte("export const a = 1;"), 0644)
	os.WriteFile(filepath.Join(withSrc, "src", "b.ts"), []byte("export const b = 2;"), 0644)

	without := t.TempDir()
	os.MkdirAll(filepath.Join(without, "src"), 0755)
	os.WriteFile(filepath.Join(without, "src", "a.d.ts"), []byte("declare const a: number;"), 0644)

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"with-src", withSrc},
		{"without-src", without},
	} {
		discovered := discoverSourceFiles(tc.dir)
		if got := hasSourceFiles(tc.dir); got != (len(discovered) > 0) {
			t.Errorf("%s: hasSourceFiles=%v but discoverSourceFiles found %d files", tc.name, got, len(discovered))
		}
	}
}

// TestRunTypes_WithTsconfigSkipsWhenNoSources verifies the tsconfig branch
// uses the emptiness gate: a project with a tsconfig but no source files must
// report success without invoking tsc.
func TestRunTypes_WithTsconfigSkipsWhenNoSources(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0644)
	// src/ exists but contains no eligible source files.
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "types.d.ts"), []byte("declare const x: number;"), 0644)

	called := false
	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		called = true
		return &exec.Result{Success: true}, nil
	})

	result, err := RunTypes("bun", dir, filepath.Join(dir, "out"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success when there are no source files")
	}
	if called {
		t.Error("tsc should not be invoked when there are no eligible source files")
	}
}

// ---- buildTscArgs ----

func TestBuildTscArgs_Basic(t *testing.T) {
	args := buildTscArgs("/output", "", []string{"src/index.ts"}, "", "")
	if args[0] != "x" || args[1] != "tsc" {
		t.Errorf("expected first args 'x tsc', got %q %q", args[0], args[1])
	}
	// Check --outDir is set
	foundOutDir := false
	for i, a := range args {
		if a == "--outDir" && i+1 < len(args) && args[i+1] == "/output" {
			foundOutDir = true
		}
	}
	if !foundOutDir {
		t.Error("expected --outDir /output")
	}
	// Last arg should be the source file
	if args[len(args)-1] != "src/index.ts" {
		t.Errorf("expected last arg 'src/index.ts', got %q", args[len(args)-1])
	}
}

func TestBuildTscArgs_WithTsconfig(t *testing.T) {
	args := buildTscArgs("/output", "/project/tsconfig.json", []string{"src/index.ts"}, "", "")
	// Should use --project instead of source files
	argSet := make(map[string]bool)
	for _, a := range args {
		argSet[a] = true
	}
	if !argSet["--project"] {
		t.Error("expected --project flag when tsconfig is provided")
	}
	// Should NOT contain source files
	if args[len(args)-1] == "src/index.ts" {
		t.Error("expected source files to be omitted when tsconfig is provided")
	}
	// Should contain the tsconfig path
	foundTsconfig := false
	for i, a := range args {
		if a == "--project" && i+1 < len(args) && args[i+1] == "/project/tsconfig.json" {
			foundTsconfig = true
		}
	}
	if !foundTsconfig {
		t.Error("expected --project /project/tsconfig.json")
	}
}

func TestBuildTscArgs_MultipleSourceFiles(t *testing.T) {
	files := []string{"src/a.ts", "src/b.tsx", "bin/cli.ts"}
	args := buildTscArgs("/out", "", files, "", "")
	// Last 3 args should be the source files
	last3 := args[len(args)-3:]
	for i, f := range files {
		if last3[i] != f {
			t.Errorf("arg[%d] = %q, want %q", i, last3[i], f)
		}
	}
}

func TestBuildTscArgs_ForcesPlainOutput(t *testing.T) {
	args := buildTscArgs("/out", "", []string{"src/index.ts"}, "", "")
	found := false
	for i, a := range args {
		if a == "--pretty" && i+1 < len(args) && args[i+1] == "false" {
			found = true
		}
	}
	if !found {
		t.Error("expected '--pretty false' so tsc emits plain, parseable diagnostics")
	}
}

func TestBuildTscArgs_ContainsRequiredFlags(t *testing.T) {
	args := buildTscArgs("/out", "", []string{"src/index.ts"}, "", "")
	requiredFlags := []string{
		"--declaration",
		"--emitDeclarationOnly",
		"--declarationMap",
		"--experimentalDecorators",
	}
	argSet := make(map[string]bool)
	for _, a := range args {
		argSet[a] = true
	}
	for _, flag := range requiredFlags {
		if !argSet[flag] {
			t.Errorf("expected %s in args", flag)
		}
	}
}

// TestBuildTscArgs_IncrementalOnlyWithBuildInfoFile pins that --incremental and
// --tsBuildInfoFile are emitted ONLY when a warm buildinfo path is supplied, and
// compose with (do not replace) --emitDeclarationOnly. An empty buildinfo path
// keeps the cold flag set byte-for-byte.
func TestBuildTscArgs_IncrementalOnlyWithBuildInfoFile(t *testing.T) {
	cold := buildTscArgs("/out", "", []string{"src/a.ts"}, "", "")
	if argsHaveFlag(cold, "--incremental") {
		t.Error("expected no --incremental when tsBuildInfoFile is empty")
	}
	if argsHaveFlag(cold, "--tsBuildInfoFile") {
		t.Error("expected no --tsBuildInfoFile when empty")
	}

	warm := buildTscArgs("/out", "", []string{"src/a.ts"}, "/cache/types.tsbuildinfo", "")
	if !argsHaveFlag(warm, "--incremental") {
		t.Error("expected --incremental when a buildinfo file is provided")
	}
	if got := argValueOf(warm, "--tsBuildInfoFile"); got != "/cache/types.tsbuildinfo" {
		t.Errorf("--tsBuildInfoFile = %q, want %q", got, "/cache/types.tsbuildinfo")
	}
	if !argsHaveFlag(warm, "--emitDeclarationOnly") {
		t.Error("expected --emitDeclarationOnly to remain alongside --incremental")
	}
}

// ---- incremental seed/mirror behavior ----

// argValueOf returns the value following flag in args, or "" if absent.
func argValueOf(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// argsHaveFlag reports whether flag appears anywhere in args.
func argsHaveFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// declSet returns the set of .d.ts files under dir, keyed by path relative to dir.
func declSet(t *testing.T, dir string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".d.ts") {
			rel, _ := filepath.Rel(dir, path)
			got[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return got
}

func assertDeclSet(t *testing.T, dir string, want ...string) {
	t.Helper()
	got := declSet(t, dir)
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected %q under %s, got set %v", w, dir, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("declaration set in %s = %v, want exactly %v", dir, got, want)
	}
}

// TestRunTypes_IncrementalSeedsCompleteSetOnPartialEmit is the anti-partial-emit
// gate for the seed/mirror design. A warm incremental run re-emits ONLY the
// changed file, yet the captured outputPath — which the scheduler clears every
// miss — must still contain the COMPLETE declaration set. RunTypes achieves this
// by SEEDing outputPath from the persistent mirror before tsc runs, while tsc
// still emits directly into outputPath so declaration maps keep correct
// outputPath-relative source paths. The mirror is REFRESHed afterward.
func TestRunTypes_IncrementalSeedsCompleteSetOnPartialEmit(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "b.ts"), []byte("export const b = 2;"), 0o644)

	warmDir := filepath.Join(t.TempDir(), "ts-types", "build-types", "proj")
	capturedOut := filepath.Join(t.TempDir(), "out", "types")

	// Pre-populate the persistent mirror with the COMPLETE prior set and a warm
	// buildinfo, exactly as a previous run would have left them.
	mirrorDir := filepath.Join(warmDir, "mirror")
	os.MkdirAll(filepath.Join(mirrorDir, "src"), 0o755)
	os.WriteFile(filepath.Join(mirrorDir, "src", "a.d.ts"), []byte("export declare const a: number;"), 0o644)
	os.WriteFile(filepath.Join(mirrorDir, "src", "b.d.ts"), []byte("export declare const b: number;"), 0o644)
	buildInfoPath := filepath.Join(warmDir, "types.tsbuildinfo")
	os.WriteFile(buildInfoPath, []byte(`{"warm":true}`), 0o644)

	const aNew = "export declare const a: 1;"
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		// Map-correctness invariant: tsc MUST emit at the captured outputPath, never
		// a relocated warm dir, so emitted .d.ts.map `sources` paths stay valid.
		if got := argValueOf(args, "--outDir"); got != capturedOut {
			t.Errorf("--outDir = %q, want captured outputPath %q", got, capturedOut)
		}
		if !argsHaveFlag(args, "--incremental") {
			t.Error("expected --incremental in args")
		}
		if got := argValueOf(args, "--tsBuildInfoFile"); got != buildInfoPath {
			t.Errorf("--tsBuildInfoFile = %q, want warm buildinfo %q", got, buildInfoPath)
		}
		outDir := argValueOf(args, "--outDir")
		// The seed must have restored the prior complete set into outputPath BEFORE
		// tsc runs, so a partial-emit run can leave b.d.ts untouched.
		if _, err := os.Stat(filepath.Join(outDir, "src", "b.d.ts")); err != nil {
			t.Errorf("expected b.d.ts seeded into outputPath before tsc runs: %v", err)
		}
		// Partial-emit: re-emit ONLY the changed a.d.ts.
		os.WriteFile(filepath.Join(outDir, "src", "a.d.ts"), []byte(aNew), 0o644)
		return &exec.Result{Success: true}, nil
	})

	res, err := RunTypes("bun", projectPath, capturedOut, warmDir)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if !res.Success {
		t.Fatal("run not success")
	}
	// Despite tsc emitting only a.d.ts, the captured set is COMPLETE via the seed.
	assertDeclSet(t, capturedOut, "src/a.d.ts", "src/b.d.ts")
	if len(res.GeneratedFiles) != 2 {
		t.Errorf("GeneratedFiles = %d, want 2 (complete set, not a partial delta)", len(res.GeneratedFiles))
	}
	if gotA, _ := os.ReadFile(filepath.Join(capturedOut, "src", "a.d.ts")); string(gotA) != aNew {
		t.Errorf("a.d.ts = %q, want regenerated %q", gotA, aNew)
	}
	// The buildinfo must NOT leak into the content-addressed captured output.
	if _, err := os.Stat(filepath.Join(capturedOut, "types.tsbuildinfo")); err == nil {
		t.Error("buildinfo must not leak into the captured output dir")
	}
	// The mirror is refreshed from outputPath: same set, and a.d.ts holds the new
	// content the run produced.
	assertDeclSet(t, mirrorDir, "src/a.d.ts", "src/b.d.ts")
	if gotMirrorA, _ := os.ReadFile(filepath.Join(mirrorDir, "src", "a.d.ts")); string(gotMirrorA) != aNew {
		t.Errorf("mirror a.d.ts = %q, want refreshed to %q", gotMirrorA, aNew)
	}
}

func TestRunTypes_IncrementalPrunesDeletedDeclarations(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)
	bSource := filepath.Join(projectPath, "src", "b.ts")
	os.WriteFile(bSource, []byte("export const b = 2;"), 0o644)

	warmDir := filepath.Join(t.TempDir(), "warm")
	mirrorDir := filepath.Join(warmDir, "mirror")
	os.MkdirAll(filepath.Join(mirrorDir, "src"), 0o755)
	os.WriteFile(filepath.Join(mirrorDir, "src", "a.d.ts"), []byte("export declare const a: number;"), 0o644)
	os.WriteFile(filepath.Join(mirrorDir, "src", "b.d.ts"), []byte("export declare const b: number;"), 0o644)
	os.WriteFile(filepath.Join(mirrorDir, "src", "b.d.ts.map"), []byte(`{"version":3}`), 0o644)
	os.WriteFile(filepath.Join(warmDir, "types.tsbuildinfo"), []byte(`{"warm":true}`), 0o644)

	if err := os.Remove(bSource); err != nil {
		t.Fatal(err)
	}
	capturedOut := filepath.Join(t.TempDir(), "types")
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		outDir := argValueOf(args, "--outDir")
		if _, err := os.Stat(filepath.Join(outDir, "src", "b.d.ts")); !os.IsNotExist(err) {
			t.Errorf("deleted source declaration was seeded into output: %v", err)
		}
		if _, err := os.Stat(filepath.Join(outDir, "src", "b.d.ts.map")); !os.IsNotExist(err) {
			t.Errorf("deleted source declaration map was seeded into output: %v", err)
		}
		os.WriteFile(filepath.Join(outDir, "src", "a.d.ts"), []byte("export declare const a: number;"), 0o644)
		return &exec.Result{Success: true}, nil
	})

	res, err := RunTypes("bun", projectPath, capturedOut, warmDir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatal("expected successful incremental build")
	}
	assertDeclSet(t, capturedOut, "src/a.d.ts")
	assertDeclSet(t, mirrorDir, "src/a.d.ts")
	if _, err := os.Stat(filepath.Join(mirrorDir, "src", "b.d.ts.map")); !os.IsNotExist(err) {
		t.Errorf("deleted declaration map remained in mirror: %v", err)
	}
}

func TestRunTypes_NoSourcesClearsIncrementalState(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	source := filepath.Join(projectPath, "src", "a.ts")
	os.WriteFile(source, []byte("export const a = 1;"), 0o644)

	warmDir := filepath.Join(t.TempDir(), "warm")
	os.MkdirAll(filepath.Join(warmDir, "mirror"), 0o755)
	os.WriteFile(filepath.Join(warmDir, "mirror", "a.d.ts"), []byte("export declare const a: number;"), 0o644)
	os.WriteFile(filepath.Join(warmDir, "types.tsbuildinfo"), []byte(`{"warm":true}`), 0o644)
	capturedOut := filepath.Join(t.TempDir(), "types")
	os.MkdirAll(capturedOut, 0o755)
	os.WriteFile(filepath.Join(capturedOut, "a.d.ts"), []byte("export declare const a: number;"), 0o644)

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	called := false
	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		called = true
		return &exec.Result{Success: true}, nil
	})

	res, err := RunTypes("bun", projectPath, capturedOut, warmDir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatal("expected success with no source files")
	}
	if called {
		t.Error("tsc should not run after the final source is removed")
	}
	if _, err := os.Stat(warmDir); !os.IsNotExist(err) {
		t.Errorf("warm incremental state remains after all sources were removed: %v", err)
	}
	if _, err := os.Stat(capturedOut); !os.IsNotExist(err) {
		t.Errorf("captured declarations remain after all sources were removed: %v", err)
	}
}

// TestRunTypes_IncrementalRegeneratesChangedNoStale is the staleness gate: after
// a source file changes, its declaration in the captured output reflects the NEW
// content (no stale bytes), while an unchanged file's declaration remains present
// and correct.
func TestRunTypes_IncrementalRegeneratesChangedNoStale(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "b.ts"), []byte("export const b = 2;"), 0o644)

	warmDir := filepath.Join(t.TempDir(), "warm")
	capturedOut := filepath.Join(t.TempDir(), "types")

	const aOld = "export declare const a: number;"
	const aNew = "export declare const a: 42;"
	const bDecl = "export declare const b: number;"

	call := 0
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		call++
		outDir := argValueOf(args, "--outDir")
		// tsc always emits at the captured outputPath (map-correctness invariant).
		if outDir != capturedOut {
			t.Errorf("call %d: --outDir = %q, want captured outputPath %q", call, outDir, capturedOut)
		}
		os.MkdirAll(filepath.Join(outDir, "src"), 0o755)
		if call == 1 {
			os.WriteFile(filepath.Join(outDir, "src", "a.d.ts"), []byte(aOld), 0o644)
			os.WriteFile(filepath.Join(outDir, "src", "b.d.ts"), []byte(bDecl), 0o644)
		} else {
			// a.ts changed: tsc re-emits only a.d.ts with new content; b.d.ts is left
			// in place (restored by the seed from the mirror before tsc runs).
			os.WriteFile(filepath.Join(outDir, "src", "a.d.ts"), []byte(aNew), 0o644)
		}
		os.WriteFile(argValueOf(args, "--tsBuildInfoFile"), []byte("info"), 0o644)
		return &exec.Result{Success: true}, nil
	})

	if _, err := RunTypes("bun", projectPath, capturedOut, warmDir); err != nil {
		t.Fatalf("cold run error: %v", err)
	}
	// Edit a.ts and let the scheduler clear the captured output before the miss.
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 42;"), 0o644)
	os.RemoveAll(capturedOut)

	if _, err := RunTypes("bun", projectPath, capturedOut, warmDir); err != nil {
		t.Fatalf("warm run error: %v", err)
	}

	gotA, err := os.ReadFile(filepath.Join(capturedOut, "src", "a.d.ts"))
	if err != nil {
		t.Fatalf("a.d.ts missing: %v", err)
	}
	if string(gotA) != aNew {
		t.Errorf("a.d.ts = %q, want regenerated %q (no stale content)", gotA, aNew)
	}
	gotB, err := os.ReadFile(filepath.Join(capturedOut, "src", "b.d.ts"))
	if err != nil {
		t.Fatalf("b.d.ts missing after warm run: %v", err)
	}
	if string(gotB) != bDecl {
		t.Errorf("b.d.ts = %q, want unchanged %q", gotB, bDecl)
	}
}

// TestRunTypes_ColdPathWhenNoBuildInfoDir pins the fallback: an empty
// tsBuildInfoDir disables incremental entirely — no --incremental, no
// --tsBuildInfoFile, and tsc writes straight into the captured output dir with no
// warm-dir indirection (byte-identical to today's behavior).
func TestRunTypes_ColdPathWhenNoBuildInfoDir(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)

	capturedOut := filepath.Join(t.TempDir(), "types")

	var sawIncremental, sawBuildInfo bool
	var seenOutDir string
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if argsHaveFlag(args, "--incremental") {
			sawIncremental = true
		}
		if argValueOf(args, "--tsBuildInfoFile") != "" {
			sawBuildInfo = true
		}
		seenOutDir = argValueOf(args, "--outDir")
		os.MkdirAll(seenOutDir, 0o755)
		os.WriteFile(filepath.Join(seenOutDir, "a.d.ts"), []byte("export declare const a: number;"), 0o644)
		return &exec.Result{Success: true}, nil
	})

	res, err := RunTypes("bun", projectPath, capturedOut, "")
	if err != nil {
		t.Fatal(err)
	}
	if sawIncremental {
		t.Error("cold path must not pass --incremental")
	}
	if sawBuildInfo {
		t.Error("cold path must not pass --tsBuildInfoFile")
	}
	if seenOutDir != capturedOut {
		t.Errorf("cold path --outDir = %q, want captured %q", seenOutDir, capturedOut)
	}
	if len(res.GeneratedFiles) != 1 {
		t.Errorf("GeneratedFiles = %d, want 1", len(res.GeneratedFiles))
	}
}

// projectListing returns every path under dir, relative to dir.
func projectListing(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

// TestRunTypes_NeverWritesIntoTheProjectDirectory pins that a project holds
// exactly its own files while tsc runs, not only after RunTypes returns. A
// provider's generated client at <provider>/clients/ts is a project of its own
// and the declared output of the provider's clientgen~generate-ts, whose drift
// check and capture can read that directory at any moment of the client's
// build~types.
func TestRunTypes_NeverWritesIntoTheProjectDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		warm bool
	}{{"warm", true}, {"cold", false}} {
		t.Run(tc.name, func(t *testing.T) {
			projectPath := t.TempDir()
			os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
			os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
			os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)
			before := projectListing(t, projectPath)

			warmDir := ""
			if tc.warm {
				warmDir = filepath.Join(t.TempDir(), "ts-types", "build-types", "clients", "ts")
			}
			var config string
			withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
				// The window in which a concurrent generator snapshots the directory.
				if during := projectListing(t, projectPath); !slices.Equal(during, before) {
					t.Errorf("project directory while tsc runs = %v, want %v", during, before)
				}
				config = argValueOf(args, "--project")
				if _, err := os.Stat(config); err != nil {
					t.Errorf("types tsconfig %q must exist while tsc runs: %v", config, err)
				}
				if tc.warm && filepath.Dir(config) != warmDir {
					t.Errorf("types tsconfig written to %q, want the task's scratch directory %q", filepath.Dir(config), warmDir)
				}
				return &exec.Result{Success: true}, nil
			})

			if _, err := RunTypes("bun", projectPath, filepath.Join(t.TempDir(), "types"), warmDir); err != nil {
				t.Fatal(err)
			}
			if after := projectListing(t, projectPath); !slices.Equal(after, before) {
				t.Errorf("project directory after RunTypes = %v, want %v", after, before)
			}
			if _, err := os.Stat(config); !os.IsNotExist(err) {
				t.Errorf("types tsconfig %q must be removed after the run, stat err = %v", config, err)
			}
		})
	}
}

// TestCreateTypesTsConfig_AnchorsEveryPathOnTheProject pins what keeps a
// config outside the project equivalent to one inside it: extends, include and
// exclude name the project absolutely, and the type roots are the ones tsc
// derives from the project directory rather than from the scratch directory.
func TestCreateTypesTsConfig_AnchorsEveryPathOnTheProject(t *testing.T) {
	projectPath := t.TempDir()
	base := filepath.Join(projectPath, "tsconfig.json")
	os.WriteFile(base, []byte(`{}`), 0o644)
	scratch := filepath.Join(t.TempDir(), "scratch")

	path, err := createTypesTsConfig(scratch, projectPath, base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != scratch {
		t.Fatalf("config written to %q, want %q", filepath.Dir(path), scratch)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Extends         string   `json:"extends"`
		Include         []string `json:"include"`
		Exclude         []string `json:"exclude"`
		CompilerOptions struct {
			TypeRoots []string `json:"typeRoots"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	project := filepath.ToSlash(projectPath)
	if cfg.Extends != filepath.ToSlash(base) {
		t.Errorf("extends = %q, want %q", cfg.Extends, filepath.ToSlash(base))
	}
	for _, pattern := range append(slices.Clone(cfg.Include), cfg.Exclude...) {
		if !strings.HasPrefix(pattern, project+"/") {
			t.Errorf("pattern %q is not anchored on the project %q", pattern, project)
		}
	}
	if !slices.Contains(cfg.Include, project+"/.gen/**/*.ts") || !slices.Contains(cfg.Exclude, project+"/**/*.test.ts") {
		t.Errorf("include %v / exclude %v lost the source and test patterns", cfg.Include, cfg.Exclude)
	}
	roots := cfg.CompilerOptions.TypeRoots
	if len(roots) == 0 || roots[0] != project+"/node_modules/@types" {
		t.Fatalf("typeRoots = %v, want the project's node_modules/@types first", roots)
	}
	if parent := filepath.ToSlash(filepath.Join(filepath.Dir(projectPath), "node_modules", "@types")); roots[1] != parent {
		t.Errorf("typeRoots[1] = %q, want the parent's %q", roots[1], parent)
	}
	for _, root := range roots {
		if strings.HasPrefix(root, filepath.ToSlash(scratch)) {
			t.Errorf("type root %q derives from the scratch directory", root)
		}
	}
}

// readTypesConfig returns the compilerOptions and files of a generated types
// config.
func readTypesConfig(t *testing.T, path string) (map[string]any, any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CompilerOptions map[string]any `json:"compilerOptions"`
		Files           any            `json:"files"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.CompilerOptions, cfg.Files
}

// TestCreateTypesTsConfig_ResolvesConfigDirAgainstTheProject pins the settings
// tsc derives from the loading config's directory: ${configDir} anywhere in the
// chain means the project directory, and a re-declared option keeps its
// relative paths anchored on the config that declares them.
func TestCreateTypesTsConfig_ResolvesConfigDirAgainstTheProject(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "tsconfig.base.json"), []byte(`{
  "compilerOptions": {
    "paths": {"@/*": ["${configDir}/src/*"], "shared/*": ["./shared/*"]},
    "declarationDir": "${configDir}/types",
    "strict": true
  },
  "files": ["${configDir}/env.d.ts"]
}`), 0o644)
	projectPath := filepath.Join(root, "apps", "web")
	os.MkdirAll(projectPath, 0o755)
	base := filepath.Join(projectPath, "tsconfig.json")
	os.WriteFile(base, []byte(`{"extends": "../../tsconfig.base.json"}`), 0o644)

	path, err := createTypesTsConfig(t.TempDir(), projectPath, base)
	if err != nil {
		t.Fatal(err)
	}
	opts, files := readTypesConfig(t, path)
	project, rootSlash := filepath.ToSlash(projectPath), filepath.ToSlash(root)
	wantPaths := map[string]any{"@/*": []any{project + "/src/*"}, "shared/*": []any{rootSlash + "/shared/*"}}
	if !reflect.DeepEqual(opts["paths"], wantPaths) {
		t.Errorf("paths = %v, want %v", opts["paths"], wantPaths)
	}
	if opts["declarationDir"] != project+"/types" {
		t.Errorf("declarationDir = %v, want %q", opts["declarationDir"], project+"/types")
	}
	if _, redeclared := opts["strict"]; redeclared {
		t.Error("an option without ${configDir} must stay inherited")
	}
	if !reflect.DeepEqual(files, []any{project + "/env.d.ts"}) {
		t.Errorf("files = %v, want the project's env.d.ts", files)
	}
}

// TestCreateTypesTsConfig_KeepsPathsRelativeToBaseURL pins that re-declared
// paths entries stay relative when a baseUrl resolves them.
func TestCreateTypesTsConfig_KeepsPathsRelativeToBaseURL(t *testing.T) {
	projectPath := t.TempDir()
	base := filepath.Join(projectPath, "tsconfig.json")
	os.WriteFile(base, []byte(`{"compilerOptions": {"baseUrl": "${configDir}", "paths": {"@/*": ["./src/*", "${configDir}/gen/*"]}}}`), 0o644)

	path, err := createTypesTsConfig(t.TempDir(), projectPath, base)
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := readTypesConfig(t, path)
	project := filepath.ToSlash(projectPath)
	if opts["baseUrl"] != project {
		t.Errorf("baseUrl = %v, want %q", opts["baseUrl"], project)
	}
	if want := map[string]any{"@/*": []any{"./src/*", project + "/gen/*"}}; !reflect.DeepEqual(opts["paths"], want) {
		t.Errorf("paths = %v, want %v", opts["paths"], want)
	}
}

// TestCreateTypesTsConfig_KeepsDeclaredTypeRoots pins that a chain's own
// typeRoots win over the defaults, anchored on the config that declares them.
func TestCreateTypesTsConfig_KeepsDeclaredTypeRoots(t *testing.T) {
	projectPath := t.TempDir()
	base := filepath.Join(projectPath, "tsconfig.json")
	os.WriteFile(base, []byte(`{"compilerOptions": {"typeRoots": ["./types", "./node_modules/@types"]}}`), 0o644)

	path, err := createTypesTsConfig(t.TempDir(), projectPath, base)
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := readTypesConfig(t, path)
	project := filepath.ToSlash(projectPath)
	if want := []any{project + "/types", project + "/node_modules/@types"}; !reflect.DeepEqual(opts["typeRoots"], want) {
		t.Errorf("typeRoots = %v, want %v", opts["typeRoots"], want)
	}
}

// TestRunTypes_RemovesLegacyTypesTsConfigs pins that a temporary config an
// interrupted earlier release left in the project is removed, and that no other
// project file is touched.
func TestRunTypes_RemovesLegacyTypesTsConfigs(t *testing.T) {
	projectPath := t.TempDir()
	os.MkdirAll(filepath.Join(projectPath, "src"), 0o755)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.types.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "src", "a.ts"), []byte("export const a = 1;"), 0o644)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.types.esm.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(projectPath, "tsconfig.types.123456.json"), []byte(`{}`), 0o644)

	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	})
	if _, err := RunTypes("bun", projectPath, filepath.Join(t.TempDir(), "types"), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{".", "src", "src/a.ts", "tsconfig.json", "tsconfig.types.esm.json", "tsconfig.types.json"}
	if got := projectListing(t, projectPath); !slices.Equal(got, want) {
		t.Errorf("project directory = %v, want %v", got, want)
	}
}
