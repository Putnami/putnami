package distributioncli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	regproto "go.putnami.dev/protocol/registry"
)

// providerReadAccessPrefix starts the registries.json access-cache key of the
// credential-provider read bearer. A per-host entry is keyed by a bare host;
// this key starts with '@' and holds spaces, so no host can collide with it.
// The hosts the bearer serves follow the prefix, so a bearer cached for one set
// of registry endpoints is never answered for another.
const providerReadAccessPrefix = "@credential-provider read "

// ProviderReadCredential is the developer-machine source of the engine's read
// credential, used when the user enables `--providers install`. It answers one
// short-lived aud=distribution bearer for the signed-in user whose scope names
// the four registry protocol markers, for the hosts of the four registry
// endpoints this CLI resolves. Each registry still decides per namespace from
// the user's workspace membership what the bearer may read.
//
// It answers nil and no error (absence, so the engine keeps its native
// credentials) when the user is not signed in, the checkout is not linked, or
// no registry endpoint resolves. The bearer is cached in registries.json until
// shortly before it expires, like the per-host registry tokens, and logout
// removes it with that file. ioctx must not write to stdout: the credential
// provider's protocol owns it.
//
// ctx bounds the mint's key creation and exchange (see
// clicore.MintWorkspaceScopedAuthContext), and a read whose ctx ended stores
// nothing, so a read the engine stopped waiting for never writes
// registries.json after it. The sign-in refresh and the key revoke do not
// follow ctx: a refresh cut short could sign the user out, and a stopped mint
// must still revoke its key.
func ProviderReadCredential(ctx context.Context, params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*regproto.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auth, err := clicore.ReadAuth(env, false)
	if err != nil {
		return nil, err
	}
	if auth == nil || (auth.AccessToken == "" && auth.RefreshToken == "") {
		return nil, nil
	}
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		// A checkout with no usable link is a usage condition, not a failure
		// of this source: the engine keeps its native credentials.
		if clicore.ExitCode(err) == clicore.ExitUsage {
			return nil, nil
		}
		return nil, err
	}
	workspaceID := strings.TrimSpace(clicore.StringValue(link["workspace_id"]))
	if workspaceID == "" {
		return nil, nil
	}
	hosts := providerReadHosts(params, env)
	if len(hosts) == 0 {
		return nil, nil
	}

	clientID := registryTokenClientID(params, env)
	scope := providerReadScope()
	key := providerReadAccessPrefix + strings.Join(hosts, " ")
	now := nowOrDefault(ioctx.Now)()
	access, cached, err := cachedRegistryAccess(env, key, clientID, scope, now)
	if err != nil {
		return nil, err
	}
	if !cached {
		minted, err := clicore.MintWorkspaceScopedAuthContext(ctx, params, env, ioctx, workspaceID, clientID, scope)
		if err != nil {
			return nil, err
		}
		if minted == nil || minted.AccessToken == "" {
			return nil, errors.New("the auth server returned an empty registry access token")
		}
		access = registryAccessToken{
			AccessToken: minted.AccessToken,
			TokenType:   clicore.FirstString(minted.TokenType, "Bearer"),
			ExpiresAt:   minted.ExpiresAt,
			Scope:       scope,
			ClientID:    clientID,
		}
		// The caller stopped waiting: it answered without this bearer, and a
		// later read may already be writing the same file.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := storeRegistryAccessToken(env, key, access); err != nil {
			return nil, err
		}
	}
	expiresAt, err := registryAccessExpiry(access)
	if err != nil {
		return nil, err
	}
	credential := &regproto.Credential{
		Bearer:    access.AccessToken,
		ExpiresAt: expiresAt.Format(time.RFC3339),
		Hosts:     hosts,
	}
	if err := regproto.ValidateCredential(*credential); err != nil {
		return nil, err
	}
	return credential, nil
}

// providerReadScope is the four registry protocol markers, sorted and
// space-joined: "go npm oci put".
func providerReadScope() string {
	markers := make([]string, 0, len(AllRegistries))
	for _, registry := range AllRegistries {
		if marker := RegistryMarkerScope(registry); marker != "" {
			markers = append(markers, marker)
		}
	}
	slices.Sort(markers)
	return strings.Join(slices.Compact(markers), " ")
}

// providerReadHosts is the sorted, unique, lowercase hosts of the registry
// endpoints this CLI resolves. A host the credential protocol cannot name is
// left out.
func providerReadHosts(params map[string]any, env map[string]string) []string {
	var hosts []string
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		host := strings.ToLower(endpoint.Host)
		if regproto.ValidCredentialHost(host) {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// registryAccessExpiry is the UTC expiry of a cached registry bearer: the
// stored expiry, else the bearer's exp claim. The fraction of a second is
// dropped, so the credential never outlives the bearer.
func registryAccessExpiry(access registryAccessToken) (time.Time, error) {
	if stored := strings.TrimSpace(access.ExpiresAt); stored != "" {
		if expiresAt, err := time.Parse(time.RFC3339Nano, stored); err == nil {
			return expiresAt.UTC().Truncate(time.Second), nil
		}
	}
	if exp := clicore.ValueFloat(clicore.DecodeJWT(access.AccessToken), "exp"); exp > 0 && exp < float64(1<<62) {
		return time.Unix(int64(exp), 0).UTC(), nil
	}
	return time.Time{}, errors.New("the registry access token carries no expiry")
}
