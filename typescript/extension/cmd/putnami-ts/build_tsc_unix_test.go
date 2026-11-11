//go:build unix

package main

import (
	"os"
	stdexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// tscWorkspaceFiles is a workspace of two tsc projects: lib, which builds with
// tsc -b, and app, which imports lib through its package and references it.
var tscWorkspaceFiles = map[string]string{
	".gitignore":   "node_modules\ndist\n*.tsbuildinfo\n.putnami\n",
	"package.json": `{"name":"ws","private":true,"workspaces":["libs/lib","apps/app"]}` + "\n",
	"libs/lib/package.json": `{"name":"lib","type":"module","main":"dist/index.js","types":"dist/index.d.ts",` +
		`"scripts":{"build":"tsc -b"}}` + "\n",
	"libs/lib/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist",` +
		` "module": "nodenext", "target": "es2022", "strict": true, "types": []}, "include": ["src"]}` + "\n",
	"libs/lib/src/index.ts": "export const add = (a: number, b: number): number => a + b;\n",
	"apps/app/package.json": `{"name":"app","type":"module","dependencies":{"lib":"workspace:*"},` +
		`"scripts":{"build":"tsc -b --verbose"}}` + "\n",
	"apps/app/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist",` +
		` "module": "nodenext", "target": "es2022", "strict": true, "types": []}, "include": ["src"],` +
		` "references": [{"path": "../../libs/lib"}]}` + "\n",
	"apps/app/src/index.ts": "import { add } from 'lib';\n\nexport const three: number = add(1, 2);\n",
}

func writeTscWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range tscWorkspaceFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := stdexec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	nodeModules := filepath.Join(root, "node_modules")
	linkPackage(t, installedNodeModules(t), nodeModules, "typescript")
	if err := os.Symlink(filepath.Join(root, "libs", "lib"), filepath.Join(nodeModules, "lib")); err != nil {
		t.Fatal(err)
	}
	return root
}

func tscJob(root, project string) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: root,
		OutputPath:    filepath.Join(root, ".putnami", "out", project, "build"),
		Project:       pctx.Project{Name: filepath.Base(project), Path: project},
		Extension:     pctx.Extension{Name: "@putnami/typescript"},
		Params:        pctx.Params{},
	}
}

// TestBuildTscCompilesWithTheScriptsTscAndLeavesAReferenceAlone runs build-tsc
// on lib and app with the real bun and TypeScript, on a PATH that holds no
// node. lib's outputs are then made older than its sources, the way a restore
// from the store leaves them on a fresh checkout: app's tsc -b, which runs as
// tsc -p, compiles app and leaves lib untouched.
func TestBuildTscCompilesWithTheScriptsTscAndLeavesAReferenceAlone(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-build-runs-the-installed-tsc", "tsc-runs-with-bun-and-no-node")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-build-runs-the-installed-tsc", "tsc-b-with-references-runs-as-tsc-p")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "phase-isolation", "a-tsc-build-writes-nothing-in-a-referenced-project")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-compiler-choice", "a-tsc-project-plans-no-bun-transpile-or-types")
	bun := requireBun(t)
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := writeTscWorkspace(t)
	// The outputs are checked against Git, so PATH holds git beside bun, and
	// still no node.
	gitBin := t.TempDir()
	git, err := stdexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(gitBin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathWithout(t, bun, gitBin))
	if node, err := stdexec.LookPath("node"); err == nil {
		t.Fatalf("PATH still holds %s", node)
	}

	status, data, err := runBuildTsc(tscJob(root, "libs/lib"), jsonl.New(), nil)
	expectJobOK(t, "lib build~tsc", status, err)
	if data[TscOutDirPort] != "dist" || data[TscBuildInfoPort] != "tsconfig.tsbuildinfo" {
		t.Errorf("lib ports = %v, want outDir dist and buildInfo tsconfig.tsbuildinfo", data)
	}
	lib := filepath.Join(root, "libs", "lib")
	for _, path := range []string{"dist/index.js", "dist/index.d.ts", "tsconfig.tsbuildinfo"} {
		if _, err := os.Stat(filepath.Join(lib, path)); err != nil {
			t.Errorf("lib emitted no %s: %v", path, err)
		}
	}

	// The Bun steps compile nothing for a tsc project.
	for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){
		"build~transpile": runBuildTranspile, "build~types": runBuildTypes,
	} {
		status, _, err := job(tscJob(root, "libs/lib"), jsonl.New(), nil)
		expectJobOK(t, "lib "+name, status, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, ".putnami", "out", "libs/lib", "build")); len(entries) > 0 {
		t.Errorf("the Bun steps wrote %d entries for a tsc project", len(entries))
	}

	// lib's outputs, as a restore leaves them: older than its sources.
	past := time.Now().Add(-time.Hour)
	for _, path := range []string{"dist/index.js", "dist/index.d.ts", "tsconfig.tsbuildinfo"} {
		if err := os.Chtimes(filepath.Join(lib, path), past, past); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(filepath.Join(lib, "tsconfig.tsbuildinfo"))
	if err != nil {
		t.Fatal(err)
	}

	status, data, err = runBuildTsc(tscJob(root, "apps/app"), jsonl.New(), nil)
	expectJobOK(t, "app build~tsc", status, err)
	if data[TscOutDirPort] != "dist" {
		t.Errorf("app ports = %v, want outDir dist", data)
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "app", "dist", "index.js")); err != nil {
		t.Errorf("app emitted no dist/index.js: %v", err)
	}
	after, err := os.Stat(filepath.Join(lib, "tsconfig.tsbuildinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("app's build rewrote lib's .tsbuildinfo (%s, was %s): it rebuilt a reference", after.ModTime(), before.ModTime())
	}
}
