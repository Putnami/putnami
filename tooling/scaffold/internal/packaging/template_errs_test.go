package packaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// skipIfRoot skips fs-permission tests when running as root, since root
// bypasses the unix permission bits the tests rely on.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("test relies on unix permissions; root bypasses them")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test relies on unix permissions; Windows os.Chmod only sets a read-only attribute, which denies neither reading a file nor listing a directory")
	}
}

// TestTemplate_ManifestReadFailureFails drives the manifest ReadFile error leg:
// Stat succeeds but ReadFile fails because the manifest path is a directory.
func TestTemplate_ManifestReadFailureFails(t *testing.T) {
	tmp := t.TempDir()
	projectDir := filepath.Join(tmp, "mytemplate")
	// Create putnami.template.json as a *directory* — Stat passes, ReadFile fails.
	if err := os.MkdirAll(filepath.Join(projectDir, "putnami.template.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when manifest ReadFile fails", status)
	}
}

// setTempDir points os.TempDir at dir for the rest of the test. It reads TMPDIR
// on Unix, and TMP first on Windows.
func setTempDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", dir)
		return
	}
	t.Setenv("TMPDIR", dir)
}

// TestTemplate_MkdirTempFailureFails drives the os.MkdirTemp failure leg: the
// temp dir is a non-existent path, so the staging temp dir cannot be created.
func TestTemplate_MkdirTempFailureFails(t *testing.T) {
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	// Point the temp dir at a path that does not exist so MkdirTemp fails.
	setTempDir(t, filepath.Join(tmp, "no-such-tmp-dir"))

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when MkdirTemp fails", status)
	}
}

// TestTemplate_ReadDirFailureFails drives the ReadDir failure leg: Stat of the
// manifest succeeds (the project dir keeps the execute bit) but ReadDir fails
// (read bit removed).
func TestTemplate_ReadDirFailureFails(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	projectDir := writeTemplateProject(t, tmp, validManifest)

	// 0o111: traverse allowed (Stat manifest OK) but read denied (ReadDir fails).
	if err := os.Chmod(projectDir, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when ReadDir fails", status)
	}
}

// TestTemplate_FileReadFailureFails drives the staged-file ReadFile error leg: a
// staged regular file is unreadable so os.ReadFile fails during the staging loop.
func TestTemplate_FileReadFailureFails(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	projectDir := filepath.Join(tmp, "mytemplate")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "putnami.template.json"), []byte(validManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unreadable regular file (no subdir, so no cp; staging loop reads it).
	secret := filepath.Join(projectDir, "secret.template")
	if err := os.WriteFile(secret, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when file ReadFile fails", status)
	}
}

// TestTemplate_OutputDirMkdirFailureFails drives the output-dir MkdirAll failure
// leg: an ancestor ("package") is a regular file so MkdirAll(archives) fails.
func TestTemplate_OutputDirMkdirFailureFails(t *testing.T) {
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	// outputDir = <ws>/.putnami/out/mytemplate/package/archives.
	// Pre-create "package" as a regular file so MkdirAll(archives) fails.
	pkgParent := filepath.Join(tmp, ".putnami", "out", "mytemplate")
	if err := os.MkdirAll(pkgParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgParent, "package"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when output MkdirAll fails", status)
	}
}

// TestTemplate_MetadataWriteFailureFails drives the metadata write failure leg:
// staging, archive creation, outputDir MkdirAll and the channel record inside
// the writable archives/ directory all succeed, but the archive publication
// manifest cannot be written because its directory ("package") is read-only.
func TestTemplate_MetadataWriteFailureFails(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	// Pre-create the output dir so tar can write the archive and the channel
	// record into it, then make its parent ("package") read-only so only the
	// metadata.json write fails.
	outputDir := filepath.Join(tmp, ".putnami", "out", "mytemplate", "package", "archives")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Dir(outputDir)
	if err := os.Chmod(pkgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pkgDir, 0o755) })

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when metadata WriteFile fails", status)
	}
}

// TestTemplate_RefusesANameThatLeavesTheArchivesDirectory pins that a
// manifest name which still is not one file name after encoding fails the job
// before anything is written, instead of joining a path that leaves the
// archives directory the task owns.
func TestTemplate_RefusesANameThatLeavesTheArchivesDirectory(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "manifest-selects-the-template", "a-name-that-is-not-one-file-name-fails-the-job")
	for _, name := range []string{"../escape", "../../../../escape", "a/b", "/abs", "..", ".", ""} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			manifest, err := json.Marshal(map[string]string{"name": name, "version": "0.0.0"})
			if err != nil {
				t.Fatal(err)
			}
			writeTemplateProject(t, tmp, string(manifest))
			ctx := newCtx(tmp)

			status, _, err := Template(ctx, jsonl.New(), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != "FAILED" {
				t.Fatalf("status = %q, want FAILED for template name %q", status, name)
			}
			archives := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
			for _, escaped := range []string{
				filepath.Join(filepath.Dir(archives), "escape-1.2.3.tar.gz"),
				filepath.Join(tmp, "escape-1.2.3.tar.gz"),
			} {
				if _, err := os.Stat(escaped); !os.IsNotExist(err) {
					t.Fatalf("an archive was written outside archives/ at %s (stat error %v)", escaped, err)
				}
			}
			if _, err := os.Stat(filepath.Join(tmp, ".putnami")); !os.IsNotExist(err) {
				t.Fatalf("a refused name still wrote packaging output (stat error %v)", err)
			}
		})
	}
}

// TestTemplate_EncodesAScopedName pins that a scoped template name is archived
// and published under the same encoded name the content-only extension
// packager gives its archives, inside the archives directory, rather than in
// an @scope/ subdirectory nobody created.
func TestTemplate_EncodesAScopedName(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "manifest-selects-the-template", "the-archive-name-encodes-a-scoped-template-name")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, `{"name":"@acme/svc","version":"0.0.0"}`)
	ctx := newCtx(tmp)

	status, result, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK for a scoped template name", status)
	}
	encoded := encodeArtifactName("@acme/svc")
	if encoded != "acme-svc" {
		t.Fatalf("encodeArtifactName(@acme/svc) = %q, want acme-svc", encoded)
	}
	archives := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	wantArchive := filepath.Join(archives, encoded+"-1.2.3.tar.gz")
	if _, err := os.Stat(wantArchive); err != nil {
		t.Fatalf("archive not found at %s: %v", wantArchive, err)
	}
	if result["archive"] != wantArchive {
		t.Fatalf("result archive = %v, want %v", result["archive"], wantArchive)
	}
	if result["name"] != "@acme/svc" {
		t.Fatalf("result name = %v, want the manifest name @acme/svc", result["name"])
	}
	meta, err := pkgmeta.ReadPackageMetadata(tmp, ctx.Project.Path)
	if err != nil {
		t.Fatalf("read archive publication manifest: %v", err)
	}
	if meta.Artifact != encoded {
		t.Fatalf("published artifact = %q, want %q", meta.Artifact, encoded)
	}
}
