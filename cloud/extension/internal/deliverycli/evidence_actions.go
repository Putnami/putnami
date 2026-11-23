// The GitHub Actions upload of `putnami cloud evidence-report`: a job that
// names the Putnami organization it uploads for, in PUTNAMI_ORGANIZATION, and
// holds no PUTNAMI_CLOUD_TOKEN exchanges its own OpenID Connect ID token for a
// short-lived ingest token, so its repository stores no Putnami secret.
//
// The exchange is the auth-server's RFC 8693 token exchange for the public
// client delivery-ingest: it mints a five-minute access token of scope
// delivery.ingest, audience delivery-api, only when that organization trusts
// the token's repository_id, or else its repository_owner_id, for
// observation. The ID token's audience is the auth-server's base URL, the
// audience its GitHub Actions verifier expects.
package deliverycli

import (
	"net/http"
	"net/url"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// actionsIDTokenURLEnv and actionsIDTokenTokenEnv are what GitHub sets in
	// a job that holds permissions id-token: write.
	actionsIDTokenURLEnv   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	actionsIDTokenTokenEnv = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
	// evidenceOrganizationEnv names the organization a job uploads for: an
	// id, not a secret.
	evidenceOrganizationEnv = "PUTNAMI_ORGANIZATION"

	deliveryIngestClientID = "delivery-ingest"
	tokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an RFC 8693 grant type, not a credential
	idTokenTokenType       = "urn:ietf:params:oauth:token-type:id_token"       //nolint:gosec // G101: an RFC 8693 token type, not a credential
	// untrustedRepositoryRefusal is in the auth-server's error_description
	// when the organization trusts neither the job's repository, which is
	// how it words a GitLab project too, nor its owner.
	untrustedRepositoryRefusal = "does not trust this repository"
)

// actionsOrganization returns the organization a GitHub Actions job uploads
// for, "" outside such a job or without PUTNAMI_ORGANIZATION.
func actionsOrganization(env map[string]string) string {
	if strings.TrimSpace(clicore.EnvGet(env, "GITHUB_ACTIONS")) != "true" {
		return ""
	}
	return strings.TrimSpace(clicore.EnvGet(env, evidenceOrganizationEnv))
}

// actionsIngestToken requests the job's ID token and exchanges it for the
// organization's ingest token. No error message carries a token.
func actionsIngestToken(params map[string]any, env map[string]string, ioctx clicore.IO, organization string) (string, error) {
	requestURL := strings.TrimSpace(clicore.EnvGet(env, actionsIDTokenURLEnv))
	requestToken := strings.TrimSpace(clicore.EnvGet(env, actionsIDTokenTokenEnv))
	if requestURL == "" || requestToken == "" {
		return "", clicore.NewError("cloud evidence-report: "+evidenceOrganizationEnv+" is set, but this GitHub Actions job cannot request an ID token: give the job `permissions: id-token: write` (GitHub withholds one from most pull requests from a fork), or set "+evidenceTokenEnv, clicore.ExitAuth)
	}
	audience := clicore.AuthBaseURL(params, env, "")
	idToken, err := requestActionsIDToken(ioctx.Client, requestURL, requestToken, audience)
	if err != nil {
		return "", err
	}
	repository := clicore.FirstString(strings.TrimSpace(clicore.EnvGet(env, "GITHUB_REPOSITORY")), "this repository")
	return exchangeIngestToken(params, env, ioctx, idTokenExchange{
		ci:           "GitHub Actions",
		organization: organization,
		idToken:      idToken,
		trustHint:    "Trust " + repository + ", or its owner,",
	})
}

// idTokenExchange is one CI job's ID token exchange: the CI that issued the
// token, the organization the job uploads for, the token, and the trust that
// an untrusted-repository refusal names.
type idTokenExchange struct {
	ci           string
	organization string
	idToken      string
	trustHint    string
}

// exchangeIngestToken exchanges a CI job's ID token at the auth-server for
// the organization's ingest token. No error message carries a token.
func exchangeIngestToken(params map[string]any, env map[string]string, ioctx clicore.IO, exchange idTokenExchange) (string, error) {
	tokenURL := clicore.AuthEndpoints(params, env, ioctx.Client, "").TokenURL
	answer, err := clicore.PostTokenEndpoint(ioctx.Client, tokenURL, map[string]any{
		"grant_type":         tokenExchangeGrantType,
		"client_id":          deliveryIngestClientID,
		"subject_token":      exchange.idToken,
		"subject_token_type": idTokenTokenType,
		"organization_id":    exchange.organization,
	}, []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized})
	if err != nil {
		return "", clicore.NewError("cloud evidence-report: the "+exchange.ci+" ID token exchange at "+tokenURL+" failed: "+err.Error(), clicore.ExitAuth)
	}
	if refusal := clicore.ValueString(answer, "error"); refusal != "" {
		reason := clicore.FirstString(clicore.ValueString(answer, "error_description"), refusal)
		if untrustedRefusal(reason) {
			return "", clicore.NewError("cloud evidence-report: Putnami Cloud refused the "+exchange.ci+" ID token exchange: "+reason+". "+exchange.trustHint+" for observation in the organization "+exchange.organization+" on its observe page in putnami.app, or set "+evidenceOrganizationEnv+" to the id of the organization that trusts it, then run the job again", clicore.ExitAuth)
		}
		return "", clicore.NewError("cloud evidence-report: Putnami Cloud refused the "+exchange.ci+" ID token exchange at "+tokenURL+": "+reason, clicore.ExitAuth)
	}
	token := strings.TrimSpace(clicore.ValueString(answer, "access_token"))
	if token == "" {
		return "", clicore.NewError("cloud evidence-report: the "+exchange.ci+" ID token exchange at "+tokenURL+" answered no access_token", clicore.ExitAuth)
	}
	return token, nil
}

// untrustedRefusal reports whether the auth-server refused the exchange
// because the organization trusts neither the job's repository, or project,
// nor its owner.
func untrustedRefusal(reason string) bool {
	return strings.Contains(reason, untrustedRepositoryRefusal)
}

// requestActionsIDToken asks the job's ID token endpoint for a token of the
// audience, with the request token GitHub gave the job as its bearer.
func requestActionsIDToken(client *http.Client, requestURL, requestToken, audience string) (string, error) {
	target, err := url.Parse(requestURL)
	if err != nil || target.Scheme != "https" || target.Host == "" {
		return "", clicore.NewError("cloud evidence-report: "+actionsIDTokenURLEnv+" is not an https URL", clicore.ExitAuth)
	}
	query := target.Query()
	query.Set("audience", audience)
	target.RawQuery = query.Encode()
	endpoint := target.Scheme + "://" + target.Host + target.Path
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		return "", clicore.RequestBuildError(endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	answer, err := clicore.SendJSON(client, req, []int{http.StatusOK})
	if err != nil {
		return "", clicore.NewError("cloud evidence-report: GitHub Actions answered no ID token at "+endpoint+": "+redactQuery(err.Error(), target.RawQuery), clicore.ExitAuth)
	}
	idToken := strings.TrimSpace(clicore.ValueString(answer, "value"))
	if idToken == "" {
		return "", clicore.NewError("cloud evidence-report: GitHub Actions answered no ID token at "+endpoint, clicore.ExitAuth)
	}
	return idToken, nil
}

// redactQuery removes the request URL's query from a message: GitHub's
// endpoint keeps it opaque, and nothing in it helps the reader.
func redactQuery(message, rawQuery string) string {
	if rawQuery == "" {
		return message
	}
	return strings.ReplaceAll(message, "?"+rawQuery, "")
}
