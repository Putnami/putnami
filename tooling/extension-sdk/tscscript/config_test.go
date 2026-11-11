package tscscript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPrefersTheLibraryConfig(t *testing.T) {
	dir := t.TempDir()
	if got := DefaultConfig(dir); got != "tsconfig.json" {
		t.Errorf("without tsconfig.lib.json = %q, want tsconfig.json", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.lib.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfig(dir); got != "tsconfig.lib.json" {
		t.Errorf("with tsconfig.lib.json = %q", got)
	}
}

func TestOwnOutDir(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]string{
		"{\n  // comment\n  \"compilerOptions\": {\"outDir\": \"./dist\",},\n}": filepath.Join(dir, "dist"),
		`{"compilerOptions": {"outDir": "${configDir}/out"}}`:                   filepath.Join(dir, "out"),
		`{"extends": "./base.json"}`:                                            "",
		`not json`:                                                              "",
	} {
		file := filepath.Join(dir, "tsconfig.json")
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := OwnOutDir(file)
		if (want != "") != ok || got != want {
			t.Errorf("OwnOutDir(%s) = %q, %v; want %q", content, got, ok, want)
		}
	}
}

func TestStripJSONCKeepsStrings(t *testing.T) {
	in := "{\"a\": \"// not a comment, \\\" /* nor this */\", /* c */ \"b\": [1, 2,],}"
	want := "{\"a\": \"// not a comment, \\\" /* nor this */\",  \"b\": [1, 2]}"
	if got := string(StripJSONC([]byte(in))); got != want {
		t.Errorf("StripJSONC = %q, want %q", got, want)
	}
}
