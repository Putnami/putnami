package clibin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestNativePinAndColdLaunchShareAuthenticatedBytesAndIntegrity(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection", "private-cli-pin-and-cold-launch-use-the-native-registry")
	const binary = "the-exact-private-cli"
	refusal := putRegistryRecording(t, "download-anonymous.401.http")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/putnami/cli/download" || r.URL.Query().Get("channel") != "1.2.3" || r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "amd64" {
			t.Errorf("unexpected native download: %s", r.URL.Redacted())
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer pkt_private_cli" {
			refusal.ServeHTTP(w, r)
			return
		}
		_, _ = w.Write([]byte(binary))
	}))
	defer server.Close()
	t.Setenv(extension.PutRegistryURLEnv, "https://wrong-environment.example.test")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "putnami.workspace.json"), []byte(`{"registries":{"put":{"registry":"`+server.URL+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "selected-cli")
	authorizations := 0
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(_ context.Context, host, selected string) (string, string) {
		authorizations++
		origin, _ := url.Parse(server.URL)
		if host != origin.Host || selected != executable {
			t.Errorf("credential changed origin or executable: %q, %q", host, selected)
		}
		return "pkt_private_cli", ""
	}
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	ctx := context.Background()
	pinner := NewWorkspace(artifactstore.New(t.TempDir()), workspace, executable)
	sha, err := pinner.Pin(ctx, "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	entry := entryFor("1.2.3", "linux", "amd64", sha)
	cold := NewWorkspace(artifactstore.New(t.TempDir()), workspace, executable)
	path, err := cold.Resolve(ctx, entry, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != binary {
		t.Fatalf("cold launch admitted different bytes: err=%v", err)
	}
	if requests != 2 || authorizations != 2 {
		t.Fatalf("pin and cold launch must each authenticate: requests=%d auth=%d", requests, authorizations)
	}
	if warmed, err := cold.Resolve(ctx, entry, "linux", "amd64"); err != nil || warmed != path || requests != 2 || authorizations != 2 {
		t.Fatalf("warm launch refetched or changed the pinned binary: path=%q err=%v", warmed, err)
	}
	badStore := artifactstore.New(t.TempDir())
	bad := NewWorkspace(badStore, workspace, executable)
	wrongSHA := strings.Repeat("0", 64)
	if _, err := bad.Resolve(ctx, entryFor("1.2.3", "linux", "amd64", wrongSHA), "linux", "amd64"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("private download bypassed committed integrity: %v", err)
	}
	if badStore.HasCLI(wrongSHA) {
		t.Fatal("bad private bytes entered the pinned store")
	}
}

func TestNativePinNeverForwardsCredentialToAnotherOrigin(t *testing.T) {
	const token = "pkt_origin_only"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("native CLI credential crossed the configured origin")
		}
		_, _ = w.Write([]byte("public-cdn-cli"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("native origin did not receive its credential")
		}
		http.Redirect(w, r, target.URL+"/binary", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv(extension.PutRegistryURLEnv, origin.URL)
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return token, "" }
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	r := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePinKeepsThePutnamiwDLMirrorContract(t *testing.T) {
	const binary = "legacy-mirror-cli"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dl/putnami" || r.URL.Query().Get("version") != "1.2.3" || r.URL.Query().Get("target") != "amd64" || r.URL.Query().Get("platform") != "linux" {
			t.Errorf("legacy mirror request = %s", r.URL.Redacted())
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("legacy mirror unexpectedly received the native registry credential")
		}
		_, _ = w.Write([]byte(binary))
	}))
	defer server.Close()
	t.Setenv(extension.PutRegistryURLEnv, server.URL+"/dl")

	r := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); err != nil {
		t.Fatalf("pin through a putnamiw-compatible /dl mirror: %v", err)
	}
}

func TestNativePinPrivateRefusalAndInsecureOriginFailBeforeAdmission(t *testing.T) {
	server := recorded.NewServer(t, nil, putRegistryRecording(t, "download-token-for-another-registry.403.http"))
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	calls := 0
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) {
		calls++
		return "", ""
	}
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	r := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); !errors.Is(err, ErrDownload) || !strings.Contains(err.Error(), "needs a credential") {
		t.Fatalf("private refusal was not actionable: %v", err)
	}
	t.Setenv(extension.PutRegistryURLEnv, "http://insecure.example.test")
	t.Setenv("PUTNAMI_ALLOW_INSECURE_REGISTRY", "")
	r = NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); err == nil || calls != 1 {
		t.Fatalf("insecure origin reached credentials or download: auth=%d err=%v", calls, err)
	}
}

func TestNativePublicPinAndColdLaunchRemainAnonymousWithoutLocalCredentials(t *testing.T) {
	const binary = "public-cli-without-an-account"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" {
			t.Error("public download acquired an Authorization header without local credentials")
		}
		_, _ = w.Write([]byte(binary))
	}))
	defer server.Close()
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) {
		return "", "not signed in"
	}
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	pinner := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	sha, err := pinner.Pin(context.Background(), "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	cold := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	path, err := cold.Resolve(context.Background(), entryFor("1.2.3", "linux", "amd64", sha), "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != binary || requests != 2 {
		t.Fatalf("anonymous pin/cold launch: requests=%d err=%v", requests, err)
	}
}

func TestColdPrivateCLIUsesInvocationBrokerBeforeWorkspaceRegistry(t *testing.T) {
	const binary = "private-cli-broker-bytes"
	requests := 0
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/put/putnami/cli/download" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected broker request: %s auth present=%t", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		_, _ = w.Write([]byte(binary))
	}))
	defer broker.Close()
	t.Setenv(extension.PrivatePutRegistryURLEnv, broker.URL+"/put")
	t.Setenv(extension.PutRegistryURLEnv, "https://unreachable.invalid/dl")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "putnami.workspace.json"), []byte(`{"registries":{"put":{"registry":"https://unreachable.invalid"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) {
		t.Error("broker download tried to bootstrap a credential provider")
		return "", ""
	}
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	pinner := NewWorkspace(artifactstore.New(t.TempDir()), workspace, "/missing-cli")
	sha, err := pinner.Pin(t.Context(), "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	cold := NewWorkspace(artifactstore.New(t.TempDir()), workspace, "/missing-cli")
	path, err := cold.Resolve(t.Context(), entryFor("1.2.3", "linux", "amd64", sha), "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != binary || requests != 2 {
		t.Fatalf("cold broker download: requests=%d err=%v", requests, err)
	}
	if _, err := cold.Resolve(t.Context(), entryFor("1.2.3", "linux", "amd64", strings.Repeat("0", 64)), "linux", "amd64"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("broker bypassed lock integrity: %v", err)
	}
	for _, invalid := range []string{"http://localhost:1234/put", "http://127.0.0.1:1234/wrong", "http://127.0.0.1/put", "https://127.0.0.1:1234/put"} {
		t.Setenv(extension.PrivatePutRegistryURLEnv, invalid)
		before := requests
		_, err := NewWorkspace(artifactstore.New(t.TempDir()), workspace, "/missing-cli").Pin(t.Context(), "1.2.3", "linux", "amd64")
		if err == nil || requests != before {
			t.Fatalf("malformed private route reached network: %s err=%v", invalid, err)
		}
	}
}

// A registry URL may carry `user:token@`. The pin error and the source a pin
// records in the committed lock name the registry without it.
func TestPinNeverPrintsOrRecordsRegistryURLCredentials(t *testing.T) {
	server := recorded.NewServer(t, nil, putRegistryRecording(t, "cli-download-private-anonymous.404.http"))
	t.Setenv(extension.PutRegistryURLEnv, strings.Replace(server.URL, "http://", "http://alice:s3cr3t@", 1))
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return "", "not signed in" }
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })

	r := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli")
	_, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64")
	if !errors.Is(err, ErrDownload) || !strings.Contains(err.Error(), server.URL+"/putnami/cli/download?arch=amd64&channel=1.2.3&os=linux") {
		t.Fatalf("pin error = %v, want the download URL", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("pin error = %q prints the registry URL's credentials", err)
	}
	if source := r.DownloadURL("1.2.3", "linux", "amd64"); source != server.URL+"/putnami/cli/download?arch=amd64&channel=1.2.3&os=linux" {
		t.Errorf("recorded source = %q, want the download URL without credentials", source)
	}
}
