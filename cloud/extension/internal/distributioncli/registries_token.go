package distributioncli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	regproto "go.putnami.dev/protocol/registry"
)

// Registry token minting knobs. DefaultRegistryTokenClientID is the OAuth2
// client id used when neither the env var nor the flag below overrides it.
const (
	apiKeyGrantType               = "urn:putnami:params:oauth:grant-type:api-key" //nolint:gosec // G101: not a credential, an OAuth2 grant-type URN string
	DefaultRegistryTokenClientID  = "distribution"
	registryTokenClientIDEnvName  = "PUTNAMI_REGISTRY_TOKEN_CLIENT_ID"
	registryTokenClientIDFlagName = "registry-token-client-id"
)

type resolvedRegistryToken struct {
	Token    string
	Endpoint RegistryEndpoint
}

// RegistryToken prints one short-lived aud=distribution bearer for the logged-in
// user and one registry KIND. The whole user contract is `--for npm|go|oci|put`
// (or `--host <host>`, which selects the same thing by endpoint): a token is
// issued for a USER and a REGISTRY KIND, never for a package or an owner
// workspace. Client-supplied scope overrides are rejected before credentials
// are resolved. The requested OAuth scope is the bare protocol marker, which
// authorizes nothing on its own — the registry derives what this user may read
// or write per namespace from the IAM-backed namespace roles.
//
// Target flags are gone on purpose. `--owner-workspace`, `--package`, and
// `--action` are rejected rather than ignored so a caller that still passes them
// learns the authority moved server-side instead of silently receiving a token
// with different authority than it asked for.
func RegistryToken(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if name := retiredRegistryTokenTarget(params); name != "" {
		return clicore.NewError(
			"cloud token does not accept --"+name+"; a registry token is issued for your user and one registry kind, and the registry derives per-namespace authority from your workspace membership",
			clicore.ExitUsage,
		)
	}
	if clicore.Truthy(clicore.Param(params, "opaque")) {
		return clicore.NewError(
			"--opaque is not supported; use a short-lived aud=distribution bearer",
			clicore.ExitUsage,
		)
	}
	endpoint, err := resolveRegistryTokenEndpoint(params, env)
	if err != nil {
		return err
	}
	token, err := mintUserRegistryToken(params, workspaceRoot, env, ioctx, endpoint)
	if err != nil {
		return err
	}
	if !regproto.ValidBearer(token) {
		return clicore.NewError("auth server returned an empty or malformed registry bearer", clicore.ExitAuth)
	}
	if clicore.Truthy(clicore.Param(params, "materialize")) {
		if err := materializeRegistryLease(env, endpoint, token); err != nil {
			return fmt.Errorf("materialize %s registry credential: %w", endpoint.Registry, err)
		}
		clicore.WriteResult(
			map[string]any{"registry": endpoint.Registry, "host": endpoint.Host},
			params,
			ioctx,
			fmt.Sprintf("Materialized a short-lived %s credential for one immediate native invocation.", endpoint.Registry),
		)
		return nil
	}
	// The token-source seam consumes exactly one bare bearer line. No diagnostic
	// may share stdout, and no error below includes the secret.
	ioctx.Stdout(token)
	return nil
}

// retiredTokenTargetFlags are the target coordinates `cloud token` used to
// accept while registry tokens were target-bound. They are refused, not
// ignored: the registry now decides per namespace and package from the user's
// IAM workspace membership, so honoring them would promise a narrowing this
// command no longer performs.
var retiredTokenTargetFlags = []string{"owner-workspace", "package", "action", "channel", "scope"}

func retiredRegistryTokenTarget(params map[string]any) string {
	for _, name := range retiredTokenTargetFlags {
		camel := registryTokenFlagCamel(name)
		if clicore.Param(params, name, camel) != nil {
			return name
		}
	}
	return ""
}

func registryTokenFlagCamel(flag string) string {
	parts := strings.Split(flag, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// resolveRegistryTokenEndpoint selects the endpoint from either form of the
// user contract: `--host <host>` (registry-token) or `--for <kind>` (token).
// Both resolve to one of the four known registries; anything else is a usage
// error rather than a silently host-wide credential.
func resolveRegistryTokenEndpoint(params map[string]any, env map[string]string) (RegistryEndpoint, error) {
	state, err := readRegistriesState(env)
	if err != nil {
		return RegistryEndpoint{}, err
	}
	if host := registryTokenHost(params); host != "" {
		endpoint, ok := registryEndpointForHost(params, env, state, host)
		if !ok {
			return RegistryEndpoint{}, clicore.NewError(
				"unknown registry host "+host+"; run `putnami cloud login` or set the registry URL",
				clicore.ExitUsage,
			)
		}
		return endpoint, nil
	}
	purpose := registryTokenPurposeParam(params)
	registry, valid := RegistryFromTokenPurpose(purpose)
	if !valid {
		return RegistryEndpoint{}, clicore.NewError(
			"cloud token --for expects one of npm, go, oci, put",
			clicore.ExitUsage,
		)
	}
	endpoint, ok := registryEndpointForRegistry(params, env, state, registry)
	if !ok {
		return RegistryEndpoint{}, clicore.NewError(
			fmt.Sprintf("cloud token --for %s requires a known registry endpoint; run `putnami cloud login` or set the registry URL", registryTokenPurpose(registry)),
			clicore.ExitAuth,
		)
	}
	return endpoint, nil
}

// mintUserRegistryToken returns a bearer for the logged-in user and the
// endpoint's registry kind, reusing the per-host cache until shortly before
// expiry so a publish that resolves the recipe for every package does not mint
// once per package.
func mintUserRegistryToken(
	params map[string]any,
	workspaceRoot string,
	env map[string]string,
	ioctx clicore.IO,
	endpoint RegistryEndpoint,
) (string, error) {
	// A private runner broker substitutes its upstream bearer. The local
	// capability already held by this gated process must never be exchanged
	// through human OAuth or cached as a registry credential.
	if token := clicore.EnvGet(env, "PUTNAMI_CLOUD_TOKEN"); token != "" && privateRegistryBroker(endpoint.URL) {
		if !regproto.ValidBearer(token) {
			return "", clicore.NewError("private registry capability is invalid", clicore.ExitAuth)
		}
		return token, nil
	}
	clientID := registryTokenClientID(params, env)
	requestedScope := RegistryMarkerScope(endpoint.Registry)
	if requestedScope == "" {
		return "", clicore.NewError(
			fmt.Sprintf("registry token minting is not provided for the %s registry", endpoint.Registry),
			clicore.ExitUsage,
		)
	}
	now := nowOrDefault(ioctx.Now)()
	if token, ok, err := cachedRegistryAccessToken(env, endpoint.Host, clientID, requestedScope, now); err != nil {
		return "", err
	} else if ok {
		return token, nil
	}
	// The api-key grant needs a home workspace for the ephemeral backing key.
	// It bounds WHERE the key lives, not what the resulting token may reach:
	// the registry authorizes per namespace from the user's IAM membership.
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return "", err
	}
	workspaceID := strings.TrimSpace(clicore.StringValue(link["workspace_id"]))
	if workspaceID == "" {
		return "", clicore.NewError(
			"linked workspace is missing workspace_id; run `putnami cloud setup --workspace <id>`",
			clicore.ExitUsage,
		)
	}
	auth, err := clicore.MintWorkspaceScopedAuth(params, env, ioctx, workspaceID, clientID, requestedScope)
	if err != nil {
		return "", err
	}
	if auth == nil || auth.AccessToken == "" {
		return "", clicore.NewError("auth server returned an empty registry access token", clicore.ExitAPI)
	}
	if err := storeRegistryAccessToken(env, endpoint.Host, registryAccessToken{
		AccessToken: auth.AccessToken,
		TokenType:   clicore.FirstString(auth.TokenType, "Bearer"),
		ExpiresAt:   auth.ExpiresAt,
		Scope:       requestedScope,
		ClientID:    clientID,
	}); err != nil {
		return "", err
	}
	return auth.AccessToken, nil
}

func registryTokenClientID(params map[string]any, env map[string]string) string {
	return clicore.FirstString(
		clicore.StringParam(params, registryTokenClientIDFlagName, "registryTokenClientId", "token-client-id", "tokenClientId"),
		clicore.EnvGet(env, registryTokenClientIDEnvName),
		DefaultRegistryTokenClientID,
	)
}

// isDistributionLeaseBearer reports whether a stored registry credential is
// already a short-lived aud=distribution JWT (three dot-separated segments
// with a decodable claims payload) rather than a legacy opaque pkt_* api key.
// Such a bearer materialized into a native credential store by a trusted
// machine broker or `cloud token --materialize` must be sent as-is; feeding it
// to the legacy api-key exchange would be rejected (grant_type api-key expects
// an opaque key, not a JWT).
func isDistributionLeaseBearer(token string) bool {
	if strings.Count(token, ".") != 2 || strings.HasPrefix(token, "pkt_") {
		return false
	}
	return len(clicore.DecodeJWT(token)) > 0
}

// stalePutLease reports whether a stored Put credential is a lease bearer
// (isDistributionLeaseBearer) that is no longer fresh at now, and returns its
// exp claim. Freshness is the rule of the registry access cache,
// clicore.AccessTokenFresh: the lease must outlive now by more than its
// margin. An opaque key, and a lease without a usable numeric exp claim, are
// never stale: this function cannot judge them, so they keep their existing
// path.
func stalePutLease(token string, now time.Time) (time.Time, bool) {
	if !isDistributionLeaseBearer(token) {
		return time.Time{}, false
	}
	exp := clicore.ValueFloat(clicore.DecodeJWT(token), "exp")
	if exp <= 0 || exp >= math.MaxInt64 {
		return time.Time{}, false
	}
	expiresAt := time.Unix(int64(exp), 0).UTC()
	return expiresAt, !clicore.AccessTokenFresh(token, expiresAt.Format(time.RFC3339Nano), now)
}

func validDistributionProtocol(protocol string) bool {
	switch protocol {
	case "gomod", "npm", "put", "oci":
		return true
	default:
		return false
	}
}

// legacyRegistryExchangeRequestedScopes is retained only for already-persisted
// pkt_* credentials used by the site-content and OCI compatibility paths.
// It must never be used to mint a new key or serve the public token command.
const legacyRegistryExchangeRequestedScopes = "registry.package.read registry.package.publish registry.package.promote"

func exchangeRegistryTokenJWT(params map[string]any, env map[string]string, ioctx clicore.IO, endpoint RegistryEndpoint, apiKey string) (string, error) {
	clientID := registryTokenClientID(params, env)
	now := nowOrDefault(ioctx.Now)()
	if token, ok, err := cachedRegistryAccessToken(env, endpoint.Host, clientID, legacyRegistryExchangeRequestedScopes, now); err != nil {
		return "", err
	} else if ok {
		return token, nil
	}

	endpoints := clicore.AuthEndpoints(params, env, ioctx.Client, "")
	resp, err := clicore.PostTokenEndpoint(ioctx.Client, endpoints.TokenURL, map[string]any{
		"grant_type": apiKeyGrantType,
		"api_key":    apiKey,
		"client_id":  clientID,
		"scope":      legacyRegistryExchangeRequestedScopes,
	}, []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized})
	if err != nil {
		return "", err
	}
	if oauthError := clicore.ValueString(resp, "error"); oauthError != "" {
		description := clicore.FirstString(clicore.ValueString(resp, "error_description"), oauthError)
		if oauthError == "invalid_client" || oauthError == "unauthorized_client" {
			return "", clicore.NewError(
				fmt.Sprintf("auth server rejected legacy registry JWT exchange client %q: %s; deploy the auth distribution client seed or configure a native machine credential", clientID, description),
				clicore.ExitAuth,
			)
		}
		return "", clicore.NewOAuthTokenError(oauthError, description, clicore.ExitAuth)
	}
	accessToken := clicore.ValueString(resp, "access_token")
	if accessToken == "" {
		return "", clicore.NewError("auth server returned empty registry access token", clicore.ExitAPI)
	}
	if err := storeRegistryAccessToken(env, endpoint.Host, registryAccessTokenFromResponse(resp, accessToken, clientID, now)); err != nil {
		return "", err
	}
	return accessToken, nil
}

func cachedRegistryAccessToken(env map[string]string, host, clientID, requestedScope string, now time.Time) (string, bool, error) {
	cached, ok, err := cachedRegistryAccess(env, host, clientID, requestedScope, now)
	return cached.AccessToken, ok, err
}

// cachedRegistryAccess returns the cached access entry under key when it was
// minted for clientID and requestedScope and is still fresh at now.
func cachedRegistryAccess(env map[string]string, key, clientID, requestedScope string, now time.Time) (registryAccessToken, bool, error) {
	if key == "" {
		return registryAccessToken{}, false, nil
	}
	state, err := readRegistriesState(env)
	if err != nil {
		return registryAccessToken{}, false, err
	}
	if state == nil || state.Access == nil {
		return registryAccessToken{}, false, nil
	}
	cached, ok := state.Access[key]
	if !ok {
		return registryAccessToken{}, false, nil
	}
	if cached.ClientID != clientID || cached.Scope != requestedScope {
		return registryAccessToken{}, false, nil
	}
	if !clicore.AccessTokenFresh(cached.AccessToken, cached.ExpiresAt, now) {
		return registryAccessToken{}, false, nil
	}
	return cached, true, nil
}

func registryAccessTokenFromResponse(resp map[string]any, accessToken, clientID string, now time.Time) registryAccessToken {
	expiresAt := ""
	if expiresIn := clicore.ValueFloat(resp, "expires_in"); expiresIn > 0 {
		expiresAt = now.Add(time.Duration(expiresIn) * time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if expiresAt == "" {
		if exp := clicore.ValueFloat(clicore.DecodeJWT(accessToken), "exp"); exp > 0 {
			expiresAt = time.Unix(int64(exp), 0).UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	tokenType := clicore.ValueString(resp, "token_type")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	scope := clicore.ValueString(resp, "scope")
	if scope == "" {
		scope = legacyRegistryExchangeRequestedScopes
	}
	return registryAccessToken{
		AccessToken: accessToken,
		TokenType:   tokenType,
		ExpiresAt:   expiresAt,
		Scope:       scope,
		ClientID:    clientID,
	}
}

func storeRegistryAccessToken(env map[string]string, host string, cached registryAccessToken) error {
	if host == "" || cached.AccessToken == "" || cached.ExpiresAt == "" {
		return nil
	}
	return mutateRegistriesState(env, func(s *RegistriesState) {
		if s.Access == nil {
			s.Access = map[string]registryAccessToken{}
		}
		s.Access[host] = cached
	})
}

func removeRegistryAccess(state *RegistriesState, host string) {
	if state == nil || state.Access == nil {
		return
	}
	delete(state.Access, host)
	if len(state.Access) == 0 {
		state.Access = nil
	}
}

func registryTokenPurposeParam(params map[string]any) string {
	purpose := clicore.StringParam(params, "for")
	if purpose == "" && clicore.Truthy(clicore.Param(params, "for-registry", "forRegistry")) {
		return "registry"
	}
	return purpose
}

// registryTokenHost reads the seam's --host flag (registry.SeamHostFlag),
// tolerating a full URL so callers can pass either `go.putnami.dev` or
// `https://go.putnami.dev`.
func registryTokenHost(params map[string]any) string {
	raw := clicore.StringParam(params, regproto.SeamHostFlag)
	if strings.Contains(raw, "://") {
		return hostFromURL(raw)
	}
	return raw
}

func resolveRegistryTokenForRegistry(params map[string]any, env map[string]string, registry Registry) (resolvedRegistryToken, error) {
	state, err := readRegistriesState(env)
	if err != nil {
		return resolvedRegistryToken{}, err
	}
	endpoint, ok := registryEndpointForRegistry(params, env, state, registry)
	if !ok {
		return resolvedRegistryToken{}, clicore.NewError(
			fmt.Sprintf("cloud token --for %s requires a known registry endpoint; run `putnami cloud login` or set the registry URL", registryTokenPurpose(registry)),
			clicore.ExitAuth)
	}
	if token, err := StoredRegistryToken(env, state, endpoint.Host); err != nil || token != "" {
		return resolvedRegistryToken{Token: token, Endpoint: endpoint}, err
	}
	return resolvedRegistryToken{}, clicore.NewError(
		"no native registry credential for "+endpoint.Host+"; run `putnami cloud login` or configure the trusted machine credential",
		clicore.ExitAuth,
	)
}

// StoredRegistryToken reads the already-persisted bearer for host out of the
// per-registry store that owns it (registries.json, ~/.netrc or ~/.npmrc). It
// returns an empty token, not an error, when the host has no recorded key —
// "not logged in" is a normal state the caller resolves by minting.
func StoredRegistryToken(env map[string]string, state *RegistriesState, host string) (string, error) {
	if state == nil {
		return "", nil
	}
	ref, ok := registryKeyByHost(state, host)
	if !ok {
		return "", nil
	}
	var token string
	var err error
	switch ref.Registry {
	case RegistryPut:
		token = state.PutAuth[host]
	case RegistryGomod:
		token, err = readNetrcToken(env, host)
	case RegistryNPM:
		token = state.Auth[host]
		if token == "" {
			token, err = readNpmrcToken(env, host)
		}
	case RegistryOCI:
		token = state.Auth[host]
		if token == "" {
			token, err = readDockerToken(env, host)
		}
	default:
		return "", clicore.NewError(
			fmt.Sprintf("registry token resolution is not provided for the %s registry (%s)", ref.Registry, host),
			clicore.ExitUsage)
	}
	if err != nil {
		return "", err
	}
	return token, nil
}

func registryEndpointForHost(params map[string]any, env map[string]string, state *RegistriesState, host string) (RegistryEndpoint, bool) {
	if ref, ok := registryKeyByHost(state, host); ok {
		return registryEndpointFromRef(ref), true
	}
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		if endpoint.Host == host {
			return endpoint, true
		}
	}
	return RegistryEndpoint{}, false
}

func registryEndpointForRegistry(params map[string]any, env map[string]string, state *RegistriesState, registry Registry) (RegistryEndpoint, bool) {
	if ref, ok := registryKeyByRegistry(state, registry); ok {
		return registryEndpointFromRef(ref), true
	}
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		if endpoint.Registry == registry {
			return endpoint, true
		}
	}
	return RegistryEndpoint{}, false
}

func registryEndpointFromRef(ref KeyRef) RegistryEndpoint {
	rawURL := ref.URL
	if rawURL == "" && ref.Host != "" {
		rawURL = "https://" + ref.Host
	}
	if rawURL == "" {
		rawURL = defaultRegistryURL[ref.Registry]
	}
	host := ref.Host
	if host == "" {
		host = hostFromURL(rawURL)
	}
	return RegistryEndpoint{Registry: ref.Registry, URL: rawURL, Host: host}
}

// readNetrcToken returns the password field of the single-line `machine <host>
// login _token password <token>` record the gomod writer persists. A missing
// file or host is not an error — it returns an empty token so the caller emits
// the same "run login" guidance as every other unconfigured host. Only the
// single-line form the CLI writes is recognized (matching filterNetrcMachine).
func readNetrcToken(env map[string]string, host string) (string, error) {
	data, err := os.ReadFile(netrcPath(env))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "machine" || fields[1] != host {
			continue
		}
		for i := 2; i+1 < len(fields); i++ {
			if fields[i] == "password" {
				return fields[i+1], nil
			}
		}
	}
	return "", nil
}

// readNpmrcToken returns a legacy per-host `_authToken` from ~/.npmrc
// (`//<host>/:_authToken=<token>`). New logins no longer write this path; it
// remains as a compatibility fallback for existing installs. A missing file or
// host yields an empty token, not an error.
func readNpmrcToken(env map[string]string, host string) (string, error) {
	data, err := os.ReadFile(npmrcPath(env))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	prefix := "//" + host + "/:_authToken="
	for line := range strings.SplitSeq(string(data), "\n") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return token, nil
		}
	}
	return "", nil
}

// readDockerToken returns a legacy bearer from ~/.docker/config.json as
// auths[<host>].auth = base64("_token:<token>"). New logins no longer write
// this path; it remains as a compatibility fallback for existing installs. A
// missing file or host yields an empty token, not an error.
func readDockerToken(env map[string]string, host string) (string, error) {
	data, err := os.ReadFile(dockerConfigPath(env))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", dockerConfigPath(env), err)
	}
	entry, ok := cfg.Auths[host]
	if !ok || entry.Auth == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", fmt.Errorf("decode docker auth for %s: %w", host, err)
	}
	// auth is "<user>:<token>"; the writer uses "_token" as the user. Split on
	// the first colon so the token (which carries none) comes back intact.
	if _, token, found := strings.Cut(string(decoded), ":"); found {
		return token, nil
	}
	return "", nil
}

func privateRegistryBroker(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	address := net.ParseIP(parsed.Hostname())
	return address != nil && address.IsLoopback()
}
