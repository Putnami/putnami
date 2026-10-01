package packaging

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// TestTemplate_ArchiveFailureFails exercises the archive-creation failure
// branch: a directory already sits at the archive path, so the archive cannot
// be created.
func TestTemplate_ArchiveFailureFails(t *testing.T) {
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)
	ctx := newCtx(tmp)
	archivePath := filepath.Join(pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives"), "my-template-1.2.3.tar.gz")
	if err := os.MkdirAll(archivePath, 0o755); err != nil {
		t.Fatal(err)
	}

	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when the archive cannot be created", status)
	}
}

// TestTemplate_RefusesALink pins that a template carrying a directory link, a
// symbolic link on Unix and a junction on Windows, fails the job instead of
// shipping an archive every host must extract and a Windows CLI refuses.
func TestTemplate_RefusesALink(t *testing.T) {
	tmp := t.TempDir()
	projectDir := writeTemplateProject(t, tmp, validManifest)
	if err := os.MkdirAll(filepath.Join(projectDir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(filepath.Join(projectDir, "src"), filepath.Join(projectDir, "config", "current")); err != nil {
		t.Fatalf("create the link: %v", err)
	}
	ctx := newCtx(tmp)

	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED for a template that carries a link", status)
	}
	archives := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	if _, err := os.Stat(filepath.Join(archives, "my-template-1.2.3.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("an archive was written for a template that carries a link (stat error %v)", err)
	}
}
