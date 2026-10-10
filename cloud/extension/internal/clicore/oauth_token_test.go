package clicore

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestPostTokenEndpointSendsTheGrantAsJSON pins the token endpoint leg
// (clientgen.external.json, RFC 6749 §3.2): one POST of the grant body with the
// JSON headers and the unified User-Agent, the answer decoded as data.
func TestPostTokenEndpointSendsTheGrantAsJSON(t *testing.T) {
	var got map[string]any
	var method, contentType, accept, userAgent string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method = req.Method
		contentType = req.Header.Get("Content-Type")
		accept = req.Header.Get("Accept")
		userAgent = req.Header.Get("User-Agent")
		raw, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("grant body is not JSON: %v", err)
		}
		return responseWith(http.StatusOK, `{"access_token":"at","token_type":"Bearer","expires_in":3600}`), nil
	})}
	response, err := PostTokenEndpoint(client, "https://auth.example/token", map[string]any{
		"grant_type": "refresh_token", "refresh_token": "rt", "client_id": "putnami-cli",
	}, []int{http.StatusOK, http.StatusBadRequest})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || contentType != "application/json" || accept != "application/json" || userAgent == "" {
		t.Fatalf("request = %s content-type=%q accept=%q user-agent=%q", method, contentType, accept, userAgent)
	}
	if got["grant_type"] != "refresh_token" || got["refresh_token"] != "rt" || got["client_id"] != "putnami-cli" {
		t.Fatalf("grant body = %v", got)
	}
	if ValueString(response, "access_token") != "at" {
		t.Fatalf("response = %v", response)
	}
}

// TestPostTokenEndpointReadsAnAllowedErrorAsData covers RFC 6749 §5.2: a grant
// that lists 400 reads the error answer as data, while a status it did not
// list is a CLI error carrying error_description, 401 exiting ExitAuth.
func TestPostTokenEndpointReadsAnAllowedErrorAsData(t *testing.T) {
	status := http.StatusBadRequest
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(status, `{"error":"invalid_grant","error_description":"refresh token expired"}`), nil
	})}
	response, err := PostTokenEndpoint(client, "https://auth.example/token", map[string]any{"grant_type": "refresh_token"}, []int{http.StatusOK, http.StatusBadRequest})
	if err != nil {
		t.Fatal(err)
	}
	if ValueString(response, "error") != "invalid_grant" {
		t.Fatalf("response = %v", response)
	}

	status = http.StatusUnauthorized
	_, err = PostTokenEndpoint(client, "https://auth.example/token", map[string]any{"grant_type": "refresh_token"}, []int{http.StatusOK})
	if err == nil || ExitCode(err) != ExitAuth {
		t.Fatalf("err = %v (exit %d), want ExitAuth", err, ExitCode(err))
	}
	if want := "request failed for https://auth.example/token: refresh token expired"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

// TestAuthEndpointsReadsTheDiscoveryDocument pins the discovery leg (OpenID
// Connect Discovery 1.0 §4): the issuer's provider configuration names the
// endpoints, and an issuer without one falls back to the conventional paths.
func TestAuthEndpointsReadsTheDiscoveryDocument(t *testing.T) {
	var requested string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = req.Method + " " + req.URL.String()
		return responseWith(http.StatusOK, `{"issuer":"https://auth.example","token_endpoint":"https://auth.example/oauth/token","device_authorization_endpoint":"https://auth.example/oauth/device","userinfo_endpoint":"https://auth.example/oauth/userinfo","revocation_endpoint":"https://auth.example/oauth/revoke"}`), nil
	})}
	endpoints := AuthEndpoints(map[string]any{"auth-url": "https://auth.example/"}, map[string]string{}, client, "")
	if requested != "GET https://auth.example/.well-known/openid-configuration" {
		t.Fatalf("requested %q", requested)
	}
	if endpoints.TokenURL != "https://auth.example/oauth/token" || endpoints.DeviceAuthorizationURL != "https://auth.example/oauth/device" ||
		endpoints.UserinfoURL != "https://auth.example/oauth/userinfo" || endpoints.RevocationURL != "https://auth.example/oauth/revoke" {
		t.Fatalf("endpoints = %+v", endpoints)
	}

	missing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusNotFound, `{"error":"not_found"}`), nil
	})}
	fallback := AuthEndpoints(map[string]any{"auth-url": "https://auth.example"}, map[string]string{}, missing, "")
	if fallback.TokenURL != "https://auth.example/token" || fallback.DeviceAuthorizationURL != "https://auth.example/device/authorize" ||
		fallback.UserinfoURL != "https://auth.example/userinfo" || fallback.RevocationURL != "https://auth.example/revoke" {
		t.Fatalf("fallback endpoints = %+v", fallback)
	}
}
