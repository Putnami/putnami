package apisurface

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// read parses each source as the package @example/lib whose one entry point
// is src/index.ts, with the typescript package this repository installs.
func read(t *testing.T, sides ...map[string]string) []*Surface {
	t.Helper()
	bun, err := toolchain.ResolveBun()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := installedTypeScript(dir); !ok {
		t.Fatal("the typescript package is not installed in this repository: run ./putnamiw install")
	}
	input := make([]Side, len(sides))
	for i, files := range sides {
		for name, text := range files {
			input[i].Files = append(input[i].Files, File{Path: name, Data: []byte(text)})
		}
		input[i].Entries = []EntryPoint{{Key: ".", Target: "src/index.ts"}}
	}
	surfaces, err := Read(bun, dir, input...)
	if err != nil {
		t.Fatal(err)
	}
	return surfaces
}

// changes returns the messages of the changes from one index.ts to another.
// A change of written type, which the check reports without failing, has the
// prefix "warning: ".
func changes(t *testing.T, old, current string) []string {
	t.Helper()
	surfaces := read(t, map[string]string{"src/index.ts": old}, map[string]string{"src/index.ts": current})
	for _, surface := range surfaces {
		if len(surface.Errors) > 0 {
			t.Fatalf("syntax errors: %+v", surface.Errors)
		}
	}
	incompatible := Incompatible("@example/lib", surfaces[0], surfaces[1])
	found := make([]string, 0, len(incompatible))
	for _, change := range incompatible {
		if change.Warning {
			found = append(found, "warning: "+change.Message)
		} else {
			found = append(found, change.Message)
		}
	}
	return found
}

func TestIncompatibleChangesAreAClosedList(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, old, current string
		want               []string
	}{
		{
			name: "a removed export", old: "export const a = 1; export class B {}", current: "export const a = 1;",
			want: []string{"class B removed"},
		},
		{
			name: "a removed parameter", old: "export function f(a: string, b: number): void {}", current: "export function f(a: string): void {}",
			want: []string{"warning: function f changed from (string, number) => void to (string) => void"},
		},
		{
			name: "a changed return type", old: "export function f(): string { return ''; }", current: "export function f(): number { return 1; }",
			want: []string{"warning: function f changed from () => string to () => number"},
		},
		{
			name: "an optional parameter made required", old: "export function f(a?: string): void {}", current: "export function f(a: string): void {}",
			want: []string{"warning: function f changed from (string?) => void to (string) => void"},
		},
		{
			name:    "a removed overload",
			old:     "export function f(a: string): string;\nexport function f(a: number): number;\nexport function f(a: any): any { return a; }",
			current: "export function f(a: string): string;\nexport function f(a: any): any { return a; }",
			want:    []string{"warning: function f no longer has the signature (number) => number"},
		},
		{
			name: "a replaced overload",
			old:  "export function f(a: string): string;\nexport function f(a: number): number;\nexport function f(a: any): any { return a; }",
			current: "export function f(a: string): string;\nexport function f(a: boolean): boolean;\n" +
				"export function f(a: any): any { return a; }",
			want: []string{"warning: function f changed overload 2 from (number) => number to (boolean) => boolean"},
		},
		{
			name: "a changed variable type", old: "export const limit: number = 1;", current: "export const limit: string = '1';",
			want: []string{"warning: variable limit changed type from number to string"},
		},
		{
			name: "a changed type alias", old: "export type Mode = 'a' | 'b';", current: "export type Mode = 'a';",
			want: []string{`warning: type Mode changed from "a" | "b" to "a"`},
		},
		{
			name: "a changed kind", old: "export function make(): void {}", current: "export class make {}",
			want: []string{"warning: function make is now a class"},
		},
		{
			name: "a type that becomes a value", old: "export interface Shape { side: number }", current: "export const Shape = 1;",
			want: []string{"interface Shape is now a variable"},
		},
		{
			name:    "interface members",
			old:     "export interface Config { port: number; host?: string; close(): void; gone: boolean }",
			current: "export interface Config { port: string; host: string; close(force: boolean): void; name: string; label?: string }",
			want: []string{
				"warning: property Config.port changed type from number to string",
				"warning: property Config.host is now required",
				"warning: method Config.close changed from () => void to (boolean) => void",
				"property Config.gone removed",
				"warning: property Config.name added as required: every value of the type built outside the package must have it",
			},
		},
		{
			name: "a required property made optional", old: "export type Result = { value: string };", current: "export type Result = { value?: string };",
			want: []string{"warning: property Result.value is now optional"},
		},
		{
			name: "class members",
			old: "export class Client extends Base { constructor(url: string) { super(); }\n" +
				"send(body: string): void {}\nstatic count: number = 0;\nopen(): void {} }\nclass Base {}",
			current: "export abstract class Client { constructor(url: string, key: string) {}\n" +
				"send(body: number): void {}\nabstract open(): void;\nabstract close(): void; }",
			want: []string{
				"class Client is now abstract",
				"warning: class Client no longer extends Base",
				"warning: constructor of Client changed from (string) => <inferred> to (string, string) => <inferred>",
				"warning: method Client.send changed from (string) => void to (number) => void",
				"static property Client.count removed",
				"warning: method Client.open is now abstract",
				"warning: method Client.close added as abstract: every subclass outside the package must implement it",
			},
		},
		{
			name: "a constructor that starts to require arguments", old: "export class Box {}", current: "export class Box { constructor(size: number) {} }",
			want: []string{"warning: constructor of Box added with required parameters"},
		},
		{
			name: "enum members", old: "export enum Level { Debug, Info, Warn = 'warn' }", current: "export enum Level { Info, Warn = 'warning' }",
			want: []string{
				"enum member Level.Debug removed",
				"warning: enum member Level.Info changed value from 1 to 0",
				`warning: enum member Level.Warn changed value from "warn" to "warning"`,
			},
		},
		{
			name: "type parameters", old: "export interface Box<T> { value: T }", current: "export interface Box<T, U> { value: T }",
			want: []string{"warning: interface Box changed its type parameters from <$0> to <$0, $1>"},
		},
		{
			name: "a re-export from another package", old: "export * from 'dep'; export { z } from 'zod';", current: "export const other = 1;",
			want: []string{`warning: export * from "dep" is not compared: the check cannot read the module it names`, "re-export z removed"},
		},
		{
			name:    "an overload added before the others",
			old:     "export function f(a: string): string { return a; }",
			current: "export function f(a: number): number;\nexport function f(a: string): string;\nexport function f(a: any): any { return a; }",
			want:    []string{"warning: function f changed overload 1 from (string) => string to (number) => number"},
		},
		{
			name: "a constructor made private", old: "export class C { constructor() {} }", current: "export class C { private constructor() {} }",
			want: []string{"constructor of C is now private"},
		},
		{
			name: "a constructor made protected", old: "export class C { constructor() {} }", current: "export class C { protected constructor() {} }",
			want: []string{"constructor of C is now protected"},
		},
		{
			name: "a private constructor added", old: "export class C {}", current: "export class C { private constructor() {} }",
			want: []string{"constructor of C is now private"},
		},
		{
			name: "a constructor with parameters removed", old: "export class C { constructor(a: string) {} }", current: "export class C {}",
			want: []string{"constructor of C removed"},
		},
		{
			name: "a member made protected", old: "export class C { run(): void {}\nname: string = ''; }", current: "export class C { protected run(): void {}\nprotected name: string = ''; }",
			want: []string{"method C.run is now protected", "property C.name is now protected"},
		},
		{
			name: "a member made private", old: "export class C { run(): void {} }", current: "export class C { private run(): void {} }",
			want: []string{"method C.run is now private"},
		},
		{
			name: "a class made abstract", old: "export class C {}", current: "export abstract class C {}",
			want: []string{"class C is now abstract"},
		},
		{
			name: "an enum made a const enum", old: "export enum E { A }", current: "export const enum E { A }",
			want: []string{"enum E is now a const enum"},
		},
		{
			name:    "a setter removed while the getter stays",
			old:     "export class C { get v(): number { return 1; }\nset v(x: number) {} }",
			current: "export class C { get v(): number { return 1; } }",
			want:    []string{"property C.v is now read-only"},
		},
		{
			name: "a property made readonly", old: "export interface I { a: string }\nexport class C { b: string = ''; }", current: "export interface I { readonly a: string }\nexport class C { readonly b: string = ''; }",
			want: []string{"property C.b is now read-only", "property I.a is now read-only"},
		},
		{
			name:    "an accessor made abstract",
			old:     "export abstract class C { get v(): number { return 1; }\nrun(): void {} }",
			current: "export abstract class C { abstract get v(): number;\nabstract run(): void; }",
			want:    []string{"warning: property C.v is now abstract", "warning: method C.run is now abstract"},
		},
		{
			name: "a type parameter default removed", old: "export interface Box<T = string> { value: T }", current: "export interface Box<T> { value: T }",
			want: []string{"warning: interface Box changed its type parameters from <$0 = string> to <$0>"},
		},
		{
			name: "a type parameter default changed", old: "export function f<T = string>(): T { throw 0; }", current: "export function f<T = number>(): T { throw 0; }",
			want: []string{"warning: function f changed from <$0 = string>() => $0 to <$0 = number>() => $0"},
		},
		{
			name:    "an interface that gains a base with a required member",
			old:     "export interface A { a: string }",
			current: "interface B { b: string; c?: number }\nexport interface A extends B { a: string }",
			want:    []string{"warning: interface A now extends B, which adds the required member b: every value of the type built outside the package must have it"},
		},
		{
			name:    "an interface that gains a base two levels up",
			old:     "export interface A { a: string }",
			current: "interface C { c: string }\ninterface B extends C { b?: string }\nexport interface A extends B { a: string }",
			want:    []string{"warning: interface A now extends B, which adds the required member c: every value of the type built outside the package must have it"},
		},
		{
			name:    "an interface that gains a base of another package",
			old:     "export interface A { a: string }",
			current: "import type { B } from 'dep';\nexport interface A extends B { a: string }",
			want:    []string{"warning: interface A now extends B, which the check cannot read: it may add a required member"},
		},
		{
			name:    "a member removed from a return type that is not exported",
			old:     "interface Inner { a: string; b: number }\nexport function f(): Inner { throw 0; }",
			current: "interface Inner { a: string }\nexport function f(): Inner { throw 0; }",
			want:    []string{"warning: property Inner.b removed"},
		},
		{
			name:    "a method removed from a base class that is not exported",
			old:     "class Base { run(): void {}\nstop(): void {} }\nexport class C extends Base {}",
			current: "class Base { stop(): void {} }\nexport class C extends Base {}",
			// Importers of C call the method it inherited.
			want: []string{"method C.run removed", "warning: method Base.run removed"},
		},
		{
			name:    "a member removed two references away, through a cycle",
			old:     "interface Leaf { v: string }\ninterface Tree { next: Tree; leaf: Leaf }\nexport const root: Tree = null as any;",
			current: "interface Leaf {}\ninterface Tree { next: Tree; leaf: Leaf }\nexport const root: Tree = null as any;",
			want:    []string{"warning: property Leaf.v removed"},
		},
		{
			name: "a type predicate that names another parameter", old: "export function f(a: unknown, b: unknown): a is string { return true; }", current: "export function f(a: unknown, b: unknown): b is string { return true; }",
			want: []string{"warning: function f changed from (unknown, unknown) => _0 is string to (unknown, unknown) => _1 is string"},
		},
		{
			name: "a class exported as a type only", old: "class A {}\nenum E { X }\nexport { A, E };", current: "class A {}\nenum E { X }\nexport type { A };\nexport { type E };",
			want: []string{"class A is now exported as a type only", "enum E is now exported as a type only"},
		},
		{
			name: "an added this parameter", old: "export function f(a: string): void {}", current: "export function f(this: Window, a: string): void {}",
			want: []string{"warning: function f changed from (string) => void to (this: Window, string) => void"},
		},
		{
			name: "an exported import alias", old: "namespace N { export class C {} }\nexport import C = N.C;", current: "namespace N { export class C {} }\nexport {};",
			want: []string{"export C removed"},
		},
		{
			name:    "a member of a namespace merged with a class",
			old:     "export class C {}\nexport namespace C { export const a = 1; export function b(): void {} }",
			current: "export class C {}\nexport namespace C { export const a = 1; }",
			want:    []string{"namespace member C.b removed"},
		},
		{
			name: "a default export", old: "export default function run(): void {}", current: "export function run(): void {}",
			want: []string{"function default removed"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := changes(t, testCase.old, testCase.current); !slices.Equal(got, testCase.want) {
				t.Fatalf("changes = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCompatibleChangesPass(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, old, current string }{
		{"added exports", "export const a = 1;", "export const a = 1; export function b(): void {}\nexport interface C {}\nexport default class {}"},
		{"a renamed parameter", "export function f(a: string): void {}", "export function f(b: string): void {}"},
		{"a renamed type parameter", "export function f<T>(a: T, b: (x: T) => T): T { return a; }", "export function f<U>(a: U, b: (y: U) => U): U { return a; }"},
		{"an added optional parameter", "export function f(a: string): void {}", "export function f(a: string, b?: number, ...rest: string[]): void {}"},
		{"a parameter that gains a default", "export function f(a: string): void {}", "export function f(a: string = ''): void {}"},
		{"a required parameter made optional", "export function f(a: string): void {}", "export function f(a?: string): void {}"},
		{"an added overload", "export function f(a: string): string { return a; }", "export function f(a: string): string;\nexport function f(a: number): number;\nexport function f(a: any): any { return a; }"},
		{"another spelling of one type", "export type T = { b: Array<string>; a: 'x' | \"y\" };", "export type T = { a: \"y\" | 'x', b: string[] };"},
		{"an added optional member", "export interface C { a: string }", "export interface C { a: string; b?: number; c?(): void }"},
		{"an added class member", "export class C { a(): void {} }", "export class C { a(): void {}\nb(): void {}\nprivate c = 1;\n#d = 2; }"},
		{"a removed private member", "export class C { private a = 1; #b = 2; private c(): void {} }", "export class C {}"},
		{"an interface written as an object type", "export interface C { a: string }", "export type C = { a: string };"},
		{"a function written as a constant", "export function f(a: string): string { return a; }", "export const f = (a: string): string => a;"},
		{"an unwritten return type", "export function f(a: string) { return a; }", "export function f(a: string): number { return 1; }"},
		{"an added type parameter with a default", "export interface Box<T> { value: T }", "export interface Box<T, U = string> { value: T }"},
		{"an added enum member at the end", "export enum Level { Debug, Info }", "export enum Level { Debug, Info, Warn }"},
		{"a name that moves to another package", "export { z } from 'zod';", "export { z } from 'zod/v4';"},
		{"a renamed type predicate parameter", "export function f(a: unknown): a is string { return true; }", "export function f(b: unknown): b is string { return true; }"},
		{"a private member that changes", "export class C { private a: string = ''; private b(): void {} }", "export class C { private a: number = 1; private b(x: Hidden): void {}\nprivate c = 2; }"},
		{"a member made public", "export class C { protected a(): void {}\nprivate b(): void {} }", "export class C { a(): void {}\nb(): void {} }"},
		{"a setter added", "export class C { get v(): number { return 1; } }", "export class C { get v(): number { return 1; }\nset v(x: number) {} }"},
		{"a type parameter that gains a default", "export interface Box<T> { value: T }", "export interface Box<T = string> { value: T }"},
		{"an interface that gains a base of optional members", "export interface A { a: string }", "interface B { b?: string }\nexport interface A extends B { a: string }"},
		{"a member moved to a base interface", "export interface A { a: string; b: number }", "interface B { b: number }\nexport interface A extends B { a: string }"},
		{"a method moved to a base class", "export class C { run(): void {} }", "class Base { run(): void {} }\nexport class C extends Base {}"},
		{"a member added to a type that is not exported and only returned", "class Inner { a(): void {} }\nexport function f(): Inner { throw 0; }", "class Inner { a(): void {}\nb(): void {} }\nexport function f(): Inner { throw 0; }"},
		{"a constructor without parameters removed", "export class C { constructor() {} }", "export class C {}"},
		{"a namespace member added", "export namespace N { export const a = 1; }", "export namespace N { export const a = 1; export const b = 2; }"},
		{"a value and a type under one name", "export const User = 1; export type User = { name: string };", "export type User = { name: string }; export const User = 2;"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := changes(t, testCase.old, testCase.current); len(got) != 0 {
				t.Fatalf("changes = %q, want none", got)
			}
		})
	}
}

// messagesOf returns the messages of the changes between two sets of files,
// warnings with their prefix.
func messagesOf(t *testing.T, old, current map[string]string) []string {
	t.Helper()
	surfaces := read(t, old, current)
	var found []string
	for _, change := range Incompatible("@example/lib", surfaces[0], surfaces[1]) {
		if change.Warning {
			found = append(found, "warning: "+change.Message)
		} else {
			found = append(found, change.Message)
		}
	}
	return found
}

func TestTwoDeclarationsOfOneNameInTwoFilesAreBothFollowed(t *testing.T) {
	t.Parallel()
	index := "export { a } from './a';\nexport { b } from './b';\n"
	old := map[string]string{
		"src/index.ts": index,
		"src/a.ts":     "interface Props { one: string; two: string }\nexport function a(): Props { throw 0; }",
		"src/b.ts":     "interface Props { three: string; four: string }\nexport function b(): Props { throw 0; }",
	}
	current := map[string]string{
		"src/index.ts": index,
		"src/a.ts":     "interface Props { one: string }\nexport function a(): Props { throw 0; }",
		"src/b.ts":     "interface Props { three: string }\nexport function b(): Props { throw 0; }",
	}
	// Importers have no name for Props: its lost members are warnings.
	want := []string{"warning: property Props.two removed", "warning: property Props.four removed"}
	if got := messagesOf(t, old, current); !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
}

func TestABaseIsResolvedInTheFileThatNamesIt(t *testing.T) {
	t.Parallel()
	index := "export { A } from './a';\nexport { B } from './b';\n"
	old := map[string]string{
		"src/index.ts": index,
		"src/a.ts":     "class Base {}\nexport class A extends Base { run(): void {} }",
		"src/b.ts":     "class Base { run(): void {} }\nexport class B extends Base {}",
	}
	current := map[string]string{
		"src/index.ts": index,
		"src/a.ts":     "class Base {}\nexport class A extends Base {}",
		"src/b.ts":     "class Base { run(): void {} }\nexport class B extends Base {}",
	}
	// The Base of a.ts has no run: the one of b.ts must not hide the removal.
	if got := messagesOf(t, old, current); !slices.Equal(got, []string{"method A.run removed"}) {
		t.Fatalf("changes = %q, want the removal of A.run", got)
	}

	// A member that moves to the base its own file names is still there.
	current["src/a.ts"] = "class Base { run(): void {} }\nexport class A extends Base {}"
	if got := messagesOf(t, old, current); len(got) != 0 {
		t.Fatalf("changes = %q, want none for a member moved to its base", got)
	}
}

func TestADeclarationFileExportsEveryTopLevelDeclaration(t *testing.T) {
	t.Parallel()
	index := "export * from './types';\n"
	old := map[string]string{
		"src/index.ts":   index,
		"src/types.d.ts": "import type { X } from 'x';\ndeclare function a(x: X): void;\ninterface Gone { a: string }",
	}
	current := map[string]string{
		"src/index.ts":   index,
		"src/types.d.ts": "import type { X } from 'x';\ndeclare function a(x: X): void;",
	}
	if got := messagesOf(t, old, current); !slices.Equal(got, []string{"interface Gone removed"}) {
		t.Fatalf("changes = %q, want the removal of Gone", got)
	}

	// With an export list, only the list is exported.
	old["src/types.d.ts"] += "\nexport { a };"
	current["src/types.d.ts"] += "\nexport { a };"
	if got := messagesOf(t, old, current); len(got) != 0 {
		t.Fatalf("changes = %q, want none: Gone was not exported", got)
	}
}

func TestAnExportAssignmentIsNotCompared(t *testing.T) {
	t.Parallel()
	surfaces := read(t,
		map[string]string{"src/index.ts": "import fs = require('fs');\nclass C { run(): void {} }\nexport = C;"},
		map[string]string{"src/index.ts": "import fs = require('fs');\nclass C {}\nexport = C;"},
	)
	comparison := Compare("@example/lib", surfaces[0], surfaces[1])
	want := []Skipped{{Subpath: ".", Target: "src/index.ts", Reason: Script}}
	if !surfaces[0].Entries[0].Script || comparison.Compared != 0 || !slices.Equal(comparison.Skipped, want) {
		t.Fatalf("comparison = %+v, want an entry point that assigns its exports with `export =` not compared", comparison)
	}
}

func TestReExportsAreFollowedInsideThePackage(t *testing.T) {
	t.Parallel()
	old := map[string]string{
		"src/index.ts":     "export * from './a';\nexport { helper as assist } from './b.js';\nimport { deep } from './dir';\nexport { deep };\nexport * as tools from './b';\n",
		"src/a.ts":         "export function one(): void {}\nexport default 1;\n",
		"src/b.tsx":        "export function helper(): void {}\n",
		"src/dir/index.ts": "export const deep: number = 1;\n",
	}
	current := map[string]string{
		"src/index.ts": "export function one(): void {}\nexport function assist(): void {}\nexport const deep: string = '';\n",
	}
	surfaces := read(t, old, current)
	names := make([]string, 0, len(surfaces[0].Entries[0].Exports))
	for _, declaration := range surfaces[0].Entries[0].Exports {
		names = append(names, declaration.Kind+" "+declaration.Name+" in "+declaration.File)
	}
	want := []string{"function assist in src/b.tsx", "variable deep in src/dir/index.ts", "function one in src/a.ts", "namespace tools in src/b.tsx"}
	if !slices.Equal(names, want) {
		t.Fatalf("exports = %q\nwant      %q", names, want)
	}
	incompatible := Incompatible("@example/lib", surfaces[0], surfaces[1])
	found := make([]string, 0, len(incompatible))
	for _, change := range incompatible {
		found = append(found, change.Package+": "+change.Message)
	}
	wantChanges := []string{"@example/lib: variable deep changed type from number to string", "@example/lib: namespace tools removed"}
	if !slices.Equal(found, wantChanges) {
		t.Fatalf("changes = %q\nwant      %q", found, wantChanges)
	}
}

func TestAFileWithoutImportOrExportIsAScript(t *testing.T) {
	t.Parallel()
	surfaces := read(t,
		map[string]string{"src/index.ts": "module.exports = { a() {} }; exports.b = 1;"},
		map[string]string{"src/index.ts": "export const a = 1;"},
		map[string]string{"src/index.ts": "export {};"},
	)
	if !surfaces[0].Entries[0].Script || surfaces[1].Entries[0].Script || surfaces[2].Entries[0].Script {
		t.Fatalf("script = %v, %v, %v, want true, false, false",
			surfaces[0].Entries[0].Script, surfaces[1].Entries[0].Script, surfaces[2].Entries[0].Script)
	}
	if Compare("@example/lib", surfaces[0], surfaces[1]).Compared != 0 || Compare("@example/lib", surfaces[1], surfaces[2]).Compared != 1 {
		t.Fatal("a released script is not compared, and a released module is")
	}
	if found := Incompatible("@example/lib", surfaces[0], surfaces[0]); len(found) != 0 {
		t.Fatalf("changes = %+v, want none for a script", found)
	}
}

func TestSyntaxErrorsAreReportedWithTheirFile(t *testing.T) {
	t.Parallel()
	surfaces := read(t, map[string]string{"src/index.ts": "export * from './broken';\n", "src/broken.ts": "\nexport function (((\n"})
	if len(surfaces[0].Errors) == 0 || surfaces[0].Errors[0].File != "src/broken.ts" || surfaces[0].Errors[0].Line != 2 {
		t.Fatalf("errors = %+v", surfaces[0].Errors)
	}
}

func TestWithoutTheTypeScriptPackageNothingIsRead(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read("bun-is-not-started", dir, Side{}); !errors.Is(err, ErrNoTypeScript) {
		t.Fatalf("err = %v, want ErrNoTypeScript", err)
	}
}

func TestAFailingReaderIsAnError(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(filepath.Join(t.TempDir(), "no-bun"), dir, Side{}); err == nil || !strings.Contains(err.Error(), "read the API: bun") {
		t.Fatalf("err = %v, want the failed start", err)
	}
}

func TestEntryPointsFollowThePackageManifest(t *testing.T) {
	t.Parallel()
	sources := []string{"src/index.ts", "src/index.browser.ts", "src/hooks.ts", "src/plugins/a.ts", "src/plugins/b.ts"}
	for _, testCase := range []struct {
		name, manifest string
		want           []string
	}{
		{"a single target", `{"exports":"./src/index.ts"}`, []string{". -> src/index.ts read"}},
		{"conditions of the root", `{"exports":{"browser":"./src/index.browser.ts","default":"./src/index.ts"}}`,
			[]string{". -> src/index.ts read", ". (browser) -> src/index.browser.ts read"}},
		{"subpaths, nested conditions, fallbacks and null", `{"exports":{".":{"import":{"types":"./dist/index.d.ts","default":["./src/index.ts","./x.js"]}},"./hooks":"./src/hooks.ts","./style.css":"./style.css","./private":null}}`,
			[]string{". (import) -> src/index.ts read", ". (import, types) -> dist/index.d.ts missing", "./hooks -> src/hooks.ts read", "./style.css -> style.css asset"}},
		{"a subpath pattern", `{"exports":{"./plugins/*":"./src/plugins/*.ts"}}`,
			[]string{"./plugins/a -> src/plugins/a.ts read", "./plugins/b -> src/plugins/b.ts read"}},
		{"exports wins over main", `{"main":"src/hooks.ts","exports":"./src/index.ts"}`, []string{". -> src/index.ts read"}},
		{"main, module and types", `{"main":"src/index.ts","module":"./dist/index.mjs","typings":"dist/index.d.ts"}`,
			[]string{". -> src/index.ts read", ". (module) -> dist/index.mjs missing", ". (types) -> dist/index.d.ts missing"}},
		{"a target without an extension, a directory and a path that resolves to nothing", `{"exports":{".":"./src/index","./plugins":"./src/plugins/a","./dir":"./src","./gone":"./lib/index"}}`,
			[]string{". -> src/index.ts read", "./dir -> src/index.ts read", "./gone -> lib/index missing", "./plugins -> src/plugins/a.ts read"}},
		{"a main that names a directory", `{"main":"src"}`, []string{". -> src/index.ts read"}},
		{"no entry point", `{"name":"x"}`, nil},
	} {
		points, err := EntryPoints([]byte(testCase.manifest), sources)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		var got []string
		for _, point := range points {
			got = append(got, point.Key+" -> "+point.Target+" "+[]string{"read", "asset", "missing"}[point.State])
		}
		if !slices.Equal(got, testCase.want) {
			t.Errorf("%s: entry points = %q\nwant %q", testCase.name, got, testCase.want)
		}
	}
	for _, manifest := range []string{`{not json`, `{"exports":{".":"./a.ts","import":"./b.ts"}}`, `{"exports":{".":7}}`} {
		if _, err := EntryPoints([]byte(manifest), nil); err == nil {
			t.Errorf("EntryPoints(%s) succeeded, want an error", manifest)
		}
	}
}

func TestAnEntryPointThatStopsNamingSourceIsIncompatible(t *testing.T) {
	t.Parallel()
	old := &Surface{Entries: []Entry{{Key: ".", State: TargetRead, Exports: []Declaration{{Name: "a", Kind: "variable"}}}, {Key: "./built", State: TargetMissing}}}
	current := &Surface{Entries: []Entry{{Key: ".", Target: "style.css", State: TargetAsset}, {Key: "./built", State: TargetRead}}}
	got := Incompatible("@example/lib", old, current)
	if len(got) != 1 || got[0].Package != "@example/lib" || got[0].Message != "entry point now names style.css, which is not a source module" {
		t.Fatalf("changes = %+v", got)
	}
	if name := PackageName("@example/lib", "./hooks (browser)"); name != "@example/lib/hooks (browser)" {
		t.Fatalf("PackageName = %q", name)
	}
}

func TestOnlySourceOutsideNodeModulesIsRead(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"src/index.ts": true, "src/view.tsx": true, "bin/run.mjs": true, "types.d.ts": true,
		"README.md": false, "package.json": false, "node_modules/dep/index.ts": false, "src/node_modules/x.js": false,
	} {
		if got := IsSource(name); got != want {
			t.Errorf("IsSource(%q) = %v, want %v", name, got, want)
		}
	}
}
