package extension

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.putnami.dev/sdk/extension/privatebroker"
)

// AllowInsecureRegistryEnv lets users explicitly accept an http:// resolver
// URL. The default is to reject anything that isn't https so a tampered
// PUTNAMI_REGISTRY_URL cannot quietly downgrade extension/template/CLI
// downloads to a MITM-able channel.
const AllowInsecureRegistryEnv = "PUTNAMI_ALLOW_INSECURE_REGISTRY"

// ValidateRegistryURL returns nil when rawURL is safe to fetch registry
// artifacts from. https:// is always accepted. http:// is accepted only
// for loopback hosts (localhost, 127.0.0.1, ::1) — the routine dev case —
// or when the user opted in via PUTNAMI_ALLOW_INSECURE_REGISTRY=1.
// Other schemes are rejected outright.
func ValidateRegistryURL(rawURL string) error {
	if _, err := privatebroker.FromEnv(PrivatePutRegistryURLEnv, "/put"); err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid registry URL %q: %w", rawURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		if os.Getenv(AllowInsecureRegistryEnv) == "1" {
			return nil
		}
		return fmt.Errorf(
			"registry URL %q uses plaintext http://; refusing to fetch over an unauthenticated channel. "+
				"Switch to https://, or set %s=1 to override",
			rawURL, AllowInsecureRegistryEnv,
		)
	default:
		return fmt.Errorf("registry URL %q has unsupported scheme %q (only https and http are accepted)", rawURL, u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// RedactRegistryURL reduces a registry URL to the parts that are safe to print:
// scheme, host and path. Userinfo, query and fragment are dropped, because both
// carry credentials — PUTNAMI_REGISTRY_URL may embed `user:token@`, and a
// private registry may sign the query.
//
// It exists because the two things a reader needs to diagnose a failed fetch —
// which endpoint answered, and with what — sit next to the one thing they must
// never see. Dropping rather than marking keeps the printed value a URL a
// reader can paste into curl.
//
// A value that does not parse returns the empty string rather than itself: a
// URL that cannot be parsed cannot be proven credential-free, so callers print
// nothing instead of guessing.
func RedactRegistryURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}
