package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

func TestANodeProjectTranspilesForNodeUnlessTheCommandNamesATarget(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-workload-runtime", "a-node-project-transpiles-for-node-unless-the-command-names-a-target")
	cases := []struct {
		params pctx.Params
		want   string
	}{
		{pctx.Params{}, "bun"},
		{pctx.Params{"runtime": json.RawMessage(`"bun"`)}, "bun"},
		{pctx.Params{"runtime": json.RawMessage(`"node"`)}, "node"},
		{pctx.Params{"runtime": json.RawMessage(`"node"`), "target": json.RawMessage(`"browser"`)}, "browser"},
		{pctx.Params{"target": json.RawMessage(`"node"`)}, "node"},
	}
	for _, c := range cases {
		params, err := transpileParams(&pctx.Context{Params: c.params})
		if err != nil {
			t.Fatal(err)
		}
		if params.Target != c.want {
			t.Errorf("params %v: target = %q, want %q", c.params, params.Target, c.want)
		}
	}
	if _, err := transpileParams(&pctx.Context{Params: pctx.Params{"runtime": json.RawMessage(`"deno"`)}}); errs.CodeOf(err) != errs.CodeInvalidRuntime {
		t.Errorf("runtime deno: error %v, want %s", err, errs.CodeInvalidRuntime)
	}
}

// nodeAppCtx is a project on the node runtime whose src/main.ts imports a
// local module, a workspace project linked into node_modules and an installed
// package.
func nodeAppCtx(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	files := map[string]string{
		"project/package.json":                       `{"name":"@test/node-app","type":"module","main":"src/main.ts","dependencies":{"left-pad":"1","@test/lib":"workspace:*"}}`,
		"project/src/main.ts":                        "import pad from \"left-pad\";\nimport { shout } from \"@test/lib\";\nimport { greeting } from \"./greeting\";\nconsole.log(shout(pad(greeting(), 8)));\nprocess.exit(Number(process.argv[2] ?? 0));\n",
		"project/src/greeting.ts":                    "export const greeting = (): string => \"hi\";\n",
		"project/node_modules/left-pad/package.json": `{"name":"left-pad","main":"index.js"}`,
		"project/node_modules/left-pad/index.js":     "module.exports = (s, n) => String(s).padStart(n, \"_\");\n",
		"lib/package.json":                           `{"name":"@test/lib","exports":{".":"./src/index.ts"},"dependencies":{"exclaim":"1"}}`,
		"lib/src/index.ts":                           "import exclaim from \"exclaim\";\nexport const shout = (s: string): string => exclaim(s.toUpperCase());\n",
		"node_modules/exclaim/package.json":          `{"name":"exclaim","main":"index.js"}`,
		"node_modules/exclaim/index.js":              "module.exports = (s) => s + \"!\";\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "project", "node_modules", "@test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "lib"), filepath.Join(dir, "project", "node_modules", "@test", "lib")); err != nil {
		t.Fatal(err)
	}
	ctx.Params = pctx.Params{"runtime": json.RawMessage(`"node"`)}
	ctx.Project.Metadata = map[string]json.RawMessage{tsExtensionName: json.RawMessage(`{"main":"src/main.ts"}`)}
	bun, err := toolchain.ResolveBun()
	if err != nil {
		t.Fatal(err)
	}
	orig := resolveBunBin
	t.Cleanup(func() { resolveBunBin = orig })
	resolveBunBin = func() (string, error) { return bun, nil }
	return ctx
}

func TestANodeCompileWritesANodeBundle(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-compile-bundle", "a-node-compile-writes-a-node-bundle")
	ctx := nodeAppCtx(t)
	status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("compile = %s, %v", status, err)
	}
	out := filepath.Join(ctx.OutputPath, "compile")
	bundle, err := build.ReadNodeBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Entrypoint != "src/main.js" {
		t.Fatalf("entrypoint = %q, want src/main.js", bundle.Entrypoint)
	}
	data, err := os.ReadFile(filepath.Join(out, "src", "main.js"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, want := range []string{`from "left-pad"`, `from "exclaim"`, "toUpperCase", `"hi"`, "//# sourceMappingURL=main.js.map"} {
		if !strings.Contains(source, want) {
			t.Errorf("bundle lacks %q:\n%s", want, source)
		}
	}
	for _, unwanted := range []string{`"@test/lib"`, `"./greeting"`, "padStart"} {
		if strings.Contains(source, unwanted) {
			t.Errorf("bundle holds %q:\n%s", unwanted, source)
		}
	}
	if pkg, err := os.ReadFile(filepath.Join(out, "package.json")); err != nil || !strings.Contains(string(pkg), `"type": "module"`) {
		t.Errorf("bundle package.json = %s, %v", pkg, err)
	}
	if _, err := os.Stat(filepath.Join(out, "src", "main.js.map")); err != nil {
		t.Errorf("no source map: %v", err)
	}
}

func TestNodeRunForwardsTheExitCode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-run", "node-run-forwards-the-exit-code")
	ctx := nodeAppCtx(t)
	ctx.Params["args"] = json.RawMessage(`"7"`)
	realNode, realNodeErr := exec.LookPath("node")
	fakeDir := t.TempDir()
	fakeNode := filepath.Join(fakeDir, "node")
	if err := os.WriteFile(fakeNode, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var gotBin string
	var gotArgs []string
	orig := runOnceFunc
	t.Cleanup(func() { runOnceFunc = orig })
	runOnceFunc = func(_ *jsonl.Emitter, bin string, args []string, _ string, _ int) int {
		gotBin, gotArgs = bin, args
		return 7
	}

	status, data, err := runRun(ctx, jsonl.New(), nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("run = %s, %v", status, err)
	}
	if data[runExitCodeKey] != 7 {
		t.Fatalf("exit code = %v, want 7", data[runExitCodeKey])
	}
	entry := filepath.Join(ctx.OutputPath, "node", "src", "main.js")
	if gotBin != fakeNode || strings.Join(gotArgs, " ") != "--enable-source-maps "+entry+" 7" {
		t.Fatalf("started %s %v", gotBin, gotArgs)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(ctx.WorkspaceRoot, "project", "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if target, err := dirlink.Resolve(filepath.Join(ctx.OutputPath, "node", "node_modules")); err != nil || target != want {
		t.Errorf("node_modules link = %q, %v; want %q", target, err, want)
	}

	// The bundle runs on a real node, which resolves the installed package
	// through that link.
	if realNodeErr == nil {
		out, err := exec.Command(realNode, "--enable-source-maps", entry, "3").CombinedOutput()
		var exitErr *exec.ExitError
		if !strings.Contains(string(out), "______HI!") || err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Errorf("node run: %s, %v", out, err)
		}
	}
}

func TestRunWithoutNodeFailsNamingNode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-run", "serve-and-run-without-node-fail-naming-node")
	ctx := nodeAppCtx(t)
	t.Setenv("PATH", t.TempDir())
	for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){"run": runRun, "serve": runServe} {
		var status string
		events := captureEvents(t, func() { status, _, _ = job(ctx, jsonl.New(), nil) })
		if status != "FAILED" {
			t.Errorf("%s without node = %s", name, status)
		}
		if found := diagnosticsWithCode(events, errs.CodeNodeNotFound.String()); len(found) != 1 {
			t.Errorf("%s without node: %s diagnostics = %v", name, errs.CodeNodeNotFound, events)
		}
	}
}

func TestAFailedNodeBundleKeepsThePreviousOne(t *testing.T) {
	ctx := nodeAppCtx(t)
	bun, err := resolveBunBin()
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, "project")
	outDir := filepath.Join(ctx.OutputPath, "node")
	bundler := nodeBundler(bun, projectPath, outDir, "src/main.ts")
	built, bundleErrors, err := bundler()
	if err != nil || len(bundleErrors) > 0 {
		t.Fatalf("bundle = %v, %v", bundleErrors, err)
	}
	if !slicesContains(built.Inputs, filepath.Join(projectPath, "src", "greeting.ts")) {
		t.Errorf("inputs %v lack src/greeting.ts", built.Inputs)
	}
	before, err := os.ReadFile(built.Entry)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projectPath, "src", "greeting.ts"), []byte("export const greeting = ( =>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, bundleErrors, err := bundler(); err != nil || len(bundleErrors) == 0 {
		t.Fatalf("a source that does not parse bundled: %v, %v", bundleErrors, err)
	}
	after, err := os.ReadFile(built.Entry)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the previous bundle did not survive a failed one: %v", err)
	}
	if _, err := dirlink.Resolve(filepath.Join(outDir, "node_modules")); err != nil {
		t.Errorf("the node_modules link did not survive a failed bundle: %v", err)
	}
	if _, err := os.Stat(outDir + ".next"); !os.IsNotExist(err) {
		t.Errorf("a failed bundle left its staging directory: %v", err)
	}
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
