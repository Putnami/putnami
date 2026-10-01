package http

import (
	"strings"

	identity "go.putnami.dev/protocol/identity/schema"
)

// Claims holds authenticated user claims with typed fields for standard
// OAuth2/OIDC claims and an Extra map for custom claims.
type Claims struct {
	// Subject is the user identifier (JWT "sub" claim).
	Subject string
	// Issuer identifies the token issuer (JWT "iss" claim).
	Issuer string
	// ClientID is the OAuth2 client identifier.
	ClientID string
	// Roles holds the user's assigned roles.
	Roles []string
	// Scopes holds the user's granted scopes.
	Scopes []string
	// Extra holds additional custom claims not covered by the typed fields.
	Extra map[string]any
}

// ClaimsFromMap creates a Claims struct from a map[string]any,
// extracting well-known keys into typed fields.
func ClaimsFromMap(m map[string]any) *Claims {
	c := &Claims{Extra: make(map[string]any)}
	for k, v := range m {
		switch k {
		case string(identity.ClaimNameSub):
			if s, ok := v.(string); ok {
				c.Subject = s
			}
		case string(identity.ClaimNameIss):
			if s, ok := v.(string); ok {
				c.Issuer = s
			}
		case string(identity.ClaimNameClientId):
			if s, ok := v.(string); ok {
				c.ClientID = s
			}
		case string(identity.ClaimNameRoles):
			c.Roles = toStringSlice(v)
		case string(identity.ClaimNameScope):
			c.Scopes = toStringSlice(v)
		default:
			c.Extra[k] = v
		}
	}
	return c
}

// HasRole returns true if the user has the given role.
func (c *Claims) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HasScope returns true if the user has the given scope.
func (c *Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Get returns a claim value by key. Checks typed fields first, then Extra.
func (c *Claims) Get(key string) any {
	switch key {
	case string(identity.ClaimNameSub):
		return c.Subject
	case string(identity.ClaimNameIss):
		return c.Issuer
	case string(identity.ClaimNameClientId):
		return c.ClientID
	case string(identity.ClaimNameRoles):
		return c.Roles
	case string(identity.ClaimNameScope):
		return c.Scopes
	default:
		return c.Extra[key]
	}
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []string:
		return val
	case []any:
		result := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case string:
		if val == "" {
			return nil
		}
		return strings.Fields(val)
	default:
		return nil
	}
}
