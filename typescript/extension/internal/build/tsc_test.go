package build

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/exec"
)

const tscFeature = "typescript/typescript-project-toolchain"

// tscWorkspace writes files under a new workspace root, with an installed
// typescript package at its root, and returns the root and the project path
// of libs/lib.
func tscWorkspace(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"node_modules/typescript/bin/tsc": "#!/usr/bin/env node\n"}
	for name, content := range files {
		all[name] = content
	}
	for name, content := range all {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, filepath.Join(root, "libs", "lib")
}

func libPackage(script string) string {
	if script == "" {
		return `{"name":"lib"}`
	}
	return `{"name":"lib","scripts":{"build":"` + script + `"}}`
}

const libConfig = `{
  // tsc reads JSONC
  "compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist", "declaration": true},
  "include": ["src"],
}`

func TestResolveCompilerFollowsTheBuildScriptUnderAuto(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-compiler-choice", "auto-compiles-a-tsc-alone-build-script-with-tsc")
	spectest.Proves(t, tscFeature, "tsc-compiler-choice", "auto-keeps-bun-for-any-other-build-script")
	spectest.Proves(t, tscFeature, "tsc-compiler-choice", "an-explicit-compiler-overrides-the-script")
	cases := []struct {
		param, script, want string
	}{
		{"auto", "tsc -b", CompilerTsc},
		{"", "tsc -p tsconfig.build.json", CompilerTsc},
		{"auto", "tsc && cp README.md dist", CompilerBun},
		{"auto", "tsc --watch", CompilerBun},
		{"auto", "tsup src/index.ts", CompilerBun},
		{"auto", "", CompilerBun},
		{"bun", "tsc -b", CompilerBun},
		{"tsc", "", CompilerTsc},
		{"tsc", "tsup", CompilerTsc},
	}
	for _, c := range cases {
		_, project := tscWorkspace(t, map[string]string{"libs/lib/package.json": libPackage(c.script)})
		got, err := ResolveCompiler(c.param, project)
		if err != nil || got != c.want {
			t.Errorf("ResolveCompiler(%q) with build script %q = %q, %v; want %q", c.param, c.script, got, err, c.want)
		}
	}
	if _, err := ResolveCompiler("swc", t.TempDir()); !isTscError(err, CodeTscCompiler) {
		t.Errorf("an unknown compiler = %v, want %s", err, CodeTscCompiler)
	}
}

func TestPlanTscRunsTheScriptArguments(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-build-runs-the-installed-tsc", "the-build-script-arguments-reach-tsc")
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":      libPackage("tsc --build tsconfig.lib.json --verbose"),
		"libs/lib/tsconfig.json":     `{"files": []}`,
		"libs/lib/tsconfig.lib.json": libConfig,
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-b", "tsconfig.lib.json", "--verbose"}; !reflect.DeepEqual(plan.Args, want) {
		t.Errorf("args = %q, want %q", plan.Args, want)
	}
	if want := filepath.Join(root, "node_modules", "typescript", "bin", "tsc"); plan.Tsc != want {
		t.Errorf("tsc = %s, want the workspace's %s", plan.Tsc, want)
	}
	if want := filepath.Join(project, "dist"); plan.OutDir != want {
		t.Errorf("outDir = %s, want %s", plan.OutDir, want)
	}
}

func TestPlanTscRunsABuildWithReferencesAsAProgram(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-build-runs-the-installed-tsc", "tsc-b-with-references-runs-as-tsc-p")
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":  libPackage("tsc -b --verbose --pretty"),
		"libs/lib/tsconfig.json": `{"compilerOptions": {"composite": true, "outDir": "dist"}, "references": [{"path": "../dep"}]}`,
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	// --verbose is a flag of build mode alone.
	if want := []string{"-p", "tsconfig.json", "--pretty"}; !reflect.DeepEqual(plan.Args, want) {
		t.Errorf("args = %q, want %q", plan.Args, want)
	}
	// Without rootDir, the .tsbuildinfo of a composite program lies in the outDir.
	if plan.BuildInfo != "" {
		t.Errorf("buildInfo = %q, want none outside the outDir", plan.BuildInfo)
	}
}

func TestPlanTscWithoutABuildScriptCompilesTheDefaultConfig(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-build-runs-the-installed-tsc", "no-build-script-runs-tsc-p-on-the-default-config")
	for _, c := range []struct {
		configs map[string]string
		want    []string
	}{
		{map[string]string{"libs/lib/tsconfig.json": `{"compilerOptions": {"outDir": "dist"}}`}, []string{"-p", "tsconfig.json"}},
		{map[string]string{
			"libs/lib/tsconfig.json":     `{"files": []}`,
			"libs/lib/tsconfig.lib.json": `{"compilerOptions": {"outDir": "../../dist/libs/lib"}}`,
		}, nil},
		{map[string]string{
			"libs/lib/tsconfig.json":     `{"files": []}`,
			"libs/lib/tsconfig.lib.json": `{"compilerOptions": {"outDir": "out-tsc"}}`,
		}, []string{"-p", "tsconfig.lib.json"}},
	} {
		files := map[string]string{"libs/lib/package.json": libPackage("")}
		for name, content := range c.configs {
			files[name] = content
		}
		root, project := tscWorkspace(t, files)
		plan, err := PlanTsc(project, root)
		if c.want == nil {
			// The Nx layout that writes outside the project is refused.
			if !isTscError(err, CodeTscOutDir) {
				t.Errorf("an outDir outside the project = %v, want %s", err, CodeTscOutDir)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(plan.Args, c.want) {
			t.Errorf("args = %q, want %q", plan.Args, c.want)
		}
	}
}

func TestPlanTscRefusesAnotherBuildScriptAndAMissingTsc(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-compiler-choice", "an-explicit-tsc-with-another-build-script-fails")
	spectest.Proves(t, tscFeature, "tsc-build-runs-the-installed-tsc", "a-missing-tsc-fails-naming-typescript")
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":  libPackage("tsc && cp README.md dist"),
		"libs/lib/tsconfig.json": `{"compilerOptions": {"outDir": "dist"}}`,
	})
	if _, err := PlanTsc(project, root); !isTscError(err, CodeTscScript) || !strings.Contains(err.Error(), "tsc && cp README.md dist") {
		t.Errorf("another build script = %v, want %s naming it", err, CodeTscScript)
	}

	root, project = tscWorkspace(t, map[string]string{
		"libs/lib/package.json":  libPackage("tsc"),
		"libs/lib/tsconfig.json": `{"compilerOptions": {"outDir": "dist"}}`,
	})
	if err := os.RemoveAll(filepath.Join(root, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanTsc(project, root); !isTscError(err, CodeTscMissing) || !strings.Contains(err.Error(), "typescript") {
		t.Errorf("a missing tsc = %v, want %s naming typescript", err, CodeTscMissing)
	}
}

func TestPlanTscDeclaresTheBuildInfoOutsideTheOutDir(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "the-tsbuildinfo-outside-the-outdir-is-declared")
	cases := []struct {
		config, script, buildInfo, declarationDir string
	}{
		// rootDir src puts it next to the config: outDir/../tsconfig.
		{libConfig, "tsc -b", "tsconfig.tsbuildinfo", ""},
		{`{"compilerOptions": {"outDir": "dist"}}`, "tsc -b", "", ""},
		{`{"compilerOptions": {"outDir": "dist"}}`, "tsc", "", ""},
		{`{"compilerOptions": {"incremental": true, "outDir": "dist", "tsBuildInfoFile": "${configDir}/.cache/lib.tsbuildinfo"}}`, "tsc", ".cache/lib.tsbuildinfo", ""},
		{`{"compilerOptions": {"outDir": "dist/js", "declaration": true, "declarationDir": "dist/types"}}`, "tsc", "", "dist/types"},
		{`{"compilerOptions": {"outDir": "dist", "declaration": true, "declarationDir": "dist/types"}}`, "tsc", "", ""},
	}
	for _, c := range cases {
		root, project := tscWorkspace(t, map[string]string{
			"libs/lib/package.json":  libPackage(c.script),
			"libs/lib/tsconfig.json": c.config,
		})
		plan, err := PlanTsc(project, root)
		if err != nil {
			t.Fatalf("%s: %v", c.config, err)
		}
		if got := relOrEmpty(project, plan.BuildInfo); got != c.buildInfo {
			t.Errorf("%s with %q: buildInfo = %q, want %q", c.config, c.script, got, c.buildInfo)
		}
		if got := relOrEmpty(project, plan.DeclarationDir); got != c.declarationDir {
			t.Errorf("%s: declarationDir = %q, want %q", c.config, got, c.declarationDir)
		}
	}
}

func TestPlanTscInheritsTheOutDirThroughExtends(t *testing.T) {
	root, project := tscWorkspace(t, map[string]string{
		"tsconfig.base.json":     `{"compilerOptions": {"outDir": "${configDir}/build", "composite": true}}`,
		"libs/lib/package.json":  libPackage("tsc -b"),
		"libs/lib/tsconfig.json": `{"extends": "../../tsconfig.base.json"}`,
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(project, "build"); plan.OutDir != want {
		t.Errorf("outDir = %s, want %s: ${configDir} names the compiled config's directory", plan.OutDir, want)
	}
}

func TestPlanTscRefusesAnOutDirItCannotOwn(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	for _, config := range []string{
		`{"compilerOptions": {}}`,
		`{"compilerOptions": {"outDir": "."}}`,
		`{"compilerOptions": {"outDir": "../shared"}}`,
		`{"compilerOptions": {"outFile": "dist/all.js"}}`,
		`{"compilerOptions": {"outDir": "dist", "noEmit": true}}`,
		`{"compilerOptions": {"outDir": "dist", "declarationDir": "../types"}}`,
	} {
		root, project := tscWorkspace(t, map[string]string{
			"libs/lib/package.json":  libPackage("tsc"),
			"libs/lib/tsconfig.json": config,
		})
		if _, err := PlanTsc(project, root); !isTscError(err, CodeTscOutDir) {
			t.Errorf("%s = %v, want %s", config, err, CodeTscOutDir)
		}
	}
	root, project := tscWorkspace(t, map[string]string{"libs/lib/package.json": libPackage("tsc -p tsconfig.build.json")})
	if _, err := PlanTsc(project, root); !isTscError(err, CodeTscConfig) {
		t.Errorf("a missing config = %v, want %s", err, CodeTscConfig)
	}
}

func TestCheckTscOutputsRefusesAnUnignoredOrTrackedOutput(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := osexec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	setup := func(gitignore string, tracked bool) (string, *TscPlan) {
		root, project := tscWorkspace(t, map[string]string{
			".gitignore":             gitignore,
			"libs/lib/package.json":  libPackage("tsc -b"),
			"libs/lib/tsconfig.json": libConfig,
		})
		git(root, "init", "-q")
		if tracked {
			if err := os.MkdirAll(filepath.Join(project, "dist"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "dist", "index.js"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			git(root, "add", "-f", "libs/lib/dist/index.js")
		}
		plan, err := PlanTsc(project, root)
		if err != nil {
			t.Fatal(err)
		}
		return project, plan
	}

	project, plan := setup("node_modules\ndist/\n*.tsbuildinfo\n", false)
	if err := CheckTscOutputs(filepath.Dir(filepath.Dir(project)), project, plan, nil); err != nil {
		t.Errorf("ignored outputs = %v, want accepted", err)
	}
	project, plan = setup("node_modules\n*.tsbuildinfo\n", false)
	if err := CheckTscOutputs(filepath.Dir(filepath.Dir(project)), project, plan, nil); !isTscError(err, CodeTscOutputIgnored) || !strings.Contains(err.Error(), "dist") {
		t.Errorf("an outDir Git does not ignore = %v, want %s naming dist", err, CodeTscOutputIgnored)
	}
	project, plan = setup("node_modules\ndist/\n", false)
	if err := CheckTscOutputs(filepath.Dir(filepath.Dir(project)), project, plan, nil); !isTscError(err, CodeTscOutputIgnored) || !strings.Contains(err.Error(), "tsconfig.tsbuildinfo") {
		t.Errorf("a .tsbuildinfo Git does not ignore = %v, want %s naming it", err, CodeTscOutputIgnored)
	}
	project, plan = setup("node_modules\ndist/\n*.tsbuildinfo\n", true)
	if err := CheckTscOutputs(filepath.Dir(filepath.Dir(project)), project, plan, nil); !isTscError(err, CodeTscOutputTracked) || !strings.Contains(err.Error(), "dist/index.js") {
		t.Errorf("a tracked file in the outDir = %v, want %s naming it", err, CodeTscOutputTracked)
	}
}

// TestCheckTscOutputsFoldsCaseOnACaseInsensitiveVolume proves that an outDir
// spelled with other case than the cut is compared as the volume compares it:
// folded where the file system ignores case, as spelled where it does not.
func TestCheckTscOutputsFoldsCaseOnACaseInsensitiveVolume(t *testing.T) {
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":  libPackage("tsc -b"),
		"libs/lib/tsconfig.json": strings.Replace(libConfig, `"outDir": "dist"`, `"outDir": "Dist"`, 1),
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(plan.OutDir) != "Dist" {
		t.Fatalf("outDir = %s, want the config's Dist", plan.OutDir)
	}
	context := func(files ...job.CandidateFile) *job.Context {
		return &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
			Directory:            "libs/lib",
			CandidateDirectories: []string{"libs/lib"},
			CandidateFiles:       append([]job.CandidateFile{{Path: "libs/lib/package.json", Mode: job.CandidateModeRegular}}, files...),
		}}
	}
	t.Setenv("PATH", "")
	previous := foldsCase
	t.Cleanup(func() { foldsCase = previous })

	for _, tc := range []struct {
		name  string
		folds bool
		file  job.CandidateFile
		code  string
	}{
		{"untracked, folding volume", true, job.CandidateFile{Path: "libs/lib/dist/index.js", Mode: job.CandidateModeUntracked}, CodeTscOutputIgnored},
		{"tracked, folding volume", true, job.CandidateFile{Path: "libs/lib/DIST/index.js", Mode: job.CandidateModeRegular}, CodeTscOutputTracked},
		{"tracked link above, folding volume", true, job.CandidateFile{Path: "libs/lib/dist", Mode: job.CandidateModeSymlink}, CodeTscOutputTracked},
		{"untracked, case-sensitive volume", false, job.CandidateFile{Path: "libs/lib/dist/index.js", Mode: job.CandidateModeUntracked}, ""},
		{"tracked, case-sensitive volume", false, job.CandidateFile{Path: "libs/lib/dist/index.js", Mode: job.CandidateModeRegular}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foldsCase = func(string) bool { return tc.folds }
			err := CheckTscOutputs(root, project, plan, context(tc.file))
			if tc.code == "" {
				if err != nil {
					t.Errorf("CheckTscOutputs = %v, want the outputs accepted", err)
				}
				return
			}
			if !isTscError(err, tc.code) || !strings.Contains(err.Error(), strings.TrimPrefix(tc.file.Path, "libs/lib/")) {
				t.Errorf("CheckTscOutputs = %v, want %s naming %s", err, tc.code, tc.file.Path)
			}
		})
	}

	// An outDir below a link the cut lists as one entry, in other case.
	below := strings.Replace(libConfig, `"outDir": "dist"`, `"outDir": "Out/dist"`, 1)
	if err := os.WriteFile(filepath.Join(project, "tsconfig.json"), []byte(below), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	link := job.CandidateFile{Path: "libs/lib/out", Mode: job.CandidateModeSymlink}
	foldsCase = func(string) bool { return true }
	if err := CheckTscOutputs(root, project, plan, context(link)); !isTscError(err, CodeTscOutputTracked) || !strings.Contains(err.Error(), "below out,") {
		t.Errorf("an outDir below a link in other case on a folding volume = %v, want %s naming the link", err, CodeTscOutputTracked)
	}
	foldsCase = func(string) bool { return false }
	if err := CheckTscOutputs(root, project, plan, context(link)); err != nil {
		t.Errorf("an outDir beside a link of other case on a case-sensitive volume = %v, want it accepted", err)
	}
}

// TestCheckTscOutputsFoldsCaseWhenItAsksGit proves the same folding on the
// path that asks Git, for a context without the cut: an outDir spelled Dist
// holds the dist/ file the index tracks where the volume ignores case.
func TestCheckTscOutputsFoldsCaseWhenItAsksGit(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root, project := tscWorkspace(t, map[string]string{
		".gitignore":             "Dist/\n",
		"libs/lib/package.json":  libPackage("tsc -b"),
		"libs/lib/tsconfig.json": strings.Replace(libConfig, `"outDir": "dist"`, `"outDir": "Dist"`, 1),
		"libs/lib/dist/index.js": "",
	})
	for _, args := range [][]string{{"init", "-q"}, {"add", "-f", "libs/lib/dist/index.js"}} {
		if out, err := osexec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	previous := foldsCase
	t.Cleanup(func() { foldsCase = previous })

	foldsCase = func(string) bool { return true }
	if err := CheckTscOutputs(root, project, plan, nil); !isTscError(err, CodeTscOutputTracked) || !strings.Contains(err.Error(), "dist/index.js") {
		t.Errorf("CheckTscOutputs on a folding volume = %v, want %s naming dist/index.js", err, CodeTscOutputTracked)
	}
	foldsCase = func(string) bool { return false }
	if err := CheckTscOutputs(root, project, plan, nil); isTscError(err, CodeTscOutputTracked) {
		t.Errorf("CheckTscOutputs on a case-sensitive volume = %v, want Dist and dist compared as spelled", err)
	}
}

// TestFoldsCaseComparesTheSwappedName proves the probe on the volume that
// holds the test's temporary directory: it folds exactly when the swapped
// name opens the same directory.
func TestFoldsCaseComparesTheSwappedName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Probe")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(dir), "pROBE"))
	want := err == nil
	if got := foldsCase(dir); got != want {
		t.Errorf("foldsCase(%s) = %v, want %v", dir, got, want)
	}
	// A distinct directory under the swapped name is not the same file.
	if !want {
		if err := os.Mkdir(filepath.Join(filepath.Dir(dir), "pROBE"), 0o755); err != nil {
			t.Fatal(err)
		}
		if foldsCase(dir) {
			t.Errorf("foldsCase(%s) = true beside a distinct pROBE", dir)
		}
	}
	// An entry of the directory is probed first: a distinct entry under the
	// swapped name is not the same file.
	if err := os.WriteFile(filepath.Join(dir, "Entry"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := foldsCase(dir); got != want {
		t.Errorf("foldsCase(%s) with an entry = %v, want %v", dir, got, want)
	}
	if !want {
		if err := os.WriteFile(filepath.Join(dir, "eNTRY"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if foldsCase(dir) {
			t.Errorf("foldsCase(%s) = true beside a distinct eNTRY", dir)
		}
	}
	// A path of no letter up to the root folds nothing.
	if foldsCase(string(filepath.Separator)) {
		t.Error("foldsCase(/) = true, want false")
	}
}

// The check reads the candidate cut its key holds from the job context and
// starts no Git (doc/adr/0015): a tracked path in an output refuses it, an
// untracked one, which Git does not ignore, refuses it too, and an output that
// holds nothing yet passes.
func TestCheckTscOutputsReadsTheCandidateCutOfItsContext(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":  libPackage("tsc -b"),
		"libs/lib/tsconfig.json": libConfig,
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	context := func(files ...job.CandidateFile) *job.Context {
		return &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
			Directory:            "libs/lib",
			CandidateDirectories: []string{"libs/lib"},
			CandidateFiles:       append([]job.CandidateFile{{Path: "libs/lib/package.json", Mode: job.CandidateModeRegular}}, files...),
		}}
	}
	tracked := func(path string) job.CandidateFile {
		return job.CandidateFile{Path: path, Mode: job.CandidateModeRegular}
	}
	untracked := func(path string) job.CandidateFile {
		return job.CandidateFile{Path: path, Mode: job.CandidateModeUntracked}
	}
	t.Setenv("PATH", "")

	for _, tc := range []struct {
		name  string
		files []job.CandidateFile
		code  string
		names string
	}{
		{"outputs not built yet", nil, "", ""},
		{"files beside the outputs", []job.CandidateFile{tracked("libs/lib/distribution.md"), untracked("libs/lib/dist.txt")}, "", ""},
		{"a tracked file in the outDir", []job.CandidateFile{tracked("libs/lib/dist/index.js")}, CodeTscOutputTracked, "dist/index.js"},
		{"an unmerged file in the outDir", []job.CandidateFile{{Path: "libs/lib/dist/index.js", Mode: job.CandidateModeUnmerged}}, CodeTscOutputTracked, "dist/index.js"},
		{"a tracked .tsbuildinfo", []job.CandidateFile{tracked("libs/lib/tsconfig.tsbuildinfo")}, CodeTscOutputTracked, "tsconfig.tsbuildinfo"},
		{"an outDir Git does not ignore", []job.CandidateFile{untracked("libs/lib/dist/index.js")}, CodeTscOutputIgnored, "dist/index.js"},
		{"a .tsbuildinfo Git does not ignore", []job.CandidateFile{untracked("libs/lib/tsconfig.tsbuildinfo")}, CodeTscOutputIgnored, "tsconfig.tsbuildinfo"},
		// Outputs are checked in order: the unignored outDir refuses before
		// the tracked .tsbuildinfo that follows it.
		{"an unignored outDir before a tracked .tsbuildinfo", []job.CandidateFile{tracked("libs/lib/tsconfig.tsbuildinfo"), untracked("libs/lib/dist/index.js")}, CodeTscOutputIgnored, "dist/index.js"},
		// Within one output, a tracked file refuses first.
		{"a tracked and an untracked file in the outDir", []job.CandidateFile{untracked("libs/lib/dist/a.js"), tracked("libs/lib/dist/b.js")}, CodeTscOutputTracked, "dist/b.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTscOutputs(root, project, plan, context(tc.files...))
			if tc.code == "" {
				if err != nil {
					t.Errorf("CheckTscOutputs = %v, want the outputs accepted", err)
				}
				return
			}
			if !isTscError(err, tc.code) || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("CheckTscOutputs = %v, want %s naming %s", err, tc.code, tc.names)
			}
		})
	}

	// A context without the cut, an older CLI's, is checked with Git; without
	// a git program, nothing is checked.
	if err := CheckTscOutputs(root, project, plan, &job.Context{WorkspaceRoot: root}); err != nil {
		t.Errorf("no cut and no git program = %v, want nothing checked", err)
	}
}

// A project inside a repository nested in the workspace's, a clone or a
// submodule, has its tracked files in that repository's index, which the
// context's listing does not read: the check asks Git there and refuses the
// tracked output.
func TestCheckTscOutputsAsksGitInANestedRepository(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	for _, tc := range []struct {
		name string
		// init initializes the nested repository at inner, from root.
		init []string
	}{
		{"a nested clone", []string{"init", "-q"}},
		{"a submodule's .git file", []string{"init", "-q", "--separate-git-dir", "../../.inner.git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := tscWorkspace(t, map[string]string{
				".gitignore":                   "node_modules\ndist/\n*.tsbuildinfo\n",
				"libs/inner/lib/package.json":  libPackage("tsc -b"),
				"libs/inner/lib/tsconfig.json": libConfig,
				"libs/inner/lib/dist/index.js": "",
			})
			project := filepath.Join(root, "libs", "inner", "lib")
			inner := filepath.Join(root, "libs", "inner")
			for _, args := range [][]string{{"-C", root, "init", "-q"}, append([]string{"-C", inner}, tc.init...), {"-C", inner, "add", "-f", "lib/dist/index.js"}} {
				if out, err := osexec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			plan, err := PlanTsc(project, root)
			if err != nil {
				t.Fatal(err)
			}
			// The workspace's cut lists nothing under the project.
			ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
				Directory: "libs/inner/lib", CandidateDirectories: []string{"libs/inner/lib"}, CandidateFiles: []job.CandidateFile{},
			}}
			err = CheckTscOutputs(root, project, plan, ctx)
			if !isTscError(err, CodeTscOutputTracked) || !strings.Contains(err.Error(), "dist/index.js") {
				t.Errorf("a tracked file in the outDir of a nested repository = %v, want %s naming it", err, CodeTscOutputTracked)
			}
		})
	}
}

func TestCleanTscOutputsRemovesWhatTscNoLongerEmits(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "a-file-tsc-stops-emitting-is-gone-after-a-rebuild")
	root, project := tscWorkspace(t, map[string]string{
		"libs/lib/package.json":         libPackage("tsc -b"),
		"libs/lib/tsconfig.json":        libConfig,
		"libs/lib/dist/removed.js":      "",
		"libs/lib/tsconfig.tsbuildinfo": "{}",
	})
	plan, err := PlanTsc(project, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanTscOutputs(plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"dist", "tsconfig.tsbuildinfo"} {
		if _, err := os.Stat(filepath.Join(project, path)); !os.IsNotExist(err) {
			t.Errorf("%s survives the clean: %v", path, err)
		}
	}
}

func TestRunTscStartsTheCompilerWithBun(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-build-runs-the-installed-tsc", "tsc-runs-with-bun-and-no-node")
	var name string
	var args []string
	t.Cleanup(SetExecRunForTesting(func(n string, a []string, _ ...exec.Option) (*exec.Result, error) {
		name, args = n, a
		return &exec.Result{Success: true}, nil
	}))
	plan := &TscPlan{Tsc: "/ws/node_modules/typescript/bin/tsc", Args: []string{"-b"}}
	if _, err := RunTsc("/bin/bun", "/ws/lib", plan); err != nil {
		t.Fatal(err)
	}
	if name != "/bin/bun" || !reflect.DeepEqual(args, []string{"/ws/node_modules/typescript/bin/tsc", "-b"}) {
		t.Errorf("ran %s %q, want bun on the compiler's script", name, args)
	}
}

func isTscError(err error, code string) bool {
	var refusal *TscError
	return errors.As(err, &refusal) && refusal.Code == code
}

func relOrEmpty(base, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// The cut lists a symbolic link, a submodule or a nested repository as one
// entry and nothing below it. An output below such an entry would pass a
// check that reads only the entries at or below the output, and emptying it
// would delete what Git tracks through the link or in the inner repository:
// the check refuses it.
func TestCheckTscOutputsRefusesAnOutputBelowAOneEntryPath(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	for _, tc := range []struct {
		name  string
		entry job.CandidateFile
		code  string
	}{
		{"a tracked symbolic link", job.CandidateFile{Path: "libs/lib/gen", Mode: job.CandidateModeSymlink}, CodeTscOutputTracked},
		{"a submodule", job.CandidateFile{Path: "libs/lib/gen", Mode: job.CandidateModeSubmodule}, CodeTscOutputTracked},
		{"an untracked nested repository", job.CandidateFile{Path: "libs/lib/gen", Mode: job.CandidateModeUntracked}, CodeTscOutputIgnored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := tscWorkspace(t, map[string]string{
				"libs/lib/package.json": libPackage("tsc -b"),
				"libs/lib/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src",` +
					` "outDir": "gen/dist", "declaration": true}, "include": ["src"]}`,
			})
			plan, err := PlanTsc(project, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", "")
			ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
				Directory:            "libs/lib",
				CandidateDirectories: []string{"libs/lib"},
				CandidateFiles:       []job.CandidateFile{tc.entry, {Path: "libs/lib/package.json", Mode: job.CandidateModeRegular}},
			}}
			err = CheckTscOutputs(root, project, plan, ctx)
			if !isTscError(err, tc.code) || !strings.Contains(err.Error(), "gen/dist") || !strings.Contains(err.Error(), "below gen") {
				t.Errorf("an outDir below %s = %v, want %s naming gen/dist below gen", tc.name, err, tc.code)
			}
		})
	}
}

// An output below a symbolic link is refused before anything is emptied,
// whatever the cut says: the link may lead out of the project, or to a
// directory of the project whose tracked files the cut lists under another
// path.
func TestCheckTscOutputsRefusesAnOutputBelowASymbolicLink(t *testing.T) {
	spectest.Proves(t, tscFeature, "tsc-output-restores-at-outdir", "an-unusable-outdir-fails-before-tsc-runs")
	for _, tc := range []struct {
		name   string
		target func(root, project string) string
		cut    []job.CandidateFile
	}{
		{"out of the project", func(root, _ string) string { return filepath.Join(root, "outside") }, nil},
		{"to a directory of the project holding a tracked file", func(_, project string) string { return filepath.Join(project, "real") },
			[]job.CandidateFile{{Path: "libs/lib/real/dist/tracked.js", Mode: job.CandidateModeRegular}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := tscWorkspace(t, map[string]string{
				"libs/lib/package.json": libPackage("tsc -b"),
				"libs/lib/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src",` +
					` "outDir": "gen/dist", "declaration": true}, "include": ["src"]}`,
				"libs/lib/real/dist/tracked.js": "",
				"outside/keep.txt":              "kept",
			})
			// The link is ignored, so it is not in the cut.
			if err := os.Symlink(tc.target(root, project), filepath.Join(project, "gen")); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanTsc(project, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", "")
			ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
				Directory:            "libs/lib",
				CandidateDirectories: []string{"libs/lib"},
				CandidateFiles:       append([]job.CandidateFile{{Path: "libs/lib/package.json", Mode: job.CandidateModeRegular}}, tc.cut...),
			}}
			err = CheckTscOutputs(root, project, plan, ctx)
			if !isTscError(err, CodeTscOutDir) || !strings.Contains(err.Error(), "below the symbolic link gen") {
				t.Errorf("an outDir below a link %s = %v, want %s naming the link", tc.name, err, CodeTscOutDir)
			}
		})
	}
}
