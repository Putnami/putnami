package extension

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// currentPlatform is the "os/arch" key the extension installer records for the
// host running the tests.
func currentPlatform() string {
	return lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
}

func TestExtractTarGz(t *testing.T) {
	// Create a test tar.gz archive
	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")

	createTestArchive(t, archivePath, map[string]string{
		"putnami.extension.json": `{"commands": {}}`,
		"bin/build":              "#!/bin/sh\necho build",
		"templates/lib/main.go":  "package main",
	})

	if err := ExtractTarGz(archivePath, destDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}

	// Verify files exist
	assertFileExists(t, filepath.Join(destDir, "putnami.extension.json"))
	assertFileExists(t, filepath.Join(destDir, "bin", "build"))
	assertFileExists(t, filepath.Join(destDir, "templates", "lib", "main.go"))

	// Verify content
	data, _ := os.ReadFile(filepath.Join(destDir, "putnami.extension.json"))
	if string(data) != `{"commands": {}}` {
		t.Errorf("unexpected content: %s", data)
	}
}

func TestExtractTarGzContextCanceledBeforeWrite(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	createTestArchive(t, archivePath, map[string]string{"AI.md": "must not be written"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ExtractTarGzContext(ctx, archivePath, destDir); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractTarGzContext error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "AI.md")); !os.IsNotExist(err) {
		t.Fatalf("canceled extraction wrote AI.md: %v", err)
	}
}

// An entry with an unsafe name fails the extraction and names its entry on
// every platform: skipping it installed a tree without the file and reported
// success. A backslash counts as a separator and a drive prefix is refused
// even on Unix, so one archive is accepted or refused identically everywhere.
func TestExtractTarGzRefusesUnsafeNames(t *testing.T) {
	cases := []struct{ name, why string }{
		{"../../../etc/passwd", "has a .. component"},
		{"safe/../../escape", "has a .. component"},
		{"safe/../inside", "has a .. component"},
		{`..\..\evil`, "has a .. component"},
		{`safe\..\..\evil`, "has a .. component"},
		{"/etc/shadow", "is an absolute or UNC path"},
		{"//server/share/evil", "is an absolute or UNC path"},
		{`\\server\share\evil`, "is an absolute or UNC path"},
		{`\evil`, "is an absolute or UNC path"},
		{`C:\Windows\evil`, "starts with a Windows drive"},
		{"c:evil", "starts with a Windows drive"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct{ name, why string }{"bin/NUL", "is not a local path on this platform"},
			struct{ name, why string }{"file:stream", "is not a local path on this platform"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outside := t.TempDir()
			destDir := filepath.Join(outside, "a", "b", "extracted")
			archivePath := filepath.Join(t.TempDir(), "unsafe.tar.gz")
			createLinkTestArchive(t, archivePath, [][2]string{{"safe/file.txt", "safe"}, {tc.name, "evil"}}, nil)

			err := ExtractTarGz(archivePath, destDir)
			if err == nil || !strings.Contains(err.Error(), "archive entry "+tc.name+" "+tc.why) {
				t.Fatalf("ExtractTarGz error = %v, want a refusal of %s that says it %s", err, tc.name, tc.why)
			}
			walkErr := filepath.WalkDir(outside, func(path string, _ os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(outside, path)
				switch {
				case rel == "." || rel == "a" || rel == filepath.Join("a", "b"):
				case rel == filepath.Join("a", "b", "extracted") ||
					strings.HasPrefix(rel, filepath.Join("a", "b", "extracted")+string(filepath.Separator)):
					if data, _ := os.ReadFile(path); string(data) == "evil" {
						t.Errorf("the refused entry was written at %s", path)
					}
				default:
					t.Errorf("extraction wrote %s outside its directory", path)
				}
				return nil
			})
			if walkErr != nil {
				t.Fatal(walkErr)
			}
		})
	}
}

// Names that only look unsafe extract: a "./" prefix, and a component that
// starts with ".." without being "..".
func TestExtractTarGzAcceptsDotPrefixedNames(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "dots.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	createLinkTestArchive(t, archivePath, [][2]string{
		{"./bin/tool", "tool"},
		{"..config", "config"},
		{"docs/..notes/a..b", "notes"},
	}, nil)

	if err := ExtractTarGz(archivePath, destDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	assertFileExists(t, filepath.Join(destDir, "bin", "tool"))
	assertFileExists(t, filepath.Join(destDir, "..config"))
	assertFileExists(t, filepath.Join(destDir, "docs", "..notes", "a..b"))
}

func TestExtractTarGzSkipsMacOSFiles(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "macos.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")

	createTestArchiveWithPaths(t, archivePath, map[string]string{
		"._putnami.extension.json": "\x00\x05\x16\x07", // binary resource fork
		".DS_Store":                "\x00\x00\x00\x01",
		"bin/._build":              "\x00\x05\x16\x07",
		"putnami.extension.json":   `{"commands": {}}`,
		"bin/build":                "#!/bin/sh\necho build",
	})

	if err := ExtractTarGz(archivePath, destDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}

	// Real files should exist
	assertFileExists(t, filepath.Join(destDir, "putnami.extension.json"))
	assertFileExists(t, filepath.Join(destDir, "bin", "build"))

	// macOS metadata files should NOT exist
	if _, err := os.Stat(filepath.Join(destDir, "._putnami.extension.json")); !os.IsNotExist(err) {
		t.Error("._putnami.extension.json should have been skipped")
	}
	if _, err := os.Stat(filepath.Join(destDir, ".DS_Store")); !os.IsNotExist(err) {
		t.Error(".DS_Store should have been skipped")
	}
	if _, err := os.Stat(filepath.Join(destDir, "bin", "._build")); !os.IsNotExist(err) {
		t.Error("bin/._build should have been skipped")
	}
}

func TestInstallerRemove(t *testing.T) {
	dir := t.TempDir()
	inst := &Installer{WorkspaceRoot: dir}

	// Create artifact in new layout
	artifactDir := layout.ArtifactDir(dir, layout.Extensions, "@putnami/go", "1.0.0")
	os.MkdirAll(artifactDir, 0o755)
	os.WriteFile(filepath.Join(artifactDir, "putnami.extension.json"), []byte("{}"), 0o644)
	layout.LinkArtifact(dir, layout.Extensions, "@putnami/go", "1.0.0")

	if err := inst.Remove("@putnami/go", "1.0.0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Remove unlinks only: the stable symlink is gone...
	if _, err := os.Lstat(layout.StableDir(dir, layout.Extensions, "@putnami/go")); !os.IsNotExist(err) {
		t.Error("symlink should have been removed")
	}
	// ...but the artifact bytes survive. In the machine-global store they are
	// shared across worktrees and repos, so reclaiming them is the artifact GC's
	// job, never remove's.
	if _, err := os.Stat(artifactDir); err != nil {
		t.Errorf("remove must not delete the artifact bytes: %v", err)
	}
}

func TestInstallFallbackToManifestVersion(t *testing.T) {
	dir := t.TempDir()
	inst := &Installer{WorkspaceRoot: dir}

	// Simulate what happens when download returns 0.0.0 but the manifest has a version:
	// create the 0.0.0 install directory with a versioned manifest
	fallbackDir := layout.ArtifactDir(dir, layout.Extensions, "@putnami/go", "0.0.0")
	os.MkdirAll(fallbackDir, 0o755)
	os.WriteFile(
		filepath.Join(fallbackDir, "putnami.extension.json"),
		[]byte(`{"name":"@putnami/go","version":"2.5.0","commands":{}}`),
		0o644,
	)

	// Read manifest and relocate (mirrors the logic in Install)
	manifestPath := filepath.Join(fallbackDir, "putnami.extension.json")
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Version != "2.5.0" {
		t.Fatalf("manifest version = %q, want %q", m.Version, "2.5.0")
	}

	correctDir := layout.ArtifactDir(dir, layout.Extensions, "@putnami/go", m.Version)
	os.RemoveAll(correctDir)
	if err := os.Rename(fallbackDir, correctDir); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Verify: 0.0.0 directory should be gone, 2.5.0 directory should exist
	if _, err := os.Stat(fallbackDir); !os.IsNotExist(err) {
		t.Error("0.0.0 directory should have been removed")
	}
	assertFileExists(t, filepath.Join(correctDir, "putnami.extension.json"))

	_ = inst // used for consistency
}

// An escaping link fails the extraction and names its entry: skipping it
// installed a tree without the file and reported success.
func TestExtractTarGzBlocksSymlinkTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "symlink.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")

	// Create archive with a symlink pointing outside destDir
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Safe file
	safeContent := []byte("safe")
	tw.WriteHeader(&tar.Header{Name: "safe.txt", Mode: 0o644, Size: int64(len(safeContent))})
	tw.Write(safeContent)

	// Malicious symlink pointing outside destDir
	tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "../../../etc/passwd",
	})

	tw.Close()
	gw.Close()
	f.Close()

	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "evil-link", "leaves the extracted tree")

	// Safe file should exist
	assertFileExists(t, filepath.Join(destDir, "safe.txt"))

	// Symlink should NOT have been created (points outside destDir)
	if _, err := os.Lstat(filepath.Join(destDir, "evil-link")); !os.IsNotExist(err) {
		t.Error("symlink traversal should have been blocked")
	}
}

func TestExtractTarGzBlocksSymlinkSiblingEscape(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "sibling.tar.gz")
	// Use a dest dir whose name is a prefix of a sibling directory
	destDir := filepath.Join(tmpDir, "ext")
	siblingDir := filepath.Join(tmpDir, "ext-escape")
	os.MkdirAll(destDir, 0o755)
	os.MkdirAll(siblingDir, 0o755)

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Safe file
	safeContent := []byte("safe")
	tw.WriteHeader(&tar.Header{Name: "safe.txt", Mode: 0o644, Size: int64(len(safeContent))})
	tw.Write(safeContent)

	// Symlink that resolves to a sibling path (../ext-escape) which has the
	// same prefix as destDir (/tmp/.../ext) — this must be blocked.
	tw.WriteHeader(&tar.Header{
		Name:     "sneaky-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "../ext-escape",
	})

	tw.Close()
	gw.Close()
	f.Close()

	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "sneaky-link", "leaves the extracted tree")

	assertFileExists(t, filepath.Join(destDir, "safe.txt"))

	// The sibling-escape symlink must NOT have been created
	if _, err := os.Lstat(filepath.Join(destDir, "sneaky-link")); !os.IsNotExist(err) {
		t.Error("symlink to sibling path (prefix escape) should have been blocked")
	}
}

// TestExtractTarGzBlocksAbsoluteSymlink covers the absolute-linkname case:
// a symlink entry whose target is an absolute path must not be
// created, because os.Symlink would otherwise write the raw absolute target
// and a subsequent regular-file entry could be written through it.
func TestExtractTarGzBlocksAbsoluteSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "abs-symlink.tar.gz")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pick a sentinel target inside tmpDir so we don't risk touching the
	// real filesystem if the test ever regresses.
	outsideDir := filepath.Join(tmpDir, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Symlink with an absolute target — must be rejected.
	tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: outsideDir,
	})

	// Following entry tries to write through the symlink to escape destDir.
	content := []byte("pwned")
	tw.WriteHeader(&tar.Header{Name: "evil-link/pwned", Mode: 0o644, Size: int64(len(content))})
	tw.Write(content)

	tw.Close()
	gw.Close()
	f.Close()

	// Extraction fails at the link and names it, so the follow-up write
	// never runs; nothing may have been written outside destDir.
	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "evil-link", "leaves the extracted tree")

	// "evil-link" may exist inside destDir as a real directory (created on
	// demand for the follow-up `evil-link/pwned` write), but must not be a
	// symlink — that is the original vulnerability.
	if info, err := os.Lstat(filepath.Join(destDir, "evil-link")); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			t.Error("absolute-linkname symlink should not have been created")
		}
	}
	// The escape target must not have been written through the planted symlink.
	if _, err := os.Stat(filepath.Join(outsideDir, "pwned")); !os.IsNotExist(err) {
		t.Error("file write through absolute symlink leaked outside destDir")
	}
}

// TestExtractTarGzBlocksSymlinkThenWriteThrough covers the
// symlink-then-write-through case: even if a symlink were somehow
// created pointing outside destDir, a later regular-file entry whose path
// traverses that symlink must not write outside destDir. Root.OpenFile
// enforces this regardless of how the symlink was planted.
func TestExtractTarGzBlocksSymlinkThenWriteThrough(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "follow.tar.gz")
	destDir := filepath.Join(tmpDir, "extracted")
	outsideDir := filepath.Join(tmpDir, "outside")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Plant a symlink inside destDir that points outside, simulating a
	// pre-existing or smuggled escape link.
	if err := os.Symlink(outsideDir, filepath.Join(destDir, "smuggled")); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	content := []byte("pwned")
	tw.WriteHeader(&tar.Header{Name: "smuggled/pwned", Mode: 0o644, Size: int64(len(content))})
	tw.Write(content)

	tw.Close()
	gw.Close()
	f.Close()

	// Extraction may fail because Root.OpenFile refuses to traverse the
	// escaping symlink. What matters is that no file is written outside.
	_ = ExtractTarGz(archivePath, destDir)

	if _, err := os.Stat(filepath.Join(outsideDir, "pwned")); !os.IsNotExist(err) {
		t.Error("file write through pre-planted symlink leaked outside destDir")
	}
}

func TestExtractTarGzRejectsOversizedFile(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "big.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Write a header claiming the file is 200MB (exceeds 100MB limit)
	// We only write a small body — the check should reject based on hdr.Size
	tw.WriteHeader(&tar.Header{
		Name: "huge.bin",
		Mode: 0o644,
		Size: 200 * 1024 * 1024,
	})
	// Write minimal data (tar will pad the rest)
	tw.Write([]byte("x"))

	tw.Close()
	gw.Close()
	f.Close()

	err = ExtractTarGz(archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestInstall_FailsClosedWithoutIntegrity verifies that when the user has
// neither a lockfile entry nor a resolver-advertised integrity hash, the
// installer refuses to proceed. Setting PUTNAMI_UNSAFE_INSTALL=1
// is the documented escape hatch.
func TestInstall_FailsClosedWithoutIntegrity(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		// Intentionally no X-Integrity / Digest header.
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}

	_, err := inst.Install(context.Background(), "@putnami/test", "latest", nil)
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "no integrity available") {
		t.Errorf("error should mention missing integrity, got: %v", err)
	}
}

// TestInstall_AcceptsAdvertisedIntegrity verifies the resolver-advertised
// path: a first install with no lockfile entry succeeds when the resolver
// emits X-Integrity and the bytes match.
func TestInstall_AcceptsAdvertisedIntegrity(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	expectedHash := sha256Bytes(archiveBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Header().Set("X-Integrity", "sha256:"+expectedHash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}

	result, err := inst.Install(context.Background(), "@putnami/test", "latest", nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", result.Version, "1.2.3")
	}
	if result.Integrity != expectedHash {
		t.Errorf("Integrity = %q, want %q", result.Integrity, expectedHash)
	}
}

// TestInstall_RejectsMismatchedAdvertisedIntegrity verifies that a lying
// resolver (advertised hash doesn't match the bytes) is rejected.
func TestInstall_RejectsMismatchedAdvertisedIntegrity(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Header().Set("X-Integrity", "sha256:"+strings.Repeat("a", 64))
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}

	_, err := inst.Install(context.Background(), "@putnami/test", "latest", nil)
	if err == nil {
		t.Fatal("expected integrity-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("error should mention integrity mismatch, got: %v", err)
	}
}

// TestInstall_UnsafeEnvEscapeHatch verifies PUTNAMI_UNSAFE_INSTALL=1
// allows installs without any integrity verification (the legacy behavior),
// for the resolver-upgrade transition period.
func TestInstall_UnsafeEnvEscapeHatch(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "1")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}

	if _, err := inst.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("Install with %s=1: %v", UnsafeInstallEnv, err)
	}
}

// TestInstall_LockfileEntryStillVerified verifies that when a lockfile
// entry already pins integrity, the existing behavior is preserved — the
// resolver-advertised integrity path is not consulted, and a mismatch
// against the lockfile is still rejected.
func TestInstall_LockfileEntryStillVerified(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}

	lockEntry := &lockfile.LockEntry{
		Version:   "1.2.3",
		Integrity: strings.Repeat("a", 64), // wrong hash
	}
	_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err == nil {
		t.Fatal("expected integrity mismatch against lockfile, got nil")
	}
	if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("error should mention integrity mismatch, got: %v", err)
	}
}

// TestInstall_CrossPlatformManifestOnlyStaysPerWorktree covers the
// cross-platform manifest binding under the machine-global store: a lock with a
// manifest hash but no digest for THIS os/arch (and a resolver that advertises
// none) still installs — the manifest binding secures it — but because the
// binary bytes were never hash-verified, the install stays PER-WORKTREE and the
// platform's digest is deliberately NOT recorded. Admitting these unverified
// bytes to the shared, cross-repo store, or laundering the digest into the lock
// so a later run shares them, would let one poisoned mirror infect every repo.
func TestInstall_CrossPlatformManifestOnlyStaysPerWorktree(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	artifactDir := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3")))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		// No X-Integrity: only the manifest hash can bind this install.
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	wsRoot := t.TempDir()
	inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: manifestHash,
		// Digest recorded only for a foreign platform — never this host.
		Integrities: map[string]string{"plan9/abc": strings.Repeat("b", 64)},
	}
	result, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("cross-platform install should verify via manifest hash, got: %v", err)
	}
	// The unverified bytes must NOT be pinned for this platform...
	if got := result.Integrities[currentPlatform()]; got != "" {
		t.Errorf("unverified cross-platform install must not record a digest, got %q", got)
	}
	if result.Integrity != "" {
		t.Errorf("unverified install must not record a legacy digest, got %q", result.Integrity)
	}
	// ...nothing may enter the shared store...
	if entries, _ := os.ReadDir(filepath.Join(artifactDir, "sha256")); len(entries) != 0 {
		t.Errorf("unverified install must not enter the shared store, found %d entries", len(entries))
	}
	// ...and the bytes land per-worktree instead.
	if _, err := os.Stat(layout.ArtifactDir(wsRoot, layout.Extensions, "@putnami/test", "1.2.3")); err != nil {
		t.Errorf("unverified install should land per-worktree: %v", err)
	}
}

// TestInstall_CrossPlatformLockRejectsTamperedManifest verifies the manifest
// binding still rejects a substituted archive whose manifest differs from the
// locked one, with a remediation hint.
func TestInstall_CrossPlatformLockRejectsTamperedManifest(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: strings.Repeat("a", 64), // does not match the archive's manifest
	}
	_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err == nil {
		t.Fatal("expected manifest integrity mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "manifest integrity mismatch") {
		t.Errorf("error should mention manifest integrity mismatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "update") {
		t.Errorf("error should carry a remediation hint, got: %v", err)
	}
}

// TestInstall_PerPlatformDigestStrict verifies that once this platform's digest
// is recorded, a mismatch is rejected with a hint (the strict steady state).
func TestInstall_PerPlatformDigestStrict(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3")))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{currentPlatform(): strings.Repeat("c", 64)}, // wrong for this host
	}
	_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err == nil {
		t.Fatal("expected per-platform integrity mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("error should mention integrity mismatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "putnami extensions update") {
		t.Errorf("error should carry a remediation hint, got: %v", err)
	}
}

// TestInstall_LegacyDigestMatchMigrates verifies a legacy single-digest lock
// that matches this platform's archive is accepted and migrated into the
// per-platform map.
func TestInstall_LegacyDigestMatchMigrates(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{Version: "1.2.3", Integrity: archiveHash} // legacy, matches host
	result, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("legacy matching digest should be accepted, got: %v", err)
	}
	if got := result.Integrities[currentPlatform()]; got != archiveHash {
		t.Errorf("legacy install should migrate into the per-platform map: got %q, want %q", got, archiveHash)
	}
}

// TestInstall_LockFirstPinsVersion verifies the installer fetches the locked
// version from a version-pinned channel instead of re-resolving "latest" — the
// behavior that broke fresh installs once latest moved ahead of the lock.
func TestInstall_LockFirstPinsVersion(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	const lockedVersion = "1.0.0"
	pinned := buildExtensionArchive(t, "@putnami/test", lockedVersion)
	latest := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	pinnedManifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", lockedVersion)))

	var gotChannel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotChannel = r.URL.Query().Get("channel")
		if gotChannel == lockedVersion {
			w.Header().Set("X-Resolved-Version", lockedVersion)
			w.Write(pinned)
			return
		}
		// "latest" (or anything else) resolves to a newer build.
		w.Header().Set("X-Resolved-Version", "2.0.0")
		w.Write(latest)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      lockedVersion,
		ManifestHash: pinnedManifestHash,
		Integrities:  map[string]string{currentPlatform(): sha256Bytes(pinned)},
	}
	// Workspace constraint is "latest", but the lock must win.
	result, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if gotChannel != lockedVersion {
		t.Errorf("installer requested channel %q, want the locked version %q", gotChannel, lockedVersion)
	}
	if result.Version != lockedVersion {
		t.Errorf("Version = %q, want locked %q (latest drifted to 2.0.0)", result.Version, lockedVersion)
	}
}

// TestInstall_FreshWorktreeZeroDownload is the linchpin: once one worktree has
// admitted a verified artifact to the machine-global store, a sibling worktree
// whose lock pins the same digest installs with NO download — just a symlink
// swap. The sibling's registry fails loudly if contacted.
func TestInstall_FreshWorktreeZeroDownload(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir()) // one shared store for both worktrees

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3")))

	// Worktree A: a normal verified install warms the shared store.
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Header().Set("X-Integrity", "sha256:"+archiveHash)
		w.Write(archiveBytes)
	}))
	defer srvA.Close()
	instA := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srvA.URL, HTTPClient: srvA.Client()}
	if _, err := instA.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("warm install: %v", err)
	}

	// Worktree B: lock pins the digest A admitted; its registry must never be hit.
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("sibling worktree must not contact the registry; the shared store should serve it")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srvB.Close()
	wsB := t.TempDir()
	instB := &Installer{WorkspaceRoot: wsB, ResolverURL: srvB.URL, HTTPClient: srvB.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{currentPlatform(): archiveHash},
	}
	result, err := instB.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("sibling install should hit the shared store, got: %v", err)
	}
	if !result.FromCache {
		t.Error("sibling install should report FromCache")
	}
	if !result.Changed {
		t.Error("first sibling install should report the stable-link restoration")
	}
	// B's stable symlink resolves into the shared store, not a per-worktree copy.
	target, err := os.Readlink(layout.StableDir(wsB, layout.Extensions, "@putnami/test"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if !strings.Contains(target, filepath.Join("sha256", archiveHash[:2], archiveHash)) {
		t.Errorf("symlink should point into the shared store, got %q", target)
	}

	result, err = instB.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("repeated cache install: %v", err)
	}
	if !result.FromCache || result.Changed {
		t.Errorf("repeated cache install = {FromCache:%v Changed:%v}, want a cache no-op", result.FromCache, result.Changed)
	}
}

func TestInstall_SharedAdmissionRejectsBadManifestWithoutPublishing(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	artifactDir := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)

	archiveBytes := buildArchiveBytes(t, map[string]string{"not-manifest.txt": "x"})
	archiveHash := sha256Bytes(archiveBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Header().Set("X-Integrity", "sha256:"+archiveHash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}
	_, err := inst.Install(context.Background(), "@putnami/test", "latest", nil)
	if err == nil {
		t.Fatal("expected missing-manifest error, got nil")
	}
	if !strings.Contains(err.Error(), "has no putnami.extension.json") {
		t.Fatalf("error = %v, want missing manifest", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(artifactDir, "sha256")); len(entries) != 0 {
		t.Fatalf("invalid artifact must not be published to shared store, found %d entries", len(entries))
	}
}

func TestInstall_InvalidLockDigestDoesNotPanic(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("invalid lock digest should return an error, not panic: %v", r)
		}
	}()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}
	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3"))),
		Integrities:  map[string]string{currentPlatform(): "bad"},
	}
	_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err == nil {
		t.Fatal("expected integrity mismatch for invalid lock digest, got nil")
	}
	if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("error = %v, want integrity mismatch", err)
	}
}

// TestInstall_UnsafeNotShared verifies an unverified (PUTNAMI_UNSAFE_INSTALL=1)
// install never enters the machine-global store: one repo's bypassed install
// must not satisfy another repo's Has() and silently serve unverified bytes.
func TestInstall_UnsafeNotShared(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "1")
	artifactDir := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	wsRoot := t.TempDir()
	inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := inst.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("unsafe install: %v", err)
	}

	if entries, _ := os.ReadDir(filepath.Join(artifactDir, "sha256")); len(entries) != 0 {
		t.Errorf("unsafe install must not enter the shared store, found %d entries", len(entries))
	}
	if _, err := os.Stat(layout.ArtifactDir(wsRoot, layout.Extensions, "@putnami/test", "1.2.3")); err != nil {
		t.Errorf("unsafe install should land per-worktree: %v", err)
	}
}

// extensionManifestJSON is the manifest content embedded in test archives. It
// is platform-independent, so its hash is the cross-platform binding the lock
// verifies against.
func extensionManifestJSON(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","commands":{}}`
}

// buildExtensionArchive produces a tar.gz containing a minimal valid
// putnami extension manifest so Install can pass post-extract validation.
func buildExtensionArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest := extensionManifestJSON(name, version)
	return buildArchiveBytes(t, map[string]string{"putnami.extension.json": manifest})
}

func buildArchiveBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Bytes(b []byte) string {
	tmp, err := os.CreateTemp("", "hash-*")
	if err != nil {
		panic(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		panic(err)
	}
	tmp.Close()
	h, err := HashFile(tmp.Name())
	if err != nil {
		panic(err)
	}
	return h
}

// --- helpers ---

func createTestArchive(t *testing.T, archivePath string, files map[string]string) {
	t.Helper()
	createTestArchiveWithPaths(t, archivePath, files)
}

func createTestArchiveWithPaths(t *testing.T, archivePath string, files map[string]string) {
	t.Helper()

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive file: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("file does not exist: %s", path)
	}
}

// TestManifestVersionProbe_AnswersForARefusedManifest pins the repair.
// The spec's ManifestVersion answers an IDENTITY question — what
// version is materialized on disk — and must keep answering for an artifact the
// contract ladder refuses to load. Probing through LoadManifest made a
// contract-2 pinned artifact report "", so lockedArtifactMatches mismatched,
// EnsureArtifactLocked unlinked the stable dir and reinstalled on every
// command, and discovery lost the skip record carrying the re-package remedy.
func TestManifestVersionProbe_AnswersForARefusedManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "putnami.extension.json")
	// Contract 2 with a command surface: exactly the shape LoadManifest rejects
	// since B6c — and exactly what a pre-flip published artifact looks like.
	refused := `{
		"name": "@putnami/cloud",
		"version": "0.0.0-e9388a298",
		"cliContract": 2,
		"commands": {"login": {"run": [{"id": "l", "task": "t"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`
	if err := os.WriteFile(manifestPath, []byte(refused), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(manifestPath); err == nil {
		t.Fatal("fixture must be below the current contract, or this test pins nothing")
	}

	if got := extensionSpec().ManifestVersion(manifestPath); got != "0.0.0-e9388a298" {
		t.Fatalf("ManifestVersion for a refused manifest = %q, want its recorded version — "+
			"an unanswerable identity probe is what wedged install on a contract-2 pin", got)
	}
	// Unreadable and unparsable still degrade to "": the probe reports what the
	// document says or nothing, never an invented value.
	if got := extensionSpec().ManifestVersion(filepath.Join(dir, "missing.json")); got != "" {
		t.Fatalf("missing manifest must probe as empty, got %q", got)
	}
}
