package remotecache

import (
	"net/http"
	"os"
)

// This is a package-local mirror of the unified-CLI User-Agent helper. The
// framework's remote-cache client stamped outbound requests via its
// go.putnami.dev/tooling/cli/internal/useragent package, which is unimportable
// from this module (it lives under the framework CLI's internal/ tree). The
// extension binary already mirrors the same helper in package cloudcli
// (setUserAgent/withUserAgent); this copy keeps the remote-cache client
// self-contained without reaching across the package boundary. It carries no
// token/presence/compression logic — only the trivial UA stamp — so it does not
// duplicate any of the single-sourced cache behavior.

// userAgentEnvVar is exported into every extension job's environment by the
// Putnami CLI. It carries a full,
// already-sanitized User-Agent such as "putnami-cli/<version>".
const userAgentEnvVar = "PUTNAMI_CLI_USER_AGENT"

// fallbackUserAgent is used when userAgentEnvVar is unset — e.g. the binary
// invoked outside a CLI job.
const fallbackUserAgent = "putnami-cli/dev"

// cliUserAgent returns the User-Agent this client stamps on outbound HTTP.
func cliUserAgent() string {
	if ua := os.Getenv(userAgentEnvVar); ua != "" {
		return ua
	}
	return fallbackUserAgent
}

// setUserAgent stamps the unified CLI User-Agent on req, but only when the
// header is absent so a User-Agent already set by a presigned grant or transfer
// header is never clobbered.
func setUserAgent(req *http.Request) {
	if req == nil || req.Header.Get("User-Agent") != "" {
		return
	}
	req.Header.Set("User-Agent", cliUserAgent())
}
