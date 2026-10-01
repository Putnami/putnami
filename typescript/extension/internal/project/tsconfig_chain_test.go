package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTsConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStripJSONC_RemovesCommentsAndTrailingCommasOnly(t *testing.T) {
	in := `{
  // line comment
  "a": "keeps // and /* inside strings",
  /* block
     comment */ "b": [1, 2,],
  "c": "escaped \" quote, //",
  "d": {"e": true,},
}`
	var got map[string]any
	if err := json.Unmarshal(StripJSONC([]byte(in)), &got); err != nil {
		t.Fatalf("stripped JSONC does not parse: %v\n%s", err, StripJSONC([]byte(in)))
	}
	want := map[string]any{
		"a": "keeps // and /* inside strings",
		"b": []any{1.0, 2.0},
		"c": `escaped " quote, //`,
		"d": map[string]any{"e": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadTsConfigChain_AppliesExtendsPrecedenceAndRecordsDeclaringDir(t *testing.T) {
	// A package config resolves through its real path.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTsConfigFile(t, filepath.Join(root, "node_modules", "@scope", "cfg", "tsconfig.json"),
		`{"compilerOptions": {"strict": false, "typeRoots": ["./types"], "baseUrl": "."}}`)
	writeTsConfigFile(t, filepath.Join(root, "base", "one.json"),
		`{"compilerOptions": {"strict": true, "jsx": "preserve"}, "files": ["x.ts"]}`)
	writeTsConfigFile(t, filepath.Join(root, "tsconfig.base.json"), `{
  // a later extends entry overrides an earlier one
  "extends": ["@scope/cfg", "./base/one"],
  "compilerOptions": {"jsx": "react-jsx",},
}`)
	project := filepath.Join(root, "apps", "web")
	writeTsConfigFile(t, filepath.Join(project, "tsconfig.json"),
		`{"extends": "../../tsconfig.base.json", "compilerOptions": {"paths": {"@/*": ["${configDir}/src/*"]}}}`)

	chain := ReadTsConfigChain(filepath.Join(project, "tsconfig.json"))

	cases := map[string]TsConfigValue{
		"strict":    {Value: true, Dir: filepath.Join(root, "base")},
		"jsx":       {Value: "react-jsx", Dir: root},
		"typeRoots": {Value: []any{"./types"}, Dir: filepath.Join(root, "node_modules", "@scope", "cfg")},
		"baseUrl":   {Value: ".", Dir: filepath.Join(root, "node_modules", "@scope", "cfg")},
		"paths":     {Value: map[string]any{"@/*": []any{"${configDir}/src/*"}}, Dir: project},
	}
	for name, want := range cases {
		if got := chain.CompilerOptions[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}
	if chain.Files == nil || chain.Files.Dir != filepath.Join(root, "base") {
		t.Errorf("files = %+v, want the list declared in base/one.json", chain.Files)
	}
}

func TestReadTsConfigChain_ToleratesCyclesAndMissingConfigs(t *testing.T) {
	root := t.TempDir()
	writeTsConfigFile(t, filepath.Join(root, "a.json"), `{"extends": ["./b.json", "missing-package"], "compilerOptions": {"strict": true}}`)
	writeTsConfigFile(t, filepath.Join(root, "b.json"), `{"extends": "./a.json", "compilerOptions": {"noEmit": true}}`)

	chain := ReadTsConfigChain(filepath.Join(root, "a.json"))
	if chain.CompilerOptions["strict"].Value != true || chain.CompilerOptions["noEmit"].Value != true {
		t.Errorf("compilerOptions = %+v, want strict and noEmit", chain.CompilerOptions)
	}
	if got := ReadTsConfigChain(filepath.Join(root, "absent.json")); len(got.CompilerOptions) != 0 || got.Files != nil {
		t.Errorf("an unreadable config resolves %+v, want nothing", got)
	}
}

func TestReadTsConfigChain_IgnoresAByteOrderMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tsconfig.json")
	writeTsConfigFile(t, path, "\xEF\xBB\xBF{\"compilerOptions\": {\"baseUrl\": \"${configDir}\"}}")
	if got := ReadTsConfigChain(path).CompilerOptions["baseUrl"].Value; got != "${configDir}" {
		t.Errorf("baseUrl = %v, want the value behind the byte-order mark", got)
	}
}

func TestReadTsConfigChain_ResolvesPackageExports(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "store", "cfg")
	writeTsConfigFile(t, filepath.Join(pkg, "package.json"), `{"exports": {
  ".": {"import": "./esm.json", "require": "./base.json"},
  "./strict": "./configs/strict.json",
  "./presets/*": "./presets/*.json"
}}`)
	writeTsConfigFile(t, filepath.Join(pkg, "base.json"), `{"compilerOptions": {"typeRoots": ["./types"]}}`)
	writeTsConfigFile(t, filepath.Join(pkg, "esm.json"), `{"compilerOptions": {"typeRoots": ["./esm"]}}`)
	writeTsConfigFile(t, filepath.Join(pkg, "configs", "strict.json"), `{"compilerOptions": {"strict": true}}`)
	writeTsConfigFile(t, filepath.Join(pkg, "presets", "bun.json"), `{"compilerOptions": {"jsx": "react-jsx"}}`)
	// The installed package is a symlink, as package managers link it.
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pkg, filepath.Join(root, "node_modules", "@scope", "cfg")); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "app")
	writeTsConfigFile(t, filepath.Join(project, "tsconfig.json"),
		`{"extends": ["@scope/cfg", "@scope/cfg/strict", "@scope/cfg/presets/bun", "@scope/cfg/hidden.json"]}`)

	chain := ReadTsConfigChain(filepath.Join(project, "tsconfig.json"))
	realPkg, err := filepath.EvalSymlinks(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := chain.CompilerOptions["typeRoots"], (TsConfigValue{Value: []any{"./types"}, Dir: realPkg}); !reflect.DeepEqual(got, want) {
		t.Errorf("typeRoots = %+v, want the require target anchored on the real package path %+v", got, want)
	}
	if chain.CompilerOptions["strict"].Value != true || chain.CompilerOptions["jsx"].Value != "react-jsx" {
		t.Errorf("compilerOptions = %+v, want the exported subpath and pattern configs", chain.CompilerOptions)
	}
}

func TestExportsTarget_PrefersTheLongerPatternKeyOnATie(t *testing.T) {
	exports := json.RawMessage(`{"./presets/*": "./short/*.json", "./presets/*.json": "./long/*.json"}`)
	if got, ok := exportsTarget(exports, "./presets/bun.json"); !ok || got != "./long/bun.json" {
		t.Errorf("target = %q, %v; want ./long/bun.json", got, ok)
	}
}

func TestReadTsConfigChain_ReadsExportsBehindAByteOrderMark(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "node_modules", "cfg")
	writeTsConfigFile(t, filepath.Join(pkg, "package.json"), "\xEF\xBB\xBF{\"exports\": {\".\": \"./base.json\"}}")
	writeTsConfigFile(t, filepath.Join(pkg, "base.json"), `{"compilerOptions": {"strict": true}}`)
	writeTsConfigFile(t, filepath.Join(pkg, "tsconfig.json"), `{"compilerOptions": {"strict": false}}`)
	writeTsConfigFile(t, filepath.Join(root, "tsconfig.json"), `{"extends": "cfg"}`)
	if got := ReadTsConfigChain(filepath.Join(root, "tsconfig.json")).CompilerOptions["strict"].Value; got != true {
		t.Errorf("strict = %v, want the exported base.json", got)
	}
}
