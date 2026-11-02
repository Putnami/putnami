//go:build unix

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
)

// typeCheckStarter writes the starter project with the real TypeScript and Bun
// types linked into its workspace, adds files, and returns the job context.
func typeCheckStarter(t *testing.T, files map[string]string) *pctx.Context {
	t.Helper()
	requireBun(t)
	installed := installedNodeModules(t)
	ctx := writeStarter(t)
	nodeModules := filepath.Join(ctx.WorkspaceRoot, "node_modules")
	for _, pkg := range []string{"typescript", "@types/bun"} {
		linkPackage(t, installed, nodeModules, pkg)
	}
	linkBin(t, nodeModules, "tsc", "typescript/bin/tsc")
	for name, content := range files {
		path := filepath.Join(ctx.WorkspaceRoot, "webapp", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx.CacheRoot = t.TempDir()
	return ctx
}

// typeCheckDiagnostics runs the type-check the step runs and returns the
// project-relative file of each diagnostic with its code.
func typeCheckDiagnostics(t *testing.T, ctx *pctx.Context) []string {
	t.Helper()
	bun, err := resolveBunBin()
	if err != nil {
		t.Fatal(err)
	}
	result, err := build.RunTypeCheck(bun, filepath.Join(ctx.WorkspaceRoot, "webapp"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		got = append(got, filepath.ToSlash(d.File)+" "+d.Code)
	}
	if !result.Success && len(got) == 0 {
		t.Fatalf("tsc failed with no diagnostic:\n%s", result.RawOutput)
	}
	return got
}

func TestTestTypesFailsOnATypeErrorInATestFile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-files-are-type-checked", "a-type-error-in-a-test-file-fails-the-test-command")
	ctx := typeCheckStarter(t, map[string]string{
		"test/wrong.test.ts": "import { handle } from '../src/main';\n\nexport const status: string = handle(new Request('http://localhost/')).status;\n",
	})

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("test~types ended %s, want FAILED", status)
	}
	if got := typeCheckDiagnostics(t, ctx); strings.Join(got, ",") != "test/wrong.test.ts TS2322" {
		t.Errorf("diagnostics %v, want the type error of test/wrong.test.ts", got)
	}
}

func TestTestTypesFailsOnAnUnusedExpectErrorInATestFile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-files-are-type-checked", "an-unused-ts-expect-error-in-a-test-file-fails-the-test-command")
	ctx := typeCheckStarter(t, map[string]string{
		"test/unused.test.ts": "// @ts-expect-error unused\nexport const ok: number = 1;\n",
	})

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("test~types ended %s, want FAILED", status)
	}
	if got := typeCheckDiagnostics(t, ctx); strings.Join(got, ",") != "test/unused.test.ts TS2578" {
		t.Errorf("diagnostics %v, want the unused directive of test/unused.test.ts", got)
	}
}

func TestTestTypesFailsOnATypeErrorUnderADotDirectory(t *testing.T) {
	ctx := typeCheckStarter(t, map[string]string{
		".gen/src/gen.ts": "export const n: number = 'not a number';\n",
	})

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("test~types ended %s, want FAILED", status)
	}
	if got := typeCheckDiagnostics(t, ctx); strings.Join(got, ",") != ".gen/src/gen.ts TS2322" {
		t.Errorf("diagnostics %v, want the type error of .gen/src/gen.ts", got)
	}
}

func TestTestTypesChecksAProjectWhoseFilesAllLieUnderADotDirectory(t *testing.T) {
	ctx := typeCheckStarter(t, map[string]string{
		".gen/src/gen.ts": "export const n: number = 1;\n",
	})
	for _, name := range []string{"src", "test"} {
		if err := os.RemoveAll(filepath.Join(ctx.WorkspaceRoot, "webapp", name)); err != nil {
			t.Fatal(err)
		}
	}

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	expectJobOK(t, "test~types", status, err)
}

func TestTestTypesReadsTheDeclarationsOfADotDirectory(t *testing.T) {
	ctx := typeCheckStarter(t, map[string]string{
		".gen/types/env.d.ts": "declare const generatedValue: number;\n",
		"test/env.test.ts":    "export const value: number = generatedValue;\n",
	})

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	expectJobOK(t, "test~types", status, err)
}

func TestTestTypesEmitsNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-files-are-type-checked", "the-type-check-emits-nothing")
	ctx := typeCheckStarter(t, map[string]string{
		"bench/bench.ts": "import { handle } from '../src/main';\n\nexport const response: Response = handle(new Request('http://localhost/'));\n",
	})
	before := projectFiles(t, ctx.WorkspaceRoot)

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	expectJobOK(t, "test~types", status, err)

	if after := projectFiles(t, ctx.WorkspaceRoot); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the type-check wrote into the workspace:\nbefore %v\nafter  %v", before, after)
	}
}

func TestTestTypesLeavesANestedProjectToItsOwner(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-files-are-type-checked", "a-nested-project-is-never-type-checked-by-its-parent")
	ctx := typeCheckStarter(t, map[string]string{
		"clients/ts/putnami.json":       "{}\n",
		"clients/ts/test/wrong.test.ts": "export const n: number = 'not a number';\n",
		"dist/wrong.ts":                 "export const n: number = 'not a number';\n",
	})

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	expectJobOK(t, "test~types", status, err)
}

// projectFiles lists every file under root except node_modules, the
// workspace's own scratch and the type-check's scratch.
func projectFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
