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
	if err := CheckTscOutputs(project, plan); err != nil {
		t.Errorf("ignored outputs = %v, want accepted", err)
	}
	project, plan = setup("node_modules\n*.tsbuildinfo\n", false)
	if err := CheckTscOutputs(project, plan); !isTscError(err, CodeTscOutputIgnored) || !strings.Contains(err.Error(), "dist") {
		t.Errorf("an outDir Git does not ignore = %v, want %s naming dist", err, CodeTscOutputIgnored)
	}
	project, plan = setup("node_modules\ndist/\n", false)
	if err := CheckTscOutputs(project, plan); !isTscError(err, CodeTscOutputIgnored) || !strings.Contains(err.Error(), "tsconfig.tsbuildinfo") {
		t.Errorf("a .tsbuildinfo Git does not ignore = %v, want %s naming it", err, CodeTscOutputIgnored)
	}
	project, plan = setup("node_modules\ndist/\n*.tsbuildinfo\n", true)
	if err := CheckTscOutputs(project, plan); !isTscError(err, CodeTscOutputTracked) || !strings.Contains(err.Error(), "dist/index.js") {
		t.Errorf("a tracked file in the outDir = %v, want %s naming it", err, CodeTscOutputTracked)
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
