package oci

import (
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	ociproto "go.putnami.dev/protocol/oci"
)

// registryHostParityRepositories are repositories read both by
// go-containerregistry and by go.putnami.dev/protocol/oci: the cases of the
// protocol's registry host table and the URI authority edge cases that
// table leaves to this test.
var registryHostParityRepositories = []string{
	"ghcr.io/putnami/cloud", "GHCR.io/putnami/cloud", "registry.putnami.dev:8443/putnami/app",
	"docker.io/library/nginx", "Docker.IO/library/nginx", "index.docker.io/putnami/app",
	"localhost:5000/putnami/app", "registry.localhost/putnami/app", "registry.localhost:5000/putnami/app",
	"127.0.0.1:5000/putnami/app", "127.0.0.1/putnami/app", "127.0.0.10:5000/putnami/app",
	"127.1.2.3:5000/putnami/app", "[::1]:5000/putnami/app", "[::1]/putnami/app",
	"10.1.2.3:5000/putnami/app", "172.16.0.1:5000/putnami/app", "172.32.0.1:5000/putnami/app",
	"192.168.1.10/putnami/app", "8.8.8.8/putnami/app", "ghcr.io/ab", "ghcr.io/team_a/app-b/c.d",
	"ghcr.io/a//b", "ghcr.io/_a", "ghcr.io/a/", "ghcr.io:/putnami/app", ".io/putnami/app",
	"ghcr.io:99999/putnami/app", "ghcr.io:x/putnami/app", "::1/putnami/app", "fe80::1/putnami/app",
	"[fe80::1%25en0]:5000/putnami/app", "ghcr%2eio/putnami/app", "\xff.io/putnami/app", "Ünicode.example/putnami/app",
	"", "app", "putnami/app", "localhost/putnami/app", "ghcr.io", "ghcr.io/", "ghcr.io/a",
	"ghcr.io/Putnami/app", "ghcr.io/putnami/app:1.0", "ghcr.io/putnami/app@sha256:abc",
	"docker.io/nginx", "index.docker.io/nginx", "Docker.io/nginx", "robot:secret@ghcr.io/putnami/app",
	"ghcr.io?x=1/putnami/app", "ghcr.io#x/putnami/app", "gh cr.io/putnami/app", "[::1/putnami/app",
	"ghcr.io/" + strings.Repeat("a", 255), "ghcr.io/" + strings.Repeat("a", 256), "ghcr.io/é",
}

// registryHostParityHosts are bare registry hosts read both by
// go-containerregistry and by go.putnami.dev/protocol/oci.
var registryHostParityHosts = []string{
	"ghcr.io", "GHCR.io", "registry.putnami.dev:8443", "docker.io", "DOCKER.IO", "index.docker.io",
	"localhost", "localhost:5000", "LOCALHOST:5000", "registry.localhost", "registry.localhost:5000",
	"127.0.0.1", "127.0.0.2", "127.0.0.10:80", "[::1]:5000", "[::1]", "::1", "fe80::1",
	"10.0.0.1:5000", "172.31.255.255:5000", "172.32.0.1", "192.168.0.1", "8.8.8.8", "ghcr.io:",
	"ghcr.io:99999", "ghcr.io:x", ".io", "ghcr%2eio", "\xff.io", "Ünicode.example", "",
	"robot@ghcr.io", "ghcr.io/putnami", "ghcr.io?x=1", "ghcr.io#x", "gh cr.io", "[::1",
}

// TestRegistryHostMatchesGoContainerRegistry proves that
// go.putnami.dev/protocol/oci derives the registry host and scheme
// go-containerregistry derives under strict validation, once the host is
// lowercased: the one rule in which the protocol differs. It fails when an
// upgrade of go-containerregistry changes how a repository or a host is read.
func TestRegistryHostMatchesGoContainerRegistry(t *testing.T) {
	t.Parallel()
	for _, input := range registryHostParityRepositories {
		lowered := input
		if host, path, found := strings.Cut(input, "/"); found {
			lowered = lowerRegistryHost(host) + "/" + path
		}
		want, wantErr := name.NewRepository(lowered, name.StrictValidation)
		got, gotErr := ociproto.RegistryHost(input)
		switch {
		case (wantErr == nil) != (gotErr == nil):
			t.Errorf("repository %q: go-containerregistry error %v, protocol error %v", input, wantErr, gotErr)
		case wantErr == nil && (got != want.RegistryStr() || ociproto.RegistryScheme(got) != want.Scheme()):
			t.Errorf("repository %q: protocol %s://%s, go-containerregistry %s://%s", input, ociproto.RegistryScheme(got), got, want.Scheme(), want.RegistryStr())
		}
	}
	for _, input := range registryHostParityHosts {
		want, wantErr := name.NewRegistry(lowerRegistryHost(input), name.StrictValidation)
		got, gotErr := ociproto.NormalizeRegistryHost(input)
		switch {
		case (wantErr == nil) != (gotErr == nil):
			t.Errorf("host %q: go-containerregistry error %v, protocol error %v", input, wantErr, gotErr)
		case wantErr == nil && (got != want.RegistryStr() || ociproto.RegistryScheme(got) != want.Scheme() || ociproto.RegistryScheme(input) != want.Scheme()):
			t.Errorf("host %q: protocol %s://%s, go-containerregistry %s://%s", input, ociproto.RegistryScheme(got), got, want.Scheme(), want.RegistryStr())
		}
	}
}

// lowerRegistryHost lowercases the ASCII letters of host, the rule
// go.putnami.dev/protocol/oci applies before go-containerregistry's.
func lowerRegistryHost(host string) string {
	lowered := []byte(host)
	for index, char := range lowered {
		if char >= 'A' && char <= 'Z' {
			lowered[index] = char + ('a' - 'A')
		}
	}
	return string(lowered)
}
