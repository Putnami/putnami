package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// RemoteSecretsSourceConfig configures a RemoteSecretsSource.
type RemoteSecretsSourceConfig struct {
	ServerURL   string
	AppName     string
	Version     string
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
}

// RemoteSecretsSource fetches secrets from a Putnami config server.
//
// Priority 55: just above RemoteConfigSource so secret values can override
// non-sensitive defaults with the same key, but field-level env: resolution
// (priority 80) still wins.
//
// On optional-source failure, returns nil so local sources provide secrets.
// On required-source failure, returns an error so startup fails loudly.
type RemoteSecretsSource struct {
	config RemoteSecretsSourceConfig
	loaded remoteLoad
}

// NewRemoteSecretsSource creates a remote secrets source.
func NewRemoteSecretsSource(cfg RemoteSecretsSourceConfig) *RemoteSecretsSource {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultRemoteSourceTimeout
	}
	return &RemoteSecretsSource{config: cfg}
}

// Name returns the source identifier used in logging and diagnostics.
func (s *RemoteSecretsSource) Name() string { return "secrets-server" }

// Priority returns the source priority (55: just above RemoteConfigSource).
func (s *RemoteSecretsSource) Priority() int { return 55 }

// Load fetches the resolved secrets tree from the secrets server.
// Results are cached for the process lifetime.
func (s *RemoteSecretsSource) Load() (map[string]any, error) {
	return s.LoadContext(context.Background())
}

// LoadContext is Load that stops its requests and retry waits once ctx ends,
// and returns an error wrapping ctx.Err().
func (s *RemoteSecretsSource) LoadContext(ctx context.Context) (map[string]any, error) {
	return s.loaded.load(ctx, s.fetch)
}

// resolveSecretsRequest matches the protocol ResolveSecretsRequest shape.
type resolveSecretsRequest struct {
	AppName     string `json:"appName"`
	Version     string `json:"version,omitempty"`
	Environment string `json:"environment"`
	SchemaHash  string `json:"schemaHash,omitempty"`
}

// resolveSecretsResponse matches the protocol ResolveSecretsResponse shape.
type resolveSecretsResponse struct {
	Secrets     map[string]any `json:"secrets"`
	Resolved    bool           `json:"resolved"`
	SchemaMatch bool           `json:"schemaMatch"`
	Layers      []struct {
		Dimension string `json:"dimension"`
		Priority  int    `json:"priority"`
	} `json:"layers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// resolveToken mirrors RemoteConfigSource.resolveToken: prefer the
// TokenSource (per-request, refreshable) and fall back to the static
// Token string. The two sources keep symmetric copies rather than share
// a base type — symmetric duplication is cheaper than premature
// abstraction at this size.
func (s *RemoteSecretsSource) resolveToken(ctx context.Context) (string, bool) {
	if s.config.TokenSource != nil {
		tok, err := s.config.TokenSource.Token(ctx)
		if err != nil {
			slog.Warn("secrets-server token source failed",
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

func (s *RemoteSecretsSource) fetch(ctx context.Context) (map[string]any, error) {
	data, failure := s.fetchRemote(ctx)
	if failure == nil {
		return data, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, remoteLoadCanceled(s.Name(), err)
	}
	return nil, s.fail(failure.message, failure.err)
}

func (s *RemoteSecretsSource) fetchRemote(ctx context.Context) (map[string]any, *remoteAttemptFailure) {
	reqBody := resolveSecretsRequest{
		AppName:     s.config.AppName,
		Version:     s.config.Version,
		Environment: s.config.Environment,
		SchemaHash:  s.config.SchemaHash,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, terminalRemoteFailure("marshal secrets resolve request", err)
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
		slog.Warn("secrets-server fetch failed; retrying",
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

func (s *RemoteSecretsSource) fetchOnce(ctx context.Context, body []byte, timeout time.Duration) (map[string]any, *remoteAttemptFailure) {
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.config.ServerURL, "/")+"/api/secrets/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, terminalRemoteFailure("build secrets resolve request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, ok := s.resolveToken(ctx); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req) //nolint:gosec // G704: target is operator-configured (CONFIG_SERVER_URL) or the GCP metadata server, not user-tainted
	if err != nil {
		return nil, retryableRemoteFailure("secrets-server unreachable", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", slog.String("error", err.Error()))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		failure := terminalRemoteFailure("secrets-server returned non-200", fmt.Errorf("status %d", resp.StatusCode))
		if retryableRemoteStatus(resp.StatusCode) {
			failure.retryable = true
		}
		return nil, failure
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, retryableRemoteFailure("read secrets-server response", err)
	}

	var result resolveSecretsResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, retryableRemoteFailure("decode secrets-server response", err)
	}

	if !result.Resolved {
		return nil, terminalRemoteFailure("secrets-server did not resolve secrets", nil)
	}

	if !result.SchemaMatch {
		slog.Warn("secrets-server schema mismatch",
			slog.String("source", s.Name()),
			slog.String("appName", s.config.AppName),
			slog.String("environment", s.config.Environment),
		)
	}
	for _, warning := range result.Warnings {
		slog.Warn("secrets-server warning",
			slog.String("source", s.Name()),
			slog.String("appName", s.config.AppName),
			slog.String("environment", s.config.Environment),
			slog.String("warning", warning),
		)
	}

	return result.Secrets, nil
}

func (s *RemoteSecretsSource) fail(message string, err error) error {
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
	slog.Warn(message+", using local secrets", attrs...)
	return nil
}

// DiscoverRemoteSecretsSource creates a RemoteSecretsSource from environment
// variables. Returns nil if CONFIG_SERVER_URL is not set or already points to
// an exact /api/configs/resolve URL that carries merged config/secrets.
//
// Reuses CONFIG_SERVER_URL, APP_NAME, APP_VERSION, and SCHEMA_HASH so the
// same identity and schema-hash are presented to both resolve endpoints.
// Bearer-token resolution matches DiscoverRemoteSource: operator token env
// vars first, then GCP metadata identity when available, otherwise
// unauthenticated.
func DiscoverRemoteSecretsSource() *RemoteSecretsSource {
	if strings.TrimSpace(os.Getenv(PreparedBootBindingEnv)) != "" {
		return nil
	}
	serverURL := os.Getenv("CONFIG_SERVER_URL")
	if serverURL == "" {
		return nil
	}
	if configServerURLIsResolveEndpoint(serverURL) {
		return nil
	}
	warnIfInsecureServerURL(serverURL, "secrets-server")
	return NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   serverURL,
		AppName:     os.Getenv("APP_NAME"),
		Version:     os.Getenv("APP_VERSION"),
		Environment: Environment(),
		SchemaHash:  os.Getenv("SCHEMA_HASH"),
		TokenSource: discoverTokenSource(serverURL),
		Timeout:     configServerTimeoutFromEnv(),
		RetryBudget: configServerRetryBudgetFromEnv(),
		Required:    remoteConfigRequired(),
	})
}
