package clicore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"go.putnami.dev/client"
	authserverclient "go.putnami.dev/cloud/clients/auth-server/go"
)

// AuthServerClient binds auth-server's generated client to authBaseURL, the
// issuer the signed-in session came from (AuthBaseURL). Every call presents the
// signed-in user's own bearer: the contract's `user` profile, whose source is
// the forwarded user token, so the caller passes the bearer per call through
// client.WithForwardedUserToken. The HTTP client keeps the IO seam.
func AuthServerClient(authBaseURL string, httpClient *http.Client) (*authserverclient.AuthClient, error) {
	return NewServiceClient[authserverclient.AuthClient](authserverclient.RegisterAuthClient,
		ServiceBindingFor(authBaseURL, withAuthServerCompat(httpClient)))
}

// APIKeyURL is the address of one API key on auth-server. Errors name it the
// way the hand-written requests did.
func APIKeyURL(authBaseURL, id string) string {
	return TrimURL(authBaseURL) + "/apikeys/" + URLPathEscape(id)
}

// APIKeyState is what auth-server's GET /apikeys/{id} tells the CLI about a
// key. NotFound reports a key auth-server no longer knows.
type APIKeyState struct {
	RevokedAt  string
	ExpiresAt  string
	LastUsedAt string
	NotFound   bool
}

// ReadAPIKey reads one of the signed-in user's API keys with the user's own
// bearer. A 404 is an answer, not an error: the key is gone.
func ReadAPIKey(ctx context.Context, httpClient *http.Client, authBaseURL, accessToken, id string) (APIKeyState, error) {
	target := APIKeyURL(authBaseURL, id)
	keys, err := AuthServerClient(authBaseURL, httpClient)
	if err != nil {
		return APIKeyState{}, err
	}
	key, err := keys.GetApikeys(client.WithForwardedUserToken(ctx, accessToken),
		authserverclient.GetApikeysInput{Path: authserverclient.GetApikeysPath{Id: id}})
	if err != nil {
		if ServiceStatus(err) == http.StatusNotFound {
			return APIKeyState{NotFound: true}, nil
		}
		return APIKeyState{}, RequestError(target, err)
	}
	return APIKeyState{
		RevokedAt:  Deref(key.RevokedAt),
		ExpiresAt:  Deref(key.ExpiresAt),
		LastUsedAt: Deref(key.LastUsedAt),
	}, nil
}

// RevokeAPIKey revokes one of the signed-in user's API keys with the user's
// own bearer. A 404 counts as done: the key is already gone, which is what the
// caller asked for.
func RevokeAPIKey(ctx context.Context, httpClient *http.Client, authBaseURL, accessToken, id string) error {
	keys, err := AuthServerClient(authBaseURL, httpClient)
	if err != nil {
		return err
	}
	_, err = keys.DeleteApikeys(client.WithForwardedUserToken(ctx, accessToken),
		authserverclient.DeleteApikeysInput{Path: authserverclient.DeleteApikeysPath{Id: id}})
	if err == nil || ServiceStatus(err) == http.StatusNotFound {
		return nil
	}
	return RequestError(APIKeyURL(authBaseURL, id), err)
}

// Compatibility: the transport below adapts two auth-server answers to the
// generated client.
//
//   - Refusals. auth-server's /apikeys refusals
//     carry the OAuth pair {error, error_description} and no `message`, while
//     the generated client carries only `message` as the refusal's free text.
//     The transport copies error_description, else error, into `message` so a
//     refused key call keeps naming its cause. Remove that half once every
//     auth-server the CLI reaches answers /apikeys refusals with `message`.
//   - Null members. auth-server sends an unset
//     key column (expires_at, revoked_at, last_used_at, tenant_id, ...) as
//     JSON null, and the generated client, whose contract types them as
//     optional strings, refuses null as "invalid JSON response". The transport
//     drops the null members of a 2xx JSON object, which the contract reads as
//     absent. Remove that half once every auth-server the CLI reaches omits
//     unset members.

// refusalBodyLimit bounds the body the transport rewrites. A larger body
// passes through untouched.
const refusalBodyLimit = 64 << 10

func withAuthServerCompat(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Transport = authServerCompat{base: base.Transport}
	return &wrapped
}

type authServerCompat struct{ base http.RoundTripper }

func (t authServerCompat) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	var rewrite func([]byte) []byte
	switch {
	case resp.StatusCode >= http.StatusBadRequest:
		rewrite = oauthRefusalWithMessage
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		rewrite = withoutNullMembers
	default:
		return resp, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, refusalBodyLimit+1))
	if readErr != nil || len(data) > refusalBodyLimit {
		// The client reads the bytes already taken, then the rest of the body,
		// and meets the read error itself.
		resp.Body = replayedBody{Reader: io.MultiReader(bytes.NewReader(data), resp.Body), Closer: resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()
	data = rewrite(data)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	if resp.Header != nil && resp.Header.Get("Content-Length") != "" {
		resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
	}
	return resp, nil
}

// withoutNullMembers returns body without the top-level members whose value
// is null. A body that is not a JSON object, or has no null member, is
// returned unchanged.
func withoutNullMembers(body []byte) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return body
	}
	dropped := false
	for name, value := range object {
		if string(bytes.TrimSpace(value)) == "null" {
			delete(object, name)
			dropped = true
		}
	}
	if !dropped {
		return body
	}
	out, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return out
}

type replayedBody struct {
	io.Reader
	io.Closer
}

// oauthRefusalWithMessage returns body with `message` set from
// error_description, else error. A body that is not a JSON object, already
// has `message`, or names no cause is returned unchanged.
func oauthRefusalWithMessage(body []byte) []byte {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return body
	}
	if _, ok := envelope["message"]; ok {
		return body
	}
	for _, member := range []string{"error_description", "error"} {
		var text string
		if json.Unmarshal(envelope[member], &text) != nil || strings.TrimSpace(text) == "" {
			continue
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return body
		}
		envelope["message"] = encoded
		out, err := json.Marshal(envelope)
		if err != nil {
			return body
		}
		return out
	}
	return body
}
