package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
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
