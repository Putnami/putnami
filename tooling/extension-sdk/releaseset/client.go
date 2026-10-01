package releaseset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/ownerperm"
)

const (
	defaultTimeout     = 60 * time.Second
	providerWaitDelay  = 100 * time.Millisecond
	maxStderrBytes     = 64 * 1024
	responseExtraBytes = 1 // the required trailing newline
)

var (
	// ErrProviderAbsent distinguishes the supported core-only/full floor from a
	// sparse channel operation that cannot proceed without a base and CAS.
	ErrProviderAbsent = errors.New("release-set provider is absent")
	// ErrProviderTimeout identifies a bounded provider call whose deadline fired.
	ErrProviderTimeout = errors.New("release-set provider timed out")
)

// Client invokes the exact distribution provider command through a Putnami
// executable. Executable is normally the current CLI binary; tests use a fake.
type Client struct {
	Executable string
	Dir        string
	Env        []string
	TempDir    string
	Timeout    time.Duration
}

// NewClient returns a bounded client for executable.
func NewClient(executable string) *Client {
	return &Client{Executable: executable, Timeout: defaultTimeout}
}

// Resolve resolves every listed channel exactly once, or one immutable
// release-set id. A channel without a head answers with a null head, never an
// error.
func (c *Client) Resolve(ctx context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	if request == nil || request.ProtocolVersion != distribution.ProtocolVersion {
		return nil, fmt.Errorf("release-set %s request must be protocolVersion %d", distribution.ResolveCommand, distribution.ProtocolVersion)
	}
	if diagnostics := distribution.ValidateResolveRequest(request); diag.HasErrors(diagnostics) {
		return nil, invalidRequestError(distribution.ResolveCommand, diagnostics)
	}
	var response distribution.ResolveResponse
	if err := c.call(ctx, distribution.ResolveCommand, request, func(data []byte) error {
		parsed, diagnostics := distribution.ParseAndValidateResolveResponse(data)
		if parsed == nil || diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ResolveCommand, diagnostics)
		}
		if diagnostics = distribution.ValidateResolveExchange(request, parsed); diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ResolveCommand, diagnostics)
		}
		response = *parsed
		return nil
	}); err != nil {
		return nil, err
	}
	return &response, nil
}

// Release stores one immutable full set and advances every listed channel by
// compare-and-swap in one provider transaction. Registries converge on the
// accepted head afterwards, from the generation the provider stamped; the
// release call itself writes no registry. Conflict is a valid provider
// response and is returned to the coordinator, which alone decides the
// publication verdict.
func (c *Client) Release(ctx context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	if diagnostics := distribution.ValidateReleaseRequest(request); diag.HasErrors(diagnostics) {
		return nil, invalidRequestError(distribution.ReleaseCommand, diagnostics)
	}
	var response distribution.ReleaseResponse
	if err := c.call(ctx, distribution.ReleaseCommand, request, func(data []byte) error {
		parsed, diagnostics := distribution.ParseAndValidateReleaseResponse(data)
		if parsed == nil || diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ReleaseCommand, diagnostics)
		}
		if diagnostics = distribution.ValidateReleaseExchange(request, parsed); diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ReleaseCommand, diagnostics)
		}
		response = *parsed
		return nil
	}); err != nil {
		return nil, err
	}
	return &response, nil
}

// ChannelSet performs the metadata-only channel move used by promotion and
// rollback: the provider re-releases an existing set on the target channel
// under the same compare-and-swap guarantees, and uploads no artifact. It
// answers with the release response, Current keyed by that one channel.
func (c *Client) ChannelSet(ctx context.Context, request *distribution.ChannelSetRequest) (*distribution.ReleaseResponse, error) {
	if diagnostics := distribution.ValidateChannelSetRequest(request); diag.HasErrors(diagnostics) {
		return nil, invalidRequestError(distribution.ChannelSetCommand, diagnostics)
	}
	var response distribution.ReleaseResponse
	if err := c.call(ctx, distribution.ChannelSetCommand, request, func(data []byte) error {
		parsed, diagnostics := distribution.ParseAndValidateReleaseResponse(data)
		if parsed == nil || diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ChannelSetCommand, diagnostics)
		}
		if diagnostics = distribution.ValidateChannelSetExchange(request, parsed); diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ChannelSetCommand, diagnostics)
		}
		response = *parsed
		return nil
	}); err != nil {
		return nil, err
	}
	return &response, nil
}

// ChannelStatus reports desired versus observed for one channel: the head the
// provider accepted, and the generation each registry has applied.
func (c *Client) ChannelStatus(ctx context.Context, request *distribution.ChannelStatusRequest) (*distribution.ChannelStatusResponse, error) {
	if diagnostics := distribution.ValidateChannelStatusRequest(request); diag.HasErrors(diagnostics) {
		return nil, invalidRequestError(distribution.ChannelStatusCommand, diagnostics)
	}
	var response distribution.ChannelStatusResponse
	if err := c.call(ctx, distribution.ChannelStatusCommand, request, func(data []byte) error {
		parsed, diagnostics := distribution.ParseAndValidateChannelStatusResponse(data)
		if parsed == nil || diag.HasErrors(diagnostics) {
			return invalidResponseError(distribution.ChannelStatusCommand, diagnostics)
		}
		response = *parsed
		return nil
	}); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *Client) call(ctx context.Context, operation string, request any, consume func([]byte) error) error {
	if c == nil || strings.TrimSpace(c.Executable) == "" {
		return fmt.Errorf("%w: install exactly one extension declaring %q", ErrProviderAbsent, distribution.ProviderCommandName)
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode release-set %s request: %w", operation, err)
	}
	if len(requestBytes) > distribution.MaxJSONBytes || !utf8.Valid(requestBytes) {
		return fmt.Errorf("release-set %s request is not bounded UTF-8 JSON", operation)
	}
	requestPath, cleanup, err := c.writeRequest(requestBytes)
	if err != nil {
		return fmt.Errorf("prepare release-set %s request file: %w", operation, err)
	}
	defer cleanup()

	var commandEnv []string
	if c.Env == nil {
		commandEnv = os.Environ()
	} else {
		commandEnv = append(make([]string, 0, len(c.Env)), c.Env...)
	}
	commandEnv = removeEnvironmentKey(commandEnv, PublishedImagesFileEnv)
	commandEnv = removeEnvironmentKey(commandEnv, MemberEvidenceFileEnv)
	if operation == distribution.ReleaseCommand {
		if data, present, evidenceErr := memberEvidenceBytes(ctx); evidenceErr != nil {
			return evidenceErr
		} else if present {
			path, cleanup, writeErr := c.writeRequest(data)
			if writeErr != nil {
				return writeErr
			}
			defer cleanup()
			commandEnv = append(commandEnv, MemberEvidenceFileEnv+"="+path)
		}
		if images, present := PublishedImagesFromContext(ctx); present {
			publishedBytes, marshalErr := marshalPublishedImages(images)
			if marshalErr != nil {
				return marshalErr
			}
			publishedPath, publishedCleanup, writeErr := c.writeRequest(publishedBytes)
			if writeErr != nil {
				return fmt.Errorf("prepare release-set published images: %w", writeErr)
			}
			defer publishedCleanup()
			commandEnv = append(commandEnv, PublishedImagesFileEnv+"="+publishedPath)
		}
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		distribution.CloudCommand,
		distribution.ReleaseSetCommand,
		operation,
		distribution.RequestFileFlag,
		requestPath,
	}
	cmd := exec.CommandContext(callCtx, c.Executable, args...) //nolint:gosec // executable is SDK configuration; argv is fixed protocol vocabulary
	cmd.WaitDelay = providerWaitDelay
	cmd.Dir = c.Dir
	cmd.Env = commandEnv
	stdout := newCappedBuffer(distribution.MaxJSONBytes + responseExtraBytes)
	stderr := newCappedBuffer(maxStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if callErr := callCtx.Err(); callErr != nil {
		if errors.Is(callErr, context.Canceled) {
			return fmt.Errorf("release-set provider %s canceled: %w", operation, callErr)
		}
		return fmt.Errorf("%w during %s: %w", ErrProviderTimeout, operation, callErr)
	}
	if stdout.overflow {
		return fmt.Errorf("release-set provider %s stdout exceeds %d bytes", operation, distribution.MaxJSONBytes+responseExtraBytes)
	}
	if stderr.overflow {
		return fmt.Errorf("release-set provider %s stderr exceeds %d bytes", operation, maxStderrBytes)
	}
	if err != nil {
		// Carry the provider's last stderr line as the reason, or a hosted run
		// reports nothing more than an exit status nobody can act on. The line
		// is bounded, stripped of control characters, and redacted of every
		// credential this process handed the child — and of any bearer it
		// printed itself — before it reaches a session or telemetry.
		if reason := providerFailureReason(stderr.Bytes(), commandEnv); reason != "" {
			return fmt.Errorf("release-set provider %s failed for %s: %w: %s", operation, distribution.ProtocolName, err, reason)
		}
		return fmt.Errorf("release-set provider %s failed for %s: %w", operation, distribution.ProtocolName, err)
	}
	responseBytes, err := exactResponseLine(stdout.Bytes())
	if err != nil {
		return fmt.Errorf("release-set provider %s stdout: %w", operation, err)
	}
	return consume(responseBytes)
}

// providerFailureReasonLimit bounds the stderr excerpt carried in an error.
const providerFailureReasonLimit = 512

// providerSecretMinLength is the shortest value redacted from a failure reason.
// Below it, matching removes ordinary words far more often than it removes a
// secret; it is the floor the CLI's own leak guard applies to a byte-derived
// needle (sensitiveNeedleMin in tooling/cli/internal/jobs).
const providerSecretMinLength = 8

// redactedSecret replaces a credential in a failure reason.
const redactedSecret = "[redacted]"

// secretEnvKeyMarkers name the environment keys whose values are never allowed
// into a failure reason, whatever the provider printed.
var secretEnvKeyMarkers = []string{"TOKEN", "SECRET", "CREDENTIAL", "CAPABILITY", "PASSWORD", "BEARER", "KEY", "AUTHORIZATION"}

// bearerCredential matches an Authorization-style bearer anywhere in a line,
// keeping the keyword and replacing only the value. It is the last line of
// defense and the only rule that can remove a credential this process never
// held: a provider mints its own registry bearers, so no environment match can
// cover them. Same matcher the managed npm publisher already applies to an
// upstream error body before quoting it (registryErrorExcerpt in
// typescript/extension/cmd/putnami-ts/publish.go).
var bearerCredential = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)

// providerFailureReason returns the last non-empty stderr line of a failed
// provider, bounded and redacted. Every credential this process handed the
// child is replaced (providerEnvSecrets), then every remaining bearer value, so
// a provider that echoes its Authorization header cannot leak it here.
//
// What it cannot promise: a credential the provider obtained on its own and
// printed in a shape no rule recognizes. Redaction covers the values this
// process knows and the one shape a token is conventionally written in; the
// 512-byte bound is what limits the rest.
func providerFailureReason(stderr []byte, env []string) string {
	lines := strings.Split(strings.TrimRight(string(stderr), "\r\n"), "\n")
	reason := ""
	for index := len(lines) - 1; index >= 0; index-- {
		if candidate := strings.TrimSpace(lines[index]); candidate != "" {
			reason = candidate
			break
		}
	}
	if reason == "" {
		return ""
	}
	// Strip control characters BEFORE redacting: an escape byte inside a
	// secret would otherwise defeat the exact-value match and be removed
	// afterwards, reassembling the secret.
	reason = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, reason)
	for _, secret := range providerEnvSecrets(env) {
		reason = strings.ReplaceAll(reason, secret, redactedSecret)
	}
	reason = bearerCredential.ReplaceAllString(reason, "${1}"+redactedSecret)
	if len(reason) > providerFailureReasonLimit {
		reason = reason[:providerFailureReasonLimit]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
		reason += "…"
	}
	return reason
}

func secretEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range secretEnvKeyMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// providerEnvSecrets returns the values in env that must never reach a failure
// reason: the value of every secret-shaped key, and the userinfo password of
// every URL-shaped value whatever its key.
//
// The second rule is not redundant. A proxy or registry URL carries its bearer
// INSIDE the value, and GOPROXY, npm_config_registry and their kind are named
// for what they address, not for what they carry, so the key-name rule cannot
// see them. It is the rule the CLI's leak guard already applies to a byte
// candidate (urlUserinfoPassword in tooling/cli/internal/jobs).
func providerEnvSecrets(env []string) []string {
	secrets := make([]string, 0, 4)
	keep := func(candidate string) {
		if len(candidate) >= providerSecretMinLength {
			secrets = append(secrets, candidate)
		}
	}
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if secretEnvKey(key) {
			keep(value)
		}
		for _, password := range urlUserinfoSecrets(value) {
			keep(password)
		}
	}
	return secrets
}

// urlUserinfoSecrets returns the password of a URL-shaped value, decoded and in
// the form a URL string actually carries, or nothing when the value is not a
// URL or holds no password. The scheme check keeps url.Parse — which accepts
// almost any string — from turning ordinary text into a spurious credential;
// the encoded form is the one the Go publisher already redacts beside the raw
// token (redactGoRegistrySecret in go/extension).
func urlUserinfoSecrets(value string) []string {
	if !strings.Contains(value, "://") {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return nil
	}
	password, present := parsed.User.Password()
	if !present {
		return nil
	}
	secrets := []string{password}
	if encoded := strings.TrimPrefix(url.UserPassword("", password).String(), ":"); encoded != password {
		secrets = append(secrets, encoded)
	}
	return secrets
}

func removeEnvironmentKey(env []string, key string) []string {
	prefix := key + "="
	result := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func (c *Client) writeRequest(data []byte) (string, func(), error) {
	dir := c.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp(absDir, "putnami-release-set-request-*.json")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	fail := func(cause error) (string, func(), error) {
		_ = file.Close()
		cleanup()
		return "", func() {}, cause
	}
	if !filepath.IsAbs(path) {
		return fail(fmt.Errorf("temporary request path is not absolute"))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	// On Windows the mode bits do not keep other users out: Restrict also
	// gives the file an access list that admits its owner alone, before the
	// request is written. On Unix it sets the same mode again.
	if err := ownerperm.Restrict(path, 0o600); err != nil {
		return fail(err)
	}
	if _, err := file.Write(data); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func exactResponseLine(data []byte) ([]byte, error) {
	if len(data) < 2 || data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("must be exactly one JSON document followed by newline")
	}
	body := data[:len(data)-1]
	if len(body) == 0 || bytes.ContainsAny(body, "\r\n") || !utf8.Valid(body) ||
		body[0] == ' ' || body[0] == '\t' || body[len(body)-1] == ' ' || body[len(body)-1] == '\t' {
		return nil, fmt.Errorf("contains contamination outside the single UTF-8 JSON response")
	}
	return body, nil
}

type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func newCappedBuffer(limit int) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (b *cappedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		_, _ = b.buf.Write(data[:min(remaining, len(data))])
	}
	if len(data) > remaining {
		b.overflow = true
	}
	return written, nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

func invalidRequestError(operation string, diagnostics []diag.Diagnostic) error {
	return fmt.Errorf("invalid release-set %s request: %s", operation, firstDiagnostic(diagnostics))
}

func invalidResponseError(operation string, diagnostics []diag.Diagnostic) error {
	return fmt.Errorf("invalid release-set %s response: %s", operation, firstDiagnostic(diagnostics))
}

func firstDiagnostic(diagnostics []diag.Diagnostic) string {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == diag.Error {
			return diagnostic.String()
		}
	}
	return "validation failed"
}

func hasDiagnosticErrors(diagnostics []diag.Diagnostic) bool { return diag.HasErrors(diagnostics) }

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("unexpected trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
