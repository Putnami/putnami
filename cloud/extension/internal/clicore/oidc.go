package clicore

import (
	"net/http"
	"strings"
)

// AuthEndpointSet is the resolved set of OIDC/OAuth2 endpoints for an issuer.
type AuthEndpointSet struct {
	Issuer                 string
	DeviceAuthorizationURL string
	TokenURL               string
	UserinfoURL            string
	RevocationURL          string
}

// AuthEndpoints resolves the auth issuer's endpoints, preferring its
// OpenID-configuration document and falling back to conventional paths.
func AuthEndpoints(params map[string]any, env map[string]string, client *http.Client, fallback string) AuthEndpointSet {
	issuer := AuthBaseURL(params, env, fallback)
	discovery, _ := discoverOpenIDConfiguration(client, issuer)
	return AuthEndpointSet{
		Issuer:                 issuer,
		DeviceAuthorizationURL: FirstString(ValueString(discovery, "device_authorization_endpoint"), issuer+"/device/authorize"),
		TokenURL:               FirstString(ValueString(discovery, "token_endpoint"), issuer+"/token"),
		UserinfoURL:            FirstString(ValueString(discovery, "userinfo_endpoint"), issuer+"/userinfo"),
		RevocationURL:          FirstString(ValueString(discovery, "revocation_endpoint"), issuer+"/revoke"),
	}
}

// discoverOpenIDConfiguration reads the issuer's provider configuration
// (OpenID Connect Discovery 1.0 §4). It is an external contract listed in
// clientgen.external.json: the document's shape is the standard's.
func discoverOpenIDConfiguration(client *http.Client, issuer string) (map[string]any, error) {
	target := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, RequestBuildError(target, err)
	}
	return SendJSON(client, req, []int{http.StatusOK})
}

// AuthBaseURL resolves the auth issuer base URL from --auth-url, the
// PUTNAMI_AUTH_URL / PUTNAMI_AUTH_ISSUER env vars, or the fallback/default.
func AuthBaseURL(params map[string]any, env map[string]string, fallback string) string {
	if fallback == "" {
		fallback = DefaultAuthURL
	}
	return TrimURL(FirstString(StringParam(params, "auth-url", "authUrl"), EnvGet(env, "PUTNAMI_AUTH_URL"), EnvGet(env, "PUTNAMI_AUTH_ISSUER"), fallback))
}

// ControlPlaneBaseURL resolves the control-plane base URL from
// --control-plane-url, the PUTNAMI_CLOUD_API_URL / PUTNAMI_CONTROL_PLANE_URL env
// vars, or the fallback/default.
func ControlPlaneBaseURL(params map[string]any, env map[string]string, fallback string) string {
	if fallback == "" {
		fallback = DefaultControlPlaneURL
	}
	return TrimURL(FirstString(StringParam(params, "control-plane-url", "controlPlaneUrl"), EnvGet(env, "PUTNAMI_CLOUD_API_URL"), EnvGet(env, "PUTNAMI_CONTROL_PLANE_URL"), fallback))
}

// TrimURL strips trailing slashes from a base URL.
func TrimURL(value string) string {
	return strings.TrimRight(value, "/")
}
