//go:build unix

package main

import (
	"encoding/json"
	"net"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// starterFiles is a TypeScript server project the way `putnami init` leaves
// one, without the framework: a ./serve export, a module it serves, and a test
// of that module. serve.ts answers one request to itself, writes the answer to
// the file STARTER_SERVED names and exits, so the serve job ends on its own.
var starterFiles = map[string]string{
	".gitignore":   "node_modules\ndist\n.putnami\ncoverage\n",
	"package.json": `{"name":"starter","private":true,"workspaces":["webapp"]}` + "\n",
	"webapp/package.json": `{
  "name": "webapp",
  "private": true,
  "main": "src/main.ts",
  "exports": {
    "./serve": "./src/serve.ts"
  }
}
`,
	"webapp/tsconfig.json": `{ "extends": "../tsconfig.json" }` + "\n",
	"webapp/src/main.ts": `export function handle(request: Request): Response {
  return Response.json({ path: new URL(request.url).pathname });
}
`,
	"webapp/src/serve.ts": `import { handle } from './main';

const server = Bun.serve({ fetch: handle });
const response = await fetch(new URL('/ready', server.url));
await Bun.write(process.env['STARTER_SERVED'] ?? 'served.json', await response.text());
await server.stop();
`,
	"webapp/test/main.test.ts": `import { expect, test } from 'bun:test';
import { handle } from '../src/main';

test('answers with the path of the request', async () => {
  const response = handle(new Request('http://localhost/ready'));
  expect(await response.json()).toEqual({ path: '/ready' });
});
`,
}

// installedNodeModules returns the nearest node_modules directory above this
// package that holds the tools the TypeScript extension installs in a
// workspace: Biome, TypeScript and the Bun types.
func installedNodeModules(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		nodeModules := filepath.Join(dir, "node_modules")
		installed := true
		for _, pkg := range []string{"@biomejs/biome", "typescript", "@types/bun"} {
			if _, err := os.Stat(filepath.Join(nodeModules, filepath.FromSlash(pkg), "package.json")); err != nil {
				installed = false
			}
		}
		if installed {
			return nodeModules
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("the workspace tools are not installed: run `putnami install`")
		}
		dir = parent
	}
}

// linkPackage links the real directory of the installed package pkg at
// nodeModules/pkg and returns that real directory.
func linkPackage(t *testing.T, installed, nodeModules, pkg string) string {
	t.Helper()
	realDir, err := filepath.EvalSymlinks(filepath.Join(installed, filepath.FromSlash(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(nodeModules, filepath.FromSlash(pkg))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	return realDir
}

// linkBin links name in nodeModules/.bin to target, relative to node_modules,
// the way a package manager does.
func linkBin(t *testing.T, nodeModules, name, target string) {
	t.Helper()
	bin := filepath.Join(nodeModules, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", filepath.FromSlash(target)), filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
}

// writeStarter writes starterFiles and the workspace configuration
// workspace-install materializes under a new workspace root, and returns the
// context of a job on its webapp project.
func writeStarter(t *testing.T) *pctx.Context {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range starterFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	extensionRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if extensionRoot, err = filepath.EvalSymlinks(extensionRoot); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		OutputPath:    filepath.Join(root, ".putnami", "output", "webapp"),
		Project:       pctx.Project{Name: "webapp", Path: "webapp"},
		Extension:     pctx.Extension{Name: "@putnami/typescript", Root: extensionRoot},
		Params:        pctx.Params{},
	}
	if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceTsConfig(ctx, jsonl.New()); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceBiomeConfig(ctx, jsonl.New()); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// pathWithout returns a PATH that holds bun and no node, after the
// directories of before.
func pathWithout(t *testing.T, bun string, before ...string) string {
	t.Helper()
	realBun, err := filepath.EvalSymlinks(bun)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(realBun, filepath.Join(bin, "bun")); err != nil {
		t.Fatal(err)
	}
	path := bin
	for i := len(before) - 1; i >= 0; i-- {
		path = before[i] + string(os.PathListSeparator) + path
	}
	return path
}

// expectJobOK fails the test unless a job ended OK.
func expectJobOK(t *testing.T, job, status string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", job, err)
	}
	if status != "OK" {
		t.Fatalf("%s ended %s, want OK", job, status)
	}
}

func withParams(ctx *pctx.Context, params map[string]any) *pctx.Context {
	job := *ctx
	job.Params = pctx.Params{}
	for name, value := range params {
		raw, _ := json.Marshal(value)
		job.Params[name] = raw
	}
	return &job
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// TestJobsRunWithNoNodeOnPath runs lint, test, build and serve on a starter
// project with the real bun, Biome and TypeScript, on a PATH that holds bun
// and nothing else.
func TestJobsRunWithNoNodeOnPath(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", "lint-test-build-and-serve-run-with-no-node-on-path")
	bun := requireBun(t)
	installed := installedNodeModules(t)

	ctx := writeStarter(t)
	nodeModules := filepath.Join(ctx.WorkspaceRoot, "node_modules")
	for _, pkg := range []string{"@biomejs/biome", "typescript", "@types/bun"} {
		linkPackage(t, installed, nodeModules, pkg)
	}
	linkBin(t, nodeModules, "biome", "@biomejs/biome/bin/biome")
	linkBin(t, nodeModules, "tsc", "typescript/bin/tsc")

	t.Setenv("PATH", pathWithout(t, bun))
	if node, err := stdexec.LookPath("node"); err == nil {
		t.Fatalf("PATH still holds %s", node)
	}
	served := filepath.Join(t.TempDir(), "served.json")
	t.Setenv("STARTER_SERVED", served)

	biome, err := resolveBiomeBinFn(filepath.Join(ctx.WorkspaceRoot, "webapp"), ctx.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(biome)) == ".bin" {
		t.Errorf("biome resolves to the launcher %s, want the native executable of the platform package", biome)
	}

	jobs := []struct {
		name string
		run  func() (string, map[string]any, error)
	}{
		{"lint", func() (string, map[string]any, error) { return runLint(ctx, jsonl.New(), nil) }},
		{"lint~format", func() (string, map[string]any, error) { return runLintFormat(ctx, jsonl.New(), nil) }},
		{"lint~check", func() (string, map[string]any, error) { return runLintCheck(ctx, jsonl.New(), nil) }},
		{"test", func() (string, map[string]any, error) { return runTest(ctx, jsonl.New(), nil) }},
		{"build", func() (string, map[string]any, error) {
			return runBuild(withParams(ctx, map[string]any{"transpile": true, "types": true}), jsonl.New(), nil)
		}},
		{"serve", func() (string, map[string]any, error) {
			return runServe(withParams(ctx, map[string]any{"watch": false, "port": freePort(t)}), jsonl.New(), nil)
		}},
	}
	for _, job := range jobs {
		t.Run(job.name, func(t *testing.T) {
			status, _, err := job.run()
			expectJobOK(t, job.name, status, err)
		})
	}

	declarations, err := filepath.Glob(filepath.Join(ctx.OutputPath, "types", "src", "*.d.ts"))
	if err != nil || len(declarations) == 0 {
		t.Errorf("build wrote no type declaration under %s: tsc did not run", filepath.Join(ctx.OutputPath, "types"))
	}
	if answer, err := os.ReadFile(served); err != nil || string(answer) != `{"path":"/ready"}` {
		t.Errorf("serve answered %q (%v), want the path of the request", answer, err)
	}
}

// TestLintStartsTheLauncherWithBunNotNode lints a starter whose platform
// package is installed where only the launcher finds it. PATH holds a node
// that records each start: lint succeeds and node never starts.
func TestLintStartsTheLauncherWithBunNotNode(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", "a-missing-platform-package-starts-the-launcher-with-bun")
	bun := requireBun(t)
	installed := installedNodeModules(t)
	realLauncherPackage, err := filepath.EvalSymlinks(filepath.Join(installed, "@biomejs", "biome"))
	if err != nil {
		t.Fatal(err)
	}
	platformPackages, err := filepath.Glob(filepath.Join(filepath.Dir(realLauncherPackage), "cli-*"))
	if err != nil || len(platformPackages) == 0 {
		t.Skipf("no platform package of Biome is installed beside %s", realLauncherPackage)
	}

	ctx := writeStarter(t)
	nodeModules := filepath.Join(ctx.WorkspaceRoot, "node_modules")
	launcherPackage := filepath.Join(nodeModules, "@biomejs", "biome")
	launcher, err := os.ReadFile(filepath.Join(installed, "@biomejs", "biome", "bin", "biome"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(launcherPackage, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcherPackage, "bin", "biome"), launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(launcherPackage, "node_modules", "@biomejs")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, platformPackage := range platformPackages {
		if err := os.Symlink(platformPackage, filepath.Join(nested, filepath.Base(platformPackage))); err != nil {
			t.Fatal(err)
		}
	}
	linkBin(t, nodeModules, "biome", "@biomejs/biome/bin/biome")

	tripwire := t.TempDir()
	started := filepath.Join(tripwire, "node.started")
	script := "#!/bin/sh\necho started >> " + strconv.Quote(started) + "\nexit 127\n"
	if err := os.WriteFile(filepath.Join(tripwire, "node"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathWithout(t, bun, tripwire))

	biome, err := resolveBiomeBinFn(filepath.Join(ctx.WorkspaceRoot, "webapp"), ctx.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(nodeModules, ".bin", "biome"); biome != want {
		t.Fatalf("biome resolves to %s, want the launcher %s", biome, want)
	}

	status, _, err := runLint(ctx, jsonl.New(), nil)
	expectJobOK(t, "lint", status, err)
	if _, err := os.Stat(started); err == nil {
		t.Error("lint started node from PATH")
	}
}
