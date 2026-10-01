package pkgmeta

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadGoModuleMetadata_ParseErrorAndReadError drives readJSON's two error
// legs (read error, parse error) via the public reader.
func TestReadGoModuleMetadata_ParseErrorAndReadError(t *testing.T) {
	wsRoot := t.TempDir()
	projectPath := "services/api"

	// Missing file → ReadFile error leg.
	if _, err := ReadGoModuleMetadata(wsRoot, projectPath); err == nil {
		t.Fatal("expected read error for missing module.json, got nil")
	}

	// Present but malformed → json.Unmarshal error leg.
	goDir := PackageOutputDir(wsRoot, projectPath, "go")
	if err := os.MkdirAll(goDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "module.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoModuleMetadata(wsRoot, projectPath); err == nil {
		t.Fatal("expected parse error for malformed module.json, got nil")
	}
}

// TestReadPackageMetadata_ParsesValid exercises the success path of readJSON so
// the non-error return is covered too.
func TestReadPackageMetadata_ParsesValid(t *testing.T) {
	wsRoot := t.TempDir()
	projectPath := "services/api"
	pkgDir := filepath.Join(PackageOutputDir(wsRoot, projectPath, "x"), "..")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetadataPath(wsRoot, projectPath),
		[]byte(`{"version":"1.0.0","artifact":"api","channels":["go"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadPackageMetadata(wsRoot, projectPath)
	if err != nil {
		t.Fatalf("ReadPackageMetadata: %v", err)
	}
	if meta.Version != "1.0.0" || meta.Artifact != "api" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if !meta.HasChannel("go") {
		t.Fatalf("expected channel go, got %+v", meta.Channels)
	}
}

// TestReadDockerManifest_Malformed exercises ReadDockerManifest's parse-error
// leg, completing readJSON coverage across all three readers.
func TestReadDockerManifest_Malformed(t *testing.T) {
	wsRoot := t.TempDir()
	projectPath := "services/api"
	dockerDir := PackageOutputDir(wsRoot, projectPath, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDockerManifest(wsRoot, projectPath); err == nil {
		t.Fatal("expected parse error for malformed manifest.json, got nil")
	}
}

// TestReadDockerManifest_ParsesValid covers ReadDockerManifest's success leg.
func TestReadDockerManifest_ParsesValid(t *testing.T) {
	wsRoot := t.TempDir()
	projectPath := "services/api"
	dockerDir := PackageOutputDir(wsRoot, projectPath, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"),
		[]byte(`{"image":"app","version":"1.0.0","contentHash":"abc","digest":"sha256:dead","layout":"oci"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadDockerManifest(wsRoot, projectPath)
	if err != nil {
		t.Fatalf("ReadDockerManifest: %v", err)
	}
	if m.Image != "app" || m.Digest != "sha256:dead" || m.Layout != "oci" {
		t.Fatalf("unexpected docker manifest: %+v", m)
	}
}

// TestReadGoModuleMetadata_ParsesValid covers ReadGoModuleMetadata's success leg.
func TestReadGoModuleMetadata_ParsesValid(t *testing.T) {
	wsRoot := t.TempDir()
	projectPath := "services/api"
	goDir := PackageOutputDir(wsRoot, projectPath, "go")
	if err := os.MkdirAll(goDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "module.json"),
		[]byte(`{"modulePath":"go.putnami.dev/x","version":"v1.2.3","sourceDir":"/src"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadGoModuleMetadata(wsRoot, projectPath)
	if err != nil {
		t.Fatalf("ReadGoModuleMetadata: %v", err)
	}
	if m.ModulePath != "go.putnami.dev/x" || m.Version != "v1.2.3" {
		t.Fatalf("unexpected go module metadata: %+v", m)
	}
}
