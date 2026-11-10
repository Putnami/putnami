package build

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

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

// bunAndTypeScript returns the bun this extension resolves and fails the test
// when the repository does not install the typescript package.
func bunAndTypeScript(t *testing.T) string {
	t.Helper()
	bun, err := toolchain.ResolveBun()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveTypeScriptPackage(repositoryRoot(t)); !ok {
		t.Fatal("the typescript package is not installed in this repository: run ./putnamiw install")
	}
	return bun
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// linkTypeScript makes the repository's typescript package resolve from dir.
func linkTypeScript(t *testing.T, dir string) {
	t.Helper()
	typescript, _ := resolveTypeScriptPackage(repositoryRoot(t))
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(typescript, filepath.Join(dir, "node_modules", "typescript")); err != nil {
		t.Fatal(err)
	}
}

func TestNodeGraphUsesNameTheShortestChainFromAnEntrypoint(t *testing.T) {
	graph := &nodeGraph{}
	graph.Inputs = map[string]struct {
		Imports []struct {
			Path     string `json:"path"`
			External bool   `json:"external"`
		} `json:"imports"`
	}{}
	add := func(module string, imports ...string) {
		entry := graph.Inputs[module]
		for _, imported := range imports {
			external := isBunModule(imported) || strings.HasPrefix(imported, "node:")
			entry.Imports = append(entry.Imports, struct {
				Path     string `json:"path"`
				External bool   `json:"external"`
			}{Path: imported, External: external})
		}
		graph.Inputs[module] = entry
	}
	add("src/index.ts", "src/a.ts", "src/b.ts")
	add("src/a.ts", "src/deep.ts")
	add("src/b.ts", "src/c.ts")
	add("src/c.ts", "src/deep.ts", "src/far.ts")
	add("src/deep.ts", "bun:sqlite", "node:fs", "bun")
	add("src/other.ts", "src/b.ts", "src/far.ts", "src/guarded.ts")
	add("src/far.ts")
	// guarded.ts imports bun:ffi only under a typeof Bun guard: bun records
	// the import, the scanner does not.
	add("src/guarded.ts", "bun:ffi")
	add("src/unreached.ts", "bun:ffi")

	uses := graph.uses([]string{"./src/index.ts", "src/other.ts"}, map[string]moduleScan{
		"src/b.ts":         {Uses: []SourcePosition{{Line: 3, Column: 7}}},
		"src/deep.ts":      {Imports: []string{"bun", "bun:sqlite"}},
		"src/far.ts":       {Uses: []SourcePosition{{Line: 2, Column: 1}}},
		"src/unreached.ts": {Uses: []SourcePosition{{Line: 1, Column: 1}}, Imports: []string{"bun:ffi"}},
	})
	want := []BunAPIUse{
		// Both entrypoints import b.ts directly: the earlier one names it.
		{Module: "src/b.ts", Globals: []SourcePosition{{Line: 3, Column: 7}}, Chain: []string{"src/index.ts", "src/b.ts"}},
		{Module: "src/deep.ts", Imports: []string{"bun", "bun:sqlite"}, Chain: []string{"src/index.ts", "src/a.ts", "src/deep.ts"}},
		// The later entrypoint reaches far.ts in one import, the first in three.
		{Module: "src/far.ts", Globals: []SourcePosition{{Line: 2, Column: 1}}, Chain: []string{"src/other.ts", "src/far.ts"}},
	}
	if !reflect.DeepEqual(uses, want) {
		t.Fatalf("uses = %#v, want %#v", uses, want)
	}
	if got := uses[1].Message(); got != "src/deep.ts imports bun, bun:sqlite, which Node.js does not provide; reached by src/index.ts -> src/a.ts -> src/deep.ts" {
		t.Fatalf("message = %q", got)
	}
}

// TestScanModulesKeepsOnlyUnguardedUses pins what counts as a use of Bun: a
// value reference to the Bun global no scope binds, a non-optional member
// access on globalThis.Bun, or a run-time import of a bun module, each
// outside a typeof Bun test that shows Bun present.
func TestScanModulesKeepsOnlyUnguardedUses(t *testing.T) {
	bun := bunAndTypeScript(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"guarded.ts": strings.Join([]string{
			"const a = typeof Bun !== 'undefined' ? Bun.file('x') : null;",
			"const b = typeof Bun === 'object' && Bun.version;",
			"if (typeof Bun !== 'undefined') { Bun.write('a', 'b'); } else { console.log(1); }",
			"const escape = (globalThis as { Bun?: { escapeHTML?: (s: string) => string } }).Bun?.escapeHTML;",
			"type Runtime = typeof Bun;",
			"function shadow(Bun: { x: number }) { return Bun.x; }",
			"const holder = { Bun: 1 }; holder.Bun;",
			"export function sleep() {",
			"  if (typeof Bun === 'undefined') return;",
			"  Bun.sleepSync(1);",
			"}",
			"const c = typeof Bun === 'undefined' ? 'node' : Bun.version;",
			"if (!(typeof Bun === 'undefined')) { Bun.gc(); }",
			"const d = typeof Bun === 'undefined' || Bun.version;",
			"export const Named = class Bun { static v() { return Bun.name; } };",
			"export const open = async () => (typeof Bun !== 'undefined' ? await import('bun:sqlite') : null);",
			"import type { Subprocess } from 'bun';",
			"export type { Server } from 'bun';",
			"export let sub: Subprocess | null = null;",
			"export { a, b, c, d, escape, shadow, holder };",
			"export type { Runtime };",
		}, "\n"),
		"unguarded.ts": strings.Join([]string{
			"export const parse = (input: string) => Bun.YAML.parse(input);",
			"export function read(path: string) {",
			"  return Bun.file(path).text();",
			"}",
			"if (typeof Bun === 'undefined') { Bun.version; }",
			"const e = typeof Bun === 'undefined' ? Bun.version : 'bun';",
			"if (flag() || typeof Bun !== 'undefined') { Bun.gc(); }",
			"globalThis.Bun.write('a', 'b');",
			"(globalThis as any)['Bun'].version;",
			"declare const flag: () => boolean;",
			"export { e };",
			"export const f = typeof Bun.serve;",
			"export const g = typeof Bun?.serve === 'function';",
			"export const h = typeof globalThis.Bun.serve;",
		}, "\n"),
		"ambient.ts": strings.Join([]string{
			"declare const Bun: { file(path: string): { text(): Promise<string> } };",
			"export const read = (path: string) => Bun.file(path).text();",
		}, "\n"),
		"imports.ts": strings.Join([]string{
			"import { Database } from 'bun:sqlite';",
			"export { $ } from 'bun';",
			"export const ffi = () => require('bun:ffi');",
			"export const open = () => new Database(':memory:');",
		}, "\n"),
		"plain.js": "export const x = 1;\n",
	})
	typescript, _ := resolveTypeScriptPackage(repositoryRoot(t))
	scans, err := scanModules(bun, typescript, dir, []string{"ambient.ts", "guarded.ts", "imports.ts", "plain.js", "unguarded.ts"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]moduleScan{
		"ambient.ts": {Uses: []SourcePosition{{Line: 2, Column: 39}}},
		"imports.ts": {Imports: []string{"bun", "bun:ffi", "bun:sqlite"}},
		"unguarded.ts": {Uses: []SourcePosition{
			{Line: 1, Column: 41}, {Line: 3, Column: 10}, {Line: 5, Column: 35},
			{Line: 6, Column: 40}, {Line: 7, Column: 45}, {Line: 8, Column: 1}, {Line: 9, Column: 1},
			{Line: 12, Column: 25}, {Line: 13, Column: 25}, {Line: 14, Column: 25},
		}},
	}
	if !reflect.DeepEqual(scans, want) {
		t.Fatalf("scans = %#v, want %#v", scans, want)
	}
}

func TestSourceModulesSkipNonCodeInputs(t *testing.T) {
	var graph nodeGraph
	if err := json.Unmarshal([]byte(`{"inputs":{"src/index.ts":{},"src/notes.txt":{},"src/data.json":{},"lib/x.mjs":{},"src/view.TSX":{}}}`), &graph); err != nil {
		t.Fatal(err)
	}
	if got, want := graph.sourceModules(), []string{"lib/x.mjs", "src/index.ts", "src/view.TSX"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source modules = %v, want %v", got, want)
	}
}

func TestUnresolvedPackagesAreTheBareSpecifiersBunCannotResolve(t *testing.T) {
	stderr := `error: Could not resolve: "optional-peer". Maybe you need to "bun install"?
error: Could not resolve: "other/sub". Maybe you need to "bun install"?
error: Could not resolve: "optional-peer". Maybe you need to "bun install"?`
	if got, want := unresolvedPackages(stderr), []string{"optional-peer", "other/sub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unresolved = %v, want %v", got, want)
	}
	if got := unresolvedPackages(stderr + "\nerror: Could not resolve: \"./missing\""); got != nil {
		t.Fatalf("an unresolved relative import externalized %v", got)
	}
}

// writeNodeProject lays out a project whose one export imports the given
// module, with the named framework packages linked from this repository.
func writeNodeProject(t *testing.T, source string, packages ...string) string {
	t.Helper()
	root := repositoryRoot(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"node-app","type":"module","exports":{".":"./src/index.ts"}}`,
		"src/index.ts": source,
	})
	linkTypeScript(t, dir)
	for _, name := range packages {
		link := filepath.Join(dir, "node_modules", "@putnami", name)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "typescript", "framework", name), link); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckBunAPIsOnNodeReportsABunModuleImport(t *testing.T) {
	bun := bunAndTypeScript(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"node-app","exports":{".":"./src/index.ts"}}`,
		"src/index.ts": "export { open } from './store';\nexport { read } from './files';\n",
		"src/store.ts": "import { Database } from 'bun:sqlite';\nexport const open = () => new Database(':memory:');\n",
		// A named import of `bun` stays external and is reported like a
		// `bun:` one.
		"src/files.ts": "import { file } from 'bun';\nexport const read = (path: string) => file(path).text();\n",
	})
	linkTypeScript(t, dir)
	uses, err := CheckBunAPIsOnNode(bun, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []BunAPIUse{
		{Module: "src/files.ts", Imports: []string{"bun"}, Chain: []string{"src/index.ts", "src/files.ts"}},
		{Module: "src/store.ts", Imports: []string{"bun:sqlite"}, Chain: []string{"src/index.ts", "src/store.ts"}},
	}
	if !reflect.DeepEqual(uses, want) {
		t.Fatalf("uses = %#v, want %#v", uses, want)
	}
}

// TestCheckBunAPIsOnNodeReadsAGraphWithAMissingPackageAndANonCodeInput reads a
// graph that imports an optional package that is not installed, a text file
// whose words name Bun, and a bun module behind a guard: none of them needs
// Bun on Node.js.
func TestCheckBunAPIsOnNodeReadsAGraphWithAMissingPackageAndANonCodeInput(t *testing.T) {
	bun := bunAndTypeScript(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"node-app","exports":{".":"./src/index.ts"},"peerDependencies":{"optional-peer":"*"},"peerDependenciesMeta":{"optional-peer":{"optional":true}}}`,
		"src/index.ts": strings.Join([]string{
			"import notes from './notes.txt';",
			"export { notes };",
			"export const peer = async () => import('optional-peer');",
			"export const store = async () => (typeof Bun !== 'undefined' ? import('bun:sqlite') : null);",
		}, "\n"),
		"src/notes.txt": "Bun runs this project in development.\n",
	})
	linkTypeScript(t, dir)
	uses, err := CheckBunAPIsOnNode(bun, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 0 {
		t.Fatalf("uses = %#v, want none", uses)
	}
}

func TestCheckBunAPIsOnNodeFollowsDependencies(t *testing.T) {
	bun := bunAndTypeScript(t)
	dir := writeNodeProject(t, "export { loadConfig } from '@putnami/runtime';\n", "runtime")
	uses, err := CheckBunAPIsOnNode(bun, dir)
	if err != nil {
		t.Fatal(err)
	}
	var found *BunAPIUse
	for i := range uses {
		if strings.HasSuffix(uses[i].Module, "typescript/framework/runtime/src/config/config-source.ts") {
			found = &uses[i]
		}
	}
	if found == nil {
		t.Fatalf("no use reported in @putnami/runtime's config source: %#v", uses)
	}
	if found.Chain[0] != "src/index.ts" || found.Chain[len(found.Chain)-1] != found.Module || len(found.Chain) < 3 {
		t.Fatalf("chain = %v, want src/index.ts through the runtime index to the config source", found.Chain)
	}
}

func TestCheckBunAPIsOnNodeWithoutTypeScriptFails(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"node-app","exports":{".":"./src/index.ts"}}`,
		"src/index.ts": "export const x = 1;\n",
	})
	// t.TempDir lies outside the repository, so no typescript resolves.
	if _, ok := resolveTypeScriptPackage(dir); ok {
		t.Skip("a typescript package resolves from the temporary directory")
	}
	if _, err := CheckBunAPIsOnNode("bun", dir); !errors.Is(err, ErrNoTypeScriptParser) {
		t.Fatalf("err = %v, want ErrNoTypeScriptParser", err)
	}
}

// nodeLoadableList reads the packages the build documentation lists as
// loading on Node.js, between its two markers.
func nodeLoadableList(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "typescript", "extension", "doc", "02-build.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "<!-- node-loadable:start -->")
	end := strings.Index(text, "<!-- node-loadable:end -->")
	if start < 0 || end < start {
		t.Fatal("doc/02-build.md has no node-loadable list between its markers")
	}
	var names []string
	for _, match := range regexp.MustCompile("(?m)^- `(@putnami/[^`]+)`").FindAllStringSubmatch(text[start:end], -1) {
		names = append(names, match[1])
	}
	return names
}

// TestTheNodeLoadableListMatchesTheCheck runs the node-target check over the
// server exports of every framework package and compares the packages that
// pass with the list the build documentation publishes.
func TestTheNodeLoadableListMatchesTheCheck(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "node-target-refuses-bun-apis", "the-node-loadable-package-list-matches-the-check")
	bun := bunAndTypeScript(t)
	framework := filepath.Join(repositoryRoot(t), "typescript", "framework")
	entries, err := os.ReadDir(framework)
	if err != nil {
		t.Fatal(err)
	}
	var loadable []string
	for _, entry := range entries {
		dir := filepath.Join(framework, entry.Name())
		pkg := readPackageName(dir)
		if !entry.IsDir() || pkg == "" {
			continue
		}
		uses, err := CheckBunAPIsOnNode(bun, dir)
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		if len(uses) == 0 {
			loadable = append(loadable, pkg)
			continue
		}
		t.Logf("%s needs Bun: %s", pkg, uses[0].Message())
	}
	sort.Strings(loadable)
	documented := nodeLoadableList(t)
	if !reflect.DeepEqual(documented, loadable) {
		t.Fatalf("doc/02-build.md lists %v as loading on Node.js; the check passes %v", documented, loadable)
	}
}

func readPackageName(dir string) string {
	pkg := project.ReadPackageJSONSafe(filepath.Join(dir, "package.json"))
	if pkg == nil {
		return ""
	}
	return pkg.Name
}
