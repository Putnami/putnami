// Package identitycli holds the identity-domain command implementations for the
// @putnami/cloud CLI extension: the OAuth2 device-authorization login flow and
// the whoami identity readout. It depends only on the shared clicore toolkit —
// no other domain (registry/distribution, delivery, …) leaks in, so the auth
// mechanism stays a generic subdomain. Cross-domain login follow-up (e.g.
// provisioning registry credentials) is the caller's orchestration, layered on
// top of the LoginResult this package returns.
package identitycli

import (
	"net/http"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// User is the resolved CLI identity, merged from the OIDC userinfo endpoint and
// the access-token claims (userinfo wins, claims are the fallback).
type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func userIdentity(userinfo map[string]any, claims map[string]any) User {
	return User{
		ID:       clicore.FirstString(clicore.ValueString(userinfo, "sub"), clicore.StringValue(claims["sub"])),
		Email:    clicore.FirstString(clicore.ValueString(userinfo, "email"), clicore.StringValue(claims["email"])),
		Name:     clicore.FirstString(clicore.ValueString(userinfo, "name"), clicore.StringValue(claims["name"])),
		Provider: clicore.FirstString(clicore.ValueString(userinfo, "provider"), clicore.StringValue(claims["provider"])),
	}
}

// fetchUserInfo reads the signed-in user's claims from the issuer's UserInfo
// endpoint with the access token as a bearer (OpenID Connect Core 1.0 §5.3).
// The endpoint is an external contract listed in clientgen.external.json; its
// URL comes from the issuer's discovery document.
func fetchUserInfo(client *http.Client, endpoints clicore.AuthEndpointSet, accessToken string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, endpoints.UserinfoURL, nil)
	if err != nil {
		return nil, clicore.RequestBuildError(endpoints.UserinfoURL, err)
	}
	for name, value := range clicore.BearerHeaders(accessToken) {
		req.Header.Set(name, value)
	}
	return clicore.SendJSON(client, req, []int{http.StatusOK})
}

// loginSuccessMessage is the sentence `putnami cloud login` ends with, in the
// wording of SignedInAs: "Signed in as dev@example.com."
func loginSuccessMessage(u User) string {
	sentence := SignedInAs(clicore.FirstString(u.Email, u.Name, u.ID))
	return strings.ToUpper(sentence[:1]) + sentence[1:] + "."
}

func deviceInstructions(device map[string]any, verificationURI, userCode string, includeCompleteLink bool) []string {
	lines := []string{
		"",
		"  To authenticate, visit:",
		"    " + verificationURI,
		"",
		"  Enter code: " + userCode,
		"",
	}
	if complete := clicore.ValueString(device, "verification_uri_complete"); includeCompleteLink && complete != "" {
		lines = append(lines, "  Or open this link directly:", "    "+complete, "")
	}
	return lines
}
