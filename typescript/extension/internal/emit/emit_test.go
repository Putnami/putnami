package emit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireBun(t *testing.T) string {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	return bun
}

// fixture is a workspace with a library the project depends on.
func fixture(t *testing.T) (string, []Member) {
	t.Helper()
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "tsconfig.base.json"), `{
  // the workspace options
  "compilerOptions": { "experimentalDecorators": true, },
}`)
	writeFile(t, filepath.Join(ws, "lib", "tsconfig.json"), `{ "extends": "../tsconfig.base.json" }`)
	writeFile(t, filepath.Join(ws, "lib", "package.json"), `{"name":"lib","main":"src/index.ts"}`)
	writeFile(t, filepath.Join(ws, "lib", "src", "index.ts"), `import type { Shape } from './shape';
// Reads the area.
export function area(s: Shape): number {
  return s.w * s.h;
}
`)
	writeFile(t, filepath.Join(ws, "lib", "src", "shape.ts"), "export interface Shape { w: number; h: number }\n")
	writeFile(t, filepath.Join(ws, "lib", "src", "query.sql"), "select 1;\n")
	writeFile(t, filepath.Join(ws, "app", "package.json"), `{"name":"app"}`)
	writeFile(t, filepath.Join(ws, "app", "test", "area.test.ts"), "import { area } from 'lib';\n")
	return ws, []Member{{Path: "app", Root: filepath.Join(ws, "app")}, {Path: "lib", Root: filepath.Join(ws, "lib")}}
}

func build(t *testing.T, ws string, members []Member, bun string) Manifest {
	t.Helper()
	m, err := Build(Options{WorkspaceRoot: ws, Members: members, Transpile: BunTranspiler(bun)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func encoded(t *testing.T, m Manifest) string {
	t.Helper()
	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestManifestHoldsForATypeOrCommentChange(t *testing.T) {
	bun := requireBun(t)
	ws, members := fixture(t)
	before := build(t, ws, members, bun)
	if before.Mode != ModeEmit {
		t.Fatalf("mode = %s (%s), want emit", before.Mode, before.Reason)
	}
	for _, e := range before.Files {
		if e.Path == "src/index.ts" && e.Kind != KindJS {
			t.Fatalf("src/index.ts keyed as %s, want js", e.Kind)
		}
	}
	writeFile(t, filepath.Join(ws, "lib", "src", "shape.ts"), "export interface Shape { w: number; h: number; label?: string }\n")
	writeFile(t, filepath.Join(ws, "lib", "src", "index.ts"), `import type { Shape } from './shape';
// Reads the area of a shape, in square units.

export function area(s: Readonly<Shape>): number {
  return s.w * s.h;
}
`)
	if got, want := encoded(t, build(t, ws, members, bun)), encoded(t, before); got != want {
		t.Fatalf("a type-only and comment-only change moved the manifest:\n%s\nwant\n%s", got, want)
	}
}

func TestManifestMovesWithWhatRuns(t *testing.T) {
	bun := requireBun(t)
	cases := map[string]func(ws string){
		"emitted JS": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", "src", "index.ts"), "export function area(s: { w: number; h: number }): number {\n  return s.h * s.w;\n}\n")
		},
		"a file read at run time": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", "src", "query.sql"), "select 2;\n")
		},
		"the governing config": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", "tsconfig.json"), `{ "extends": "../tsconfig.base.json", "compilerOptions": { "jsx": "react" } }`)
		},
		"the package manifest": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", "package.json"), `{"name":"lib","main":"src/shape.ts"}`)
		},
		"a removed module": func(ws string) {
			if err := os.Remove(filepath.Join(ws, "lib", "src", "shape.ts")); err != nil {
				t.Fatal(err)
			}
		},
		"a generated file": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", ".gen", "routes.ts"), "export const routes = [];\n")
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			ws, members := fixture(t)
			before := encoded(t, build(t, ws, members, bun))
			change(ws)
			if encoded(t, build(t, ws, members, bun)) == before {
				t.Fatalf("a change to %s left the manifest as it was", name)
			}
		})
	}
}

func TestDeclarationFilesAreNotKeyed(t *testing.T) {
	bun := requireBun(t)
	ws, members := fixture(t)
	writeFile(t, filepath.Join(ws, "lib", "src", "globals.d.ts"), "declare const X: number;\n")
	before := encoded(t, build(t, ws, members, bun))
	writeFile(t, filepath.Join(ws, "lib", "src", "globals.d.ts"), "declare const X: string;\n")
	if encoded(t, build(t, ws, members, bun)) != before {
		t.Fatal("a declaration file moved the manifest")
	}
}

func TestATransformingHookKeysEveryModuleOnItsBytes(t *testing.T) {
	bun := requireBun(t)
	hooks := map[string]func(ws string){
		"a Bun plugin": func(ws string) {
			writeFile(t, filepath.Join(ws, "app", "test", "setup.ts"), "Bun.plugin({ name: 'x', setup() {} });\n")
		},
		"a plugin import": func(ws string) {
			writeFile(t, filepath.Join(ws, "lib", "src", "hook.ts"), "import { plugin } from 'bun';\nplugin({ name: 'x', setup() {} });\n")
		},
		"a project preload": func(ws string) {
			writeFile(t, filepath.Join(ws, "app", "bunfig.toml"), "[test]\npreload = [\"./test/setup.ts\"]\n")
		},
		"a workspace preload": func(ws string) {
			writeFile(t, filepath.Join(ws, "bunfig.toml"), "preload = [\"./setup.ts\"]\n")
		},
	}
	for name, hook := range hooks {
		t.Run(name, func(t *testing.T) {
			ws, members := fixture(t)
			hook(ws)
			before := build(t, ws, members, bun)
			if before.Mode != ModeBytes || before.Reason == "" {
				t.Fatalf("mode = %s (%q), want bytes with a reason", before.Mode, before.Reason)
			}
			writeFile(t, filepath.Join(ws, "lib", "src", "shape.ts"), "export interface Shape { w: number; h: number; label?: string }\n")
			if encoded(t, build(t, ws, members, bun)) == encoded(t, before) {
				t.Fatal("a type-only change kept the manifest of a closure a hook can transform")
			}
		})
	}
}

func TestAnUnreadableConfigChainKeysItsModulesOnTheirBytes(t *testing.T) {
	bun := requireBun(t)
	ws, members := fixture(t)
	writeFile(t, filepath.Join(ws, "lib", "tsconfig.json"), `{ "extends": "@tsconfig/bun" }`)
	m := build(t, ws, members, bun)
	for _, e := range m.Files {
		if e.Project == "lib" && e.Path == "src/index.ts" && e.Kind != KindBytes {
			t.Fatalf("src/index.ts keyed as %s under a package extends, want bytes", e.Kind)
		}
	}
}

func TestStripJSONC(t *testing.T) {
	got := string(StripJSONC([]byte(`{ "a": "x // y", /* c */ "b": [1, 2,], }`)))
	if got != `{ "a": "x // y",  "b": [1, 2] }` {
		t.Fatalf("StripJSONC = %q", got)
	}
}

func TestAConfigOutsideTheKeyKeysItsModulesOnTheirBytes(t *testing.T) {
	bun := requireBun(t)
	ws, members := fixture(t)
	writeFile(t, filepath.Join(ws, "shared", "tsconfig.json"), `{ "compilerOptions": {} }`)
	writeFile(t, filepath.Join(ws, "lib", "tsconfig.json"), `{ "extends": "../shared/tsconfig.json" }`)
	for _, e := range build(t, ws, members, bun).Files {
		if e.Project == "lib" && e.Path == "src/index.ts" && e.Kind != KindBytes {
			t.Fatalf("src/index.ts keyed as %s under a config outside the key, want bytes", e.Kind)
		}
	}
}
