//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
func typeCheckDiagnostics(t *testing.T, ctx *pctx.Context, checker string) []string {
	t.Helper()
	bun, err := resolveBunBin()
	if err != nil {
		t.Fatal(err)
	}
	result, err := build.RunTypeCheck(bun, filepath.Join(ctx.WorkspaceRoot, "webapp"), t.TempDir(), false, checker)
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
	if got := typeCheckDiagnostics(t, ctx, build.TypeCheckerTsc); strings.Join(got, ",") != "test/wrong.test.ts TS2322" {
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
	if got := typeCheckDiagnostics(t, ctx, build.TypeCheckerTsc); strings.Join(got, ",") != "test/unused.test.ts TS2578" {
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
	if got := typeCheckDiagnostics(t, ctx, build.TypeCheckerTsc); strings.Join(got, ",") != ".gen/src/gen.ts TS2322" {
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

// typeCheckFindings runs the type-check with checker and returns each
// diagnostic whole: file, position, code and message.
func typeCheckFindings(t *testing.T, ctx *pctx.Context, checker string) []string {
	t.Helper()
	bun, err := resolveBunBin()
	if err != nil {
		t.Fatal(err)
	}
	result, err := build.RunTypeCheck(bun, filepath.Join(ctx.WorkspaceRoot, "webapp"), t.TempDir(), false, checker)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || len(result.Diagnostics) == 0 {
		t.Fatalf("%s found nothing, want every error of the project:\n%s", checker, result.RawOutput)
	}
	got := make([]string, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		got = append(got, fmt.Sprintf("%s(%d,%d) %s %s %s", filepath.ToSlash(d.File), d.Line, d.Column, d.Category, d.Code, d.Message))
	}
	sort.Strings(got)
	return got
}

func TestTestTypesBunCheckReportsTheFindingsTscReports(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "test-type-checker-choice", "bun-check-reports-the-findings-tsc-reports")
	ctx := typeCheckStarter(t, map[string]string{
		"package.json":        `{"name":"webapp","private":true,"main":"src/main.ts","scripts":{"check":"exit 0"}}` + "\n",
		"test/wrong.test.ts":  "import { handle } from '../src/main';\n\nexport const status: string = handle(new Request('http://localhost/')).status;\n",
		"test/unused.test.ts": "// @ts-expect-error unused\nexport const ok: number = 1;\n",
		".gen/src/gen.ts":     "export const n: number = 'not a number';\n",
		"bench/missing.ts":    "import { absent } from '../src/main';\n\nexport const value = absent;\n",
		"src/implicit.ts":     "export function twice(value) {\n  return value * 2;\n}\n",
		"fixtures/repeat.ts":  strings.Repeat("export const r: number = 'x';\n", 60),
	})

	tsc := typeCheckFindings(t, ctx, build.TypeCheckerTsc)
	bun := typeCheckFindings(t, ctx, build.TypeCheckerBun)
	if strings.Join(bun, "\n") != strings.Join(tsc, "\n") {
		t.Errorf("bun check and tsc disagree:\nbun check %d:\n%s\ntsc %d:\n%s", len(bun), strings.Join(bun, "\n"), len(tsc), strings.Join(tsc, "\n"))
	}
	if len(tsc) < 65 {
		t.Errorf("tsc found %d errors, want the 65 the fixture holds:\n%s", len(tsc), strings.Join(tsc, "\n"))
	}

	ctx.Params = pctx.Params{"type-checker": json.RawMessage(`"bun"`)}
	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Errorf("test~types with bun check ended %s, want FAILED", status)
	}
}

func TestTestTypesWithBunCheckPassesACleanProject(t *testing.T) {
	ctx := typeCheckStarter(t, nil)
	ctx.Params = pctx.Params{"type-checker": json.RawMessage(`"bun"`)}

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	expectJobOK(t, "test~types", status, err)
}

func TestTestTypesFailsOnAnUnknownTypeChecker(t *testing.T) {
	ctx := typeCheckStarter(t, nil)
	ctx.Params = pctx.Params{"type-checker": json.RawMessage(`"tsgo"`)}

	status, _, err := runTestTypes(ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "tsgo") {
		t.Errorf("test~types ended %s, %v, want FAILED naming tsgo", status, err)
	}
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
