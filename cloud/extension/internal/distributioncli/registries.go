package distributioncli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Registry identifies one of the four package registry protocols the CLI
// authenticates against on behalf of the user. The values are stable wire
// strings used in the on-disk registries.json state file.
type Registry string

// The four registry protocols, in the canonical AllRegistries order.
const (
	RegistryNPM   Registry = "npm"
	RegistryGomod Registry = "gomod"
	RegistryOCI   Registry = "oci"
	RegistryPut   Registry = "put"
)

// AllRegistries is the canonical ordering for iteration and display. Registry
// recipe writes, explicit setup, logout, and status traverse this list in order.
var AllRegistries = []Registry{RegistryNPM, RegistryGomod, RegistryOCI, RegistryPut}

// RegistriesFileRelative is the path under PUTNAMI_HOME where the per-host
// state file lives. The same file doubles as the put-server client config
// until a dedicated put client exists.
const RegistriesFileRelative = ".putnami/registries.json"

// Default production hostnames. Override per-registry via env vars
// (PUTNAMI_REGISTRY_<NPM|GOMOD|OCI|PUT>_URL) or, for tests, via login params.
var defaultRegistryURL = map[Registry]string{
	RegistryNPM:   "https://npm.putnami.dev",
	RegistryGomod: "https://go.putnami.dev",
	RegistryOCI:   "https://oci.putnami.dev",
	RegistryPut:   "https://put.putnami.dev",
}

// RegistryEndpoint locates one registry by URL/host.
type RegistryEndpoint struct {
	Registry Registry
	URL      string
	Host     string
}

// KeyRef remembers a registry endpoint and, once minted, its server-side api
// key metadata so logout can revoke it and status can introspect it. The raw
// token is NEVER persisted here — only the protected resolver stores
// (registries.json auth / put_auth) and, where a native client still needs it,
// ecosystem credential files touch the raw token after minting.
//
// Token is retained only to decode state written by older CLIs. New state does
// not write host-only recipes because they cannot name the immutable owner
// workspace and exact package required by a Distribution lease.
type KeyRef struct {
	Registry  Registry             `json:"registry"`
	ID        string               `json:"id"`
	Prefix    string               `json:"prefix"`
	Host      string               `json:"host"`
	URL       string               `json:"url,omitempty"`
	Name      string               `json:"name,omitempty"`
	CreatedAt string               `json:"created_at,omitempty"`
	Token     *clicore.TokenSource `json:"token,omitempty"`
}

// registryTokenPurpose is the `--for <kind>` value for one registry: the four
// user-facing kinds npm, go, oci, put. It is also the string recorded in each
// key's token recipe, so the recipe a login writes and the flag a user types
// are always the same word.
func registryTokenPurpose(registry Registry) string {
	switch registry {
	case RegistryNPM:
		return "npm"
	case RegistryGomod:
		return "go"
	case RegistryOCI:
		return "oci"
	case RegistryPut:
		return "put"
	default:
		return string(registry)
	}
}

// RegistryMarkerScope is the OAuth scope a user registry token requests: the
// bare protocol marker, identical to the `--for` kind. It names WHICH registry
// surface the bearer is for and nothing else — it carries no namespace, no
// package, and no action, and every registry rejects a marker minted for a
// different surface. Authority itself comes
// from the user's IAM workspace membership, resolved by the registry.
func RegistryMarkerScope(registry Registry) string {
	switch registry {
	case RegistryNPM, RegistryGomod, RegistryOCI, RegistryPut:
		return registryTokenPurpose(registry)
	default:
		return ""
	}
}

// RegistryTokenRecipe is the argv a native client runs to obtain a fresh bearer
// for one registry. It is the whole public token contract: kind in, bare bearer
// on stdout.
func RegistryTokenRecipe(registry Registry) []string {
	if RegistryMarkerScope(registry) == "" {
		return nil
	}
	return []string{"putnami", "cloud", "token", "--for", registryTokenPurpose(registry)}
}

// RegistryFromTokenPurpose is the inverse of registryTokenPurpose: it maps a
// `putnami cloud token --for <purpose>` value back to its registry, reporting
// ok=false for anything outside the four known purposes.
func RegistryFromTokenPurpose(purpose string) (Registry, bool) {
	switch strings.ToLower(strings.TrimSpace(purpose)) {
	case "npm":
		return RegistryNPM, true
	case "go":
		return RegistryGomod, true
	case "oci":
		return RegistryOCI, true
	case "registry", "put":
		return RegistryPut, true
	default:
		return "", false
	}
}

// registryAccessToken is a short-lived registry JWT cached by host after the
// backing pkt_* api key is exchanged through the auth server.
type registryAccessToken struct {
	AccessToken string `json:"access_token"` //nolint:gosec // G117: OAuth2 token response; field/JSON names are spec-mandated
	TokenType   string `json:"token_type,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Scope       string `json:"scope,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
}

// RegistriesState is the persisted index of per-registry api keys plus
// protected resolver credentials. Keys remains non-secret and client-readable;
// Auth stores raw per-host api keys for registries whose native client config
// should not receive a static bearer (npm/oci). PutAuth is the legacy put
// registry store kept for existing put publish paths. Access caches short-lived
// registry JWTs until near expiry. The file is 0o600.
type RegistriesState struct {
	Version int                            `json:"version"`
	Keys    []KeyRef                       `json:"keys"`
	Auth    map[string]string              `json:"auth,omitempty"`     // host → raw token for registry token resolver
	PutAuth map[string]string              `json:"put_auth,omitempty"` // host → raw token, legacy put store
	Access  map[string]registryAccessToken `json:"access,omitempty"`   // host → cached registry JWT
}

// resolveRegistryEndpoints reads the four registry URLs from env/params with
// production defaults. Empty/invalid URLs are skipped — that registry stays
// unconfigured and SetupRegistries omits it.
func resolveRegistryEndpoints(params map[string]any, env map[string]string) []RegistryEndpoint {
	envKeys := map[Registry]string{
		RegistryNPM:   "PUTNAMI_REGISTRY_NPM_URL",
		RegistryGomod: "PUTNAMI_REGISTRY_GOMOD_URL",
		RegistryOCI:   "PUTNAMI_REGISTRY_OCI_URL",
		RegistryPut:   "PUTNAMI_REGISTRY_PUT_URL",
	}
	paramKeys := map[Registry][]string{
		RegistryNPM:   {"registry-npm-url", "registryNpmUrl"},
		RegistryGomod: {"registry-gomod-url", "registryGomodUrl"},
		RegistryOCI:   {"registry-oci-url", "registryOciUrl"},
		RegistryPut:   {"registry-put-url", "registryPutUrl"},
	}
	out := make([]RegistryEndpoint, 0, len(AllRegistries))
	for _, r := range AllRegistries {
		raw := clicore.FirstString(
			clicore.StringParam(params, paramKeys[r]...),
			clicore.EnvGet(env, envKeys[r]),
			defaultRegistryURL[r],
		)
		raw = clicore.TrimURL(raw)
		if raw == "" {
			continue
		}
		host := hostFromURL(raw)
		if host == "" {
			continue
		}
		out = append(out, RegistryEndpoint{Registry: r, URL: raw, Host: host})
	}
	return out
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Host != "" {
		return u.Host
	}
	// Tolerate "host.example" without scheme.
	if u.Path != "" && !strings.Contains(u.Path, "/") {
		return u.Path
	}
	return ""
}

func registriesStatePath(env map[string]string) string {
	return filepath.Join(clicore.PutnamiHome(env), "registries.json")
}

// readRegistriesState returns nil, nil when the file does not yet exist —
// a fresh login has no prior state to revoke.
func readRegistriesState(env map[string]string) (*RegistriesState, error) {
	data, err := os.ReadFile(registriesStatePath(env))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Treat an empty file as "no state", the same as a missing file. Writes are
	// atomic (see WriteRegistriesState), so a zero-byte file should not occur;
	// this guards against a residual truncated file left by an older CLI rather
	// than crashing every reader that races it.
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var s RegistriesState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("registries.json: %w", err)
	}
	return &s, nil
}

// WriteRegistriesState persists the per-host registries.json state, sorted by
// registry for a stable diff and written atomically with 0o600 permissions so
// a concurrent reader never observes a truncated file.
func WriteRegistriesState(env map[string]string, s *RegistriesState) error {
	file := registriesStatePath(env)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sort.SliceStable(s.Keys, func(i, j int) bool { return s.Keys[i].Registry < s.Keys[j].Registry })
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// Write atomically via a temp file + rename. registries.json is rewritten
	// by every `cloud token` refresh and read concurrently by every publish
	// job; a plain truncating write leaves a window where a reader observes an
	// empty file and fails with "registries.json: unexpected end of JSON input".
	// os.Rename over the target is atomic, so a reader always sees either the
	// old or the new complete file, never a truncated one.
	tmp, err := os.CreateTemp(dir, "registries-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup; a no-op once the rename below succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, file)
}

func removeRegistriesState(env map[string]string) error {
	err := os.Remove(registriesStatePath(env))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
