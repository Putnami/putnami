package clicore

import (
	"net/http"
)

// PostTokenEndpoint sends one request to the issuer's OAuth 2.0 token
// endpoint (RFC 6749 §3.2) and returns its JSON answer. Every grant the CLI
// uses goes through it:
//
//   - refresh_token (RFC 6749 §6), the stored-session refresh;
//   - urn:ietf:params:oauth:grant-type:device_code (RFC 8628 §3.4), the
//     `putnami cloud login` poll;
//   - urn:putnami:params:oauth:grant-type:api-key, an extension grant
//     (RFC 6749 §4.5) that trades an API key for a scoped access token.
//
// The endpoint is an external contract, not a first-party provider contract:
// its URL comes from the issuer's discovery document (AuthEndpoints) and its
// shape is the standard's, so it is listed in clientgen.external.json rather
// than generated. allowStatuses lets a grant read the RFC 6749 §5.2 error
// answer (400, and 401 for invalid_client) as data.
func PostTokenEndpoint(client *http.Client, tokenURL string, form map[string]any, allowStatuses []int) (map[string]any, error) {
	body, err := JSONBody(form)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, body)
	if err != nil {
		return nil, RequestBuildError(tokenURL, err)
	}
	return SendJSON(client, req, allowStatuses)
}

// PostRevocationEndpoint asks the issuer's revocation_endpoint to revoke one
// token (OAuth 2.0 Token Revocation, RFC 7009 §2.1): `putnami cloud logout`
// revokes the stored refresh token there. The endpoint is an external
// contract listed in clientgen.external.json; its URL comes from the issuer's
// discovery document. allowStatuses lets logout read a refusal as data and
// still delete the local session.
func PostRevocationEndpoint(client *http.Client, revocationURL string, form map[string]any, allowStatuses []int) (map[string]any, error) {
	body, err := JSONBody(form)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, revocationURL, body)
	if err != nil {
		return nil, RequestBuildError(revocationURL, err)
	}
	return SendJSON(client, req, allowStatuses)
}
