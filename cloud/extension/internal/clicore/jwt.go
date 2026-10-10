package clicore

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// ClaimedWorkspaceID reads the workspace identifier from the issuer's nested
// scope_ref claim. The OAuth2 standard "scope" string claim stays untouched
// alongside; the resource scope lives in "scope_ref" so the two don't collide.
func ClaimedWorkspaceID(claims map[string]any) string {
	nested, _ := claims["scope_ref"].(map[string]any)
	if nested == nil {
		return ""
	}
	id, _ := nested["workspace_id"].(string)
	return id
}

// DecodeJWT base64url-decodes a JWT's payload into its claims, returning an
// empty map for any malformed token (the CLI only reads non-security-critical
// display/scope claims).
func DecodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return map[string]any{}
	}
	return claims
}
