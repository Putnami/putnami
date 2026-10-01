package clibin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A cold launch downloads the pinned CLI with the installed read credential
// first, as every other registry download does. The credential goes only to
// the hosts it names; any other host keeps the host-keyed seam; a refusal
// fails the download before any request goes out. Removing the credential
// source returns the download to the host-keyed seam.
func TestColdLaunchAsksTheInstalledReadCredentialFirst(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bootstrap-provider-serves-only-the-locked-downloads")
	const binary = "the-pinned-cli-behind-a-provider"
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(binary))
	}))
	defer server.Close()
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return "pkt_host_keyed", "" }
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })
	sum := sha256.Sum256([]byte(binary))
	entry := entryFor("1.2.3", "linux", "amd64", hex.EncodeToString(sum[:]))

	resolve := func(read extension.RegistryCredential) error {
		restore := extension.InstallRegistryReadCredential(read)
		defer restore()
		_, err := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli").Resolve(context.Background(), entry, "linux", "amd64")
		return err
	}
	serving := func(hosts ...string) extension.RegistryCredential {
		return func(_ context.Context, target *url.URL) (string, bool, error) {
			return "pat_bootstrap", slices.Contains(hosts, target.Host), nil
		}
	}
	refusal := errors.New("the account is suspended")

	for _, c := range []struct {
		name string
		read extension.RegistryCredential
		want []string
		err  error
	}{
		{name: "its host", read: serving(origin.Host), want: []string{"Bearer pat_bootstrap"}},
		{name: "another host", read: serving("put.example.test"), want: []string{"Bearer pkt_host_keyed"}},
		{name: "refusal", read: func(context.Context, *url.URL) (string, bool, error) { return "", false, refusal }, err: refusal},
		{name: "removed", want: []string{"Bearer pkt_host_keyed"}},
	} {
		authorizations = nil
		err := resolve(c.read)
		if c.err != nil {
			if !errors.Is(err, c.err) || !errors.Is(err, ErrDownload) {
				t.Errorf("%s: resolve = %v, want a download failure wrapping %v", c.name, err, c.err)
			}
		} else if err != nil {
			t.Errorf("%s: resolve = %v", c.name, err)
		}
		if !slices.Equal(authorizations, c.want) {
			t.Errorf("%s: the registry saw %q, want %q", c.name, authorizations, c.want)
		}
	}
}
