package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// identityLib is the dependency of the measured module: app's test links it.
const identityLib = "package lib\n\n// Add returns a plus b.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

// writeIdentityModule lays out, under dir, a module example.com/m of two
// packages: lib, and app, which calls lib.Add. Each has a test.
func writeIdentityModule(t *testing.T, dir string) string {
	t.Helper()
	for _, sub := range []string{"lib", "app"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(dir, "lib", "lib.go"), identityLib)
	mustWrite(t, filepath.Join(dir, "lib", "lib_test.go"),
		"package lib\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"1 + 2 != 3\")\n\t}\n}\n")
	mustWrite(t, filepath.Join(dir, "app", "app.go"),
		"package app\n\nimport \"example.com/m/lib\"\n\n// Three returns three.\nfunc Three() int { return lib.Add(1, 2) }\n")
	mustWrite(t, filepath.Join(dir, "app", "app_test.go"),
		"package app\n\nimport \"testing\"\n\nfunc TestThree(t *testing.T) {\n\tif Three() != 3 {\n\t\tt.Fatal(\"not three\")\n\t}\n}\n")
	return dir
}

// realIdentity runs the identity job with the real toolchain on the module
// at dir, with the default parameters of a test run (coverage instrumented).
func realIdentity(t *testing.T, dir string) (IdentityManifest, string) {
	t.Helper()
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "m", FullPath: dir},
		Params:        pctx.Params{},
	}
	status, _, err := RunIdentity(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("RunIdentity(%s) status=%q err=%v", dir, status, err)
	}
	data, err := os.ReadFile(filepath.Join(ctx.OutputPath, IdentityManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	return decodeIdentity(t, data), string(data)
}

// editLib rewrites lib.go of the module at dir with edit applied to the
// original source.
func editLib(t *testing.T, dir string, edit func(string) string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "lib", "lib.go"), edit(identityLib))
}

// TestIdentityFollowsTheCompiledBytesNotTheSourceText is the measurement of
// ADR 0017 in miniature, against the real toolchain: the identity of app's
// test binary, which links lib, is the same at two checkout paths and after
// an end-of-line comment in lib, and it moves with a line-shifting comment
// (lib's line table and app's inlined copy of Add move) and with a body edit.
func TestIdentityFollowsTheCompiledBytesNotTheSourceText(t *testing.T) {
	t.Setenv(envDatabaseTestBindings, "")
	root := t.TempDir()
	first := writeIdentityModule(t, filepath.Join(root, "first"))
	second := writeIdentityModule(t, filepath.Join(root, "elsewhere", "second"))
	const app = "example.com/m/app.test"

	base, baseBytes := realIdentity(t, first)
	if base.Unresolved != "" || base.Tests[app] == "" || base.Tests["example.com/m/lib.test"] == "" {
		t.Fatalf("identity = %s, want both test binaries resolved", baseBytes)
	}
	if base.Packages["example.com/m/app"] == "" || base.Packages["example.com/m/lib"] == "" {
		t.Fatalf("identity = %s, want both packages named", baseBytes)
	}
	if !strings.Contains(strings.Join(base.Flags, " "), "-coverpkg example.com/m/app,example.com/m/lib") {
		t.Fatalf("identity flags = %q, want the module's packages instrumented as test-exec instruments them", base.Flags)
	}

	if _, elsewhere := realIdentity(t, second); elsewhere != baseBytes {
		t.Fatalf("the same sources at another checkout path name other binaries:\n%s\nvs\n%s", elsewhere, baseBytes)
	}

	editLib(t, first, func(src string) string {
		return strings.Replace(src, "return a + b\n", "return a + b // the sum\n", 1)
	})
	if _, eol := realIdentity(t, first); eol != baseBytes {
		t.Fatalf("an end-of-line comment, which compiles to the same bytes, moved the identity:\n%s\nvs\n%s", eol, baseBytes)
	}

	editLib(t, first, func(src string) string { return "// A line before the package clause.\n" + src })
	if shifted, _ := realIdentity(t, first); shifted.Tests[app] == base.Tests[app] {
		t.Fatalf("a line-shifting comment in lib left app's test binary unchanged: %s", shifted.Tests[app])
	}

	editLib(t, first, func(src string) string { return strings.Replace(src, "return a + b\n", "return a + b + 1\n", 1) })
	if body, _ := realIdentity(t, first); body.Tests[app] == base.Tests[app] || body.Packages["example.com/m/lib"] == base.Packages["example.com/m/lib"] {
		t.Fatalf("a body edit in lib left the identity unchanged: %+v", body)
	}
}
