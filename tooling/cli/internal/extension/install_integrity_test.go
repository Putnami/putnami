package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// integrityProbeServer serves an archive body for any download request and
// records the query the installer sent. handler may add headers before the body
// is written.
func integrityProbeServer(t *testing.T, body []byte, headers map[string]string) (*httptest.Server, *url.Values, *int) {
	t.Helper()
	var got url.Values
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = r.URL.Query()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &calls
}

// TestResolveArtifactIntegrity_ReadsAdvertisedHeaderForRequestedPlatform pins the
// design decision behind the cross-platform carry-forward: a foreign
// platform's digest is REGISTRY-ASSERTED, read from the advertised header, and
// the archive body is never downloaded or hashed. The body served here hashes to
// something else entirely, so a result equal to the header proves the bytes were
// not the source of truth.
func TestResolveArtifactIntegrity_ReadsAdvertisedHeaderForRequestedPlatform(t *testing.T) {
	body := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	advertised := strings.Repeat("ab", 32)
	if sha256Bytes(body) == advertised {
		t.Fatal("test setup: body digest must differ from the advertised digest")
	}
	srv, query, calls := integrityProbeServer(t, body, map[string]string{
		"X-Resolved-Version": "2.0.0",
		"X-Integrity":        "sha256:" + advertised,
	})

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	digest, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", "2.0.0", "linux", "amd64")
	if err != nil {
		t.Fatalf("ResolveIntegrityForPlatform: %v", err)
	}
	if digest != advertised {
		t.Errorf("digest = %q, want the advertised %q", digest, advertised)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want exactly 1 registry round-trip", *calls)
	}
	if q := query.Get("os"); q != "linux" {
		t.Errorf("os = %q, want linux (not the host %q)", q, runtime.GOOS)
	}
	if q := query.Get("arch"); q != "amd64" {
		t.Errorf("arch = %q, want amd64 (not the host %q)", q, runtime.GOARCH)
	}
	// Version-pinned channel: the digest must be bound to the NEW version.
	if q := query.Get("channel"); q != "2.0.0" {
		t.Errorf("channel = %q, want the exact version 2.0.0", q)
	}
}

func TestResolveArtifactIntegrity_ErrorsWhenRegistryAdvertisesNoIntegrity(t *testing.T) {
	body := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	srv, _, _ := integrityProbeServer(t, body, map[string]string{"X-Resolved-Version": "2.0.0"})

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", "2.0.0", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error when the registry advertises no integrity")
	}
	if !strings.Contains(err.Error(), "advertised no integrity") {
		t.Errorf("error = %v, want it to name the missing integrity", err)
	}
}

// TestResolveArtifactIntegrity_RejectsDifferentResolvedVersion pins the
// never-carry-a-stale-digest invariant from the other side: a digest the
// registry binds to a different version must never be recorded under the
// requested one.
func TestResolveArtifactIntegrity_RejectsDifferentResolvedVersion(t *testing.T) {
	body := buildExtensionArchive(t, "@putnami/test", "1.0.0")
	srv, _, _ := integrityProbeServer(t, body, map[string]string{
		"X-Resolved-Version": "1.0.0",
		"X-Integrity":        "sha256:" + sha256Bytes(body),
	})

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", "2.0.0", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error when the registry resolves another version")
	}
	if !strings.Contains(err.Error(), "registry resolved 1.0.0") {
		t.Errorf("error = %v, want it to name the mismatched version", err)
	}
}

func TestResolveArtifactIntegrity_RejectsInvalidAdvertisedIntegrity(t *testing.T) {
	body := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	srv, _, _ := integrityProbeServer(t, body, map[string]string{
		"X-Resolved-Version": "2.0.0",
		"X-Integrity":        "not-a-digest",
	})

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	if _, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", "2.0.0", "linux", "amd64"); err == nil {
		t.Fatal("expected an error for a malformed advertised integrity")
	}
}

// TestResolveArtifactIntegrity_UnpinnableVersionMakesNoRequest keeps the
// resolver off the network when there is no exact version to bind a digest to.
func TestResolveArtifactIntegrity_UnpinnableVersionMakesNoRequest(t *testing.T) {
	srv, _, calls := integrityProbeServer(t, []byte("unused"), nil)
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	for _, version := range []string{"", "latest", "0.0.0"} {
		if _, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", version, "linux", "amd64"); err == nil {
			t.Errorf("version %q: expected an error, got nil", version)
		}
	}
	if *calls != 0 {
		t.Errorf("calls = %d, want 0 registry round-trips for unpinnable versions", *calls)
	}
}

func TestResolveArtifactIntegrity_PropagatesRegistryFailure(t *testing.T) {
	srv := recorded.NewServer(t, nil, putRegistryRecording(t, "download-version-not-found.404.http"))

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.ResolveIntegrityForPlatform(context.Background(), "@putnami/test", "2.0.0", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to carry the HTTP status", err)
	}
}

func TestInstallerPlatform_IsHostPlatform(t *testing.T) {
	inst := &Installer{WorkspaceRoot: t.TempDir()}
	if got, want := inst.Platform(), lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("Platform() = %q, want %q", got, want)
	}
}

// TestResolveOnlyLookupsSendHead pins that a lookup reading only the resolved
// version and digest sends HEAD, so the registry sends no archive, while an
// install still sends GET: the local platform's digest comes from hashing the
// downloaded bytes.
func TestResolveOnlyLookupsSendHead(t *testing.T) {
	body := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("X-Resolved-Version", "2.0.0")
		w.Header().Set("X-Integrity", "sha256:"+sha256Bytes(body))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client(), AnonymousRegistry: true}
	spec := extensionSpec()
	spec.OS, spec.Arch = "linux", "amd64"

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"ResolveArtifact", func() error {
			_, err := inst.ResolveArtifact(context.Background(), "@putnami/test", "canary", spec)
			return err
		}, http.MethodHead},
		{"ResolveArtifactIntegrity", func() error {
			_, err := inst.ResolveArtifactIntegrity(context.Background(), "@putnami/test", "2.0.0", spec)
			return err
		}, http.MethodHead},
		{"download", func() error {
			path, _, _, err := inst.download(context.Background(), "@putnami/test", "2.0.0", spec)
			if err == nil {
				os.Remove(path)
			}
			return err
		}, http.MethodGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			methods = nil
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(methods) != 1 || methods[0] != tc.want {
				t.Errorf("methods = %v, want [%s]", methods, tc.want)
			}
		})
	}
}

// TestArtifactResolvedVersion_RefusesAHeaderThatIsNotAVersion pins the check at
// the point the X-Resolved-Version header is read: the value names a directory,
// so anything that is not a version, or carries a path separator in its
// prerelease, is refused. A missing header still falls back to the constraint.
func TestArtifactResolvedVersion_RefusesAHeaderThatIsNotAVersion(t *testing.T) {
	for _, header := range []string{
		"../../victim",
		"1.0.0/../../victim",
		"1.0.0-x/../../victim",
		"1.2.3-x/../../victim",
		`1.0.0-x\..\victim`,
		"latest",
	} {
		if v, err := artifactResolvedVersion(header, "1.0.0"); err == nil {
			t.Errorf("artifactResolvedVersion(%q) = %q, want an error", header, v)
		} else if !strings.Contains(err.Error(), "not a version") {
			t.Errorf("artifactResolvedVersion(%q) error = %v, want it to say not a version", header, err)
		}
	}
	for header, want := range map[string]string{
		"1.2.3":                          "1.2.3",
		"1.2.3-beta.1":                   "1.2.3-beta.1",
		"1.2.3-rc.1+build.5":             "1.2.3-rc.1+build.5",
		"0.0.0-e9388a298":                "0.0.0-e9388a298",
		"0.0.0-20261002155126-05f5df1b2": "0.0.0-20261002155126-05f5df1b2",
		"":                               "0.0.0",
	} {
		if got, err := artifactResolvedVersion(header, "latest"); err != nil || got != want {
			t.Errorf("artifactResolvedVersion(%q) = %q, %v, want %q", header, got, err, want)
		}
	}
}

// victimDir creates a directory next to the workspace root, outside its
// artifacts directory, holding a sentinel file an escaping install would delete.
func victimDir(t *testing.T) (wsRoot, sentinel string) {
	t.Helper()
	base := t.TempDir()
	wsRoot = filepath.Join(base, "ws")
	sentinel = filepath.Join(base, "victim", "sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wsRoot, sentinel
}

// escapeToVictim climbs from .putnami/bin/artifacts/extensions/<name>@<v> to
// the directory that holds the workspace root.
const escapeToVictim = "/../../../../../../victim"

// TestInstall_RefusesAResolvedVersionThatEscapes covers the unverified
// per-worktree install, reached by default for a lock with a manifest hash but
// no digest for this platform when the registry advertises none. The resolved
// version names the directory removed and extracted into before the manifest
// hash is checked, so a header that is a path must fail the install and leave
// every directory outside the artifacts directory alone.
func TestInstall_RefusesAResolvedVersionThatEscapes(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	for _, header := range []string{"../../victim", "1.0.0" + escapeToVictim, "1.0.0-x" + escapeToVictim} {
		t.Run(header, func(t *testing.T) {
			archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.0.0")
			srv, _, _ := integrityProbeServer(t, archiveBytes, map[string]string{"X-Resolved-Version": header})
			wsRoot, sentinel := victimDir(t)
			inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}

			lockEntry := &lockfile.LockEntry{
				Version:      "1.0.0",
				ManifestHash: sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.0.0"))),
				Integrities:  map[string]string{"plan9/abc": strings.Repeat("b", 64)},
			}
			_, err := inst.Install(context.Background(), "@putnami/test", "latest", lockEntry)
			assertFileExists(t, sentinel)
			if err == nil || !strings.Contains(err.Error(), "not a version") {
				t.Fatalf("Install error = %v, want a refused resolved version", err)
			}
		})
	}
}

// TestInstall_RefusesAManifestVersionThatEscapes covers the relocation of an
// install the registry could not name: the version read from the unverified
// archive's manifest names the directory it moves to, removed first.
func TestInstall_RefusesAManifestVersionThatEscapes(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "1")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archiveBytes := buildExtensionArchive(t, "@putnami/test", "1.0.0-x"+escapeToVictim)
	srv, _, _ := integrityProbeServer(t, archiveBytes, nil)
	wsRoot, sentinel := victimDir(t)
	inst := &Installer{WorkspaceRoot: wsRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.Install(context.Background(), "@putnami/test", "latest", nil)
	assertFileExists(t, sentinel)
	if err == nil || !strings.Contains(err.Error(), "manifest version") {
		t.Fatalf("Install error = %v, want a refused manifest version", err)
	}
	if _, err := os.Stat(layout.ArtifactDir(wsRoot, layout.Extensions, "@putnami/test", "0.0.0")); !os.IsNotExist(err) {
		t.Errorf("the refused install must not leave its directory behind: %v", err)
	}
}
