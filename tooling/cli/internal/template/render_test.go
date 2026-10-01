package template

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// TestRenderDirFollowsTheDirectoryLinkATemplateIsInstalledAs pins how an
// installed template reaches RenderDir: .putnami/bin/templates/<name> is a
// directory link into the store, a symbolic link on Unix and a junction on
// Windows. filepath.EvalSymlinks does not follow a junction, so a walk that
// resolves the link with it reads the template directory as a file and fails
// with "Incorrect function" on Windows.
func TestRenderDirFollowsTheDirectoryLinkATemplateIsInstalledAs(t *testing.T) {
	store := t.TempDir()
	files := map[string]string{
		ManifestFilename:                       `{"name":"probe"}`,
		"main.go":                              "package main\n",
		"go.mod.template":                      "module <%= projectModule %>\n",
		filepath.Join("schema", ".gitkeep"):    "",
		filepath.Join("__module__", "doc.txt"): "nested\n",
	}
	for rel, content := range files {
		path := filepath.Join(store, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "templates", "probe")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(store, link); err != nil {
		t.Fatalf("create the template link: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "api")
	if err := RenderDir(link, dst, RenderVars{ProjectName: "api", ProjectModule: "example.com/api"}); err != nil {
		t.Fatalf("RenderDir through a directory link: %v", err)
	}

	for rel, want := range map[string]string{
		"main.go":                           "package main\n",
		"go.mod":                            "module example.com/api\n",
		filepath.Join("schema", ".gitkeep"): "",
		filepath.Join("example.com/api", "doc.txt"): "nested\n",
	} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("rendered %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("rendered %s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ManifestFilename)); !os.IsNotExist(err) {
		t.Errorf("the template manifest was copied into the project (stat error %v)", err)
	}
}

// UsesVariable finds a placeholder only where RenderDir substitutes it: in a
// .template file, nested or not, reached through the directory link an
// installed template is. A plain file is copied verbatim, so a placeholder in
// it does not count.
func TestUsesVariableFindsThePlaceholderRenderDirSubstitutes(t *testing.T) {
	write := func(t *testing.T, root string, files map[string]string) string {
		t.Helper()
		for rel, content := range files {
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(t.TempDir(), "templates", "probe")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := dirlink.Create(root, link); err != nil {
			t.Fatalf("create the template link: %v", err)
		}
		return link
	}
	placeholder := "<%= " + GoFrameworkVersionVariable + " %>"
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"a nested .template file", map[string]string{
			ManifestFilename:                        `{"name":"probe"}`,
			filepath.Join("cmd", "go.mod.template"): "require go.putnami.dev/app " + placeholder + "\n",
		}, true},
		{"a plain file only", map[string]string{
			ManifestFilename:  `{"name":"probe"}`,
			"README.md":       "pin " + placeholder + "\n",
			"go.mod.template": "module <%= projectModule %>\n",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := write(t, t.TempDir(), tc.files)
			got, err := UsesVariable(src, GoFrameworkVersionVariable)
			if err != nil {
				t.Fatalf("UsesVariable: %v", err)
			}
			if got != tc.want {
				t.Fatalf("UsesVariable = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := UsesVariable(filepath.Join(t.TempDir(), "missing"), GoFrameworkVersionVariable); err == nil {
		t.Fatal("UsesVariable on a missing template reported no error")
	}
}
