package versioncmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

func TestVersionList_Empty(t *testing.T) {
	dir := t.TempDir()

	// Empty bin directory — should not error
	if err := VersionList(context.Background(), dir); err != nil {
		t.Errorf("VersionList on empty dir: %v", err)
	}
}

func TestVersionList_NoBinDir(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".putnami", "bin")
	// binDir does not exist — should not error
	if err := VersionList(context.Background(), binDir); err != nil {
		t.Errorf("VersionList on non-existent dir: %v", err)
	}
}

func TestVersionList_WithBinaries(t *testing.T) {
	dir := t.TempDir()

	// Create fake binaries with putnami- prefix
	for _, name := range []string{"putnami-go-1.0.0", "putnami-ts-2.0.0"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho fake"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Create a non-putnami file that should be ignored
	os.WriteFile(filepath.Join(dir, "other-binary"), []byte("x"), 0o755)

	// Symlink to one version
	os.Symlink("putnami-go-1.0.0", filepath.Join(dir, "putnami"))

	if err := VersionList(context.Background(), dir); err != nil {
		t.Errorf("VersionList: %v", err)
	}
}

func TestVersionUse_MissingName(t *testing.T) {
	dir := t.TempDir()
	err := VersionUse(context.Background(), dir, "", false)
	if err == nil {
		t.Error("VersionUse with empty name should return error")
	}
}

func TestVersionUse_NotFound(t *testing.T) {
	dir := t.TempDir()
	err := VersionUse(context.Background(), dir, "nonexistent", false)
	if err == nil {
		t.Error("VersionUse for non-existent binary should return error")
	}
}

// versionLineRepo is a workspace of two version lines in a real git
// repository: version is derived from git, so a fixture that is not a
// repository cannot exercise any of it.
func versionLineRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"}, {"checkout", "-b", "main"},
	} {
		gitInRepo(t, dir, args...)
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":     "test",
		"includes": []string{"tooling", "typescript"},
	})
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "tooling", wsproto.ConfigFilename), map[string]any{
		"line": map[string]any{}, "includes": []string{"cli"},
	})
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "tooling", "cli", wsproto.ConfigFilename), map[string]any{
		"name": "@putnami/cli",
	})
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "typescript", wsproto.ConfigFilename), map[string]any{
		"line": map[string]any{"tag": "ts/v{version}"}, "includes": []string{"web"},
	})
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "typescript", "web", wsproto.ConfigFilename), map[string]any{
		"name": "@putnami/web",
	})
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "feat: the workspace")
	return dir
}

func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeRepoFile(t *testing.T, dir, relPath, contents string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDownloadBinary_RejectsOversizedPayload verifies the response-size
// cap. A resolver that streams more than maxBinaryDownloadSize
// bytes must not consume unbounded local disk; the download must fail
// closed and the partial file must be cleaned up.
func TestDownloadBinary_RejectsOversizedPayload(t *testing.T) {
	// The LimitReader+1 check is size-agnostic, so lower the cap to keep the
	// test from streaming the production 500 MiB through the HTTP stack.
	origMax := maxBinaryDownloadSize
	maxBinaryDownloadSize = 4 * 1024 * 1024
	t.Cleanup(func() { maxBinaryDownloadSize = origMax })

	// Serve maxBinaryDownloadSize+1 bytes — enough to exceed the cap by one,
	// which is what the LimitReader+1 check uses to detect oversize.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write zero bytes in a tight loop to keep the test cheap; the
		// scheduler still sees > cap bytes overall.
		buf := make([]byte, 1024*1024) // 1 MiB chunks
		want := maxBinaryDownloadSize + 1
		for written := int64(0); written < want; {
			chunk := min(want-written, int64(len(buf)))
			n, err := w.Write(buf[:chunk])
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer srv.Close()

	_, err := downloadBinary(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should mention the size limit, got: %v", err)
	}
}

func TestExtractDownloadedBinary_RawBinaryPassesThrough(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "asset")
	want := []byte("\x7fELF...raw binary bytes")
	if err := os.WriteFile(asset, want, 0o644); err != nil {
		t.Fatal(err)
	}

	got, cleanup, err := extractDownloadedBinary(asset)
	if err != nil {
		t.Fatalf("extractDownloadedBinary: %v", err)
	}
	defer cleanup()

	if got != asset {
		t.Errorf("raw binary path = %q, want %q (passthrough)", got, asset)
	}
}

func TestExtractDownloadedBinary_TarGzExtractsBinary(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "asset.tar.gz")
	wantContent := []byte("fake-putnami-binary-payload")
	writeTarGz(t, asset, map[string][]byte{
		cliArchiveEntry:       wantContent,
		"framework-docs/x.md": []byte("# docs"),
	})

	got, cleanup, err := extractDownloadedBinary(asset)
	if err != nil {
		t.Fatalf("extractDownloadedBinary: %v", err)
	}
	defer cleanup()

	if want := hostExecutable("putnami"); filepath.Base(got) != want {
		t.Errorf("extracted path base = %q, want %q", filepath.Base(got), want)
	}
	gotContent, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Equal(gotContent, wantContent) {
		t.Errorf("extracted content mismatch:\n got %q\nwant %q", gotContent, wantContent)
	}
}

func TestExtractDownloadedBinary_TarGzMissingBinaryFails(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "asset.tar.gz")
	writeTarGz(t, asset, map[string][]byte{
		"framework-docs/x.md": []byte("# docs"),
	})

	_, cleanup, err := extractDownloadedBinary(asset)
	defer cleanup()
	if err == nil {
		t.Fatal("expected error when archive lacks putnami binary, got nil")
	}
}

// hostExecutable is the file name the CLI gives the executable name on this
// host: name itself, or name.exe on Windows.
func hostExecutable(name string) string {
	return pkgmeta.ExecutableName(runtime.GOOS, name)
}

// cliArchiveEntry is where a release archive for this host carries the CLI.
var cliArchiveEntry = "compiled/" + hostExecutable("putnami")

func writeTarGz(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeIntegrity(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"raw hex", hex64, hex64, false},
		{"sha256:", "sha256:" + hex64, hex64, false},
		{"sha-256:", "sha-256:" + hex64, hex64, false},
		{"sha256-", "sha256-" + hex64, hex64, false},
		{"uppercase normalized", strings.ToUpper(hex64), hex64, false},
		{"too short", "abc", "", true},
		{"non-hex chars", strings.Repeat("z", 64), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeIntegrity(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeIntegrity(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("normalizeIntegrity(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFetchLatestRelease_ReadsIntegrityHeaders(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name: "x-integrity preferred",
			headers: map[string]string{
				"X-Resolved-Version": "1.2.3",
				"X-Integrity":        "sha256:" + hex64,
				"Digest":             "sha-256=ffff",
			},
			want: "sha256:" + hex64,
		},
		{
			name: "digest fallback",
			headers: map[string]string{
				"X-Resolved-Version": "1.2.3",
				"Digest":             "sha-256=" + hex64,
			},
			want: hex64,
		},
		{
			name: "no integrity",
			headers: map[string]string{
				"X-Resolved-Version": "1.2.3",
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
			}))
			defer srv.Close()

			_, _, integrity, err := fetchLatestRelease(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("fetchLatestRelease: %v", err)
			}
			if integrity != tt.want {
				t.Errorf("integrity = %q, want %q", integrity, tt.want)
			}
		})
	}
}

// TestFetchRelease_SendsHead pins that the channel lookup reads only headers:
// it sends HEAD, so the registry sends no binary. downloadBinary fetches the
// binary itself when the CLI installs it.
func TestFetchRelease_SendsHead(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("X-Resolved-Version", "1.2.3")
	}))
	defer srv.Close()

	version, _, _, err := fetchRelease(context.Background(), srv.Client(), srv.URL, "canary")
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	if version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", version)
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Errorf("methods = %v, want [HEAD]", methods)
	}
}

func TestVersionUpdate_FailsClosedWithoutIntegrity(t *testing.T) {
	// Ensure no global env from another test leaks in.
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Resolver advertises a new version but no integrity hash.
		w.Header().Set("X-Resolved-Version", "9.9.9")
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	err := VersionUpdate(context.Background(), "1.0.0", t.TempDir())
	if err == nil {
		t.Fatal("expected fail-closed error when resolver lacks integrity, got nil")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("error should mention integrity, got: %v", err)
	}
}

func TestVersionUpdate_AcceptsValidIntegrity(t *testing.T) {
	// Build a tar.gz containing a fake putnami binary, compute its sha256,
	// serve it through a fake resolver that advertises that integrity, and
	// verify VersionUpdate installs the binary.
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	binDir := t.TempDir()
	stagingDir := t.TempDir()
	archivePath := filepath.Join(stagingDir, "release.tar.gz")
	writeTarGz(t, archivePath, map[string][]byte{
		cliArchiveEntry: []byte("fake-binary"),
	})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashFileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+hash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	if err := VersionUpdate(context.Background(), "1.0.0", binDir); err != nil {
		t.Fatalf("VersionUpdate: %v", err)
	}
	// The installed binary should exist.
	target := filepath.Join(binDir, hostExecutable("putnami-go-9.9.9"))
	if _, err := os.Stat(target); err != nil {
		t.Errorf("installed binary missing: %v", err)
	}
}

func TestVersionUpdate_RejectsStaleStamp(t *testing.T) {
	// A binary whose embedded --version disagrees with the channel-resolved
	// version must not be installed. This is the
	// publish/upgrade split: the channel resolved a new version, but
	// the downloaded archive still contains an older CLI.
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	binDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	// An executable that reports a different version than the channel resolves.
	writeTarGz(t, archivePath, map[string][]byte{
		cliArchiveEntry: fakeCLI(t, "putnami 1.2.3"),
	})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashFileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+hash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	err = VersionUpdate(context.Background(), "1.0.0", binDir)
	if err == nil {
		t.Fatal("expected stale-stamp rejection, got nil")
	}
	for _, want := range []string{"stale", "9.9.9", "putnami 1.2.3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(binDir, hostExecutable("putnami-go-9.9.9"))); !os.IsNotExist(statErr) {
		t.Errorf("stale binary should not install, stat err = %v", statErr)
	}
}

func TestVersionUpdate_AcceptsMatchingStamp(t *testing.T) {
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	binDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	writeTarGz(t, archivePath, map[string][]byte{
		cliArchiveEntry: fakeCLI(t, "putnami 9.9.9"),
	})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashFileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+hash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	result, err := VersionUpdateWithOptions(context.Background(), "1.0.0", binDir, VersionUpdateOptions{Channel: "latest"})
	if err != nil {
		t.Fatalf("VersionUpdateWithOptions: %v", err)
	}
	target := filepath.Join(binDir, hostExecutable("putnami-go-9.9.9"))
	if !result.Updated || result.Version != "9.9.9" || result.BinaryPath != target {
		t.Errorf("result = %+v, want updated target %q for 9.9.9", result, target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("installed binary missing: %v", err)
	}
}

func TestVersionUpdate_ActivationFailureReturnsError(t *testing.T) {
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	binDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	writeTarGz(t, archivePath, map[string][]byte{
		cliArchiveEntry: fakeCLI(t, "putnami 9.9.9"),
	})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashFileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+hash)
		_, _ = w.Write(archiveBytes)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	originalActivate := activateCLIUpdate
	t.Cleanup(func() { activateCLIUpdate = originalActivate })
	activateCLIUpdate = func(string, string) error { return errors.New("read-only filesystem") }

	result, err := VersionUpdateWithOptions(context.Background(), "1.0.0", binDir, VersionUpdateOptions{Channel: "latest"})
	if !errors.Is(err, ErrCLIActivation) {
		t.Fatalf("err = %v, want activation error", err)
	}
	var activation *CLIActivationError
	if !errors.As(err, &activation) || activation.BinaryPath != filepath.Join(binDir, hostExecutable("putnami-go-9.9.9")) {
		t.Fatalf("activation = %+v, want installed binary path", activation)
	}
	if !strings.Contains(err.Error(), activation.BinaryPath) || !strings.Contains(err.Error(), activation.LinkPath) {
		t.Errorf("err = %q, want both installed binary and failed link paths", err)
	}
	if result.Updated || result.BinaryPath != "" {
		t.Errorf("result = %+v, want no successful update result", result)
	}
	if _, err := os.Stat(filepath.Join(binDir, hostExecutable("putnami-go-9.9.9"))); err != nil {
		t.Errorf("verified binary should remain installed for manual recovery: %v", err)
	}
}

func TestVersionUpdate_RejectsBadIntegrity(t *testing.T) {
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	stagingDir := t.TempDir()
	archivePath := filepath.Join(stagingDir, "release.tar.gz")
	writeTarGz(t, archivePath, map[string][]byte{
		cliArchiveEntry: []byte("fake-binary"),
	})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// Advertise a wrong hash — server is lying about the file's identity.
	wrongHash := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+wrongHash)
		w.Write(archiveBytes)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	err = VersionUpdate(context.Background(), "1.0.0", t.TempDir())
	if err == nil {
		t.Fatal("expected integrity failure, got nil")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("error should mention integrity check failed, got: %v", err)
	}
}
