package packaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

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

// TestTemplate_OutputDirMkdirFailureFails drives the output-dir MkdirAll failure
// leg: an ancestor ("package") is a regular file so MkdirAll(archives) fails.
func TestTemplate_OutputDirMkdirFailureFails(t *testing.T) {
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	// outputDir = <ws>/.putnami/out/mytemplate/~package/archives.
	// Pre-create "~package" as a regular file so MkdirAll(archives) fails.
	pkgParent := filepath.Join(tmp, ".putnami", "out", "mytemplate")
	if err := os.MkdirAll(pkgParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgParent, "~package"), []byte("not a dir"), 0o644); err != nil {
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
	meta, err := readPackageMetadata(tmp, ctx.Project.Path)
	if err != nil {
		t.Fatalf("read archive publication manifest: %v", err)
	}
	if meta.Artifact != encoded {
		t.Fatalf("published artifact = %q, want %q", meta.Artifact, encoded)
	}
}
