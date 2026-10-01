package versioncmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func usePinCredential(t *testing.T) {
	t.Helper()
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(_ context.Context, host, executable string) (string, string) {
		if host == "" || !filepath.IsAbs(executable) {
			t.Error("pin must provide the registry host and its selected absolute CLI executable")
		}
		return "pkt_pin", ""
	}
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
}

// buildTarGzArchive builds a gzip+tar archive with compiled/putnami=content and
// returns the bytes plus the SHA-256 of the binary (the digest pin must
// record). It returns an error instead of failing a *testing.T so it can also
// run inside an httptest handler goroutine, where t.Fatal is unsafe.
func buildTarGzArchive(content string) (archive []byte, sha string, err error) {
	return buildTarGzArchiveAt("compiled/putnami", content)
}

// buildTarGzArchiveAt is buildTarGzArchive with the binary at name.
func buildTarGzArchiveAt(name, content string) (archive []byte, sha string, err error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return nil, "", err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// pinArchive is buildTarGzArchive for the main test goroutine, where a build
// failure (which should never actually happen for this tiny fixed content) can
// fail the test directly.
func pinArchive(t *testing.T, content string) (archive []byte, sha string) {
	t.Helper()
	archive, sha, err := buildTarGzArchive(content)
	if err != nil {
		t.Fatal(err)
	}
	return archive, sha
}

// platformArchiveServer fakes the put registry's per-platform CLI download
// endpoint (GET /putnami/cli/download?channel=&arch=<arch>&os=<os>): it serves a
// distinct archive for each "os/arch" pair — content "binary-for-<os>/<arch>",
// so a test can recompute the expected digest with buildTarGzArchive, at
// compiled/putnami.exe for windows and compiled/putnami elsewhere, as the
// packager lays them out — and answers unavailable platforms with 404, modeling
// a version that does not publish an archive for that platform.
func platformArchiveServer(t *testing.T, unavailable map[string]bool) *httptest.Server {
	t.Helper()
	usePinCredential(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/putnami/cli/download" || r.Header.Get("Authorization") != "Bearer pkt_pin" || r.URL.Query().Get("channel") == "" {
			t.Errorf("pin missed the authenticated native registry: %s", r.URL.Redacted())
			http.Error(w, "native registry authorization required", http.StatusUnauthorized)
			return
		}
		platform := lockfile.PlatformKey(r.URL.Query().Get("os"), r.URL.Query().Get("arch"))
		if unavailable[platform] {
			http.NotFound(w, r)
			return
		}
		archive, _, err := buildTarGzArchiveAt(
			"compiled/"+pkgmeta.ExecutableName(r.URL.Query().Get("os"), "putnami"), "binary-for-"+platform)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(archive)
	}))
}

// TestCLIPinSetShowRemove drives the pin command end-to-end against a mock
// registry, through the real resolver, store, and lock file.
func TestCLIPinSetShowRemove(t *testing.T) {
	usePinCredential(t)
	archive, sha := pinArchive(t, "the-pinned-binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/putnami/cli/download" || r.URL.Query().Get("channel") != "1.2.3" || r.Header.Get("Authorization") != "Bearer pkt_pin" {
			t.Errorf("unexpected native pin request: %s", r.URL.Redacted())
			http.Error(w, "native pin required", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_REGISTRY_URL", "https://wrong-environment.example.test")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir()) // isolate the machine-global store
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "putnami.workspace.json"), []byte(`{"registries":{"put":{"registry":"`+srv.URL+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// pin <version> — accepts a leading "v"
	if err := CLIPinForVersion(context.Background(), ws, []string{"v1.2.3"}, "1.2.3"); err != nil {
		t.Fatalf("CLIPin set: %v", err)
	}
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil || lf == nil {
		t.Fatalf("read lock after pin: lf=%v err=%v", lf, err)
	}
	if lf.Version != lockfile.FormatVersionV4 {
		t.Fatalf("pin downgraded a new v4 lock to v%d", lf.Version)
	}
	entry, ok := lf.GetCLI()
	if !ok {
		t.Fatal("no CLI pin written")
	}
	if entry.Version != "1.2.3" {
		t.Errorf("pinned version = %q, want 1.2.3 (leading v trimmed)", entry.Version)
	}
	if got := entry.IntegrityFor(runtime.GOOS, runtime.GOARCH); got != sha {
		t.Errorf("pinned digest = %q, want %q (= the served binary's SHA-256)", got, sha)
	}
	if entry.Source == "" {
		t.Error("pin should record a Source URL")
	}
	if entry.ProtocolVersion != 2 {
		t.Errorf("pin protocolVersion = %d, want 2", entry.ProtocolVersion)
	}

	// pin --remove
	if err := CLIPin(context.Background(), ws, []string{"--remove"}); err != nil {
		t.Fatalf("CLIPin remove: %v", err)
	}
	lf2, _ := lockfile.ReadLockFile(ws)
	if _, ok := lf2.GetCLI(); ok {
		t.Error("pin still present after --remove")
	}
}

// TestCLIPinRecordsEveryDefaultPlatform is the regression test for a bug where
// `putnami pin <version>` used to record only the
// invoking machine's platform, dropping every other platform's digest from the
// lock. A single `putnami pin` call must now record the local platform AND
// every platform in defaultPinPlatforms, each verified against ITS OWN archive
// (not the local one reused), from one fake put endpoint standing in for the
// registry's per-platform manifest.
func TestCLIPinRecordsEveryDefaultPlatform(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-executables-by-name", "pin-records-windows-amd64-from-any-host")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := t.TempDir()

	srv := platformArchiveServer(t, nil) // every platform published
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	if err := CLIPin(context.Background(), ws, []string{"3.0.0"}); err != nil {
		t.Fatalf("CLIPin: %v", err)
	}

	lf, err := lockfile.ReadLockFile(ws)
	if err != nil || lf == nil {
		t.Fatalf("read lock after pin: lf=%v err=%v", lf, err)
	}
	entry, ok := lf.GetCLI()
	if !ok {
		t.Fatal("no CLI pin written")
	}

	want := append([]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)}, defaultPinPlatforms...)
	// Windows is in the default matrix: a pin from any host records it,
	// read from the Windows archive's compiled/putnami.exe.
	if !slices.Contains(defaultPinPlatforms, "windows/amd64") {
		t.Errorf("defaultPinPlatforms = %v, want windows/amd64 recorded from every host", defaultPinPlatforms)
	}
	for _, platform := range want {
		platformOS, platformArch, ok := lockfile.SplitPlatformKey(platform)
		if !ok {
			t.Fatalf("bad platform key %q", platform)
		}
		_, wantSHA, err := buildTarGzArchive("binary-for-" + platform)
		if err != nil {
			t.Fatal(err)
		}
		if got := entry.IntegrityFor(platformOS, platformArch); got != wantSHA {
			t.Errorf("integrity for %s = %q, want %q (one `putnami pin` call should record every default platform, each against its own archive)",
				platform, got, wantSHA)
		}
	}
}

// TestCLIPinRepinNewVersionDropsUnpublishedPlatform is the other half of the
// regression: re-pinning a DIFFERENT version must still verify and keep every
// platform the previous pin carried that the new version publishes, must drop
// only the one it does not, and must say so on stderr — never silently lose
// every platform but the one running `putnami pin`, which is what stranded the
// hosted linux/amd64 CI runner in the reported bug.
func TestCLIPinRepinNewVersionDropsUnpublishedPlatform(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := t.TempDir()

	local := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	// linux/amd64 is the platform from the reported bug; fall
	// back to another default platform if the test happens to run on that host.
	victim := "linux/amd64"
	if victim == local {
		victim = "darwin/amd64"
	}
	survivor := "darwin/arm64"
	if survivor == local || survivor == victim {
		survivor = "linux/arm64"
	}

	// Seed a lock pinned to 1.0.0 carrying the local platform, the platform
	// 2.0.0 will drop, and one that survives into 2.0.0.
	lf := lockfile.NewLockFile()
	old := lockfile.LockEntry{Version: "1.0.0"}
	old.SetPlatformIntegrity(local, "cafefeed")
	old.SetPlatformIntegrity(victim, "deadbeef")
	old.SetPlatformIntegrity(survivor, "f00dbabe")
	lf.SetCLI(old)
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	srv := platformArchiveServer(t, map[string]bool{victim: true}) // 2.0.0 does not publish victim
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	stderr := sharedtest.CaptureStderr(t, func() {
		if err := CLIPin(context.Background(), ws, []string{"2.0.0"}); err != nil {
			t.Errorf("CLIPin: %v", err)
		}
	})

	got, err := lockfile.ReadLockFile(ws)
	if err != nil || got == nil {
		t.Fatalf("read lock after pin: lf=%v err=%v", got, err)
	}
	entry, ok := got.GetCLI()
	if !ok {
		t.Fatal("no CLI pin written")
	}
	if entry.Version != "2.0.0" {
		t.Fatalf("version = %q, want 2.0.0", entry.Version)
	}

	localOS, localArch, _ := lockfile.SplitPlatformKey(local)
	_, wantLocalSHA, err := buildTarGzArchive("binary-for-" + local)
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.IntegrityFor(localOS, localArch); got != wantLocalSHA {
		t.Errorf("local platform (%s) digest = %q, want %q", local, got, wantLocalSHA)
	}

	victimOS, victimArch, _ := lockfile.SplitPlatformKey(victim)
	if got := entry.IntegrityFor(victimOS, victimArch); got != "" {
		t.Errorf("%s digest = %q, want dropped (2.0.0 does not publish it)", victim, got)
	}

	survivorOS, survivorArch, _ := lockfile.SplitPlatformKey(survivor)
	_, wantSurvivorSHA, err := buildTarGzArchive("binary-for-" + survivor)
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.IntegrityFor(survivorOS, survivorArch); got != wantSurvivorSHA {
		t.Errorf("%s digest = %q, want %q (a previously carried platform the new version still publishes must survive)",
			survivor, got, wantSurvivorSHA)
	}

	if !strings.Contains(stderr, victim) || !strings.Contains(stderr, "2.0.0") {
		t.Errorf("stderr = %q, want a warning naming %s and 2.0.0", stderr, victim)
	}
}

func TestParsePinArgs(t *testing.T) {
	cases := []struct {
		args    []string
		version string
		remove  bool
		wantErr bool
	}{
		{nil, "", false, false},
		{[]string{"1.2.3"}, "1.2.3", false, false},
		{[]string{"--remove"}, "", true, false},
		{[]string{"--unpin"}, "", false, true},
		{[]string{"1.2.3", "--remove"}, "", false, true}, // remove + version
		{[]string{"--bogus"}, "", false, true},           // unknown flag
		{[]string{"1.2.3", "extra"}, "", false, true},    // extra positional
	}
	for _, c := range cases {
		v, r, err := parsePinArgs(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("parsePinArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if err == nil && (v != c.version || r != c.remove) {
			t.Errorf("parsePinArgs(%v) = (%q,%v), want (%q,%v)", c.args, v, r, c.version, c.remove)
		}
	}
}

// ─── source workspace ────────────────────────────────────────────────────────

// captureCLIPinStdout runs fn with os.Stdout redirected and returns what it
// printed. `pin` writes through iox to the process stdout, so this is the only
// way to assert what a user actually reads.
func captureCLIPinStdout(t *testing.T, fn func() error) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	runErr := fn()
	os.Stdout = previous
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("pin: %v", runErr)
	}
	return buf.String()
}

func writeSourceWorkspaceLock(t *testing.T, ws string) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
}

// `pin` with no argument must print the sentinel legibly. Printing
// "pinned to " with an empty version would read as a corrupt lock; this
// workspace is not unpinned, it is self-hosted.
func TestCLIPinShowsTheSourceWorkspaceSentinel(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	out := captureCLIPinStdout(t, func() error {
		return CLIPin(context.Background(), ws, nil)
	})
	for _, want := range []string{"built from this workspace", lockfile.SourceWorkspace, "./putnamiw"} {
		if !strings.Contains(out, want) {
			t.Errorf("`putnami pin` output %q must name %q", out, want)
		}
	}
	if strings.Contains(out, "pinned to \n") || strings.Contains(out, "No CLI version pinned") {
		t.Errorf("the sentinel must not be reported as unpinned or as an empty pin: %q", out)
	}
}

// `pin --remove` is the way out of self-hosting, and it must say what it removed.
func TestCLIPinRemoveClearsTheSourceWorkspaceSentinel(t *testing.T) {
	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)
	out := captureCLIPinStdout(t, func() error {
		return CLIPin(context.Background(), ws, []string{"--remove"})
	})
	if !strings.Contains(out, "source-workspace") {
		t.Errorf("removal output %q must say what it removed", out)
	}
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lf.GetCLI(); ok {
		t.Error("the sentinel survived `pin --remove`")
	}
}

// `pin <version>` must replace the sentinel with a real published pin, and must
// not carry the sentinel's source forward. `pin` is launcher-exempt, so this is
// always reachable even inside a source workspace.
func TestCLIPinReplacesTheSourceWorkspaceSentinel(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	srv := platformArchiveServer(t, nil)
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	ws := t.TempDir()
	writeSourceWorkspaceLock(t, ws)

	if err := CLIPinForVersion(context.Background(), ws, []string{"1.2.3"}, "1.2.3"); err != nil {
		t.Fatalf("CLIPin set over the sentinel: %v", err)
	}
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := lf.GetCLI()
	if !ok {
		t.Fatal("no CLI pin written")
	}
	if entry.IsWorkspaceSource() {
		t.Fatalf("the sentinel survived a real pin: %+v", entry)
	}
	if entry.Version != "1.2.3" {
		t.Errorf("pinned version = %q, want 1.2.3", entry.Version)
	}
	if !strings.HasPrefix(entry.Source, srv.URL) {
		t.Errorf("pin source = %q, want the registry download URL", entry.Source)
	}
	if entry.IntegrityFor(runtime.GOOS, runtime.GOARCH) == "" {
		t.Error("a real pin must record this platform's digest")
	}
}
