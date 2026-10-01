package security

import (
	"crypto/subtle"

	phttp "go.putnami.dev/http"
	identity "go.putnami.dev/protocol/identity/schema"
)

// APIKeyConfig configures API key authentication.
type APIKeyConfig struct {
	// Keys is the set of valid API keys.
	Keys []string

	// Header is the HTTP header to read the key from.
	// Defaults to "X-Api-Key".
	Header string

	// Subject is the subject claim set on successful authentication.
	// Defaults to "apikey".
	Subject string

	// Scopes are the scopes granted to API key clients.
	Scopes []string
}

// APIKey returns an identity resolver middleware that authenticates
// requests using a static API key sent via an HTTP header.
//
// Usage:
//
//	server.Use(security.APIKey(security.APIKeyConfig{
//	    Keys:   []string{"my-secret-key"},
//	    Scopes: []string{"ingest"},
//	}))
func APIKey(cfg APIKeyConfig) phttp.Middleware {
	if cfg.Header == "" {
		cfg.Header = "X-Api-Key"
	}
	if cfg.Subject == "" {
		cfg.Subject = string(identity.PrincipalKindApiKey)
	}

	keys := make([][]byte, len(cfg.Keys))
	for i, k := range cfg.Keys {
		keys[i] = []byte(k)
	}

	return IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		key := ctx.Header(cfg.Header)
		if key == "" {
			return nil
		}
		keyBytes := []byte(key)
		matched := false
		for _, valid := range keys {
			if subtle.ConstantTimeCompare(keyBytes, valid) == 1 {
				matched = true
			}
		}
		if !matched {
			return nil
		}
		return &phttp.Claims{
			Subject:  cfg.Subject,
			ClientID: "apikey-client",
			Scopes:   cfg.Scopes,
		}
	})
}
