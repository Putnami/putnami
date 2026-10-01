package oci

import (
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// bearerKeychain injects a caller-supplied registry token as a Bearer credential
// for exactly one registry host. Composed (via authn.NewMultiKeychain) ahead of
// authn.DefaultKeychain, it lets one registry authenticate with a fresh,
// dedicated token (e.g. the credential @putnami/cloud resolved for that host)
// while every other registry — GCP Artifact Registry, ghcr, … — keeps its native
// docker-config / gcloud / ADC credentials unchanged. An empty token makes
// Resolve defer to the next keychain.
type bearerKeychain struct {
	host  string
	token string
}

// Resolve returns a Bearer authenticator for k.host when a token is set, and
// authn.Anonymous otherwise so a MultiKeychain proceeds to the next keychain.
func (k bearerKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k.token != "" && res.RegistryStr() == k.host {
		return authn.FromConfig(authn.AuthConfig{RegistryToken: k.token}), nil
	}
	return authn.Anonymous, nil
}

// NewRegistryKeychain composes a token-injecting keychain for host (when token
// is non-empty) ahead of the Docker default keychain. With an empty token it is
// equivalent to authn.DefaultKeychain alone, so other registries and
// credential-less setups behave exactly as before.
func NewRegistryKeychain(host, token string) authn.Keychain {
	return authn.NewMultiKeychain(bearerKeychain{host: host, token: token}, authn.DefaultKeychain)
}

// RegistryHost extracts the registry host (the first path segment) from a docker
// registry reference such as "us-docker.pkg.dev/project/repo" → "us-docker.pkg.dev"
// or "registry.example.com" → "registry.example.com". Returns "" for an empty registry.
func RegistryHost(reg string) string {
	reg = strings.TrimRight(reg, "/")
	if reg == "" {
		return ""
	}
	host, _, _ := strings.Cut(reg, "/")
	return host
}
