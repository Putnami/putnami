package clicore

import (
	"net/http"
	"os"
)

// UserAgentEnvVar is exported into every extension job's environment by the
// Putnami CLI. It carries a full,
// already-sanitized User-Agent string such as "putnami-cli/<version>", which
// outbound HTTP from this extension stamps verbatim so its control-plane,
// registry, auth, and download traffic is attributable to the unified CLI
// identity instead of the stdlib "Go-http-client/1.1" default.
const UserAgentEnvVar = "PUTNAMI_CLI_USER_AGENT"

// FallbackUserAgent is used when UserAgentEnvVar is unset — e.g. the extension
// binary invoked outside a CLI job. Mirrors the framework useragent package
// and the first-party TypeScript/Go extensions.
const FallbackUserAgent = "putnami-cli/dev"

// CLIUserAgent returns the User-Agent this extension stamps on outbound HTTP.
func CLIUserAgent() string {
	if ua := os.Getenv(UserAgentEnvVar); ua != "" {
		return ua
	}
	return FallbackUserAgent
}

// SetUserAgent stamps the unified CLI User-Agent on req, but only when the
// header is absent so a User-Agent already set by a presigned grant or transfer
// header is never clobbered.
func SetUserAgent(req *http.Request) {
	if req == nil || req.Header.Get("User-Agent") != "" {
		return
	}
	req.Header.Set("User-Agent", CLIUserAgent())
}

// userAgentTransport stamps the unified CLI User-Agent on every request that
// doesn't already carry one. It hardens the shared HTTP client so a future
// request builder that forgets SetUserAgent cannot silently regress to the
// stdlib default — the same belt-and-suspenders the framework applied to its
// remote-cache client.
type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Header.Get("User-Agent") == "" {
		// RoundTrip must not mutate the caller's request — clone before stamping.
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", CLIUserAgent())
	}
	return base.RoundTrip(req)
}

// WithUserAgent returns a copy of client whose Transport stamps the unified CLI
// User-Agent when absent. A nil client is treated as http.DefaultClient and the
// global default is never mutated. Wrapping is idempotent.
func WithUserAgent(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	if _, ok := client.Transport.(userAgentTransport); ok {
		return client
	}
	wrapped := *client
	wrapped.Transport = userAgentTransport{base: client.Transport}
	return &wrapped
}
