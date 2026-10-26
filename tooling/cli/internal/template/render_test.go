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

// TestRenderDirLeavesOutWhatTheArchiveLeavesOut pins that a template rendered
// from its source directory, as projects create renders a workspace template,
// creates the project its archive creates. The template project's own
// configuration, feature, specs and baseline, its installed state and its
// root dotfiles stay behind; the same names one level down are content.
func TestRenderDirLeavesOutWhatTheArchiveLeavesOut(t *testing.T) {
	src := t.TempDir()
	for rel, content := range map[string]string{
		ManifestFilename:          `{"name":"probe"}`,
		"putnami.json":            `{"name":"probe","type":"template"}`,
		"putnami.json.template":   `{"name":"<%= projectName %>"}`,
		"putnami.features.json":   `{"protocolVersion":2,"namespace":"tooling","features":[]}`,
		"specs/probe.json":        `{"protocolVersion":1,"feature":"tooling/probe"}`,
		"specs.baseline.json":     `{"protocolVersion":2,"executableRequirements":[]}`,
		"node_modules/x/index.js": "",
		".gitignore":              "node_modules\n",
		"src/specs/fixture.json":  "{}\n",
		"src/.gitkeep":            "",
	} {
		path := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "api")
	if err := RenderDir(src, dst, RenderVars{ProjectName: "api", ProjectModule: "api"}); err != nil {
		t.Fatalf("RenderDir: %v", err)
	}

	for _, absent := range []string{"putnami.features.json", "specs", "specs.baseline.json", "node_modules", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dst, absent)); !os.IsNotExist(err) {
			t.Errorf("rendered project holds %s, which the archive leaves out (stat error %v)", absent, err)
		}
	}
	for rel, want := range map[string]string{
		"putnami.json":           `{"name":"api"}`,
		"src/specs/fixture.json": "{}\n",
		"src/.gitkeep":           "",
	} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("rendered %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("rendered %s = %q, want %q", rel, got, want)
		}
	}
}
