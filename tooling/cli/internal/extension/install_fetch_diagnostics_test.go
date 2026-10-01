package extension

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
)

// notFoundRegistry answers 404 to every download, which is what the registry did
// to an anonymous reader while serving credentialed ones normally. The
// answer is the registry's own recorded "version not found".
func notFoundRegistry(t *testing.T) *recorded.Server {
	t.Helper()
	return recorded.NewServer(t, nil, putRegistryRecording(t, "download-version-not-found.404.http"))
}

// TestArtifactFetchError_NamesPinPlatformAndAnonymity is the diagnosis a past
// incident asks for. The credential-free gate went red for nine hours on a pinned
// version that answered 404 to an anonymous download, and the message it left
// behind — "download @putnami/cloud: HTTP 404" — named neither the version, nor
// the URL, nor the fact that the request carried no credential. Each of those
// three is asserted separately so a future edit cannot drop one quietly.
func TestArtifactFetchError_NamesPinPlatformAndAnonymity(t *testing.T) {
	srv := notFoundRegistry(t)
	inst := &Installer{
		WorkspaceRoot:     t.TempDir(),
		ResolverURL:       srv.URL,
		HTTPClient:        srv.Client(),
		AnonymousRegistry: true,
	}

	_, err := inst.ResolveIntegrityForPlatform(
		context.Background(), "@putnami/cloud", "0.0.0-20260910214131-50206b165", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	msg := err.Error()

	for _, want := range []string{
		"@putnami/cloud",
		"0.0.0-20260910214131-50206b165",
		"linux/amd64",
		"HTTP 404",
		srv.URL + "/putnami/cloud/download?",
		"arch=amd64",
		"os=linux",
		"the request was anonymous",
		"withdrawn",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to carry %q", msg, want)
		}
	}
}

// TestArtifactFetchError_SaysWhenTheRequestWasCredentialed keeps the anonymity
// clause a fact rather than a constant: the same 404 from a credentialed reader
// is a different bug, and a message that always said "anonymous" would send the
// next reader down the wrong path.
//
// The token is stubbed because the clause reads the Authorization header the
// request actually carried, not the intent to look for one — a machine that
// holds no credential is anonymous however the caller was configured.
func TestArtifactFetchError_SaysWhenTheRequestWasCredentialed(t *testing.T) {
	srv := notFoundRegistry(t)
	stubRegistryToken(t, "a-token", "")
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.ResolveIntegrityForPlatform(
		context.Background(), "@putnami/cloud", "1.2.3", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if msg := err.Error(); !strings.Contains(msg, "the request carried a credential") {
		t.Errorf("error = %q, want it to say the request was credentialed", msg)
	}
}

// TestArtifactFetchError_ReadsAnonymousWhenDiscoveryFindsNoToken covers the
// case the AnonymousRegistry flag misses: a caller that did look for a
// credential and found none sent an anonymous request, and saying otherwise
// would point the reader at their account instead of at the pin.
func TestArtifactFetchError_ReadsAnonymousWhenDiscoveryFindsNoToken(t *testing.T) {
	srv := notFoundRegistry(t)
	stubRegistryToken(t, "", "warning: an extension was skipped\nnot signed in")
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.ResolveIntegrityForPlatform(
		context.Background(), "@putnami/cloud", "1.2.3", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	msg := err.Error()
	if !strings.Contains(msg, "the request was anonymous") {
		t.Errorf("error = %q, want it to say the request was anonymous", msg)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if want := "`putnami cloud registry-token --host " + host + "` provided no credential: not signed in"; !strings.Contains(msg, want) {
		t.Errorf("error = %q, want it to name the credential command and its reason %q", msg, want)
	}
}

// TestArtifactFetchError_WithholdsResolverCredentials is the reason the message
// rebuilds its URL instead of quoting the request's. PUTNAMI_REGISTRY_URL may
// carry userinfo and a signed query, and this error is printed on CI.
func TestArtifactFetchError_WithholdsResolverCredentials(t *testing.T) {
	srv := notFoundRegistry(t)
	credentialed := strings.Replace(srv.URL, "http://", "http://alice:s3cr3t@", 1) + "?sig=deadbeef"
	inst := &Installer{
		WorkspaceRoot:     t.TempDir(),
		ResolverURL:       credentialed,
		HTTPClient:        srv.Client(),
		AnonymousRegistry: true,
	}

	_, err := inst.ResolveIntegrityForPlatform(
		context.Background(), "@putnami/cloud", "1.2.3", "linux", "amd64")
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	msg := err.Error()
	for _, secret := range []string{"s3cr3t", "alice", "deadbeef", "sig="} {
		if strings.Contains(msg, secret) {
			t.Errorf("error = %q, want it to withhold %q", msg, secret)
		}
	}
	// The endpoint itself still has to be there, or the redaction has thrown
	// away the diagnosis along with the credential.
	if !strings.Contains(msg, srv.Listener.Addr().String()) {
		t.Errorf("error = %q, want it to name the registry host", msg)
	}
}

// TestInstall_NamesTheArtifactOnceOnAFailedFetch pins the deduplication. The
// install frame used to add "download <name>: " on top of a message that named
// the artifact itself, and the command's own renderer adds a third copy, so the
// first line a contributor read said "@putnami/cloud" three times before saying
// anything useful.
func TestInstall_NamesTheArtifactOnceOnAFailedFetch(t *testing.T) {
	t.Setenv(HTTPRetryBackoffEnv, "0")
	srv := notFoundRegistry(t)
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	_, err := inst.Install(context.Background(), "@putnami/cloud", "1.2.3", nil)
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	msg := err.Error()
	if got := strings.Count(msg, "@putnami/cloud"); got != 1 {
		t.Errorf("error = %q, names the artifact %d times, want 1", msg, got)
	}
	if !strings.Contains(msg, "HTTP 404") {
		t.Errorf("error = %q, want it to carry the HTTP status", msg)
	}
}

func TestRedactRegistryURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain URL is unchanged", "https://put.putnami.dev", "https://put.putnami.dev"},
		{"path is kept", "https://put.putnami.dev/base", "https://put.putnami.dev/base"},
		{"userinfo is dropped", "https://alice:s3cr3t@put.putnami.dev", "https://put.putnami.dev"},
		{"username alone is dropped", "https://alice@put.putnami.dev", "https://put.putnami.dev"},
		{"query is dropped", "https://put.putnami.dev/base?sig=abc", "https://put.putnami.dev/base"},
		{"bare question mark is dropped", "https://put.putnami.dev/base?", "https://put.putnami.dev/base"},
		{"fragment is dropped", "https://put.putnami.dev/base#tok", "https://put.putnami.dev/base"},
		{"surrounding space is trimmed", "  https://put.putnami.dev  ", "https://put.putnami.dev"},
		{"unparseable yields nothing", "https://put.putnami.dev/%zz", ""},
		{"empty yields nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactRegistryURL(tc.in); got != tc.want {
				t.Errorf("RedactRegistryURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
