package apisurface

import (
	"os"
	"slices"
	"testing"

	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// comparePackages reads two states of the package @x/lib, each a set of files
// with its package.json, the way the check reads them, and compares them.
func comparePackages(t *testing.T, old, current map[string]string) Comparison {
	t.Helper()
	bun, err := toolchain.ResolveBun()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sides := make([]Side, 2)
	for i, files := range []map[string]string{old, current} {
		var names []string
		for name, text := range files {
			if IsSource(name) {
				sides[i].Files = append(sides[i].Files, File{Path: name, Data: []byte(text)})
				names = append(names, name)
			}
		}
		manifest := []byte(files[ManifestFilename])
		if sides[i].Entries, err = EntryPoints(manifest, names); err != nil {
			t.Fatal(err)
		}
		sides[i].Imports = ImportAliases(manifest)
	}
	surfaces, err := Read(bun, dir, sides...)
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range surfaces {
		if len(surface.Errors) > 0 {
			t.Fatalf("syntax errors: %+v", surface.Errors)
		}
	}
	return Compare("@x/lib", surfaces[0], surfaces[1])
}

// describe returns the changes of a comparison as "<package>: <message>", a
// warning with the prefix "warning: ".
func describe(comparison Comparison) []string {
	found := make([]string, 0, len(comparison.Changes))
	for _, change := range comparison.Changes {
		text := change.Package + ": " + change.Message
		if change.Warning {
			text = "warning: " + text
		}
		found = append(found, text)
	}
	return found
}

// rootPackage is a package.json whose one entry point is src/index.ts.
const rootPackage = `{"name":"@x/lib","exports":{".":"./src/index.ts"}}`

func TestComparisons(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name         string
		old, current map[string]string
		want         []string
	}{
		// A subpath is what any of its conditions exports.
		{
			name:    "conditions that name one file become one target",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"types":"./src/index.ts","default":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1;"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export const a = 1;"},
		},
		{
			name:    "one target split into conditions",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export const a = 1;"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"import":"./src/index.ts","require":"./src/index.cts"}}}`, "src/index.ts": "export const a = 1;", "src/index.cts": "export const a = 1;"},
		},
		{
			name:    "an import condition that becomes the default",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"import":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1;"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export const a = 1;"},
		},
		{
			name:    "a name one condition still exports",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"browser":"./src/browser.ts","default":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1;", "src/browser.ts": "export const a = 1; export const b = 2;"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"browser":"./src/browser.ts","default":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1; export const b = 2;", "src/browser.ts": "export const a = 1;"},
		},
		{
			name:    "a name no condition exports any more",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"browser":"./src/browser.ts","default":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1;", "src/browser.ts": "export const a = 1; export const b = 2;"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{".":{"browser":"./src/browser.ts","default":"./src/index.ts"}}}`, "src/index.ts": "export const a = 1;", "src/browser.ts": "export const a = 1;"},
			want:    []string{"@x/lib: variable b removed"},
		},
		{
			name:    "a pattern that a null pattern excludes",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{"./*":"./src/*.ts","./internal/*":null}}`, "src/a.ts": "export const a = 1;", "src/internal/helper.ts": "export const h = 1;"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{"./*":"./src/*.ts","./internal/*":null}}`, "src/a.ts": "export const a = 1;", "src/internal/renamed.ts": "export const h = 1;"},
		},
		{
			name:    "a target deleted while the package still names it",
			old:     map[string]string{"package.json": `{"name":"@x/lib","exports":{".":"./src/index.ts","./legacy":"./src/legacy.ts"}}`, "src/index.ts": "export const a = 1;", "src/legacy.ts": "export const old = 1;"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{".":"./src/index.ts","./legacy":"./src/legacy.ts"}}`, "src/index.ts": "export const a = 1;"},
			want:    []string{"@x/lib/legacy: entry point names src/legacy.ts, which the release held and the working tree does not"},
		},
		// Another kind of declaration that keeps its uses is a warning.
		{
			name:    "a function that becomes a typed variable",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export function handler(event: string): void {}"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "type Handler = (event: string) => void;\nexport const handler: Handler = (event) => {};"},
			want:    []string{"warning: @x/lib: function handler is now a variable"},
		},
		{
			name:    "a typed variable that becomes a function",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export const handler: (e: string) => void = (e) => {};"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export function handler(e: string): void {}"},
			want:    []string{"warning: @x/lib: variable handler is now a function"},
		},
		{
			name:    "an interface that becomes an intersection",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export interface Props { a: string; b: number }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "interface Base { a: string }\nexport type Props = Base & { b: number };"},
			want:    []string{"warning: @x/lib: interface Props is now a type"},
		},
		{
			name:    "an intersection that becomes an interface",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "interface Base { a: string }\nexport type Props = Base & { b: number };"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "interface Base { a: string }\nexport interface Props extends Base { b: number }"},
			want:    []string{"warning: @x/lib: type Props is now an interface"},
		},
		{
			name:    "an interface that becomes a schema and its inferred type",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export interface User { id: string }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "import { z } from 'zod';\nexport const User = z.object({ id: z.string() });\nexport type User = z.infer<typeof User>;"},
			want:    []string{"warning: @x/lib: interface User is now a type"},
		},
		{
			name:    "a class that becomes a function",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class make {}"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export function make(): void {}"},
			want:    []string{"@x/lib: class make is now a function"},
		},
		{
			name:    "an enum that becomes an object",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export enum Level { Debug }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export const Level = { Debug: 0 } as const;"},
			want:    []string{"@x/lib: enum Level is now a variable"},
		},
		// Members a base the check cannot read may still provide.
		{
			name:    "a member moved to a base of another package",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { send(): void {} close(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "import { BaseClient } from '@x/core';\nexport class Client extends BaseClient { send(): void {} }"},
			want:    []string{"warning: @x/lib: method Client.close removed, unless a base the check cannot read declares it"},
		},
		{
			name:    "a member moved to a base named through a namespace import",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { send(): void {} close(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "import * as b from './base';\nexport class Client extends b.Base { send(): void {} }", "src/base.ts": "export class Base { close(): void {} }"},
			want:    []string{"warning: @x/lib: method Client.close removed, unless a base the check cannot read declares it"},
		},
		{
			name: "members moved to a mixin",
			old:  map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { send(): void {} close(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "function Closable<T extends new (...a: any[]) => object>(B: T) { return class extends B { close(): void {} } }\n" +
				"class Core { send(): void {} }\nexport class Client extends Closable(Core) {}"},
			want: []string{
				"warning: @x/lib: method Client.send removed, unless a base the check cannot read declares it",
				"warning: @x/lib: method Client.close removed, unless a base the check cannot read declares it",
			},
		},
		{
			name:    "a member moved to an interface that merges with the class",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class C { a(): void {} b(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export interface C { b(): void }\nexport class C { a(): void {} }"},
		},
		{
			name:    "a member moved to a base of an interface that merges with the class",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class C { a(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "interface CMethods { a(): void }\nexport interface C extends CMethods {}\nexport class C {}"},
		},
		{
			name:    "a constructor moved to a base",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { constructor(url: string) {} send(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "class Base { constructor(url: string) {} }\nexport class Client extends Base { send(): void {} }"},
		},
		{
			name:    "a constructor moved to a base that protects it",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { constructor(url: string) {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "class Base { protected constructor(url: string) {} }\nexport class Client extends Base {}"},
			want:    []string{"@x/lib: constructor of Client is now protected"},
		},
		{
			name:    "a constructor moved to a base of another package",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { constructor(url: string) {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "import { Base } from '@x/core';\nexport class Client extends Base {}"},
			want:    []string{"warning: @x/lib: constructor of Client removed, unless a base the check cannot read declares it"},
		},
		// Inheritance is read on the released side too.
		{
			name:    "a dropped base",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "class Base { close(): void {} }\nexport class Client extends Base { send(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "class Base { close(): void {} }\nexport class Client { send(): void {} }"},
			want: []string{
				"warning: @x/lib: class Client no longer extends Base",
				"@x/lib: method Client.close removed",
			},
		},
		{
			name:    "a base whose member moved into the class",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "class Base { close(): void {} }\nexport class Client extends Base { send(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export class Client { send(): void {} close(): void {} }"},
			want:    []string{"warning: @x/lib: class Client no longer extends Base"},
		},
		// `export * as` and `export type *`.
		{
			name:    "a namespace written as export * as",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export namespace Util { export const a = 1; export function b() {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1; export function b() {}"},
		},
		{
			name:    "a member gone from the module of export * as",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1; export function b() {}"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1;"},
			want:    []string{"@x/lib: namespace member Util.b removed"},
		},
		{
			name:    "export * as a module the check cannot read whole",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1; export function b() {}"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1;\nexport * from 'dep';"},
			want: []string{
				"warning: @x/lib: the members of namespace Util are not compared: the check cannot read the whole module it names",
			},
		},
		{
			name:    "export * as a module the package does not hold",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './util';", "src/util.ts": "export const a = 1;"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * as Util from './generated';"},
			want: []string{
				"warning: @x/lib: the members of namespace Util are not compared: the check cannot read the whole module it names",
			},
		},
		{
			name:    "a class re-exported with export type *",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './k';", "src/k.ts": "export class K { m(): void {} }"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export type * from './k';", "src/k.ts": "export class K { m(): void {} }"},
			want:    []string{"@x/lib: class K is now exported as a type only"},
		},
		// An `export *` is compared by what it exports, never by its text.
		{
			name:    "export * through a package.json import alias",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './foo';", "src/foo.ts": "export const a = 1; export function b() {}"},
			current: map[string]string{"package.json": `{"name":"@x/lib","exports":{".":"./src/index.ts"},"imports":{"#src/*":"./src/*.ts"}}`, "src/index.ts": "export * from '#src/foo';", "src/foo.ts": "export const a = 1; export function b() {}"},
		},
		{
			name:    "export * of a .js specifier that names a .ts file",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './foo';", "src/foo.ts": "export const a = 1;"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './foo.js';", "src/foo.ts": "export const a = 1;"},
		},
		{
			name:    "export * of an alias that resolves to nothing",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './foo';", "src/foo.ts": "export const a = 1; export function b() {}"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * from '#src/foo';", "src/foo.ts": "export const a = 1; export function b() {}"},
			want: []string{
				`warning: @x/lib: export * from "#src/foo" is not compared: the check cannot read the module it names`,
				"warning: @x/lib: variable a removed, unless an export * the check cannot read exports it",
				"warning: @x/lib: function b removed, unless an export * the check cannot read exports it",
			},
		},
		{
			name:    "export * that the release could not follow",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from '@/foo';", "src/foo.ts": "export const a = 1;"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './foo';", "src/foo.ts": "export const a = 1;"},
			want:    []string{`warning: @x/lib: export * from "@/foo" is not compared: the check cannot read the module it names`},
		},
		{
			name:    "export * of a generated file on both sides",
			old:     map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './generated';"},
			current: map[string]string{"package.json": rootPackage, "src/index.ts": "export * from './generated.js';"},
			want: []string{
				`warning: @x/lib: export * from "src/generated" is not compared: the check cannot read the module it names`,
				`warning: @x/lib: export * from "src/generated.js" is not compared: the check cannot read the module it names`,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := describe(comparePackages(t, testCase.old, testCase.current)); !slices.Equal(got, testCase.want) {
				t.Fatalf("changes = %q\nwant      %q", got, testCase.want)
			}
		})
	}
}

func TestASubpathIsComparedOnceWhateverItsConditions(t *testing.T) {
	t.Parallel()
	conditional := `{"name":"@x/lib","exports":{".":{"types":"./src/index.ts","default":"./src/index.ts"},"./gen":{"types":"./dist/gen.d.ts","default":"./src/gen.ts"}}}`
	files := map[string]string{"package.json": conditional, "src/index.ts": "export const a = 1;", "src/gen.ts": "export const g = 1;"}
	comparison := comparePackages(t, files, files)
	if comparison.Subpaths != 2 || comparison.Compared != 1 || len(comparison.Changes) != 0 {
		t.Fatalf("comparison = %+v, want 2 subpaths, 1 compared and no change", comparison)
	}
	want := []Skipped{{Subpath: "./gen", Target: "dist/gen.d.ts", Reason: MissingNow}}
	if !slices.Equal(comparison.Skipped, want) {
		t.Fatalf("skipped = %+v, want %+v", comparison.Skipped, want)
	}
}

func TestImportAliasesNameRelativeFiles(t *testing.T) {
	t.Parallel()
	got := ImportAliases([]byte(`{"imports":{"#src/*":"./src/*.ts","#dep":"dep","#cond":{"node":"./a/../node.ts","default":["./fallback.ts"]},"plain":"./x.ts"}}`))
	want := map[string][]string{"#src/*": {"src/*.ts"}, "#cond": {"fallback.ts", "node.ts"}}
	if len(got) != len(want) {
		t.Fatalf("aliases = %v, want %v", got, want)
	}
	for key, targets := range want {
		if !slices.Equal(got[key], targets) {
			t.Fatalf("aliases = %v, want %v", got, want)
		}
	}
	if aliases := ImportAliases([]byte(`{not json`)); len(aliases) != 0 {
		t.Fatalf("aliases of a broken manifest = %v, want none", aliases)
	}
}
