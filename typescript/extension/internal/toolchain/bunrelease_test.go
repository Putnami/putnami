package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sha256Hex is the SHA-256 of data as 64 lowercase hexadecimal characters, the
// form a lock and a release document give a digest in.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeBunLock writes a workspace lock that pins release.
func writeBunLock(t *testing.T, workspace string, release BunRelease) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version":    3,
		"toolchains": map[string]any{"bun": release},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// declareBun writes a root package.json that declares packageManager.
func declareBun(t *testing.T, workspace, packageManager string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"name": "ws", "packageManager": packageManager})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The default release ships one valid SHA-256 for each platform Bun publishes
// an archive for, and it is the release this repository's own lock pins, with
// the digests the CLI read from the vendor when it wrote that lock.
func TestDefaultBunRelease_ShipsTheVendorDigests(t *testing.T) {
	release := DefaultBunRelease()
	if !IsPlainBunRelease(release.Version) {
		t.Fatalf("the default version %q is not a release", release.Version)
	}
	if len(release.Integrities) != 6 {
		t.Fatalf("the default release ships %d digests, want 6", len(release.Integrities))
	}
	for platform := range bunTargets {
		digest := release.Integrities[platform]
		if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size || strings.ToLower(digest) != digest {
			t.Errorf("the %s digest %q is not a lowercase SHA-256", platform, digest)
		}
	}
	// The returned release is a copy: a caller cannot change what the
	// extension ships.
	release.Integrities["linux/amd64"] = "changed"
	if DefaultBunRelease().Integrities["linux/amd64"] == "changed" {
		t.Fatal("DefaultBunRelease hands out the shipped digests themselves")
	}

	root := repositoryRoot(t)
	if root == "" {
		t.Skip("not inside the repository: no lock to compare with")
	}
	locked, pinned, err := lockedBunRelease(root)
	if err != nil || !pinned {
		t.Fatalf("the repository lock pins no Bun: %v", err)
	}
	want := DefaultBunRelease()
	if locked.Version != want.Version || locked.Source != want.Source {
		t.Fatalf("the repository lock pins Bun %s from %s; the default release is Bun %s from %s",
			locked.Version, locked.Source, want.Version, want.Source)
	}
	for platform, digest := range want.Integrities {
		if locked.Integrities[platform] != digest {
			t.Errorf("%s: the default release ships %s, the repository lock records %s", platform, digest, locked.Integrities[platform])
		}
	}
}

// repositoryRoot returns the directory above this package that holds the
// repository's lock, or "".
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if isRegularFile(filepath.Join(dir, WorkspaceLockFile)) && isRegularFile(filepath.Join(dir, "putnamiw")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// The archive of a release is bun-<target>.zip under the release's source,
// for the six platforms Bun publishes, and the vendor's downloads when the
// release names no source.
func TestBunArchiveFor(t *testing.T) {
	release := BunRelease{Version: "1.9.9", Integrities: map[string]string{"darwin/arm64": "digest"}}
	for platform, target := range map[string]string{
		"darwin/amd64": "darwin-x64", "darwin/arm64": "darwin-aarch64",
		"linux/amd64": "linux-x64", "linux/arm64": "linux-aarch64",
		"windows/amd64": "windows-x64", "windows/arm64": "windows-aarch64",
	} {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, ok := bunArchiveFor(release, goos, goarch)
		wantURL := "https://github.com/oven-sh/bun/releases/download/bun-v1.9.9/bun-" + target + ".zip"
		if !ok || archive.pin.URL != wantURL || archive.dir != "bun-"+target || archive.pin.SHA256 != release.Integrities[platform] {
			t.Errorf("%s: bunArchiveFor = (%+v, %v), want %s", platform, archive, ok, wantURL)
		}
	}
	if _, ok := bunArchiveFor(release, "linux", "riscv64"); ok {
		t.Error("linux/riscv64 has an archive")
	}
}

// The declared release is read the way the CLI reads it before it pins the
// lock.
func TestDeclaredBunVersion(t *testing.T) {
	for _, tc := range []struct{ declaration, want string }{
		{"bun@1.4.0", "1.4.0"},
		{" bun@v1.4.0", "1.4.0"},
		{"bun@1.4", "1.4.0"},
		{"pnpm@9.0.0", ""},
		{"", ""},
	} {
		workspace := t.TempDir()
		declareBun(t, workspace, tc.declaration)
		if got := declaredBunVersion(workspace); got != tc.want {
			t.Errorf("packageManager %q: declaredBunVersion = %q, want %q", tc.declaration, got, tc.want)
		}
	}
	if got := declaredBunVersion(t.TempDir()); got != "" {
		t.Errorf("no package.json: declaredBunVersion = %q, want none", got)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "package.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := declaredBunVersion(broken); got != "" {
		t.Errorf("a package.json that does not parse: declaredBunVersion = %q, want none", got)
	}
}

func TestLockedBunRelease(t *testing.T) {
	if _, pinned, err := lockedBunRelease(t.TempDir()); pinned || err != nil {
		t.Fatalf("no lock = (%v, %v), want no pin", pinned, err)
	}
	for name, content := range map[string]string{
		"no toolchains":   `{"version":3}`,
		"no bun":          `{"toolchains":{"go":{"version":"1.26.1"}}}`,
		"a null bun":      `{"toolchains":{"bun":null}}`,
		"a blank version": `{"toolchains":{"bun":{"version":"  "}}}`,
	} {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, pinned, err := lockedBunRelease(workspace); pinned || err != nil {
			t.Errorf("%s = (%v, %v), want no pin", name, pinned, err)
		}
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte(`{"toolchains":{"bun":"1.4.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lockedBunRelease(workspace); err == nil {
		t.Error("a bun entry of the wrong shape was read")
	}
	// A document that grew a field this extension does not know is read.
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile),
		[]byte(`{"future":true,"toolchains":{"bun":{"version":" 1.4.0 ","future":1,"source":"https://example.test/"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	release, pinned, err := lockedBunRelease(workspace)
	if err != nil || !pinned || release.Version != "1.4.0" || release.Source != "https://example.test/" {
		t.Fatalf("lockedBunRelease = (%+v, %v, %v), want Bun 1.4.0", release, pinned, err)
	}
}
