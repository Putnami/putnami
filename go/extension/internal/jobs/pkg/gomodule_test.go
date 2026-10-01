package pkg

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// A subdirectory containing its own go.mod is a separate Go module. It must
// not be staged into the parent module's source tree, otherwise Go's module
// zip validator rejects the published zip with errMisplacedModFile.
//
// Regression: go.putnami.dev/migration was published with migratecli/go.mod
// inside its zip, breaking go mod download for every downstream consumer.
func TestPrepareGoModuleSkipsNestedModules(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "parent.go"), "package parent\n")

	nested := filepath.Join(projectRoot, "nestedmod")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	mustWrite(t, filepath.Join(nested, "go.mod"), "module example.com/parent/nested\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(nested, "nested.go"), "package nested\n")

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "parent",
			Path:     "parent",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed")
	}

	stageDir := filepath.Join(outputRoot, "go", "source")

	if _, err := os.Stat(filepath.Join(stageDir, "nestedmod")); !os.IsNotExist(err) {
		t.Errorf("nested module directory was staged; want it skipped (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, "nestedmod", "go.mod")); !os.IsNotExist(err) {
		t.Error("nested go.mod was staged into the parent module")
	}

	zipPath := filepath.Join(outputRoot, "go", "example.com/parent@v1.2.3.zip")
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open module zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.Contains(f.Name, "nestedmod/") {
			t.Errorf("module zip contains nested-module entry %q; should have been skipped", f.Name)
		}
	}
}

// tools/versions.json is embedded by the extension's tools package, so it
// must ship in the module source and zip consumed by downstream users.
func TestPrepareGoModuleIncludesToolVersionsManifest(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "tools", "versions.go"), "package tools\n")
	mustWrite(t, filepath.Join(projectRoot, "tools", "versions.json"), `{"schemaVersion":1}`)
	mustWrite(t, filepath.Join(projectRoot, "tools", "unrelated.json"), `{}`)

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "parent",
			Path:     "parent",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed")
	}

	stageDir := filepath.Join(outputRoot, "go", "source")
	if _, err := os.Stat(filepath.Join(stageDir, "tools", "versions.json")); err != nil {
		t.Fatalf("staged module is missing tools/versions.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, "tools", "unrelated.json")); !os.IsNotExist(err) {
		t.Errorf("unrelated tools JSON was staged; want it excluded (err=%v)", err)
	}

	zipPath := filepath.Join(outputRoot, "go", "example.com/parent@v1.2.3.zip")
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open module zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "example.com/parent@v1.2.3/tools/versions.json" {
			return
		}
	}
	t.Fatal("module zip is missing tools/versions.json")
}

// validateModuleZip is the last-resort check on the artifact: even if a
// future regression slips a nested go.mod past the staging walker,
// every published zip must satisfy the same rules go mod download
// enforces. We craft an intentionally-malformed zip and assert the
// validator rejects it.
func TestValidateModuleZip_RejectsNestedGoMod(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "example.com/parent@v1.2.3.zip")
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	const prefix = "example.com/parent@v1.2.3/"
	for _, entry := range []struct{ path, body string }{
		{prefix + "go.mod", "module example.com/parent\n\ngo 1.24\n"},
		{prefix + "parent.go", "package parent\n"},
		{prefix + "nested/go.mod", "module example.com/parent/nested\n\ngo 1.24\n"},
		{prefix + "nested/nested.go", "package nested\n"},
	} {
		w, err := zw.Create(entry.path)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", entry.path, err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write zip entry %s: %v", entry.path, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	err = validateModuleZip(zipPath, "example.com/parent", "v1.2.3")
	if err == nil {
		t.Fatal("validateModuleZip accepted a zip with a nested go.mod; expected rejection")
	}
}

// A well-formed zip with no funny business should pass validation
// cleanly. Guards against false positives if we ever tighten the
// validator's rule set.
func TestValidateModuleZip_AcceptsWellFormedZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "example.com/parent@v1.2.3.zip")
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	const prefix = "example.com/parent@v1.2.3/"
	for _, entry := range []struct{ path, body string }{
		{prefix + "go.mod", "module example.com/parent\n\ngo 1.24\n"},
		{prefix + "parent.go", "package parent\n"},
	} {
		w, err := zw.Create(entry.path)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", entry.path, err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write zip entry %s: %v", entry.path, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if err := validateModuleZip(zipPath, "example.com/parent", "v1.2.3"); err != nil {
		t.Errorf("validateModuleZip rejected a well-formed zip: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
