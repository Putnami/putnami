package extension

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Archive fixtures own their HTTP servers and temporary artifact stores. A
// developer's installed Cloud provider must not download a CLI into that store
// or contact a live account while these unit tests resolve credentials, and a
// hosted run's invocation broker must not replace the fixture's registry: the
// broker wins over every authored route, and it answers 401 to a test archive.
// Tests that exercise the broker set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	ResolveRegistryToken = func(string) (string, string) { return "", "" }
	os.Unsetenv(PrivatePutRegistryURLEnv)
	os.Exit(m.Run())
}

// stubRegistryToken replaces the credential seam for one test. The production
// seam shells out to @putnami/cloud, which a unit test has no business needing.
func stubRegistryToken(t *testing.T, token, hint string) {
	t.Helper()
	original := ResolveRegistryToken
	ResolveRegistryToken = func(string) (string, string) { return token, hint }
	t.Cleanup(func() { ResolveRegistryToken = original })
}

func TestResolvePutRegistryURL_PrefersTheWorkspaceEntry(t *testing.T) {
	t.Setenv(PutRegistryURLEnv, "https://env.example")

	registries := map[string]json.RawMessage{
		"put": json.RawMessage(`{"registry":"https://declared.example/"}`),
	}
	if got := ResolvePutRegistryURL(registries); got != "https://declared.example" {
		t.Errorf("with a declared entry = %q, want the workspace entry without its trailing slash", got)
	}

	// An entry that names no registry is not an answer, so the chain continues.
	empty := map[string]json.RawMessage{"put": json.RawMessage(`{}`)}
	if got := ResolvePutRegistryURL(empty); got != "https://env.example" {
		t.Errorf("with an empty entry = %q, want the environment override", got)
	}

	t.Setenv(PutRegistryURLEnv, "")
	if got := ResolvePutRegistryURL(nil); got != DefaultPutRegistryURL {
		t.Errorf("with nothing declared = %q, want %q", got, DefaultPutRegistryURL)
	}
}

// A hosted run reads no registry the workspace declares: the runner's
// environment, or the default, says where the pinned CLI and the extensions
// come from.
func TestResolvePutRegistryURL_AHostedRunIgnoresTheWorkspaceEntry(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-ignores-the-workspace-registry")
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	registries := map[string]json.RawMessage{
		"put": json.RawMessage(`{"registry":"https://declared.example/"}`),
	}
	t.Setenv(PutRegistryURLEnv, "https://env.example")
	if got := ResolvePutRegistryURL(registries); got != "https://env.example" {
		t.Errorf("hosted, with a declared entry = %q, want the environment override", got)
	}
	t.Setenv(PutRegistryURLEnv, "")
	if got := ResolvePutRegistryURL(registries); got != DefaultPutRegistryURL {
		t.Errorf("hosted, with a declared entry = %q, want %q", got, DefaultPutRegistryURL)
	}
}

// The put projection of a channel can be private, so the archive request carries
// the user's credential. Absence stays ordinary: no token, no header, and the
// registry's own access control decides.
func TestArchiveDownloadSendsTheCredential(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection", "archive-download-sends-the-credential")

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("X-Resolved-Version", "1.2.3")
		w.Header().Set("X-Integrity", "sha256:"+strings.Repeat("a", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}
	spec := ArtifactSpec{OS: "linux", Arch: "amd64"}

	stubRegistryToken(t, "cloud-issued-token", "")
	if _, err := inst.ResolveArtifact(context.Background(), "@putnami/go", "canary", spec); err != nil {
		t.Fatalf("ResolveArtifact with a credential: %v", err)
	}

	stubRegistryToken(t, "", "")
	if _, err := inst.ResolveArtifact(context.Background(), "@putnami/go", "canary", spec); err != nil {
		t.Fatalf("ResolveArtifact without a credential: %v", err)
	}

	want := []string{"Bearer cloud-issued-token", ""}
	if len(seen) != len(want) {
		t.Fatalf("saw %d requests, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d Authorization = %q, want %q", i, seen[i], want[i])
		}
	}
}

func TestRegistryRedirectDoesNotForwardCredentialAcrossOrigins(t *testing.T) {
	client := NewRegistryHTTPClient()
	original, err := http.NewRequest(http.MethodGet, "https://registry.example.test/archive", nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"same origin", "https://registry.example.test/other", "Bearer target-bound"},
		{"subdomain", "https://cdn.registry.example.test/archive", ""},
		{"other port", "https://registry.example.test:8443/archive", ""},
		{"https downgrade", "http://registry.example.test/archive", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			redirect, requestErr := http.NewRequest(http.MethodGet, tc.url, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			// CheckRedirect receives the headers net/http elected to copy. Seed
			// the sensitive one to pin our stricter exact-origin policy.
			redirect.Header.Set("Authorization", "Bearer target-bound")
			if redirectErr := client.CheckRedirect(redirect, []*http.Request{original}); redirectErr != nil {
				t.Fatal(redirectErr)
			}
			if got := redirect.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization after redirect = %q, want %q", got, tc.want)
			}
		})
	}
	if err := client.CheckRedirect(original, make([]*http.Request, 10)); err == nil {
		t.Fatal("registry client accepted more redirects than net/http's default limit")
	}
}

// A private channel answers 401 to an anonymous reader and to a rejected
// credential, and 403 to a credential it will not honor. None is a network
// fault, and the error says what cures it. Each answer is the registry's own.
func TestArchiveDownloadNamesTheLoginCommandOnRefusal(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection", "archive-download-sends-the-credential")

	for _, recording := range []string{
		"download-anonymous.401.http",
		"download-invalid-credential.401.http",
		"download-token-for-another-registry.403.http",
	} {
		refusal := putRegistryRecording(t, recording)
		srv := recorded.NewServer(t, nil, refusal)
		inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}
		stubRegistryToken(t, "", "")

		_, err := inst.ResolveArtifact(context.Background(), "@putnami/go", "canary", ArtifactSpec{OS: "linux", Arch: "amd64"})
		if err == nil {
			t.Fatalf("%s was accepted", recording)
		}
		if !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(refusal.Status())) {
			t.Errorf("%s: error = %q, want the status", recording, err)
		}
		if next := protocolcli.SuggestedNext(err); next != "putnami cloud login" {
			t.Errorf("%s: suggested next = %q, want `putnami cloud login`", recording, next)
		}
	}
}

// A refusal of a request that went out without the credential it
// asked for names the credential command and its reason. The registry answers
// an anonymous reader of a private archive with 404, so a 404 names both
// causes and suggests no command. The endpoint never shows its userinfo.
func TestRegistryRefusalErrorNamesTheCredentialCommand(t *testing.T) {
	const endpoint = "https://put.example.test/putnami/cli/download"
	const missing = "`putnami cloud registry-token --host put.example.test` provided no credential: not signed in"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		err := RegistryRefusalError("https://alice:s3cr3t@put.example.test/putnami/cli/download?sig=x", status, missing)
		if err == nil {
			t.Fatalf("HTTP %d: no error", status)
		}
		for _, want := range []string{"registry " + endpoint + " answered", "HTTP " + strconv.Itoa(status), missing} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("HTTP %d: error = %q, want %q", status, err, want)
			}
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "sig=") {
			t.Errorf("HTTP %d: error = %q leaks the registry URL's credentials", status, err)
		}
		wantNext := "putnami cloud login"
		if status == http.StatusNotFound {
			wantNext = ""
		}
		if next := protocolcli.SuggestedNext(err); next != wantNext {
			t.Errorf("HTTP %d: suggested next = %q, want %q", status, next, wantNext)
		}
	}
	if err := RegistryRefusalError(endpoint, http.StatusNotFound, "missing"); !strings.Contains(err.Error(), "does not exist, or it is private") {
		t.Errorf("404 error = %q, want both causes", err)
	}
	if err := RegistryRefusalError(endpoint, http.StatusBadGateway, missing); err != nil {
		t.Errorf("HTTP 502 = %v, want nil", err)
	}
	if err := RegistryRefusalError(endpoint, http.StatusNotFound, ""); err != nil {
		t.Errorf("credentialed 404 = %v, want nil", err)
	}
	if err := RegistryRefusalError(endpoint, http.StatusUnauthorized, ""); err == nil ||
		err.Error() != "registry "+endpoint+" answered HTTP 401: this archive needs a credential" {
		t.Errorf("credentialed 401 = %v", err)
	}
}

func TestAuthorizeRegistryRequestReportsTheMissingCredential(t *testing.T) {
	newRequest := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "https://put.example.test/putnami/cli/download", nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	stubRegistryToken(t, "pkt_minted", "")
	req := newRequest()
	if missing, err := AuthorizeRegistryRequest(req); err != nil || missing != "" || req.Header.Get("Authorization") != "Bearer pkt_minted" {
		t.Fatalf("minted credential: missing=%q header=%q", missing, req.Header.Get("Authorization"))
	}

	stubRegistryToken(t, "", "")
	if missing, _ := AuthorizeRegistryRequest(newRequest()); missing != "`putnami cloud registry-token --host put.example.test` provided no credential" {
		t.Errorf("silent credential command: missing = %q", missing)
	}

	stubRegistryToken(t, "", "first line\n"+strings.Repeat("x", 400)+"\n")
	missing, _ := AuthorizeRegistryRequest(newRequest())
	if strings.Contains(missing, "first line") || !strings.HasSuffix(missing, strings.Repeat("x", 300)+"…") {
		t.Errorf("long reason: missing = %q, want the last line cut at 300 bytes", missing)
	}

	preset := newRequest()
	preset.Header.Set("Authorization", "Bearer caller")
	if missing, err := AuthorizeRegistryRequest(preset); err != nil || missing != "" {
		t.Errorf("caller credential: missing = %q, want none asked for", missing)
	}

	previous := ResolveRegistryTokenWithCLI
	ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return "", "not signed in" }
	t.Cleanup(func() { ResolveRegistryTokenWithCLI = previous })
	if missing, err := AuthorizeRegistryRequestWithCLI(newRequest(), "/selected-cli"); err != nil || !strings.HasSuffix(missing, "provided no credential: not signed in") {
		t.Errorf("pin credential command: missing = %q", missing)
	}
}

// A hosted run never asks the host-keyed seam: that child, `putnami cloud
// registry-token`, is a CLI without the run credential that loads the
// workspace's path extensions. A download that no credential-provider serves
// goes out anonymous, and a refusal fails with ErrHostedRegistryCredential,
// which says to install a user-scope credential-provider.
func TestHostedDownloadNeverAsksTheRegistryTokenSeam(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-runs-only-store-and-path-extensions")
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	seam := 0
	previous, previousWithCLI := ResolveRegistryToken, ResolveRegistryTokenWithCLI
	ResolveRegistryToken = func(string) (string, string) { seam++; return "seam-token", "" }
	ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { seam++; return "seam-token", "" }
	t.Cleanup(func() { ResolveRegistryToken, ResolveRegistryTokenWithCLI = previous, previousWithCLI })

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		var authorization []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = append(authorization, r.Header.Get("Authorization"))
			w.WriteHeader(status)
		}))
		inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}
		_, err := inst.ResolveArtifact(context.Background(), "@putnami/go", "canary", ArtifactSpec{OS: "linux", Arch: "amd64"})
		srv.Close()
		if !errors.Is(err, ErrHostedRegistryCredential) || !strings.Contains(err.Error(), "putnami extensions install --user") {
			t.Errorf("hosted HTTP %d: error = %v, want ErrHostedRegistryCredential naming the user-scope install", status, err)
		}
		// A 404 also answers a version that does not exist, so it names both
		// causes, as it does without the flag.
		if notFound := err != nil && strings.Contains(err.Error(), "the version does not exist, or"); notFound != (status == http.StatusNotFound) {
			t.Errorf("hosted HTTP %d: error = %v; it names a missing version: %v, want %v", status, err, notFound, status == http.StatusNotFound)
		}
		if len(authorization) != 1 || authorization[0] != "" {
			t.Errorf("hosted HTTP %d: Authorization headers = %q, want one anonymous request", status, authorization)
		}
	}

	// A credential-provider that holds no credential for the host leaves the
	// request anonymous too, and the pin download's seam is not asked either.
	restoreRead := InstallRegistryReadCredential(func(context.Context, *url.URL) (string, bool, error) { return "", false, nil })
	t.Cleanup(restoreRead)
	req, err := http.NewRequest(http.MethodGet, "https://put.example.test/putnami/cli/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := AuthorizeRegistryRequestWithCLI(req, "/selected-cli")
	if err != nil || req.Header.Get("Authorization") != "" || !strings.Contains(missing, "put.example.test") {
		t.Errorf("hosted pin download: missing = %q, err = %v, Authorization = %q; want anonymous with the host named", missing, err, req.Header.Get("Authorization"))
	}
	if refusal := RegistryRefusalError("https://put.example.test", http.StatusUnauthorized, missing); !errors.Is(refusal, ErrHostedRegistryCredential) {
		t.Errorf("hosted pin refusal = %v, want ErrHostedRegistryCredential", refusal)
	}
	if seam != 0 {
		t.Errorf("a hosted run asked the host-keyed seam %d times, want 0", seam)
	}
}

func TestArchiveInstallerUsesInvocationBrokerWithoutCredentialBootstrap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/put/putnami/go/download" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong broker request: %s", r.URL.Path)
		}
		w.Header().Set("X-Resolved-Version", "1.2.3")
		_, _ = w.Write([]byte("archive"))
	}))
	defer server.Close()
	t.Setenv(PrivatePutRegistryURLEnv, server.URL+"/put")
	previous := ResolveRegistryToken
	ResolveRegistryToken = func(string) (string, string) { t.Error("archive broker queried credential provider"); return "", "" }
	t.Cleanup(func() { ResolveRegistryToken = previous })
	installer := NewInstaller(t.TempDir())
	response, _, err := installer.openArtifact(t.Context(), "@putnami/go", "1.2.3", ArtifactSpec{OS: "linux", Arch: "x64"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("archive status %d", response.StatusCode)
	}
}
