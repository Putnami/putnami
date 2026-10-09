package gosource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A module module that declares no package main anywhere can only be imported.
func TestModuleHasMain_LibraryModule(t *testing.T) {
	root := t.TempDir()
	writeSource(t, filepath.Join(root, "go.mod"), "module example.com/lib\n\ngo 1.25\n")
	writeSource(t, filepath.Join(root, "lib.go"), "package lib\n")
	writeSource(t, filepath.Join(root, "internal", "deep", "deep.go"), "package deep\n")

	if hasMain, err := ModuleHasMain(root); err != nil || hasMain {
		t.Fatalf("ModuleHasMain = (%v, %v), want (false, nil)", hasMain, err)
	}
}

// The main package does not have to sit at the module root: cmd/<name>/main.go
// is the ordinary Go layout, and a classifier that only looked at the root
// would call every one of them a library.
func TestModuleHasMain_FindsMainBelowTheRoot(t *testing.T) {
	root := t.TempDir()
	writeSource(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.25\n")
	writeSource(t, filepath.Join(root, "internal", "svc", "svc.go"), "package svc\n")
	writeSource(t, filepath.Join(root, "cmd", "app", "main.go"), "package main\n\nfunc main() {}\n")

	if hasMain, err := ModuleHasMain(root); err != nil || !hasMain {
		t.Fatalf("ModuleHasMain = (%v, %v), want (true, nil)", hasMain, err)
	}
}

// The four trees whose package main belongs to somebody else. Each one is a
// library that a naive scan would call an application.
func TestModuleHasMain_IgnoresMainTheBuildNeverLinks(t *testing.T) {
	const mainSource = "package main\n\nfunc main() {}\n"
	cases := []struct {
		name string
		file string
		src  string
	}{
		{"nested module", filepath.Join("submodule", "main.go"), mainSource},
		{"vendored command", filepath.Join("vendor", "example.com/dep", "main.go"), mainSource},
		{"testdata fixture", filepath.Join("testdata", "fixture", "main.go"), mainSource},
		{"underscore directory", filepath.Join("_scratch", "main.go"), mainSource},
		{"test file", "lib_test.go", "package main\n"},
		{"generator script", "gen.go", "//go:build ignore\n\n" + mainSource},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, filepath.Join(root, "go.mod"), "module example.com/lib\n\ngo 1.25\n")
			writeSource(t, filepath.Join(root, "lib.go"), "package lib\n")
			writeSource(t, filepath.Join(root, c.file), c.src)
			if c.name == "nested module" {
				writeSource(t, filepath.Join(root, "submodule", "go.mod"), "module example.com/sub\n\ngo 1.25\n")
			}

			hasMain, err := ModuleHasMain(root)
			if err != nil {
				t.Fatalf("ModuleHasMain: %v", err)
			}
			if hasMain {
				t.Errorf("%s made the module an application; its binary is not this module's", c.name)
			}
		})
	}
}

// A platform-gated main still produces a binary — for that platform. Reading
// GOOS to decide otherwise would make one tree classify differently on a Linux
// runner than on a macOS laptop, and the probe's answer keys a cache.
func TestModuleHasMain_PlatformGatedMainStillCounts(t *testing.T) {
	for _, constraintLine := range []string{"//go:build windows", "//go:build linux && amd64", "//go:build customtag"} {
		root := t.TempDir()
		writeSource(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.25\n")
		writeSource(t, filepath.Join(root, "main.go"), constraintLine+"\n\npackage main\n\nfunc main() {}\n")

		hasMain, err := ModuleHasMain(root)
		if err != nil {
			t.Fatalf("ModuleHasMain: %v", err)
		}
		if !hasMain {
			t.Errorf("%q hid a main package; classification must not depend on the probing host", constraintLine)
		}
	}
}

// The package clause is read with the Go grammar, so text that only LOOKS like
// one — inside a string, inside a block comment — cannot move the answer.
func TestHeaderOf_ReadsTheRealPackageClause(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"plain", "package lib\n", "lib"},
		{"main", "package main\n", "main"},
		{"doc block comment", "/*\npackage main is not this file's clause.\n*/\npackage lib\n", "lib"},
		{"string literal", "package lib\n\nconst s = \"package main\"\n", "lib"},
		{"no clause", "// just a comment\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := headerOf([]byte(c.src)).Package; got != c.want {
				t.Errorf("Package = %q, want %q", got, c.want)
			}
		})
	}
}

// buildable is the packager's rule, kept intact by the move: `ignore` is pinned
// false because nothing sets -tags ignore, every other tag is brute-forced.
func TestHeader_Buildable(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"package p\n", true},
		{"//go:build ignore\n\npackage p\n", false},
		{"//go:build ignore || linux\n\npackage p\n", true},
		{"//go:build !ignore\n\npackage p\n", true},
		{"//go:build linux && !linux\n\npackage p\n", false},
		{"// +build ignore\n\npackage p\n", false},
		{"// +build ignore\npackage p\n", true},
		{"/*\n//go:build ignore\n*/\npackage p\n", true},
		{"/*\n// +build ignore\n*/\n\npackage p\n", true},
		{"//go:build darwin\n\npackage p\n", true},
	}
	for _, c := range cases {
		if got := headerOf([]byte(c.src)).buildable(); got != c.want {
			t.Errorf("buildable(%q) = %v, want %v", c.src, got, c.want)
		}
		if got := ExcludedByBuildConstraint([]byte(c.src)); got == c.want {
			t.Errorf("ExcludedByBuildConstraint(%q) = %v, want the negation of buildable", c.src, got)
		}
	}
}

// Only a prefix of each file is read, so a preamble longer than the prefix must
// fall back to the whole file rather than report "no package clause" — which
// would silently demote an application whose main.go carries a large header.
func TestReadHeader_FallsBackWhenThePrefixMissesTheClause(t *testing.T) {
	root := t.TempDir()
	preamble := strings.Repeat("// a very long license preamble line\n", (headerPrefixBytes/37)+64)
	path := filepath.Join(root, "main.go")
	writeSource(t, path, preamble+"\npackage main\n\nfunc main() {}\n")
	if len(preamble) <= headerPrefixBytes {
		t.Fatalf("fixture preamble is %d bytes; it must exceed the %d-byte prefix", len(preamble), headerPrefixBytes)
	}

	header, err := readHeader(path)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if header.Package != "main" {
		t.Errorf("Package = %q, want main read from the full file", header.Package)
	}
}

func TestToolingIgnoresPath(t *testing.T) {
	for path, want := range map[string]bool{
		"internal/svc/svc.go":  false,
		"testdata/fixture.go":  true,
		"internal/testdata/x":  true,
		"_scratch/main.go":     true,
		".gen/generated.go":    true,
		"cmd/app/main_test.go": false,
	} {
		if got := ToolingIgnoresPath(path); got != want {
			t.Errorf("ToolingIgnoresPath(%q) = %v, want %v", path, got, want)
		}
	}
}
