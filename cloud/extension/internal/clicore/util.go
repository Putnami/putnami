package clicore

import (
	"os"
	"strings"
)

// EnvGet reads key from env, falling back to the process environment when env
// is nil. This lets commands run with an injected env map under test while
// using the real environment in production.
func EnvGet(env map[string]string, key string) string {
	if env == nil {
		return os.Getenv(key)
	}
	return env[key]
}

// URLPathEscape percent-encodes the characters that would otherwise break a
// value embedded in a URL path segment.
func URLPathEscape(value string) string {
	replacer := strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23", " ", "%20")
	return replacer.Replace(value)
}

// ShortDigest trims an optional "sha256:" prefix and truncates to 12 hex chars
// for display.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// TokenSource describes where a client fetches a per-user bearer at run time.
// Exactly one of Command / URL is populated: Command runs an argv and uses the
// trimmed stdout as the token; URL fetches the trimmed response body. The same
// shape backs both the build cache config (.putnami/cache.json `token`) and
// each registry key's `token` recipe (registries.json keys[].token) — it is
// always a token *source*, never a secret. Note that registries.json itself is
// still private 0o600 state because it may carry resolver credentials outside
// keys[].
type TokenSource struct {
	Command []string `json:"command,omitempty"`
	URL     string   `json:"url,omitempty"`
}
