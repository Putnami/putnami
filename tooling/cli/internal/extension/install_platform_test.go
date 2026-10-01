package extension

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// foreignTarget returns a platform that is guaranteed NOT to be the host's, so
// the cross-platform assertions below mean the same thing on every CI runner.
func foreignTarget() ArtifactTarget {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		return ArtifactTarget{OS: "darwin", Arch: "arm64"}
	}
	return ArtifactTarget{OS: "linux", Arch: "amd64"}
}

func foreignPlatformKey() string {
	t := foreignTarget()
	return lockfile.PlatformKey(t.OS, t.Arch)
}

// platformArchiveServer serves body for every download and records the query the
// installer sent, so a test can prove which os/arch was actually requested.
func platformArchiveServer(t *testing.T, body []byte, headers map[string]string) (*httptest.Server, *url.Values) {
	t.Helper()
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// buildModedArchive builds a tar.gz whose entries carry deliberately awkward
// modes and timestamps — the exact host-dependent metadata a materialization has
// to erase.
func buildModedArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest := extensionManifestJSON(name, version)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	write := func(hdr *tar.Header, content string) {
		hdr.Size = int64(len(content))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	write(&tar.Header{Name: "putnami.extension.json", Mode: 0o644, Typeflag: tar.TypeReg}, manifest)
	if err := tw.WriteHeader(&tar.Header{Name: "bin", Mode: 0o777, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	write(&tar.Header{Name: "bin/tool", Mode: 0o777, Typeflag: tar.TypeReg}, "#!/bin/sh\nexit 0\n")
	write(&tar.Header{Name: "bin/data.json", Mode: 0o600, Typeflag: tar.TypeReg}, `{"a":1}`)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// treeSnapshot is a byte-comparable walk of a materialized tree: relative path,
// mode and mtime for every entry, plus the content of every file.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// WalkDir already yields lexical order, which makes the snapshot independent
	// of readdir order on the two trees being compared.
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(rel + " mode=" + info.Mode().String() + " mtime=" + info.ModTime().UTC().Format("2006-01-02T15:04:05.000000000Z"))
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(" sha=" + sha256Bytes(data))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestInstall_TargetPlatformFetchesAndVerifiesThatPlatform is the core promise:
// asking for another platform must actually ASK the registry for that platform
// and check what comes back against THAT platform's lock digest — not the host's.
func TestInstall_TargetPlatformFetchesAndVerifiesThatPlatform(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	artifactDir := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)

	target := foreignTarget()
	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	srv, query := platformArchiveServer(t, archiveBytes, map[string]string{"X-Resolved-Version": "1.2.3"})

	wsRoot := t.TempDir()
	inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client(), Target: target}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3"))),
		// Only the FOREIGN platform is pinned: the host's digest is deliberately
		// absent, so a host-platform fetch would fail closed instead of passing.
		Integrities: map[string]string{foreignPlatformKey(): archiveHash},
	}
	result, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("cross-platform install: %v", err)
	}

	if got := query.Get("os"); got != target.OS {
		t.Errorf("requested os = %q, want %q (host is %q)", got, target.OS, runtime.GOOS)
	}
	if got := query.Get("arch"); got != target.Arch {
		t.Errorf("requested arch = %q, want %q (host is %q)", got, target.Arch, runtime.GOARCH)
	}
	if result.Integrity != archiveHash {
		t.Errorf("Integrity = %q, want the archive digest %q", result.Integrity, archiveHash)
	}
	if got := result.Integrities[foreignPlatformKey()]; got != archiveHash {
		t.Errorf("Integrities[%s] = %q, want %q", foreignPlatformKey(), got, archiveHash)
	}

	// Verified bytes still reach the content-addressed store — that is the whole
	// point: the result must be a materialized tree a packaging step can copy.
	entry := filepath.Join(artifactDir, "sha256", archiveHash[:2], archiveHash, "putnami.extension.json")
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("materialized tree missing from the store: %v", err)
	}

	// ...but the workspace itself is untouched: repointing the stable link would
	// make the next `putnami build` exec a foreign binary.
	stable := layout.StableDir(wsRoot, layout.Extensions, "@putnami/test")
	if _, err := os.Lstat(stable); !os.IsNotExist(err) {
		t.Errorf("cross-platform install must not create %s (err=%v)", stable, err)
	}
}

// TestInstall_TargetPlatformRelinkIsSkippedOnTheCachedPath pins the same
// no-side-effects rule on the zero-download fast path, which has its own link
// call: a warm store must not become a back door to repointing the symlink.
func TestInstall_TargetPlatformRelinkIsSkippedOnTheCachedPath(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	target := foreignTarget()
	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3")))
	srv, _ := platformArchiveServer(t, archiveBytes, map[string]string{"X-Resolved-Version": "1.2.3"})

	lockEntry := &lockfile.LockEntry{
		Version:      "1.2.3",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{foreignPlatformKey(): archiveHash},
	}

	// First run warms the store.
	warm := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client(), Target: target}
	if _, err := warm.Install(context.Background(), "@putnami/test", "latest", lockEntry); err != nil {
		t.Fatalf("warm install: %v", err)
	}

	// Second run must take the cached path — and still leave the worktree alone.
	wsRoot := t.TempDir()
	cold := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client(), Target: target}
	result, err := cold.Install(context.Background(), "@putnami/test", "latest", lockEntry)
	if err != nil {
		t.Fatalf("cached cross-platform install: %v", err)
	}
	if !result.FromCache {
		t.Fatal("second cross-platform install should have come from the store")
	}
	stable := layout.StableDir(wsRoot, layout.Extensions, "@putnami/test")
	if _, err := os.Lstat(stable); !os.IsNotExist(err) {
		t.Errorf("cached cross-platform install must not create %s (err=%v)", stable, err)
	}
}

// TestInstall_TargetPlatformFailsClosedWithoutADigest is the fail-closed half.
// The cross-platform lock branch of verifyArchiveIntegrity accepts unverified
// bytes into a PER-WORKTREE install, which is contained. A materialization has
// no containment — its output is a CAS-shaped tree baked into an image — so the
// same input must be a hard error.
func TestInstall_TargetPlatformFailsClosedWithoutADigest(t *testing.T) {
	for _, unsafe := range []string{"", "1"} {
		t.Run("unsafe="+unsafe, func(t *testing.T) {
			t.Setenv(UnsafeInstallEnv, unsafe)
			artifactDir := t.TempDir()
			t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactDir)

			target := foreignTarget()
			archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
			// No X-Integrity header: nothing can vouch for these bytes.
			srv, _ := platformArchiveServer(t, archiveBytes, map[string]string{"X-Resolved-Version": "1.2.3"})

			wsRoot := t.TempDir()
			inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client(), Target: target}
			lockEntry := &lockfile.LockEntry{
				Version:      "1.2.3",
				ManifestHash: sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.2.3"))),
				// A digest for some OTHER platform only — never the requested one.
				Integrities: map[string]string{"plan9/abc": strings.Repeat("b", 64)},
			}

			_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
			if err == nil {
				t.Fatal("expected a fail-closed error for unverifiable foreign bytes")
			}
			if !strings.Contains(err.Error(), "cannot materialize") {
				t.Errorf("error = %v, want it to name the refused materialization", err)
			}
			if !strings.Contains(err.Error(), target.OS+"/"+target.Arch) {
				t.Errorf("error = %v, want it to name the requested platform", err)
			}

			// Neither store nor worktree may hold the unverified bytes.
			if entries, _ := os.ReadDir(filepath.Join(artifactDir, "sha256")); len(entries) != 0 {
				t.Errorf("unverified materialization reached the shared store (%d entries)", len(entries))
			}
			if _, err := os.Stat(layout.ArtifactDir(wsRoot, layout.Extensions, "@putnami/test", "1.2.3")); !os.IsNotExist(err) {
				t.Errorf("unverified materialization fell through to the per-worktree path (err=%v)", err)
			}
		})
	}
}

// TestInstall_DestStoreRootLeavesTheGlobalStoreUntouched proves --dest is a
// drop-in artifact-store root a packaging step can pick up without reaching into
// $HOME, and that it does not also warm (or disturb) the machine-global store.
func TestInstall_DestStoreRootLeavesTheGlobalStoreUntouched(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	globalDir := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", globalDir)
	dest := filepath.Join(t.TempDir(), "warm")

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	srv, _ := platformArchiveServer(t, archiveBytes, map[string]string{
		"X-Resolved-Version": "1.2.3",
		"X-Integrity":        "sha256:" + archiveHash,
	})

	wsRoot := t.TempDir()
	inst := &Installer{
		WorkspaceRoot: wsRoot,
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
		Target:        ArtifactTarget{StoreRoot: dest},
	}
	if _, err := inst.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("--dest install: %v", err)
	}
	if err := inst.FinalizeMaterialization(); err != nil {
		t.Fatalf("FinalizeMaterialization: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "sha256", archiveHash[:2], archiveHash, "putnami.extension.json")); err != nil {
		t.Errorf("--dest must produce a sha256/<xx>/<digest>/ layout: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(globalDir, "sha256")); len(entries) != 0 {
		t.Errorf("--dest must not write the machine-global store (%d entries)", len(entries))
	}
	stable := layout.StableDir(wsRoot, layout.Extensions, "@putnami/test")
	if _, err := os.Lstat(stable); !os.IsNotExist(err) {
		t.Errorf("--dest must not repoint %s (err=%v)", stable, err)
	}

	// Finalize strips the host-local bookkeeping so the tree is packageable.
	for _, leftover := range []string{"tmp", "locks", ".lock"} {
		if _, err := os.Lstat(filepath.Join(dest, leftover)); !os.IsNotExist(err) {
			t.Errorf("finalized destination still carries %s (err=%v)", leftover, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dest, "sha256", archiveHash[:2], archiveHash, "lastused")); !os.IsNotExist(err) {
		t.Error("finalized destination still carries a wall-clock lastused sidecar")
	}
}

// TestInstall_MaterializationIsByteIdentical is the determinism contract the
// downstream image content key depends on: the same lock and the same platform
// must produce the same bytes, on any machine, on any run. Extraction otherwise
// stamps the wall clock on every file and masks the archive's modes through the
// process umask.
func TestInstall_MaterializationIsByteIdentical(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildModedArchive(t, "@putnami/test", "1.2.3")
	archiveHash := sha256Bytes(archiveBytes)
	srv, _ := platformArchiveServer(t, archiveBytes, map[string]string{
		"X-Resolved-Version": "1.2.3",
		"X-Integrity":        "sha256:" + archiveHash,
	})

	materialize := func() string {
		t.Helper()
		dest := filepath.Join(t.TempDir(), "warm")
		inst := &Installer{
			WorkspaceRoot: t.TempDir(),
			ResolverURL:   srv.URL,
			HTTPClient:    srv.Client(),
			Target:        ArtifactTarget{OS: foreignTarget().OS, Arch: foreignTarget().Arch, StoreRoot: dest},
		}
		if _, err := inst.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := inst.FinalizeMaterialization(); err != nil {
			t.Fatalf("FinalizeMaterialization: %v", err)
		}
		return dest
	}

	// Two independent cold materializations of the same archive. Without
	// normalization their extraction mtimes alone would already diverge.
	first := materialize()
	second := materialize()

	if a, b := treeSnapshot(t, first), treeSnapshot(t, second); a != b {
		t.Errorf("materializations are not byte-identical:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}

	// Equality alone could pass vacuously on a coarse-granularity filesystem, so
	// assert the absolute contract too: the fixed epoch, and modes collapsed away
	// from the archive's 0o777/0o600 (and from whatever the umask would allow).
	for _, dest := range []string{first, second} {
		assertNormalized(t, filepath.Join(dest, "sha256", archiveHash[:2], archiveHash))
	}
}

// assertNormalized checks the concrete normalization contract on a materialized
// entry: a fixed mtime everywhere, and modes collapsed to one
// executable/non-executable pair regardless of what the archive or the umask
// asked for.
func assertNormalized(t *testing.T, entryDir string) {
	t.Helper()
	cases := []struct {
		rel  string
		mode os.FileMode
	}{
		{"putnami.extension.json", 0o644},
		{"bin", 0o755 | os.ModeDir},
		{"bin/tool", 0o755},
		{"bin/data.json", 0o644},
	}
	for _, c := range cases {
		info, err := os.Lstat(filepath.Join(entryDir, filepath.FromSlash(c.rel)))
		if err != nil {
			t.Fatalf("stat %s: %v", c.rel, err)
		}
		// Windows keeps only a read-only attribute, so the modes collapse on
		// Unix alone; the mtime is fixed everywhere.
		if runtime.GOOS != "windows" && info.Mode() != c.mode {
			t.Errorf("%s mode = %v, want %v", c.rel, info.Mode(), c.mode)
		}
		if !info.ModTime().UTC().Equal(artifactstore.NormalizedTime) {
			t.Errorf("%s mtime = %v, want the normalized %v", c.rel, info.ModTime().UTC(), artifactstore.NormalizedTime)
		}
	}
	// The manifest BYTES must survive normalization untouched: their SHA-256 is
	// the lock's platform-independent binding, re-checked on every cached link.
	data, err := os.ReadFile(filepath.Join(entryDir, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != extensionManifestJSON("@putnami/test", "1.2.3") {
		t.Errorf("normalization rewrote the manifest bytes: %s", data)
	}
}

// TestArtifactTarget_ZeroValueIsAHostInstall pins the compatibility floor: every
// pre-existing caller constructs an Installer without a Target and must keep
// getting exactly today's behavior.
func TestArtifactTarget_ZeroValueIsAHostInstall(t *testing.T) {
	var zero ArtifactTarget
	if zero.Materializes() {
		t.Error("the zero target must not materialize")
	}
	if (ArtifactTarget{OS: runtime.GOOS, Arch: runtime.GOARCH}).Materializes() {
		t.Error("an explicit host platform must not materialize")
	}
	if !(ArtifactTarget{StoreRoot: "/tmp/x"}).Materializes() {
		t.Error("--dest alone must materialize: its tree is packaging output")
	}
	if !foreignTarget().Materializes() {
		t.Error("a foreign platform must materialize")
	}

	inst := &Installer{WorkspaceRoot: t.TempDir()}
	if got, want := inst.Platform(), lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("Platform() = %q, want the host %q", got, want)
	}
	targeted := &Installer{WorkspaceRoot: t.TempDir(), Target: foreignTarget()}
	if got, want := targeted.Platform(), foreignPlatformKey(); got != want {
		t.Errorf("targeted Platform() = %q, want %q", got, want)
	}
}
