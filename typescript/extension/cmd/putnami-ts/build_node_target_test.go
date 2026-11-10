package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

// repositoryRoot returns the root of this repository, from the package
// directory the test runs in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err != nil {
		t.Fatalf("repository root not found at %s: %v", dir, err)
	}
	return dir
}

// nodeProjectCtx lays out a project whose one export re-exports from pkg, a
// framework package of this repository linked into its node_modules with the
// typescript package, and returns a context that transpiles it for Node.js.
func nodeProjectCtx(t *testing.T, pkg, source string) *pctx.Context {
	t.Helper()
	root := repositoryRoot(t)
	typescript := filepath.Join(root, "node_modules", "typescript")
	if _, err := os.Stat(filepath.Join(typescript, "package.json")); err != nil {
		t.Fatal("the typescript package is not installed in this repository: run ./putnamiw install")
	}
	ctx, dir := makeTestCtx(t)
	project := filepath.Join(dir, "project")
	files := map[string]string{
		"package.json": `{"name":"@test/node-app","type":"module","exports":{".":"./src/index.ts"},"dependencies":{"@putnami/` + pkg + `":"*"}}`,
		"src/index.ts": source,
	}
	for name, content := range files {
		path := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		filepath.Join(project, "node_modules", "typescript"):    typescript,
		filepath.Join(project, "node_modules", "@putnami", pkg): filepath.Join(root, "typescript", "framework", pkg),
	}
	for link, target := range links {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	ctx.Params = pctx.Params{"target": json.RawMessage(`"node"`)}
	return ctx
}

func diagnosticsWithCode(events []map[string]any, code string) []map[string]any {
	var out []map[string]any
	for _, event := range events {
		if event["type"] == "diagnostic" && event["code"] == code {
			out = append(out, event)
		}
	}
	return out
}

func TestANodeBuildThatImportsTheRuntimeFailsAndNamesTheChain(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-target-refuses-bun-apis", "a-node-build-fails-on-a-bun-api-and-names-the-chain")
	ctx := nodeProjectCtx(t, "runtime", "export { loadConfig } from '@putnami/runtime';\n")

	var status string
	var err error
	events := captureEvents(t, func() { status, _, err = runBuildTranspile(ctx, jsonl.New(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	found := false
	for _, event := range diagnosticsWithCode(events, errs.CodeBunAPIOnNode.String()) {
		message, _ := event["message"].(string)
		if strings.Contains(message, "framework/runtime/src/config/config-source.ts references the Bun global") &&
			strings.Contains(message, "reached by src/index.ts -> ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s diagnostic names the runtime's config source and its chain: %v", errs.CodeBunAPIOnNode, events)
	}
}

func TestANodeBuildThatImportsOnlyTheCLIProtocolPasses(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-target-refuses-bun-apis", "a-node-build-passes-without-a-bun-api")
	ctx := nodeProjectCtx(t, "cli-protocol", "export * from '@putnami/cli-protocol';\n")

	var status string
	var err error
	events := captureEvents(t, func() { status, _, err = runBuildTranspile(ctx, jsonl.New(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK: %v", status, events)
	}
}

// stubNodeCheck replaces the node-target check for the duration of a test.
func stubNodeCheck(t *testing.T, fn func(string, string) ([]build.BunAPIUse, error)) {
	t.Helper()
	original := checkBunAPIsOnNodeFunc
	t.Cleanup(func() { checkBunAPIsOnNodeFunc = original })
	checkBunAPIsOnNodeFunc = fn
}

func TestTheNodeCheckRunsOnlyForTheNodeTarget(t *testing.T) {
	called := false
	stubNodeCheck(t, func(string, string) ([]build.BunAPIUse, error) {
		called = true
		return nil, nil
	})
	for _, target := range []string{"bun", "browser", ""} {
		if diagnostics := checkBunAPIsOnNode("bun", t.TempDir(), build.TranspileParams{Target: target}); diagnostics != nil || called {
			t.Fatalf("target %q ran the node-target check", target)
		}
	}
}

func TestTheNodeCheckReportsEachUseWithItsFirstPosition(t *testing.T) {
	stubNodeCheck(t, func(string, string) ([]build.BunAPIUse, error) {
		return []build.BunAPIUse{
			{Module: "src/a.ts", Globals: []build.SourcePosition{{Line: 4, Column: 2}, {Line: 9, Column: 1}}, Chain: []string{"src/index.ts", "src/a.ts"}},
			{Module: "src/b.ts", Imports: []string{"bun:sqlite"}, Chain: []string{"src/index.ts", "src/b.ts"}},
		}, nil
	})
	diagnostics := checkBunAPIsOnNode("bun", t.TempDir(), build.TranspileParams{Target: "node"})
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics = %#v, want two", diagnostics)
	}
	first := diagnostics[0]
	if first.Category != errs.CodeBunAPIOnNode.String() || first.File != "src/a.ts" || first.Line != 4 || first.Column != 2 {
		t.Fatalf("first diagnostic = %#v", first)
	}
	if second := diagnostics[1]; second.File != "src/b.ts" || second.Line != 0 || !strings.Contains(second.Description, "imports bun:sqlite") {
		t.Fatalf("second diagnostic = %#v", second)
	}
}

func TestADependencyThatNeedsBunIsNamedInTheMessageOnly(t *testing.T) {
	stubNodeCheck(t, func(string, string) ([]build.BunAPIUse, error) {
		return []build.BunAPIUse{{Module: "../runtime/src/config.ts", Globals: []build.SourcePosition{{Line: 8, Column: 38}}, Chain: []string{"src/index.ts", "../runtime/src/config.ts"}}}, nil
	})
	diagnostics := checkBunAPIsOnNode("bun", t.TempDir(), build.TranspileParams{Target: "node"})
	if len(diagnostics) != 1 || diagnostics[0].File != "" || diagnostics[0].Line != 0 ||
		!strings.HasPrefix(diagnostics[0].Description, "../runtime/src/config.ts references the Bun global at 8:38") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestANodeCheckThatCannotReadTheGraphFailsTheBuild(t *testing.T) {
	stubNodeCheck(t, func(string, string) ([]build.BunAPIUse, error) {
		return nil, errors.New("node-target check: bun could not bundle the graph")
	})
	diagnostics := checkBunAPIsOnNode("bun", t.TempDir(), build.TranspileParams{Target: "node"})
	if len(diagnostics) != 1 || diagnostics[0].Category != errs.CodeTranspileError.String() || diagnostics[0].Severity != "error" {
		t.Fatalf("diagnostics = %#v, want one transpile error", diagnostics)
	}
}

func TestABatchedNodeTranspileFailsTheMemberThatNeedsBun(t *testing.T) {
	mockBunResolution(t)
	ctx := setupTranspileProject(t)
	ctx.Params = pctx.Params{"target": json.RawMessage(`"node"`)}
	ctx.SelectedProjects = []pctx.ProjectRef{{ID: "@test/pkg", Name: "@test/pkg", Path: "project"}}
	mockAllExec(t, successExec)
	stubNodeCheck(t, func(string, string) ([]build.BunAPIUse, error) {
		return []build.BunAPIUse{{Module: "src/index.ts", Imports: []string{"bun"}, Chain: []string{"src/index.ts"}}}, nil
	})

	status, data, err := runBuildTranspile(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("status = %q, err = %v; a batch returns OK and fails its members", status, err)
	}
	results, _ := data["batchResults"].([]buildBatchProjectResult)
	if len(results) != 1 || results[0].Status != "FAILED" || len(results[0].Diagnostics) != 1 ||
		results[0].Diagnostics[0].Category != errs.CodeBunAPIOnNode.String() {
		t.Fatalf("batch results = %#v", results)
	}
}
