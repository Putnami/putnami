package remotecache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// tokenResolveTimeout bounds a single command or URL token resolution so a
// misbehaving provider cannot hang the build.
const tokenResolveTimeout = 30 * time.Second

const metadataResolveTimeout = 2 * time.Second

// maxTokenBytes caps a token read from a URL provider.
const maxTokenBytes = 64 << 10

// maxTokenCommandStderrBytes bounds the private command diagnostic inspected
// after a failure. Command stdout is the bearer and is therefore never attached
// to an error; stderr is captured separately, classified into a small set of
// safe diagnostics, and otherwise fully redacted.
const maxTokenCommandStderrBytes = 4 << 10

// metadataIDTokenURL is the GCP metadata endpoint that mints an ID token for
// the workload's attached service account. It is a well-known URL, not a
// credential.
//
//nolint:gosec // G101: URL constant, not a credential
const metadataIDTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// TokenClass names HOW a cache bearer was obtained. It is a redaction-safe
// label — never the bearer, never its claims — and it carries the one property
// the client needs at refusal time: whether the runner can mint a replacement.
type TokenClass string

const (
	// TokenClassNone means no token source is configured; requests go out
	// anonymous and the cache server answers them unauthenticated.
	TokenClassNone TokenClass = "none"
	// TokenClassEnv is the static PUTNAMI_CACHE_TOKEN override — in CI, the
	// control plane's injected workspace machine token. The runner holds bytes it
	// cannot re-derive, so this class is NOT renewable.
	TokenClassEnv TokenClass = "env"
	// TokenClassStatic is a literal bearer handed to NewClient, or an opaque
	// caller-supplied resolver (WithTokenFunc). Renewability is unknown, so it is
	// treated as static: a source that cannot be PROVEN renewable is never
	// re-minted.
	TokenClassStatic TokenClass = "static"
	// TokenClassCommand runs the configured recipe — canonically
	// `putnami cloud token --for cache`. Renewable: each invocation mints a fresh
	// short-lived bearer.
	TokenClassCommand TokenClass = "command"
	// TokenClassURL GETs a token endpoint. Renewable.
	TokenClassURL TokenClass = "url"
	// TokenClassMetadata mints an audience-bound Google ID token from the Cloud
	// Run metadata service — the secretless CI path. Renewable.
	TokenClassMetadata TokenClass = "metadata"
	// TokenClassRun is the run-scoped cache token Delivery issued for the
	// hosted run's credential (protocol/cache authenticate). The provider holds
	// it in memory for the session and cannot mint another, so it is NOT
	// renewable.
	TokenClassRun TokenClass = "run"
)

// Renewable reports whether a REFUSED bearer of this class can be replaced by
// re-invoking its source. Only sources the runner can re-run qualify. A static
// env token and an opaque resolver cannot be re-minted, so "refreshing" one
// would resend the same rejected bytes — a fake refresh that costs a round trip
// and proves nothing.
func (c TokenClass) Renewable() bool {
	switch c {
	case TokenClassCommand, TokenClassURL, TokenClassMetadata:
		return true
	default:
		return false
	}
}

// Bearer is a resolved cache credential paired with the class of the source
// that produced it. The class travels with the token so a refusal can be
// classified and logged without the token ever reaching a diagnostic.
type Bearer struct {
	Token string
	Class TokenClass
}

// TokenSource describes how to obtain the cache bearer token at run time
// without persisting a secret in the config file: run a command and read its
// stdout, or GET a URL and read its body. The PUTNAMI_CACHE_TOKEN environment
// variable overrides both. When neither explicit source is configured,
// Audience enables a GCP metadata-server ID-token fallback for secretless CI.
type TokenSource struct {
	// Command is an argv whose trimmed stdout is the token.
	Command []string `json:"command,omitempty"`
	// URL is a GET endpoint whose trimmed body is the token.
	URL string `json:"url,omitempty"`
	// Audience enables the GCP metadata ID-token fallback. Config.ResolveToken
	// defaults it to the cache URL, so it normally needs no persisted value.
	Audience string `json:"audience,omitempty"`

	// metadataToken is an in-process test seam; never persisted.
	metadataToken func(context.Context, string) (string, error) `json:"-"`
}

// Configured reports whether the source has a command, URL, or metadata
// audience to resolve. The PUTNAMI_CACHE_TOKEN env override is intentionally
// not considered here: callers use this to decide whether a recipe is present,
// checking the env separately.
func (t TokenSource) Configured() bool {
	return len(t.Command) > 0 || t.URL != "" || strings.TrimSpace(t.Audience) != ""
}

// Class reports which source Resolve would use, applying the SAME precedence.
// Like Resolve it reads the environment at call time, so a run with
// PUTNAMI_CACHE_TOKEN set is classified static even when a command recipe is
// also configured — which is exactly the CI shape, where an injected machine
// token overrides the persisted `cloud token --for cache` recipe.
func (t TokenSource) Class() TokenClass {
	if strings.TrimSpace(os.Getenv(TokenEnv)) != "" {
		return TokenClassEnv
	}
	switch {
	case len(t.Command) > 0:
		return TokenClassCommand
	case t.URL != "":
		return TokenClassURL
	case strings.TrimSpace(t.Audience) != "":
		return TokenClassMetadata
	default:
		return TokenClassNone
	}
}

// Resolve obtains the bearer token. The PUTNAMI_CACHE_TOKEN env var wins when
// set; otherwise the command, URL provider, then audience-bound GCP metadata
// source is used. An entirely unconfigured source resolves to an empty token.
// A configured metadata source that is unavailable returns a clear error rather
// than silently sending an anonymous cache request. The command/metadata source
// runs lazily, and the client memoizes its result per session.
func (t TokenSource) Resolve(ctx context.Context) (string, error) {
	b, err := t.ResolveBearer(ctx)
	return b.Token, err
}

// ResolveBearer is Resolve plus the class of the source that produced the
// credential. Classification and resolution are driven by the one Class switch
// so the reported class can never disagree with the credential actually sent —
// the property the refresh gate depends on: it must be impossible to label a
// static injected token "renewable" and re-mint it.
func (t TokenSource) ResolveBearer(ctx context.Context) (Bearer, error) {
	class := t.Class()
	switch class {
	case TokenClassEnv:
		return Bearer{Token: strings.TrimSpace(os.Getenv(TokenEnv)), Class: class}, nil
	case TokenClassCommand:
		token, err := t.resolveCommand(ctx)
		return Bearer{Token: token, Class: class}, err
	case TokenClassURL:
		token, err := t.resolveURL(ctx)
		return Bearer{Token: token, Class: class}, err
	case TokenClassMetadata:
		token, err := t.resolveMetadata(ctx)
		return Bearer{Token: token, Class: class}, err
	default:
		return Bearer{Class: TokenClassNone}, nil
	}
}

func (t TokenSource) resolveMetadata(ctx context.Context) (string, error) {
	audience := strings.TrimSpace(t.Audience)
	resolve := t.metadataToken
	if resolve == nil {
		resolve = fetchMetadataIDToken
	}
	token, err := resolve(ctx, audience)
	if err != nil {
		return "", fmt.Errorf("cache token metadata source (aud=%q): %w", audience, err)
	}
	if token = strings.TrimSpace(token); token == "" {
		return "", fmt.Errorf("cache token metadata source (aud=%q): empty token", audience)
	}
	return token, nil
}

func fetchMetadataIDToken(ctx context.Context, audience string) (string, error) {
	return fetchMetadataIDTokenFrom(ctx, audience, metadataIDTokenURL, http.DefaultClient)
}

func fetchMetadataIDTokenFrom(ctx context.Context, audience, endpoint string, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataResolveTimeout)
	defer cancel()

	query := url.Values{}
	query.Set("audience", audience)
	query.Set("format", "full")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req) //nolint:gosec // G704: production passes the fixed GCP metadata endpoint; tests pass loopback
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes))
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

// TokenCommandError is a redaction-safe failure from a configured token
// command. It deliberately exposes neither stdout (the bearer) nor raw stderr:
// Diagnostic is one of the fixed messages recognized by
// classifyTokenCommandStderr, and every unrecognized stderr is represented by a
// fixed redaction marker. Callers may use errors.As to collapse the same
// memoized failure into one session-level cache warning.
type TokenCommandError struct {
	command    string
	cause      error
	diagnostic string
	recovery   string
	timedOut   bool
}

func (e *TokenCommandError) Error() string {
	message := fmt.Sprintf("cache token command %q", e.command)
	if e.timedOut {
		message += " timed out"
	} else if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	if e.diagnostic != "" {
		message += "; diagnostic: " + e.diagnostic
	}
	if e.recovery != "" {
		message += "; " + e.recovery
	}
	return message
}

func (e *TokenCommandError) Unwrap() error { return e.cause }

// resolveCommand runs the configured command and returns its trimmed stdout.
func (t TokenSource) resolveCommand(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenResolveTimeout)
	defer cancel()

	var stderr boundedCommandStderr
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		cause := err
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		diagnostic, authFailure := classifyTokenCommandStderr(stderr.Bytes())
		recovery := ""
		if authFailure && isPutnamiCacheTokenCommand(t.Command) {
			recovery = "run `putnami cloud login`; if the workspace cache link is missing or stale, then run `putnami cloud setup`"
		}
		return "", &TokenCommandError{
			command:    t.Command[0],
			cause:      cause,
			diagnostic: diagnostic,
			recovery:   recovery,
			timedOut:   timedOut,
		}
	}
	return strings.TrimSpace(string(out)), nil
}

// boundedCommandStderr keeps only the first safe-to-inspect bytes while always
// acknowledging the complete write. Returning a short write would make
// os/exec replace the command's real exit error with an I/O error.
type boundedCommandStderr struct{ bytes.Buffer }

func (w *boundedCommandStderr) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxTokenCommandStderrBytes - w.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.Buffer.Write(p)
	}
	return written, nil
}

// classifyTokenCommandStderr converts raw, untrusted stderr into a fixed safe
// diagnostic. The command may accidentally print a bearer, client secret, or
// terminal control sequence, so raw text is never returned. Known auth failure
// phrases are useful without their surrounding values; everything else is
// represented by a redaction marker.
func classifyTokenCommandStderr(raw []byte) (diagnostic string, authFailure bool) {
	message := strings.ToLower(normalizeTokenCommandStderr(raw))
	switch {
	case strings.Contains(message, "invalid client credentials"):
		return "Invalid client credentials", true
	case strings.Contains(message, "invalid_client"):
		return "cloud client credentials are invalid", true
	case strings.Contains(message, "session expired"):
		return "cloud session expired", true
	case strings.Contains(message, "session is not refreshable"):
		return "cloud session is not refreshable", true
	case strings.Contains(message, "not authenticated"):
		return "cloud authentication is required", true
	case strings.Contains(message, "invalid_grant"):
		return "cloud authentication grant is invalid or expired", true
	case strings.Contains(message, "unauthorized"):
		return "cloud authentication was refused", true
	case message != "":
		return "[redacted: unrecognized token-command stderr]", false
	default:
		return "", false
	}
}

func normalizeTokenCommandStderr(raw []byte) string {
	text := strings.ToValidUTF8(string(raw), " ")
	var normalized strings.Builder
	space := true
	for _, r := range text {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			if !space {
				normalized.WriteByte(' ')
				space = true
			}
			continue
		}
		normalized.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(normalized.String())
}

func isPutnamiCacheTokenCommand(command []string) bool {
	if len(command) != 5 {
		return false
	}
	executable := filepath.Base(command[0])
	if executable != "putnami" && executable != "putnamiw" {
		return false
	}
	return command[1] == "cloud" && command[2] == "token" && command[3] == "--for" && command[4] == "cache"
}

// resolveURL GETs the configured endpoint and returns its trimmed body.
func (t TokenSource) resolveURL(ctx context.Context) (string, error) {
	if err := ValidateURL(t.URL); err != nil {
		return "", fmt.Errorf("cache token endpoint: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, tokenResolveTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return "", fmt.Errorf("cache token request: %w", err)
	}
	setUserAgent(req)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: request targets the user-configured cache token endpoint, https-validated by ValidateURL, not arbitrary user-tainted input
	if err != nil {
		return "", fmt.Errorf("cache token fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes))
	if err != nil {
		return "", fmt.Errorf("cache token read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("cache token endpoint: HTTP %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}
