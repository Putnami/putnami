// Package tscbuild drives a workspace of two TypeScript libraries built with
// tsc through the real CLI: @putnami/typescript and @putnami/dev, loaded from
// this tree, compile each with the tsc its build script runs, into the outDir
// its tsconfig names, and a later build restores a deleted outDir from the
// cache. These tests swap the process streams and the working directory, so
// they run in their own test binary.
package tscbuild

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	abs, err := filepath.Abs("../../../../../..")
	if err != nil {
		panic(err)
	}
	repoRoot = abs
	os.Exit(clitest.Main(m))
}

// repoRoot is the root of this repository, absolute, since the CLI runs
// change the working directory.
var repoRoot string

// files is a workspace of lib, built with tsc -b, and app, which imports lib
// through its package, references it and builds with tsc -b too.
var files = map[string]string{
	".gitignore":             "node_modules\ndist\n*.tsbuildinfo\n.putnami\n.gen\n",
	"package.json":           `{"name": "ws", "private": true, "packageManager": "bun@1.4.0", "workspaces": ["libs/lib", "apps/app"]}` + "\n",
	"putnami.workspace.json": `{"name": "ws", "extensions": ["/.ext/typescript", "/.ext/dev"], "includes": ["libs/lib", "apps/app"]}` + "\n",
	"libs/lib/putnami.json":  `{"name": "lib", "extensions": ["@putnami/typescript"]}` + "\n",
	"libs/lib/package.json": `{"name": "lib", "type": "module", "main": "dist/index.js", "types": "dist/index.d.ts",` +
		` "scripts": {"build": "tsc -b"}}` + "\n",
	"libs/lib/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist",` +
		` "module": "nodenext", "target": "es2022", "strict": true, "types": []}, "include": ["src"]}` + "\n",
	"libs/lib/src/index.ts": "export const add = (a: number, b: number): number => a + b;\n",
	"apps/app/putnami.json": `{"name": "app", "extensions": ["@putnami/typescript"], "dependencies": ["/libs/lib"]}` + "\n",
	"apps/app/package.json": `{"name": "app", "type": "module", "dependencies": {"lib": "workspace:*"},` +
		` "scripts": {"build": "tsc -b"}}` + "\n",
	"apps/app/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist",` +
		` "module": "nodenext", "target": "es2022", "strict": true, "types": []}, "include": ["src"],` +
		` "references": [{"path": "../../libs/lib"}]}` + "\n",
	"apps/app/src/index.ts": "import { add } from 'lib';\n\nexport const three: number = add(1, 2);\n",
}

// writeWorkspace writes files under a new Git work tree, links
// @putnami/typescript and @putnami/dev from this tree under .ext, which the
// project scan never enters, and installs the workspace the way a package
// manager would: this repository's typescript, and lib linked under its name.
func writeWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	links := map[string]string{
		".ext/typescript":         filepath.Join(repoRoot, "typescript/extension"),
		".ext/dev":                filepath.Join(repoRoot, "tooling/dev"),
		"node_modules/typescript": filepath.Join(repoRoot, "node_modules/typescript"),
		"node_modules/lib":        filepath.Join(root, "libs/lib"),
	}
	for rel, target := range links {
		real, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Skipf("%s is not installed: %v", target, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	pinToolchains(t, root, "bun", "go")
	return root
}

// pinToolchains copies this repository's pins of the toolchains into the
// workspace lock: the extensions loaded from this tree compile their runtimes
// with the Go release it pins, and the Bun pin keeps the run offline.
func pinToolchains(t *testing.T, root string, names ...string) {
	t.Helper()
	var source map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(repoRoot, "putnami.lock.json"))
	if err != nil || json.Unmarshal(data, &source) != nil {
		t.Fatalf("read this repository's lock: %v", err)
	}
	var from map[string]json.RawMessage
	if err := json.Unmarshal(source["toolchains"], &from); err != nil {
		t.Fatal(err)
	}
	into := map[string]json.RawMessage{}
	for _, name := range names {
		into[name] = from[name]
	}
	lock := struct {
		Version    json.RawMessage            `json:"version"`
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}{source["version"], into}
	if data, err = json.MarshalIndent(lock, "", "  "); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.lock.json"), string(data)+"\n")
}

// jobLine returns the status the run printed for a job, such as done or
// cached, or "" when it printed none.
func jobLine(out, project, job string) string {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(project+" "+job) + `\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestATscLibraryBuildsBeforeItsDependentAndItsOutDirRestoresFromTheCache is
// the acceptance of a tsc build: lib and app build with tsc, app after lib;
// once lib's outDir is deleted, the next build restores it from the cache.
func TestATscLibraryBuildsBeforeItsDependentAndItsOutDirRestoresFromTheCache(t *testing.T) {
	clitest.RequireShell(t)
	for _, tool := range []string{"bun", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the build runs %s, which is not on PATH: %v", tool, err)
		}
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := writeWorkspace(t)

	code, out := clitest.RunGateArgs(t, root, "build", "--all")
	if code != 0 {
		t.Fatalf("build exit %d:\n%s", code, out)
	}
	for _, project := range []string{"lib", "app"} {
		if status := jobLine(out, project, "build~tsc-auto"); status != "done" {
			t.Errorf("%s build~tsc-auto = %q, want done:\n%s", project, status, out)
		}
	}
	// app compiled against lib's declarations, so lib's build ran first.
	for _, path := range []string{"libs/lib/dist/index.js", "libs/lib/dist/index.d.ts", "apps/app/dist/index.js"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("the build emitted no %s: %v", path, err)
		}
	}
	if strings.Index(out, "lib build~tsc-auto") > strings.Index(out, "app build~tsc-auto") {
		t.Errorf("app's tsc finished before lib's:\n%s", out)
	}

	if err := os.RemoveAll(filepath.Join(root, "libs/lib/dist")); err != nil {
		t.Fatal(err)
	}
	code, out = clitest.RunGateArgs(t, root, "build", "--all")
	if code != 0 {
		t.Fatalf("second build exit %d:\n%s", code, out)
	}
	if status := jobLine(out, "lib", "build~tsc-auto"); status != "cached" {
		t.Errorf("lib build~tsc-auto = %q, want cached:\n%s", status, out)
	}
	data, err := os.ReadFile(filepath.Join(root, "libs/lib/dist/index.js"))
	if err != nil || !strings.Contains(string(data), "add") {
		t.Errorf("the cache did not restore lib's outDir: %v\n%s", err, data)
	}
}
