package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// PreparedBootBindingEnv carries the opaque prepared-configuration lookup
	// the platform stamps on the workload.
	PreparedBootBindingEnv = "PUTNAMI_CONFIG_BOOT_BINDING"
	preparedBootPath       = "/internal/config/prepared-configuration/boot"
	preparedBootPrefix     = "pcb_"
	maxPreparedBootBody    = 4 << 20
)

// PreparedBootSourceConfig configures one fixed Config-origin boot consumer.
// Reference is lookup only; the server authorizes the signed workload token.
type PreparedBootSourceConfig struct {
	ServerURL   string
	Audience    string
	Reference   string
	Token       string
	TokenSource TokenSource
	Timeout     time.Duration
	RetryBudget time.Duration
	HTTPClient  *http.Client
}

// PreparedBootSource fetches the complete current prepared tree once during
// startup. It has no snapshot or legacy endpoint fallback because either would
// bypass current Config revocation and owner-generation checks.
type PreparedBootSource struct {
	config PreparedBootSourceConfig
	origin string
	client *http.Client
	loaded remoteLoad
}

// NewPreparedBootSource fixes the request to ServerURL's origin and the one
// boot path. Any path, query, fragment or redirect is incapable of selecting a
// credential destination.
func NewPreparedBootSource(cfg PreparedBootSourceConfig) *PreparedBootSource {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultRemoteSourceTimeout
	}
	origin, _ := preparedBootOrigin(cfg.ServerURL)
	client := &http.Client{}
	if cfg.HTTPClient != nil {
		copy := *cfg.HTTPClient
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &PreparedBootSource{config: cfg, origin: origin, client: client}
}

// Name identifies the prepared configuration source.
func (s *PreparedBootSource) Name() string { return "prepared-config-boot" }

// Priority matches the ordinary config source it replaces. CONFIG_DATA keeps
// the framework's established higher-priority local emergency override.
func (s *PreparedBootSource) Priority() int { return 50 }

// Load fetches the prepared configuration once and retains its result.
func (s *PreparedBootSource) Load() (map[string]any, error) {
	return s.LoadContext(context.Background())
}

// LoadContext is Load that stops its requests and retry waits once ctx ends,
// and returns an error wrapping ctx.Err().
func (s *PreparedBootSource) LoadContext(ctx context.Context) (map[string]any, error) {
	return s.loaded.load(ctx, s.fetch)
}

func (s *PreparedBootSource) fetch(ctx context.Context) (map[string]any, error) {
	if s == nil || s.client == nil || s.origin == "" || !serverURLIsSecure(s.origin) ||
		!validPreparedBootAudience(s.config.Audience) || !validPreparedBootReference(s.config.Reference) ||
		(s.config.TokenSource == nil && strings.TrimSpace(s.config.Token) == "") {
		return nil, errors.New("prepared configuration boot is not fully configured")
	}
	body, err := json.Marshal(struct {
		Reference string `json:"reference"`
	}{Reference: s.config.Reference})
	if err != nil {
		return nil, errors.New("encode prepared configuration boot request")
	}

	retryBudget := remoteRetryBudget(true, s.config.RetryBudget)
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
				return nil, preparedBootError(last)
			}
			if remaining > 0 && remaining < timeout {
				timeout = remaining
			}
		}
		tree, failure := s.fetchOnce(ctx, body, timeout)
		if failure == nil {
			return tree, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, remoteLoadCanceled(s.Name(), err)
		}
		last = failure
		if !failure.retryable || retryBudget <= 0 {
			return nil, preparedBootError(failure)
		}
		failedAttempts++
		delay := remoteRetryDelay(failedAttempts)
		if !deadline.IsZero() && time.Until(deadline) <= delay {
			return nil, preparedBootError(failure)
		}
		slog.Warn("prepared configuration boot failed; retrying",
			slog.Int("attempt", failedAttempts), slog.Duration("delay", delay))
		if !waitRemoteRetry(ctx, delay) {
			return nil, remoteLoadCanceled(s.Name(), ctx.Err())
		}
	}
}

func (s *PreparedBootSource) fetchOnce(ctx context.Context, body []byte, timeout time.Duration) (map[string]any, *remoteAttemptFailure) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	token, err := s.resolveToken(ctx)
	if err != nil {
		return nil, retryableRemoteFailure("resolve workload boot token", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.origin+preparedBootPath, bytes.NewReader(body))
	if err != nil {
		return nil, terminalRemoteFailure("build prepared configuration boot request", nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req) //nolint:gosec // URL is reduced to the operator-configured fixed Config origin.
	if err != nil {
		return nil, retryableRemoteFailure("prepared configuration boot endpoint unreachable", nil)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		failure := terminalRemoteFailure("prepared configuration boot rejected", nil)
		if retryableRemoteStatus(resp.StatusCode) {
			failure.retryable = true
		}
		return nil, failure
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPreparedBootBody+1))
	if err != nil {
		return nil, retryableRemoteFailure("read prepared configuration boot response", nil)
	}
	if len(raw) > maxPreparedBootBody {
		return nil, terminalRemoteFailure("prepared configuration boot response exceeds limit", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var response struct {
		Config map[string]any `json:"config"`
	}
	if err := decoder.Decode(&response); err != nil {
		return nil, terminalRemoteFailure("decode prepared configuration boot response", nil)
	}
	if err := rejectTrailingJSON(decoder); err != nil || response.Config == nil {
		return nil, terminalRemoteFailure("decode prepared configuration boot response", nil)
	}
	converted, err := convertPreparedBootNumbers(response.Config)
	if err != nil {
		return nil, terminalRemoteFailure("decode prepared configuration boot numbers", nil)
	}
	tree, ok := converted.(map[string]any)
	if !ok {
		return nil, terminalRemoteFailure("decode prepared configuration boot object", nil)
	}
	return tree, nil
}

func (s *PreparedBootSource) resolveToken(ctx context.Context) (string, error) {
	if s.config.TokenSource != nil {
		token, err := s.config.TokenSource.Token(ctx)
		if err != nil || !validBearerToken(token) {
			return "", errors.New("prepared configuration workload token unavailable")
		}
		return token, nil
	}
	token := strings.TrimSpace(s.config.Token)
	if !validBearerToken(token) {
		return "", errors.New("prepared configuration workload token unavailable")
	}
	return token, nil
}

// DiscoverPreparedBootSource builds the boot source from the workload environment.
func DiscoverPreparedBootSource() *PreparedBootSource {
	reference := strings.TrimSpace(os.Getenv(PreparedBootBindingEnv))
	if reference == "" {
		return nil
	}
	serverURL := strings.TrimSpace(os.Getenv("CONFIG_SERVER_URL"))
	audience := strings.TrimSpace(os.Getenv("CONFIG_SERVER_AUDIENCE"))
	var tokenSource TokenSource
	for _, name := range configServerTokenEnvNames {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			tokenSource = NewEnvVarTokenSource(name)
			break
		}
	}
	if tokenSource == nil && onGCP() {
		tokenSource = NewGcpMetadataTokenSource(audience)
	}
	if tokenSource == nil {
		tokenSource = NewEnvVarTokenSource("CONFIG_SERVER_TOKEN")
	}
	return NewPreparedBootSource(PreparedBootSourceConfig{
		ServerURL: serverURL, Audience: audience, Reference: reference,
		TokenSource: tokenSource, Timeout: configServerTimeoutFromEnv(), RetryBudget: configServerRetryBudgetFromEnv(),
	})
}

func preparedBootOrigin(raw string) (string, error) {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("invalid Config origin")
	}
	return (&neturl.URL{Scheme: u.Scheme, Host: u.Host}).String(), nil
}

func validPreparedBootReference(reference string) bool {
	if len(reference) != len(preparedBootPrefix)+43 || !strings.HasPrefix(reference, preparedBootPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(reference, preparedBootPrefix))
	return err == nil && len(raw) == 32
}

func validPreparedBootAudience(audience string) bool {
	return audience != "" && audience == strings.TrimSpace(audience) && len(audience) <= 2048 &&
		!strings.ContainsAny(audience, "\x00\r\n")
}

func validBearerToken(token string) bool {
	return token != "" && len(token) <= 64<<10 && token == strings.TrimSpace(token) && !strings.ContainsAny(token, " \t\r\n")
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func convertPreparedBootNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			converted, err := convertPreparedBootNumbers(child)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			converted, err := convertPreparedBootNumbers(child)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	case json.Number:
		raw := typed.String()
		if !strings.ContainsAny(raw, ".eE") {
			integer, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, err
			}
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return decimal, nil
	default:
		return value, nil
	}
}

func preparedBootError(failure *remoteAttemptFailure) error {
	if failure == nil {
		return errors.New("prepared configuration boot unavailable")
	}
	return fmt.Errorf("prepared configuration boot unavailable: %s", failure.message)
}
