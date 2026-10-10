package clicore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.putnami.dev/client"
	authserverclient "go.putnami.dev/cloud/clients/auth-server/go"
)

// StoredToken is the on-disk OAuth credential the CLI persists at
// ~/.putnami/auth.json: the base session tokens plus a per-workspace cache of
// scoped access tokens.
type StoredToken struct {
	AccessToken     string                           `json:"access_token"`            //nolint:gosec // G117: OAuth2 token response; field/JSON names are spec-mandated
	RefreshToken    string                           `json:"refresh_token,omitempty"` //nolint:gosec // G117: OAuth2 token response; field/JSON names are spec-mandated
	IDToken         string                           `json:"id_token,omitempty"`
	TokenType       string                           `json:"token_type"`
	ExpiresAt       string                           `json:"expires_at,omitempty"`
	Scope           string                           `json:"scope,omitempty"`
	Issuer          string                           `json:"issuer,omitempty"`
	ClientID        string                           `json:"client_id,omitempty"`
	WorkspaceAccess map[string]StoredWorkspaceAccess `json:"workspace_access,omitempty"`
}

// StoredWorkspaceAccess is a cached workspace-scoped access token keyed by
// workspace id under StoredToken.WorkspaceAccess.
type StoredWorkspaceAccess struct {
	AccessToken string `json:"access_token"` //nolint:gosec // G117: OAuth2 token response; field/JSON names are spec-mandated
	TokenType   string `json:"token_type"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// ActiveAuth returns a fresh base session token, refreshing it when expired.
func ActiveAuth(params map[string]any, env map[string]string, ioctx IO) (*StoredToken, error) {
	auth, err := ReadAuth(env, true)
	if err != nil {
		return nil, err
	}
	if AccessTokenFresh(auth.AccessToken, auth.ExpiresAt, ioctx.Now()) {
		return auth, nil
	}
	if auth.RefreshToken == "" {
		return nil, NewError("session expired; run putnami cloud login", ExitAuth)
	}
	next, base, err := RefreshStoredAuthWithRetry(params, env, ioctx, auth, "")
	if err != nil {
		return nil, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = base.RefreshToken
	}
	next.WorkspaceAccess = base.WorkspaceAccess
	if err := WriteAuth(next, env); err != nil {
		return nil, err
	}
	return next, nil
}

// UserSessionError is the reason a read that needs a signed-in user gives
// when the command runs on PUTNAMI_CLOUD_TOKEN with no stored session: the
// machine token is valid, and ActiveAuth never accepts it. Any other error is
// returned as is.
func UserSessionError(err error, env map[string]string) error {
	if err == nil || ExitCode(err) != ExitAuth || !machineTokenOnly(env) {
		return err
	}
	return NewError("needs a signed-in user; PUTNAMI_CLOUD_TOKEN cannot read it", ExitAuth)
}

// UserSessionStatus is the unknown status of a read that needs a signed-in
// user and failed with err. A missing or expired session names
// `putnami cloud login` as the fix; on PUTNAMI_CLOUD_TOKEN alone there is no
// fix to name.
func UserSessionStatus(id, title string, err error, env map[string]string) StatusNode {
	fix := ""
	if ExitCode(err) == ExitAuth && !machineTokenOnly(env) {
		fix = "putnami cloud login"
	}
	return UnknownStatus(id, title, UserSessionError(err, env), fix)
}

// machineTokenOnly reports whether the command runs on PUTNAMI_CLOUD_TOKEN
// with no stored user session.
func machineTokenOnly(env map[string]string) bool {
	if EnvGet(env, "PUTNAMI_CLOUD_TOKEN") == "" {
		return false
	}
	auth, err := ReadAuth(env, false)
	return err != nil || auth == nil || auth.AccessToken == ""
}

// WorkspaceAuth returns an access token scoped to workspaceID, reusing the
// per-workspace cache and refreshing/exchanging as needed. A caller may provide
// PUTNAMI_CLOUD_TOKEN explicitly for a target-bound, non-interactive workspace
// operation. That bearer is never accepted by ActiveAuth, persisted, refreshed,
// or exposed to machine/operator management paths. An empty workspaceID falls
// back to ActiveAuth.
func WorkspaceAuth(params map[string]any, env map[string]string, ioctx IO, workspaceID string) (*StoredToken, error) {
	if workspaceID == "" {
		return ActiveAuth(params, env, ioctx)
	}
	if token := EnvGet(env, "PUTNAMI_CLOUD_TOKEN"); token != "" {
		fields := strings.Fields(token)
		if len(fields) != 1 || fields[0] != token {
			return nil, NewError("PUTNAMI_CLOUD_TOKEN must contain exactly one bearer token", ExitAuth)
		}
		return &StoredToken{AccessToken: token, TokenType: "Bearer"}, nil
	}
	auth, err := ReadAuth(env, true)
	if err != nil {
		return nil, err
	}
	if cached := cachedWorkspaceAuth(auth, workspaceID, ioctx.Now()); cached != nil {
		return cached, nil
	}
	if AccessTokenFresh(auth.AccessToken, auth.ExpiresAt, ioctx.Now()) && ClaimedWorkspaceID(DecodeJWT(auth.AccessToken)) == workspaceID {
		return auth, nil
	}
	if auth.RefreshToken == "" {
		return nil, NewError("session is not refreshable; run putnami cloud login", ExitAuth)
	}
	scoped, base, err := RefreshStoredAuthWithRetry(params, env, ioctx, auth, workspaceID)
	if err != nil {
		return nil, err
	}
	if claimed := ClaimedWorkspaceID(DecodeJWT(scoped.AccessToken)); claimed != workspaceID {
		next := refreshedBaseAuth(scoped, base)
		if err := WriteAuth(&next, env); err != nil {
			return nil, err
		}
		return nil, NewError(
			fmt.Sprintf("auth server returned a token without scope_ref.workspace_id for configured workspace %s; re-run putnami cloud login and try again", workspaceID),
			ExitAuth,
		)
	}
	next := *base
	if scoped.RefreshToken != "" {
		next.RefreshToken = scoped.RefreshToken
	} else {
		scoped.RefreshToken = base.RefreshToken
	}
	if next.WorkspaceAccess == nil {
		next.WorkspaceAccess = map[string]StoredWorkspaceAccess{}
	}
	next.WorkspaceAccess[workspaceID] = StoredWorkspaceAccess{
		AccessToken: scoped.AccessToken,
		TokenType:   scoped.TokenType,
		ExpiresAt:   scoped.ExpiresAt,
		Scope:       scoped.Scope,
	}
	if err := WriteAuth(&next, env); err != nil {
		return nil, err
	}
	return scoped, nil
}

// MintWorkspaceScopedAuth mints a least-privilege, access-only bearer for a
// workspace through the OAuth api-key grant. The ordinary session credential is
// used only to create the short-lived backing key; it is never returned as the
// purpose bearer, and neither the base access token nor WorkspaceAccess cache can
// satisfy this mint.
//
// The backing key is revoked after the JWT exchange (and is also given a bounded
// expiry in case cleanup fails). Resource servers verify the returned JWT
// offline, so revoking the key prevents another exchange without invalidating
// the already-minted access token before its normal expiry.
func MintWorkspaceScopedAuth(
	params map[string]any,
	env map[string]string,
	ioctx IO,
	workspaceID, clientID, requestedScope string,
) (*StoredToken, error) {
	return MintWorkspaceScopedAuthContext(context.Background(), params, env, ioctx, workspaceID, clientID, requestedScope)
}

// apiKeyRevokeTimeout bounds the revoke of a scoped mint's backing key.
const apiKeyRevokeTimeout = 5 * time.Second

// MintWorkspaceScopedAuthContext is MintWorkspaceScopedAuth for a caller that
// may stop waiting. ctx bounds only the key creation and the exchange.
//
// The sign-in refresh ignores ctx: the auth server rotates the refresh token
// and revokes the old one in the same step, so a refresh cut short after that
// step would leave auth.json with a revoked refresh token and sign the user
// out. The key revoke ignores ctx too: it runs on ioctx's client with its own
// apiKeyRevokeTimeout, so a mint stopped after the key exists still revokes it.
func MintWorkspaceScopedAuthContext(
	ctx context.Context,
	params map[string]any,
	env map[string]string,
	ioctx IO,
	workspaceID, clientID, requestedScope string,
) (*StoredToken, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	clientID = strings.TrimSpace(clientID)
	requestedScope = strings.Join(strings.Fields(requestedScope), " ")
	if workspaceID == "" || clientID == "" || requestedScope == "" {
		return nil, NewError("workspace-scoped token mint requires workspace, client, and scope", ExitUsage)
	}

	session, err := ActiveAuth(params, env, ioctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mintClient := contextBoundClient(ctx, ioctx.Client)
	authBaseURL := AuthBaseURL(params, env, session.Issuer)
	keys, err := AuthServerClient(authBaseURL, mintClient)
	if err != nil {
		return nil, err
	}
	// Do not send allowed_client_ids here: that API field accepts internal
	// oauth_clients row UUIDs, not the public clientID available to the CLI. The
	// key is instead bounded to ten minutes, restricted to requestedScope, and
	// revoked immediately after its one exchange. The grant then applies the
	// strict key ∩ client ∩ requested intersection server-side.
	expiresAt := ioctx.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	created, err := keys.CreateApikeys(client.WithForwardedUserToken(ctx, session.AccessToken), authserverclient.CreateApikeysInput{
		Body: authserverclient.CreateApikeysBody{
			Name:          fmt.Sprintf("cli:%s:%s", clientID, workspaceID),
			AllowedScopes: &requestedScope,
			WorkspaceId:   &workspaceID,
			ExpiresAt:     &expiresAt,
		},
	})
	if err != nil {
		return nil, RequestError(authBaseURL+"/apikeys", err)
	}
	keyID := created.Id
	if keyID != "" {
		defer revokeScopedAPIKey(ioctx, authBaseURL, session.AccessToken, keyID)
	}
	rawKey := created.RawToken
	if keyID == "" || rawKey == "" {
		return nil, NewError("auth server returned an incomplete scoped-token api key", ExitAPI)
	}

	endpoints := AuthEndpoints(params, env, mintClient, session.Issuer)
	response, err := PostTokenEndpoint(mintClient, endpoints.TokenURL, map[string]any{
		"grant_type":   "urn:putnami:params:oauth:grant-type:api-key",
		"api_key":      rawKey,
		"client_id":    clientID,
		"scope":        requestedScope,
		"workspace_id": workspaceID,
	}, []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized})
	if err != nil {
		return nil, err
	}
	if oauthError := ValueString(response, "error"); oauthError != "" {
		description := FirstString(ValueString(response, "error_description"), oauthError)
		return nil, NewOAuthTokenError(oauthError, description, ExitAuth)
	}

	scoped := StoredAuth(response, endpoints.Issuer, clientID, ioctx.Now())
	if fields := strings.Fields(scoped.AccessToken); len(fields) != 1 || fields[0] != scoped.AccessToken {
		return nil, NewError("auth server returned an empty or malformed scoped access token", ExitAPI)
	}
	claims := DecodeJWT(scoped.AccessToken)
	if audience := ValueString(claims, "aud"); audience != clientID {
		return nil, NewError(
			fmt.Sprintf("auth server returned a scoped token for audience %q, expected %q", audience, clientID),
			ExitAuth,
		)
	}
	if claimed := ClaimedWorkspaceID(claims); claimed != workspaceID {
		return nil, NewError(
			fmt.Sprintf("auth server returned a scoped token for workspace %q, expected %q", claimed, workspaceID),
			ExitAuth,
		)
	}
	grantedScope := ValueString(claims, "scope")
	if !scopeContainsAll(grantedScope, requestedScope) {
		return nil, NewError(
			fmt.Sprintf("auth server returned a scoped token without required scopes %q", requestedScope),
			ExitAuth,
		)
	}
	if scoped.Scope == "" {
		scoped.Scope = grantedScope
	}
	return scoped, nil
}

// revokeScopedAPIKey revokes a scoped mint's backing key on ioctx's client,
// bounded by apiKeyRevokeTimeout, and warns when it cannot. A key auth-server
// no longer knows (404) needs no revoke.
func revokeScopedAPIKey(ioctx IO, authBaseURL, accessToken, keyID string) {
	revokeClient := *WithUserAgent(ioctx.Client)
	if revokeClient.Timeout <= 0 || revokeClient.Timeout > apiKeyRevokeTimeout {
		revokeClient.Timeout = apiKeyRevokeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiKeyRevokeTimeout)
	defer cancel()
	err := RevokeAPIKey(ctx, &revokeClient, authBaseURL, accessToken, keyID)
	if err != nil && ioctx.Stderr != nil {
		ioctx.Stderr("warning: failed to revoke ephemeral scoped-token api key " + keyID + ": " + err.Error())
	}
}

func scopeContainsAll(granted, required string) bool {
	grantedSet := make(map[string]struct{}, len(strings.Fields(granted)))
	for _, scope := range strings.Fields(granted) {
		grantedSet[scope] = struct{}{}
	}
	for _, scope := range strings.Fields(required) {
		if _, ok := grantedSet[scope]; !ok {
			return false
		}
	}
	return true
}

func refreshedBaseAuth(refreshed, base *StoredToken) StoredToken {
	next := *refreshed
	if next.RefreshToken == "" {
		next.RefreshToken = base.RefreshToken
	}
	next.WorkspaceAccess = base.WorkspaceAccess
	return next
}

// RefreshStoredAuthWithRetry refreshes auth's token, tolerating the
// parallel-rotation race (when a sibling process rotates the shared refresh
// token first). It returns the usable token plus the base token whose refresh
// token produced it.
func RefreshStoredAuthWithRetry(params map[string]any, env map[string]string, ioctx IO, auth *StoredToken, workspaceID string) (*StoredToken, *StoredToken, error) {
	return refreshStoredAuthWithRetry(params, env, ioctx, auth, workspaceID, true)
}

// RefreshStoredAuthFreshWithRetry refreshes auth without ever satisfying an
// invalid_grant rotation race from a cached access token. It is the strict
// sibling used when a command promises a newly minted credential: a sibling
// process may have rotated the refresh token first, but its access bearer is not
// proof that this call completed a fresh mint. The retry may use the rotated
// refresh token, preserving concurrent rotation without surfacing cached state.
func RefreshStoredAuthFreshWithRetry(params map[string]any, env map[string]string, ioctx IO, auth *StoredToken, workspaceID string) (*StoredToken, *StoredToken, error) {
	return refreshStoredAuthWithRetry(params, env, ioctx, auth, workspaceID, false)
}

func refreshStoredAuthWithRetry(params map[string]any, env map[string]string, ioctx IO, auth *StoredToken, workspaceID string, allowCached bool) (*StoredToken, *StoredToken, error) {
	next, err := refreshStoredAuth(params, env, ioctx, auth, workspaceID)
	if err == nil {
		return next, auth, nil
	}
	if !IsOAuthTokenError(err, "invalid_grant") {
		return nil, auth, err
	}
	lastErr := err
	base := auth
	// Parallel publish jobs can all start with the same refresh token. When
	// one process rotates it first, the others may receive invalid_grant just
	// before the rotated auth.json is visible locally; poll briefly and reuse
	// the freshly cached access token instead of rotating again.
	for _, delay := range []time.Duration{
		0,
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
	} {
		if delay > 0 {
			time.Sleep(delay)
		}
		latest, readErr := ReadAuth(env, true)
		if readErr != nil || latest.RefreshToken == "" {
			continue
		}
		if allowCached {
			if reusable := reusableAuthAfterRefreshRace(latest, workspaceID, ioctx.Now()); reusable != nil {
				return reusable, latest, nil
			}
		}
		if latest.RefreshToken == base.RefreshToken {
			continue
		}
		next, retryErr := refreshStoredAuth(params, env, ioctx, latest, workspaceID)
		if retryErr == nil {
			return next, latest, nil
		}
		lastErr = retryErr
		if !IsOAuthTokenError(retryErr, "invalid_grant") {
			return nil, latest, retryErr
		}
		base = latest
	}
	return nil, base, lastErr
}

func reusableAuthAfterRefreshRace(auth *StoredToken, workspaceID string, now time.Time) *StoredToken {
	if auth == nil {
		return nil
	}
	if workspaceID != "" {
		if cached := cachedWorkspaceAuth(auth, workspaceID, now); cached != nil {
			return cached
		}
		if AccessTokenFresh(auth.AccessToken, auth.ExpiresAt, now) && ClaimedWorkspaceID(DecodeJWT(auth.AccessToken)) == workspaceID {
			return auth
		}
		return nil
	}
	if AccessTokenFresh(auth.AccessToken, auth.ExpiresAt, now) {
		return auth
	}
	return nil
}

func refreshStoredAuth(params map[string]any, env map[string]string, ioctx IO, auth *StoredToken, workspaceID string) (*StoredToken, error) {
	endpoints := AuthEndpoints(params, env, ioctx.Client, auth.Issuer)
	clientID := StringParam(params, "client-id", "clientId")
	if clientID == "" {
		clientID = FirstString(auth.ClientID, "putnami-cli")
	}
	body := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": auth.RefreshToken,
		"client_id":     clientID,
	}
	if workspaceID != "" {
		body["workspace_id"] = workspaceID
	}
	response, err := PostTokenEndpoint(ioctx.Client, endpoints.TokenURL, body, []int{http.StatusOK, http.StatusBadRequest})
	if err != nil {
		return nil, err
	}
	if oauthError := ValueString(response, "error"); oauthError != "" {
		description := FirstString(ValueString(response, "error_description"), oauthError)
		return nil, NewOAuthTokenError(oauthError, description, ExitAuth)
	}
	return StoredAuth(response, endpoints.Issuer, clientID, ioctx.Now()), nil
}

type oauthTokenError struct {
	*ExitError
	oauthError string
}

func (e *oauthTokenError) Unwrap() error { return e.ExitError }

// NewOAuthTokenError wraps an OAuth2 error response as an ExitError, appending a
// re-login hint for invalid_grant.
func NewOAuthTokenError(oauthError, description string, code int) error {
	if oauthError == "invalid_grant" && !strings.Contains(strings.ToLower(description), "putnami cloud login") {
		description += "; run `putnami cloud login` to refresh cloud credentials"
	}
	return &oauthTokenError{
		ExitError:  newExitError(description, code),
		oauthError: oauthError,
	}
}

// IsOAuthTokenError reports whether err is an OAuth token error carrying the
// given OAuth2 error code (e.g. "invalid_grant").
func IsOAuthTokenError(err error, oauthError string) bool {
	var tokenErr *oauthTokenError
	return errors.As(err, &tokenErr) && tokenErr.oauthError == oauthError
}

func cachedWorkspaceAuth(auth *StoredToken, workspaceID string, now time.Time) *StoredToken {
	if auth.WorkspaceAccess == nil {
		return nil
	}
	cached, ok := auth.WorkspaceAccess[workspaceID]
	if !ok || !AccessTokenFresh(cached.AccessToken, cached.ExpiresAt, now) {
		return nil
	}
	if ClaimedWorkspaceID(DecodeJWT(cached.AccessToken)) != workspaceID {
		return nil
	}
	return &StoredToken{
		AccessToken:  cached.AccessToken,
		RefreshToken: auth.RefreshToken,
		TokenType:    FirstString(cached.TokenType, "Bearer"),
		ExpiresAt:    cached.ExpiresAt,
		Scope:        cached.Scope,
		Issuer:       auth.Issuer,
		ClientID:     auth.ClientID,
	}
}

// AccessTokenFresh reports whether token is non-empty and expiresAt is at least
// 30s in the future.
func AccessTokenFresh(token, expiresAt string, now time.Time) bool {
	if token == "" || expiresAt == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false
	}
	return exp.After(now.Add(30 * time.Second))
}

// StoredAuth builds a StoredToken from an OAuth2 token response, computing the
// absolute expiry from expires_in.
func StoredAuth(response map[string]any, issuer, clientID string, now time.Time) *StoredToken {
	expiresAt := ""
	if expiresIn := ValueFloat(response, "expires_in"); expiresIn > 0 {
		expiresAt = now.Add(time.Duration(expiresIn) * time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	tokenType := ValueString(response, "token_type")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return &StoredToken{
		AccessToken:  ValueString(response, "access_token"),
		RefreshToken: ValueString(response, "refresh_token"),
		IDToken:      ValueString(response, "id_token"),
		TokenType:    tokenType,
		ExpiresAt:    expiresAt,
		Scope:        ValueString(response, "scope"),
		Issuer:       issuer,
		ClientID:     clientID,
	}
}
