package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	registryproto "go.putnami.dev/protocol/registry"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/privatebroker"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/useragent"
)

// PutRegistryEcosystem is the ecosystem id whose workspace registries entry
// names the endpoint archive members are served from. The CLI, the extensions,
// the templates and the agent workflows are all archive members of that
// projection, so they all resolve through this one entry.
const PutRegistryEcosystem = "put"

// PutRegistryURLEnv overrides the put registry endpoint for one machine or one
// test. It is the fallback, not the source: a workspace that declares
// registries.put.registry has already answered the question for everyone who
// works on it.
const PutRegistryURLEnv = "PUTNAMI_REGISTRY_URL"

// DefaultPutRegistryURL is the endpoint used when neither the workspace nor the
// environment names one.
const DefaultPutRegistryURL = "https://put.putnami.dev"

// putRegistryEntry is the slice of the workspace `registries.put` entry the CLI
// reads. The ecosystem profile owns the entry's full shape, so the value stays
// raw in the workspace config and is decoded here, leniently: an entry shaped
// for a field the CLI does not know must not break an upgrade.
type putRegistryEntry struct {
	Registry string `json:"registry"`
}

// PrivatePutRegistryURLEnv is the invocation-owned archive transport endpoint.
const PrivatePutRegistryURLEnv = "PUTNAMI_REGISTRY_PUT_URL"

// ResolvePutRegistryURL uses the invocation broker when present, otherwise the
// workspace Put registry, PutRegistryURLEnv, or DefaultPutRegistryURL. The
// returned URL has no trailing slash.
func ResolvePutRegistryURL(registries map[string]json.RawMessage) string {
	// Invocation-owned transport overrides authored registry selection, exactly
	// like the SDK's native publishers. Malformed local routes are retained so
	// ValidateRegistryURL rejects them before credentials or network activity.
	if broker, err := privatebroker.FromEnv(PrivatePutRegistryURLEnv, "/put"); err != nil {
		return os.Getenv(PrivatePutRegistryURLEnv)
	} else if broker != nil {
		return broker.URL
	}
	if declared, ok := declaredPutRegistryURL(registries); ok {
		return declared
	}
	if fromEnv := strings.TrimSpace(os.Getenv(PutRegistryURLEnv)); fromEnv != "" {
		return strings.TrimRight(fromEnv, "/")
	}
	return DefaultPutRegistryURL
}

// declaredPutRegistryURL returns only an authored workspace registry. Callers
// that need to distinguish that native registry contract from the legacy
// PUTNAMI_REGISTRY_URL download mirror use it before applying environment
// fallbacks.
//
// A hosted run (runcredential.Hosted) reads no authored registry: the
// repository would choose where the pinned CLI and the extensions that run
// with the run credential come from. The runner's environment or the default
// registry decides instead.
func declaredPutRegistryURL(registries map[string]json.RawMessage) (string, bool) {
	if runcredential.Hosted() {
		return "", false
	}
	raw, ok := registries[PutRegistryEcosystem]
	if !ok {
		return "", false
	}
	var entry putRegistryEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false
	}
	declared := strings.TrimSpace(entry.Registry)
	if declared == "" {
		return "", false
	}
	return strings.TrimRight(declared, "/"), true
}

// WorkspacePutRegistryURL resolves the put registry for a workspace root,
// reading its configuration itself. An empty root, or a root that is not a
// workspace, falls back to the environment and then the default.
func WorkspacePutRegistryURL(workspaceRoot string) string {
	if strings.TrimSpace(workspaceRoot) == "" {
		return ResolvePutRegistryURL(nil)
	}
	cfg := wsproto.Load(workspaceRoot)
	if cfg == nil {
		return ResolvePutRegistryURL(nil)
	}
	return ResolvePutRegistryURL(cfg.Registries)
}

// WorkspaceDeclaredPutRegistryURL returns an authored native Put registry and
// does not consult environment fallbacks. A false result leaves callers free to
// preserve the legacy PUTNAMI_REGISTRY_URL /dl mirror contract.
func WorkspaceDeclaredPutRegistryURL(workspaceRoot string) (string, bool) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", false
	}
	cfg := wsproto.Load(workspaceRoot)
	if cfg == nil {
		return "", false
	}
	return declaredPutRegistryURL(cfg.Registries)
}

// ResolveRegistryToken is the credential seam an archive download authenticates
// through. It is a package var so a test can answer without a cloud on PATH.
var ResolveRegistryToken = registrycred.ResolveToken

// ResolveRegistryTokenWithCLI is the same host-only seam with the bootstrap
// executable explicit. Tests can supply a credential without executing a CLI.
var ResolveRegistryTokenWithCLI = registrycred.ResolveTokenWithCLI

// RegistryCredential answers a purpose-keyed credential for one request
// target. served is true only when the credential provider holds a credential
// whose hosts include target; bearer is then the value to send. An error is a
// provider refusal or failure: the request must not go out on another
// credential instead.
type RegistryCredential func(ctx context.Context, target *url.URL) (bearer string, served bool, err error)

// registryReadCredential is the read-purpose credential source the invocation
// enabled with `--providers install`; nil leaves every request on the
// host-keyed seam, or anonymous on a hosted run.
var registryReadCredential atomic.Pointer[RegistryCredential]

// InstallRegistryReadCredential makes read the first credential source of
// AuthorizeRegistryRequest and AuthorizeRegistryRequestWithCLI, and returns
// the function that restores the previous source. A nil read removes the
// source.
func InstallRegistryReadCredential(read RegistryCredential) (restore func()) {
	var next *RegistryCredential
	if read != nil {
		next = &read
	}
	previous := registryReadCredential.Swap(next)
	return func() { registryReadCredential.Store(previous) }
}

// AuthorizeRegistryRequest attaches the user's registry credential to an archive
// request.
//
// When the invocation enabled the credential provider for installs, its read
// credential is asked first. A credential whose hosts include the request's
// host and port is attached, and nothing else is consulted. An error from the
// provider, a refusal above all, is returned: the caller fails the download
// with it rather than retrying on another credential. A provider that holds no
// credential, or one scoped to other hosts, leaves the request to the
// host-keyed seam below, exactly as without the provider.
//
// The host-keyed seam attaches the credential @putnami/cloud resolves for the
// request's host. Absence is ordinary — a core-only install, a user who is not
// signed in, a host the cloud does not manage — and the request then goes out
// anonymous, with the registry's own access control deciding. A caller that
// already set an Authorization header keeps it.
//
// A hosted run (runcredential.Hosted) never asks the seam. The seam starts
// `putnami cloud registry-token`, a CLI without the run credential that loads
// the workspace's path extensions: repository code that this process would not
// record. A request that no credential-provider served goes out anonymous, and
// RegistryRefusalError turns a refusal into ErrHostedRegistryCredential.
//
// It returns why the request goes out without a credential it asked for, for
// RegistryRefusalError to name, and "" when it attached one or asked for none.
func AuthorizeRegistryRequest(req *http.Request) (missing string, err error) {
	return authorizeRegistryRequest(req, func(host string) (string, string) {
		return ResolveRegistryToken(host)
	})
}

// AuthorizeRegistryRequestWithCLI authenticates a CLI pin download without
// recursively launching through that pin. Only the selected CLI child bypasses
// relaunch; the original launch still verifies and executes the pinned bytes.
// It asks the installed read credential first, as AuthorizeRegistryRequest
// does, and its results are AuthorizeRegistryRequest's.
func AuthorizeRegistryRequestWithCLI(req *http.Request, executable string) (missing string, err error) {
	return authorizeRegistryRequest(req, func(host string) (string, string) {
		return ResolveRegistryTokenWithCLI(req.Context(), host, executable)
	})
}

// authorizeRegistryRequest asks the installed read credential for req's
// credential, then resolve, the host-keyed seam. A hosted run never calls
// resolve.
func authorizeRegistryRequest(req *http.Request, resolve func(host string) (token, hint string)) (missing string, err error) {
	if req == nil || req.URL == nil || req.Header.Get("Authorization") != "" || privateArchiveRequest(req) {
		return "", nil
	}
	if read := registryReadCredential.Load(); read != nil {
		bearer, served, err := (*read)(req.Context(), req.URL)
		if err != nil {
			return "", fmt.Errorf("registry credential for %s: %w", req.URL.Host, err)
		}
		if served {
			req.Header.Set("Authorization", "Bearer "+bearer)
			return "", nil
		}
	}
	if runcredential.Hosted() {
		return fmt.Sprintf("no credential-provider served a credential for %s", req.URL.Host), nil
	}
	token, hint := resolve(req.URL.Host)
	return attachRegistryToken(req, token, hint), nil
}

// ErrHostedRegistryCredential reports a registry that refused a download of a
// hosted run (runcredential.Hosted) that went out without a credential. A
// hosted run takes a registry credential from a credential-provider only: the
// one of the user scope for the pinned CLI and the lock-pinned extensions.
var ErrHostedRegistryCredential = errors.New("a hosted run takes a registry credential from a credential-provider only; " +
	"install a user-scope credential-provider with `putnami extensions install --user <extension>`")

// attachRegistryToken sets the bearer, or describes the credential command
// that provided none, whether it failed or could not start. The command's own
// reason is its stderr: the last line carries the error, the lines above it
// are the warnings it printed first.
func attachRegistryToken(req *http.Request, token, hint string) (missing string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return ""
	}
	missing = fmt.Sprintf("`putnami %s %s --%s %s` provided no credential",
		registryproto.SeamParentCommand, registryproto.SeamSubcommand, registryproto.SeamHostFlag, req.URL.Host)
	if reason := lastLine(hint); reason != "" {
		missing += ": " + reason
	}
	return missing
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	const limit = 300
	if len(line) > limit {
		line = strings.ToValidUTF8(line[:limit], "") + "…"
	}
	return line
}

// The bootstrap broker authenticates upstream itself. Calling a repository's
// cloud provider for its loopback host is unnecessary and can recurse into the
// extensions that this download is installing.
func privateArchiveRequest(req *http.Request) bool {
	broker, err := privatebroker.FromEnv(PrivatePutRegistryURLEnv, "/put")
	return err == nil && broker != nil && req.URL.Scheme == "http" && req.URL.Host == broker.Host && strings.HasPrefix(req.URL.Path, "/put/")
}

// RegistryRefusalError turns a registry's refusal into an error naming the
// command that fixes it, and returns nil for every other status. missing is
// AuthorizeRegistryRequest's result: why the request carried no credential.
//
// A private channel answers 401 to an anonymous reader and 403 to a reader
// whose account may not see it; neither is a network fault, and both are cured
// by signing in. A request that went out without the credential it asked for
// also names the credential command and its reason, and its 404 names both
// causes, because the registry answers an anonymous reader of a private archive
// the way it answers for a version that does not exist. That 404 suggests no
// command: signing in does not publish a missing version.
//
// On a hosted run, a request without a credential that the registry answers
// with 401, 403 or 404 fails with ErrHostedRegistryCredential, which names the
// user-scope credential-provider to install instead of `putnami cloud login`.
// Its 404 still names both causes.
//
// The endpoint is redacted: a registry URL may carry userinfo.
func RegistryRefusalError(endpoint string, status int, missing string) error {
	endpoint = RedactRegistryURL(endpoint)
	if missing != "" && runcredential.Hosted() {
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("registry %s answered HTTP %d to a hosted run's request without a credential (%s): %w",
				endpoint, status, missing, ErrHostedRegistryCredential)
		case http.StatusNotFound:
			return fmt.Errorf("registry %s answered HTTP %d to a hosted run's request without a credential (%s): "+
				"the version does not exist, or it is private and %w", endpoint, status, missing, ErrHostedRegistryCredential)
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		err := fmt.Errorf("registry %s answered HTTP %d: this archive needs a credential", endpoint, status)
		if missing != "" {
			err = fmt.Errorf("%w, and the request carried none: %s", err, missing)
		}
		return protocolcli.WithNext(err, "putnami cloud login")
	case http.StatusNotFound:
		if missing == "" {
			return nil
		}
		return fmt.Errorf("registry %s answered HTTP %d to a request without a credential: "+
			"the version does not exist, or it is private and needs a credential: %s", endpoint, status, missing)
	}
	return nil
}

// HTTPTimeoutEnv lets users tune the per-request HTTP timeout for
// registry artifact fetches (extension installs, template installs,
// self-update) and toolchain metadata reads. The default — 60s —
// copes with average broadband but times out long downloads on slow links
// or large CLI binaries.
//
// Format: any value parseable by time.ParseDuration ("90s", "2m"), or a
// bare integer interpreted as seconds for ergonomics ("90").
const HTTPTimeoutEnv = "PUTNAMI_HTTP_TIMEOUT"

// DefaultHTTPTimeout is the fallback when the env var is unset or invalid.
const DefaultHTTPTimeout = 60 * time.Second

// HTTPRetryAttempts is the maximum number of attempts for an idempotent
// GET. Combined with HTTPRetryBackoff the worst-case extra wait is
// (1+2)*HTTPRetryBackoff before bubbling the error.
const HTTPRetryAttempts = 3

// HTTPRetryBackoff is the base delay between retries; the wrapper doubles
// it after each transient failure (1×, 2×).
const HTTPRetryBackoff = 1 * time.Second

// HTTPRetryBackoffEnv overrides the base retry backoff. It mirrors
// HTTPTimeoutEnv (a duration string or bare integer seconds), but unlike
// the timeout a value of "0" is honored — it disables the inter-attempt
// sleep entirely while still making all HTTPRetryAttempts. Failure-path
// tests set it to "0" so they don't pay the full retry budget waiting on a
// dead registry that refuses instantly. Negative or invalid values fall
// back to HTTPRetryBackoff.
const HTTPRetryBackoffEnv = "PUTNAMI_HTTP_RETRY_BACKOFF"

// NewRegistryHTTPClient returns a *http.Client configured for registry
// artifact downloads. Timeout is read from PUTNAMI_HTTP_TIMEOUT (env);
// invalid or missing values fall back to DefaultHTTPTimeout so the
// existing behavior is preserved for users that don't touch the knob.
func NewRegistryHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       resolveHTTPTimeout(os.Getenv(HTTPTimeoutEnv)),
		Transport:     useragent.NewTransport(nil),
		CheckRedirect: registryRedirectPolicy,
	}
}

// registryRedirectPolicy keeps following ordinary download redirects while
// enforcing the credential seam's exact-origin boundary. net/http may forward
// Authorization from a host to its subdomains; a registry bearer is minted for
// the configured origin only, so a CDN or downgrade redirect must never receive
// it. Public downloads continue unchanged because there is no header to remove.
func registryRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if req == nil || req.URL == nil || len(via) == 0 || via[0] == nil || via[0].URL == nil {
		return nil
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, origin.Scheme) || !strings.EqualFold(req.URL.Host, origin.Host) {
		req.Header.Del("Authorization")
	}
	return nil
}

// resolveHTTPTimeout parses raw into a Duration. Exposed for tests; the
// production caller always passes os.Getenv(HTTPTimeoutEnv).
func resolveHTTPTimeout(raw string) time.Duration {
	if raw == "" {
		return DefaultHTTPTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	// Allow bare integers interpreted as seconds: PUTNAMI_HTTP_TIMEOUT=90.
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return DefaultHTTPTimeout
}

// resolveRetryBackoff parses raw into the base retry delay. Same format as
// resolveHTTPTimeout, but zero is a valid (sleep-disabling) value; only
// negative or unparseable input falls back to HTTPRetryBackoff.
func resolveRetryBackoff(raw string) time.Duration {
	if raw == "" {
		return HTTPRetryBackoff
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	// Allow bare integers interpreted as seconds: PUTNAMI_HTTP_RETRY_BACKOFF=2.
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return HTTPRetryBackoff
}

// DoWithRetry runs req via client, retrying on transient failures with
// exponential backoff. Only safe for idempotent GETs — the wrapper
// rebuilds the request per attempt so a body cannot be consumed once.
//
// Transient = network error, 408 Request Timeout, 429 Too Many Requests,
// or any 5xx response. Non-transient responses (200, 404, etc.) and the
// context being canceled return immediately.
//
// On retry, the response from a prior attempt is fully drained and
// closed so the connection can be reused.
func DoWithRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	var lastErr error
	delay := resolveRetryBackoff(os.Getenv(HTTPRetryBackoffEnv))
	for attempt := range HTTPRetryAttempts {
		// Re-clone with the loop's context so callers see the original ctx
		// in the response and we don't pin a stale per-attempt context.
		attemptReq := req.Clone(ctx)
		useragent.Set(attemptReq)
		// This transport retries an already constructed request; the caller owns
		// destination validation because private registries are supported.
		resp, err := client.Do(attemptReq) //nolint:gosec // G704: request trust boundary belongs to caller
		if err != nil {
			lastErr = err
			if !isTransientErr(err) || attempt == HTTPRetryAttempts-1 {
				return nil, err
			}
		} else {
			if !isTransientStatus(resp.StatusCode) {
				return resp, nil
			}
			lastErr = fmt.Errorf("transient HTTP %d", resp.StatusCode)
			drainAndClose(resp)
			if attempt == HTTPRetryAttempts-1 {
				return nil, lastErr
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return nil, lastErr
}

func isTransientErr(err error) bool {
	// net.OpError, *url.Error, EOF, context.DeadlineExceeded surface here
	// — they all represent failed connection / partial response cases
	// that a retry might recover. context.Canceled is NOT transient:
	// the caller asked us to stop.
	return err != nil && !errors.Is(err, context.Canceled)
}

func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Best-effort drain so the connection can be reused. 64 KiB is plenty
	// for typical error responses.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
}
