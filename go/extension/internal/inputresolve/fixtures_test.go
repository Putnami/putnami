package inputresolve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goSource returns a Go source of package pkg with one //go:embed directive
// per pattern.
func goSource(pkg string, patterns ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nimport _ \"embed\"\n", pkg)
	for i, pattern := range patterns {
		fmt.Fprintf(&b, "\n//go:embed %s\nvar embedded%d string\n", pattern, i)
	}
	return b.String()
}

// ignoredSource returns goSource behind a //go:build ignore header.
func ignoredSource(pkg string, patterns ...string) string {
	return "//go:build ignore\n\n" + goSource(pkg, patterns...)
}

// writeFile writes content at rel under root, creating its directories.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkdir creates the directory rel under root.
func mkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// symlink creates a link at rel under root whose target text is target.
func symlink(t *testing.T, root, rel, target string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash(target), abs); err != nil {
		t.Fatal(err)
	}
}

// remove deletes rel under root, with everything below it.
func remove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// rename moves from to to under root, creating to's directory.
func rename(t *testing.T, root, from, to string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(from)), target); err != nil {
		t.Fatal(err)
	}
}

// replaceWithLink moves the directory rel to aside and leaves a link to it at
// rel, so the tree reads the same through the link.
func replaceWithLink(t *testing.T, root, rel, aside, target string) {
	t.Helper()
	rename(t, root, rel, aside)
	symlink(t, root, rel, target)
}

// caseInsensitive reports whether the file system under dir opens a name in
// another letter case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// appFixture is a Go project at app/ whose sources embed by glob, by
// directory, with all:, and through an ignored directory; read a source link
// whose referent sits in an ignored directory; and hold a selected Go link in
// testdata, a nested module, ignored sources and an excluded broken link.
func appFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"app/go.mod":                "module example.com/app\n",
		"app/main.go":               goSource("main", "static/*", "assets", "all:web", "_private/data.txt"),
		"app/static/a.txt":          "a",
		"app/static/b.txt":          "b",
		"app/assets/x.txt":          "x",
		"app/assets/sub/y.txt":      "y",
		"app/assets/.hidden":        "hidden",
		"app/assets/_skip/z.txt":    "z",
		"app/web/index.html":        "<html>",
		"app/web/.well-known/k.txt": "k",
		"app/_private/data.txt":     "data",
		"app/lib/lib.go":            goSource("lib", "tmpl/*.tmpl"),
		"app/lib/tmpl/a.tmpl":       "tmpl",
		"app/lib/lib_test.go":       goSource("lib", "fixture.txt"),
		"app/lib/fixture.txt":       "fixture",
		"app/_shared/real.go":       goSource("lib"),
		"app/_shared/other.go":      goSource("lib", "fixture.txt"),
		"app/ignored/ign.go":        ignoredSource("ignored", "missing.txt"),
		"app/vendor/v.go":           goSource("v", "missing.txt"),
		"app/nested/go.mod":         "module example.com/nested\n",
		"app/nested/n.go":           goSource("nested", "missing.txt"),
		"app/_gen/g.go":             goSource("gen", "missing.txt"),
		"app/cmd/tool/main.go":      goSource("main", "version.txt"),
		"app/cmd/tool/version.txt":  "1.0.0",
		"app/README.md":             "readme",
	}
	for rel, content := range files {
		writeFile(t, root, rel, content)
	}
	symlink(t, root, "app/lib/link.go", "../_shared/real.go")
	symlink(t, root, "app/testdata/td.go", "../_shared/real.go")
	symlink(t, root, "app/.gen/x.go", "../nowhere.go")
}

// rootFixture is a Go project at the workspace root whose task input selects
// links through a "**" include with a literal prefix, a glob include, a
// literal include and an exclusion, and whose embed directive starts with a
// glob.
func rootFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"go.mod":               "module example.com/root\n",
		"src/main.txt":         goSource("main", "VERSION"),
		"src/other.txt":        goSource("main"),
		"VERSION":              "1",
		"_impl/tool.go":        goSource("main", "version.txt"),
		"_impl/a.go":           goSource("a"),
		"_impl/s.go":           goSource("skip"),
		"cmd/tool/version.txt": "1",
		"site/site.go":         goSource("site", "*/page.html"),
		"site/en/page.html":    "en",
		"site/fr/page.html":    "fr",
		"site/README":          "readme",
		"_alt/a.go":            goSource("a"),
		"_de/other.html":       "de",
		"_internal/a/a.go":     goSource("a"),
	}
	for rel, content := range files {
		writeFile(t, root, rel, content)
	}
	symlink(t, root, "main.go", "src/main.txt")
	symlink(t, root, "cmd/tool/tool.go", "../../_impl/tool.go")
	symlink(t, root, "internal/a/a.go", "../../_impl/a.go")
	symlink(t, root, "internal/skip/s.go", "../../_impl/s.go")
}
