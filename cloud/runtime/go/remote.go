package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// RemoteSourceConfig configures a RemoteConfigSource.
type RemoteSourceConfig struct {
	// ServerURL is the legacy base URL. RemoteConfigSource appends
	// /api/configs/resolve and POSTs the resolve request body.
	ServerURL string
	// URL is an exact /api/configs/resolve URL. RemoteConfigSource fetches it
	// as-is with GET, preserving query parameters such as secretsMode=reveal.
	URL string
	// AudienceFallback is the canonical config-server audience. A legacy raw
	// Cloud Run fetch URL can become unreachable after ingress is locked to the
	// load balancer; on its 404 only, the source retries the same request through
	// this origin.
	AudienceFallback string
	AppName          string
	Version          string
	// Pinned sends pinned=true alongside Version so the server resolves ONLY the
	// frozen version layer and fails loud on a missing pin. Set only for a real
	// CONFIG_VERSION revision pin; a bare Version stays additive.
	Pinned      bool
	Environment string
	SchemaHash  string // optional: enables schema match checking
	Token       string // optional: static bearer token; ignored if TokenSource is non-nil
	// TokenSource resolves the bearer token per-request. When non-nil it
	// takes precedence over Token, letting callers refresh tokens (e.g.
	// GCP metadata-server ID tokens) without restarting the process.
	TokenSource TokenSource
	Timeout     time.Duration // per-attempt timeout, defaults to 5s
	RetryBudget time.Duration // total retry budget for required sources, defaults to 30s; negative disables retries
	Required    bool          // fail startup on fetch/resolve failures
	// SnapshotURI is a gs://<bucket>/<object> pointer to the durable,
	// KMS-encrypted last-known-good config snapshot the release path stamped
	// for this workload. When set, a config-server fetch that exhausts its retry
	// budget falls back to this snapshot instead of failing startup.
	SnapshotURI string
	// SnapshotKMSKey is the Cloud KMS cryptoKey resource name that decrypts the
	// snapshot object. Ignored when SnapshotURI is empty.
	SnapshotKMSKey string
}

// RemoteConfigSource fetches configuration from a Putnami config server.
//
// Priority 50: between file sources (10-20) and CONFIG_DATA (60).
// On optional-source failure, returns nil so local sources provide config.
// On required-source failure, returns an error so startup fails loudly.
type RemoteConfigSource struct {
	config RemoteSourceConfig
	loaded remoteLoad
}

// NewRemoteConfigSource creates a remote config source.
func NewRemoteConfigSource(cfg RemoteSourceConfig) *RemoteConfigSource {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultRemoteSourceTimeout
	}
	return &RemoteConfigSource{config: cfg}
}

// Name returns the source identifier used in logging and diagnostics.
func (s *RemoteConfigSource) Name() string { return "config-server" }

// Priority returns the source priority (50: between file sources and CONFIG_DATA).
func (s *RemoteConfigSource) Priority() int { return 50 }

// Load fetches the resolved config tree from the config server.
// Results are cached for the process lifetime.
func (s *RemoteConfigSource) Load() (map[string]any, error) {
	return s.LoadContext(context.Background())
}

// LoadContext is Load that stops its requests, retry waits and snapshot
// fallback once ctx ends, and returns an error wrapping ctx.Err().
func (s *RemoteConfigSource) LoadContext(ctx context.Context) (map[string]any, error) {
	return s.loaded.load(ctx, s.fetch)
}

// resolveRequest matches the protocol ResolveRequest shape plus the cloud
// pinned flag.
type resolveRequest struct {
	AppName     string `json:"appName"`
	Version     string `json:"version,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Environment string `json:"environment"`
	SchemaHash  string `json:"schemaHash,omitempty"`
}

// resolveResponse matches the protocol ResolveResponse shape.
type resolveResponse struct {
	Config      map[string]any `json:"config"`
	Resolved    bool           `json:"resolved"`
	SchemaMatch bool           `json:"schemaMatch"`
	Layers      []struct {
		Dimension string `json:"dimension"`
		Priority  int    `json:"priority"`
	} `json:"layers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// resolveToken returns the bearer token to attach to a request. It prefers
// the TokenSource (per-request, refreshable) and falls back to the static
// Token string. A TokenSource error is logged and the request is sent
// without an Authorization header: the server responds with 401 and the
// source falls back to local config, as it does for an unset token.
func (s *RemoteConfigSource) resolveToken(ctx context.Context) (string, bool) {
	if s.config.TokenSource != nil {
		tok, err := s.config.TokenSource.Token(ctx)
		if err != nil {
			slog.Warn("config-server token source failed",
				slog.String("source", s.Name()),
				slog.String("error", err.Error()),
			)
			return "", false
		}
		return tok, tok != ""
	}
	if s.config.Token != "" {
		return s.config.Token, true
	}
	return "", false
}

// fetch resolves config from the config server, falling back to the durable
// snapshot when the server is unreachable for the whole retry budget. Without
// a reachable server or a snapshot, the terminal failure semantics apply.
func (s *RemoteConfigSource) fetch(ctx context.Context) (map[string]any, error) {
	data, failure := s.fetchRemote(ctx)
	if failure == nil {
		return data, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, remoteLoadCanceled(s.Name(), err)
	}
	if snap, ok := s.loadSnapshotFallback(ctx, failure); ok {
		return snap, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, remoteLoadCanceled(s.Name(), err)
	}
	return nil, s.fail(failure.message, failure.err)
}

// fetchRemote runs the config-server fetch with its retry budget. It returns a
// non-nil *remoteAttemptFailure describing the terminal failure when the server
// could not be resolved, or when ctx ended; the caller decides whether to fall
// back or fail.
func (s *RemoteConfigSource) fetchRemote(ctx context.Context) (map[string]any, *remoteAttemptFailure) {
	reqBody := resolveRequest{
		AppName:     s.config.AppName,
		Version:     s.config.Version,
		Pinned:      s.config.Pinned && s.config.Version != "",
		Environment: s.config.Environment,
		SchemaHash:  s.config.SchemaHash,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, terminalRemoteFailure("marshal config resolve request", err)
	}

	retryBudget := remoteRetryBudget(s.config.Required, s.config.RetryBudget)
	var deadline time.Time
	if retryBudget > 0 {
		deadline = time.Now().Add(retryBudget)
	}

	var last *remoteAttemptFailure
	for failedAttempts := 0; ; {
		timeout := s.config.Timeout
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 && last != nil {
				return nil, last
			}
			if remaining > 0 && remaining < timeout {
				timeout = remaining
			}
		}

		data, failure := s.fetchOnce(ctx, body, timeout)
		if failure == nil {
			return data, nil
		}
		last = failure
		if !s.config.Required || !failure.retryable || retryBudget <= 0 || ctx.Err() != nil {
			return nil, failure
		}

		failedAttempts++
		delay := remoteRetryDelay(failedAttempts)
		if !deadline.IsZero() && time.Until(deadline) <= delay {
			return nil, failure
		}
		slog.Warn("config-server fetch failed; retrying",
			slog.String("source", s.Name()),
			slog.Int("attempt", failedAttempts),
			slog.Duration("delay", delay),
			slog.String("error", remoteFailureDetail(failure)),
		)
		if !waitRemoteRetry(ctx, delay) {
			return nil, failure
		}
	}
}

// loadSnapshotFallback attempts the durable last-known-good config snapshot
// after the config server was unreachable for the whole retry budget. It
// returns ok=false when no snapshot is configured, the failure is terminal
// rather than a retryable outage, or the snapshot could not be loaded, so the
// caller applies the unchanged terminal-failure semantics.
//
// The fallback is gated to retryable (outage-class) failures on purpose. A
// terminal failure is authoritative and must stay terminal: a 401/403 (config
// access revoked) or a 404 / resolved:false (config deleted) means the workload
// is no longer entitled to that config, so booting it on a stale, secret-bearing
// snapshot would defeat the revocation. A 404 from a legacy raw Cloud Run URL
// first gets one canonical-audience retry; only its resulting failure reaches
// this policy. Only "the config server could not be reached across the retry
// budget" earns the durable snapshot.
//
// On success it emits a loud, structured WARN: config loading precedes the
// metrics pipeline (it runs before the DI graph exists), so this log line — not
// an OTel counter — is the "running on snapshot" telemetry signal a log-based
// alert keys on. The stale-config mode is deliberately noisy.
func (s *RemoteConfigSource) loadSnapshotFallback(ctx context.Context, remoteFailure *remoteAttemptFailure) (map[string]any, bool) {
	if s.config.SnapshotURI == "" {
		return nil, false
	}
	if !remoteFailure.retryable {
		return nil, false
	}
	snap, err := loadConfigSnapshot(ctx, s.config.SnapshotURI, s.config.SnapshotKMSKey, s.config.Timeout)
	if err != nil {
		if ctx.Err() != nil {
			// A canceled boot is not a snapshot outage; keep the alert line quiet.
			return nil, false
		}
		slog.Warn("config snapshot fallback failed; no durable config available",
			slog.String("source", s.Name()),
			slog.String("remoteError", remoteFailureDetail(remoteFailure)),
			slog.String("snapshotError", err.Error()),
		)
		return nil, false
	}
	slog.Warn("config server unreachable; booting on durable config snapshot (stale-config mode)",
		slog.String("source", s.Name()),
		slog.String("appName", s.config.AppName),
		slog.String("environment", s.config.Environment),
		slog.String("snapshotURI", s.config.SnapshotURI),
		slog.String("remoteError", remoteFailureDetail(remoteFailure)),
	)
	return snap, true
}

func (s *RemoteConfigSource) fetchOnce(ctx context.Context, body []byte, timeout time.Duration) (map[string]any, *remoteAttemptFailure) {
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := s.newRequest(ctx, body)
	if err != nil {
		return nil, terminalRemoteFailure("build config resolve request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, ok := s.resolveToken(ctx); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req) //nolint:gosec // G704: target is operator-configured (CONFIG_SERVER_URL) or the GCP metadata server, not user-tainted
	if err != nil {
		return nil, retryableRemoteFailure("config-server unreachable", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		if fallback, ok := s.audienceFallbackRequest(ctx, req, body); ok {
			if err := resp.Body.Close(); err != nil {
				slog.Warn("failed to close config-server response before audience fallback")
			}
			resp, err = client.Do(fallback) //nolint:gosec // fallback origin is operator-configured CONFIG_SERVER_AUDIENCE
			if err != nil {
				return nil, retryableRemoteFailure("config-server unreachable", err)
			}
		}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", slog.String("error", err.Error()))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		failure := terminalRemoteFailure("config-server returned non-200", fmt.Errorf("status %d", resp.StatusCode))
		if retryableRemoteStatus(resp.StatusCode) {
			failure.retryable = true
		}
		return nil, failure
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, retryableRemoteFailure("read config-server response", err)
	}

	var result resolveResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, retryableRemoteFailure("decode config-server response", err)
	}

	if !result.Resolved {
		if configResponseIsEmptySchemaMatch(result) {
			return map[string]any{}, nil
		}
		return nil, terminalRemoteFailure("config-server did not resolve config", nil)
	}

	if !result.SchemaMatch {
		slog.Warn("config-server schema mismatch",
			slog.String("source", s.Name()),
			slog.String("appName", s.config.AppName),
			slog.String("environment", s.config.Environment),
		)
	}
	for _, warning := range result.Warnings {
		slog.Warn("config-server warning",
			slog.String("source", s.Name()),
			slog.String("appName", s.config.AppName),
			slog.String("environment", s.config.Environment),
			slog.String("warning", warning),
		)
	}

	return result.Config, nil
}

func (s *RemoteConfigSource) audienceFallbackRequest(ctx context.Context, req *http.Request, body []byte) (*http.Request, bool) {
	fallbackURL, ok := configServerAudienceFallbackURL(req.URL.String(), s.config.AudienceFallback)
	if !ok {
		return nil, false
	}
	var reader io.Reader
	if req.Method != http.MethodGet {
		reader = bytes.NewReader(body)
	}
	fallback, err := http.NewRequestWithContext(ctx, req.Method, fallbackURL, reader)
	if err != nil {
		return nil, false
	}
	fallback.Header = req.Header.Clone()
	return fallback, true
}

func configServerAudienceFallbackURL(requestURL, audience string) (string, bool) {
	if strings.TrimSpace(audience) == "" {
		return "", false
	}
	request, err := neturl.Parse(requestURL)
	if err != nil || request.Scheme == "" || request.Host == "" {
		return "", false
	}
	fallback, err := neturl.Parse(audience)
	if err != nil || fallback.Scheme == "" || fallback.Host == "" || request.Scheme == fallback.Scheme && request.Host == fallback.Host {
		return "", false
	}
	request.Scheme = fallback.Scheme
	request.Host = fallback.Host
	return request.String(), true
}

func configResponseIsEmptySchemaMatch(result resolveResponse) bool {
	return result.SchemaMatch && len(result.Config) == 0 && len(result.Warnings) == 0
}

func (s *RemoteConfigSource) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	if s.config.URL != "" {
		// The exact-URL form is a GET with the resolve query baked in, so the
		// config version pin must ride the query string — the POST body
		// (which carries Version + Pinned) is never sent on this path.
		target := s.config.URL
		if s.config.Version != "" {
			target = withVersionQuery(target, s.config.Version, s.config.Pinned)
		}
		return http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	}
	base := strings.TrimRight(s.config.ServerURL, "/")
	return http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/configs/resolve", bytes.NewReader(body))
}

// withVersionQuery adds version=<version> (and pinned=true when pinned) to an
// exact resolve URL's query string so the config server pins the resolve to a
// frozen revision. An existing version query is left untouched (an
// operator-set pin wins).
func withVersionQuery(rawURL, version string, pinned bool) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Get("version") != "" {
		return rawURL
	}
	q.Set("version", version)
	if pinned {
		q.Set("pinned", "true")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *RemoteConfigSource) fail(message string, err error) error {
	if s.config.Required {
		if err != nil {
			return fmt.Errorf("%s: %w", message, err)
		}
		return fmt.Errorf("%s", message)
	}
	attrs := []any{slog.String("source", s.Name())}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.Warn(message+", using local config", attrs...)
	return nil
}

// serverURLIsSecure reports whether serverURL uses an encrypted transport.
// https is always accepted; plain http is accepted only for loopback hosts
// (local development). Every other case — http to a non-loopback host, or a
// non-http(s) scheme — sends the long-lived bearer token and the plaintext
// resolve response in the clear to any on-path observer.
func serverURLIsSecure(serverURL string) bool {
	u, err := neturl.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// warnIfInsecureServerURL logs a loud warning when CONFIG_SERVER_URL would
// transmit credentials and resolved values over an unencrypted channel.
// The URL itself is not logged (it is operator config and may embed
// sensitive components); only the source name is, so operators can locate
// the misconfiguration without leaking it into logs.
func warnIfInsecureServerURL(serverURL, sourceName string) {
	if serverURLIsSecure(serverURL) {
		return
	}
	slog.Warn("CONFIG_SERVER_URL is not HTTPS: the bearer token and resolved config/secret values will be sent in cleartext; use https:// (plain http:// is allowed only for loopback/local development)",
		slog.String("source", sourceName),
	)
}

// DiscoverRemoteSource creates a RemoteConfigSource from environment variables.
// Returns nil if CONFIG_SERVER_URL is not set.
//
// Bearer-token resolution follows this precedence:
//   - Operator-provided env var wins (first non-empty among
//     PUTNAMI_CLOUD_TOKEN, CONFIG_SERVER_TOKEN, PUTNAMI_TOKEN — most
//     specific first).
//   - Otherwise, on GCP (detected via K_SERVICE / GOOGLE_CLOUD_PROJECT,
//     or a TCP probe to the metadata server as safety net), tokens are
//     minted at request time bound to CONFIG_SERVER_AUDIENCE when set,
//     otherwise CONFIG_SERVER_URL.
//   - Otherwise the call is unauthenticated; the server will reject it
//     and the framework logs a startup warning.
func DiscoverRemoteSource() *RemoteConfigSource {
	if strings.TrimSpace(os.Getenv(PreparedBootBindingEnv)) != "" {
		return nil
	}
	serverURL := os.Getenv("CONFIG_SERVER_URL")
	if serverURL == "" {
		return nil
	}
	warnIfInsecureServerURL(serverURL, "config-server")
	exactURL := configServerURLIsResolveEndpoint(serverURL)
	configVersion := configVersionFromEnv()
	cfg := RemoteSourceConfig{
		AppName: os.Getenv("APP_NAME"),
		Version: configVersion,
		// A CONFIG_VERSION pin means EXCLUSIVE, fail-loud resolution: send
		// pinned=true so the server reads only the frozen version layer. Bare
		// version-less discovery stays additive/unpinned.
		Pinned:           configVersion != "",
		Environment:      Environment(),
		SchemaHash:       os.Getenv("SCHEMA_HASH"),
		TokenSource:      discoverTokenSource(serverURL),
		Timeout:          configServerTimeoutFromEnv(),
		RetryBudget:      configServerRetryBudgetFromEnv(),
		Required:         remoteConfigRequired(),
		AudienceFallback: strings.TrimSpace(os.Getenv("CONFIG_SERVER_AUDIENCE")),
		SnapshotURI:      strings.TrimSpace(os.Getenv("CONFIG_SNAPSHOT_URI")),
		SnapshotKMSKey:   strings.TrimSpace(os.Getenv("CONFIG_SNAPSHOT_KMS_KEY")),
	}
	if exactURL {
		cfg.URL = serverURL
	} else {
		cfg.ServerURL = serverURL
	}
	return NewRemoteConfigSource(cfg)
}

// configVersionFromEnv resolves the config version pin for the config source
// from CONFIG_VERSION alone, the revision pin the deploy stamps. Empty (the
// common case: an unpinned or local workload) resolves the version-agnostic
// config.
//
// It deliberately does NOT fall back to APP_VERSION. A pinned resolve is
// EXCLUSIVE and fail-loud (the server reads only the frozen version layer), and
// APP_VERSION is populated with the workload's build semver at runtime — sending
// it as the version would make every workload fail to boot on a semver that was
// never frozen. APP_VERSION stays a build-identity signal, never a config pin.
func configVersionFromEnv() string {
	return strings.TrimSpace(os.Getenv("CONFIG_VERSION"))
}

func remoteFailureDetail(failure *remoteAttemptFailure) string {
	if failure == nil {
		return ""
	}
	if failure.err != nil {
		return failure.err.Error()
	}
	return failure.message
}

func configServerURLIsResolveEndpoint(value string) bool {
	u, err := neturl.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/api/configs/resolve")
}

func remoteConfigRequired() bool {
	if explicit := strings.ToLower(strings.TrimSpace(os.Getenv("CONFIG_SERVER_REQUIRED"))); explicit != "" {
		return explicit == "1" || explicit == "true" || explicit == "yes" || explicit == "on"
	}
	appEnv := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	return os.Getenv("K_SERVICE") != "" ||
		os.Getenv("NODE_ENV") == "production" ||
		os.Getenv("NODE_ENV") == "prod" ||
		appEnv == "prod" ||
		appEnv == "production"
}

func configServerAudience(serverURL string) string {
	if audience := strings.TrimSpace(os.Getenv("CONFIG_SERVER_AUDIENCE")); audience != "" {
		return audience
	}
	return serverURL
}

// configServerTokenEnvNames lists the env vars discovery checks for an
// operator-provided bearer, in precedence order (most-specific first).
// The TypeScript runtime destination reads the same list, so a workspace's
// env-var setup behaves identically across runtimes.
var configServerTokenEnvNames = []string{
	"PUTNAMI_CLOUD_TOKEN", // explicit: cloud control-plane bearer
	"CONFIG_SERVER_TOKEN", // legacy: original framework name
	"PUTNAMI_TOKEN",       // generic Putnami token, broad fallback
}

// discoverTokenSource picks the bearer-token resolver for both
// config-server and secrets-server discovery. Used by
// DiscoverRemoteSource and DiscoverRemoteSecretsSource — one identity
// covers both endpoints (same SA, same audience).
//
// Precedence: operator override (env var) > workload identity >
// unauthenticated.
// The TCP probe is a safety net for GCE/Cloud Run instances where the
// env hints are absent.
func discoverTokenSource(serverURL string) TokenSource {
	for _, name := range configServerTokenEnvNames {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return NewEnvVarTokenSource(name)
		}
	}
	if onGCP() {
		return NewGcpMetadataTokenSource(configServerAudience(serverURL))
	}
	slog.Warn("CONFIG_SERVER_URL is set but no credentials are configured (PUTNAMI_CLOUD_TOKEN / CONFIG_SERVER_TOKEN / PUTNAMI_TOKEN unset and no GCP workload identity detected); resolve calls will be unauthenticated and may be rejected")
	return NewEnvVarTokenSource("CONFIG_SERVER_TOKEN")
}

// onGCP reports whether this process is running on a GCP runtime.
// Explicit env-var hints take
// precedence (K_SERVICE for Cloud Run, GOOGLE_CLOUD_PROJECT broadly),
// with the TCP probe as a fallback so plain GCE VMs without those env
// vars still pick up workload identity.
func onGCP() bool {
	if os.Getenv("K_SERVICE") != "" || os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
		return true
	}
	return metadataReachableOnce()
}

// metadataProbeTimeout bounds the TCP probe to the GCP metadata server
// at discovery time. 200ms is short enough to not delay startup outside
// GCP, long enough for a same-host metadata server to respond.
const metadataProbeTimeout = 200 * time.Millisecond

// metadataReachableOnce is the GCP-detection fallback. It's a swappable
// var so tests can pin it to a deterministic value without depending on
// where they run (a self-hosted GCP runner would otherwise see metadata
// as reachable and pick GcpMetadataTokenSource even when no env-var
// hints are set). Default is a sync.Once-cached TCP probe so config +
// secrets discovery share a single 200ms cost off-GCP.
var metadataReachableOnce = sync.OnceValue(func() bool {
	return MetadataServerReachable(metadataProbeTimeout)
})
