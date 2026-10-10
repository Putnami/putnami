// Package remotecache implements the build-system client half of the remote
// build cache: it turns the plan-time key set into a single batch "negotiate"
// request and resolves per-key hit/miss against the cloud cache server.
//
// It speaks the HTTP+JSON contract defined by go.putnami.dev/protocol/cache.
// The control round trips go through cache-server's generated client (see
// control.go); the byte transfers over the presigned URLs in the answers use
// the pooled net/http client directly, because a presigned URL carries its own
// credential and contract.
package remotecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// TokenEnv is the environment variable carrying the per-user bearer token.
// There are no shared secrets: each runner authenticates as its own user.
const TokenEnv = "PUTNAMI_CACHE_TOKEN"

// EntryProvenanceHeader opts a current Cloud cache provider into the
// Cloud-specific cache-entry metadata response. Older clients do not send it,
// so the shared HTTP cache response stays strict-v1 compatible.
const EntryProvenanceHeader = "X-Putnami-Cache-Entry-Provenance"

const entryProvenanceVersion = "v1"

// DefaultTimeout bounds a single control-plane round trip (negotiate, store,
// commit). It is applied per attempt via the context in doAuthed rather than as
// a blanket http.Client.Timeout, so it deliberately does NOT cap blob
// upload/download streams — a large artifact must not fail just because it takes
// longer than a control round trip. Blob transfers are bounded by the caller's
// context and the transport's own dial/handshake/response-header timeouts.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes caps a control answer to avoid unbounded memory use on a
// misbehaving or hostile server. An answer past the cap is an error, not a
// truncated body. cache-server declares the same bound for its user routes.
const maxResponseBytes = 64 << 20 // 64 MiB

// maxIdleConnsPerHost sizes the kept-alive (idle) connection pool per host. The
// whole build's CAS traffic — concurrent blob uploads (jobs.uploadConcurrency ×
// blobUploadConcurrency) plus speculative downloads (jobs.prefetchConcurrency) —
// fans out against one or two hosts (control plane and CAS). This does not cap
// live concurrency (Go opens as many connections as the fan-out needs); it caps
// reuse. The stdlib default keeps only 2 idle connections per host, so beyond
// that, finished transfers can't be pooled and are discarded, making the next
// transfer pay a fresh TCP+TLS handshake (churn). Sizing the idle pool above the
// peak lets those connections be reused instead. Keep it ahead of the
// caller-side concurrency if those caps grow.
const maxIdleConnsPerHost = 64

// maxIdleConns bounds idle connections across all hosts (control plane + CAS).
const maxIdleConns = 128

// newTransport returns the pooled HTTP transport the default client uses for the
// control round trips and every presigned blob upload/download. It clones the
// stdlib default (preserving proxy, dial, and TLS settings) and only widens the
// idle-connection pool and forces HTTP/2 attempt so parallel transfers reuse
// connections instead of churning them.
func newTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t := base.Clone()
	t.MaxIdleConns = maxIdleConns
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	t.ForceAttemptHTTP2 = true
	return t
}

// TokenFromEnv returns the per-user bearer token from the environment.
func TokenFromEnv() string {
	return os.Getenv(TokenEnv)
}

// Client negotiates the remote cache for a build.
type Client struct {
	baseURL string

	// bearerFn lazily resolves the per-session credential; see bearer. It runs at
	// most once per token generation, only when a request first needs a bearer —
	// so a TokenSource command does not run for a build that makes no cache
	// request. The resolved bearer is held in memory only, never persisted.
	bearerFn func(context.Context) (Bearer, error)

	// tokenMu guards the memoized credential and its generation. tokenGen is what
	// makes "refresh once" safe under concurrency: a caller refused with a stale
	// bearer asks to invalidate the exact generation it used, so a burst of
	// simultaneous 401s collapses into ONE re-mint instead of one token command
	// per refused request.
	tokenMu  sync.Mutex
	tokenGen uint64
	tokenCur *resolvedBearer

	// authObserver receives a redacted record of every refused (401/403)
	// exchange. Nil disables reporting; it never affects request behavior.
	authObserver func(AuthFailure)

	// httpClient carries the presigned blob transfers and the protocol routes
	// cache-server does not serve; controls wraps a copy of it for the
	// generated control calls, so both share one connection pool.
	httpClient *http.Client
	controls   controlClients
	mode       cache.Mode
	breakEven  cache.BreakEvenParams
}

// resolvedBearer is a memoized credential tagged with the generation it belongs
// to, so a refusal can name the exact credential it was refused on.
type resolvedBearer struct {
	Bearer
	err error
	gen uint64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client (e.g. for tests or custom timeouts).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithMode sets the materialization mode advertised to the server.
func WithMode(m cache.Mode) Option { return func(c *Client) { c.mode = m } }

// WithBreakEven overrides the break-even parameters used to filter keys.
func WithBreakEven(p cache.BreakEvenParams) Option { return func(c *Client) { c.breakEven = p } }

// WithTokenFunc sets a lazy per-session bearer resolver used in place of a
// static token. The resolver's renewability is unknown, so its credential is
// classed static and is never re-minted after a refusal; use WithBearerFunc to
// wire a source whose class the client can act on.
func WithTokenFunc(fn func(context.Context) (string, error)) Option {
	return func(c *Client) {
		c.bearerFn = func(ctx context.Context) (Bearer, error) {
			token, err := fn(ctx)
			return Bearer{Token: token, Class: TokenClassStatic}, err
		}
	}
}

// WithBearerFunc sets a lazy per-session resolver that also reports the class of
// the source it used. It runs at most once per token generation — on the first
// request that needs a bearer — and the result (and any error) is memoized, so a
// TokenSource command executes once, only when a cache request needs auth, and
// is never persisted. A 401 on a renewable class re-runs it exactly once (see
// doAuthed).
func WithBearerFunc(fn func(context.Context) (Bearer, error)) Option {
	return func(c *Client) { c.bearerFn = fn }
}

// WithAuthObserver registers a callback invoked once for every cache exchange
// the server refuses with 401 or 403. It receives redacted classification only
// (see AuthFailure) — never the bearer. It runs on the calling goroutine and
// cache requests run concurrently, so it must be safe for concurrent use and
// must not block.
func WithAuthObserver(fn func(AuthFailure)) Option {
	return func(c *Client) { c.authObserver = fn }
}

// NewClient builds a Client for the cache server at baseURL authenticating with
// the given per-user bearer token. A static token becomes a constant resolver;
// pass WithBearerFunc to resolve a bearer lazily from a TokenSource instead.
func NewClient(baseURL, token string, opts ...Option) *Client {
	class := TokenClassStatic
	if token == "" {
		class = TokenClassNone
	}
	c := &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		bearerFn: func(context.Context) (Bearer, error) { return Bearer{Token: token, Class: class}, nil },
		// No client-wide Timeout: it would also cap blob upload/download streams.
		// The control-plane timeout is applied per attempt in doAuthed instead.
		httpClient: &http.Client{Transport: newTransport()},
		mode:       cache.DefaultMode,
		breakEven:  cache.DefaultBreakEven,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.controls = newControlClients(c.baseURL, c.httpClient)
	return c
}

// bearer lazily resolves the per-session credential, memoizing the first result
// and error for the current generation. The token source (env, a TokenSource
// command, or the GCP metadata service) runs at most once per generation, only
// when a request first needs a bearer.
//
// Resolution happens under the lock so concurrent first-callers wait for one
// resolution instead of each running the token command — the same serialization
// the previous sync.Once provided. TokenSource applies its own per-source
// timeout, so a hung provider cannot hold the lock indefinitely.
func (c *Client) bearer(ctx context.Context) (*resolvedBearer, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.tokenCur != nil {
		return c.tokenCur, c.tokenCur.err
	}
	if c.bearerFn == nil {
		return &resolvedBearer{Bearer: Bearer{Class: TokenClassNone}}, nil
	}
	b, err := c.bearerFn(ctx)
	c.tokenCur = &resolvedBearer{Bearer: b, err: err, gen: c.tokenGen}
	return c.tokenCur, err
}

// UseRunBearer makes token the session's only bearer, in place of the
// configured token source and of any bearer it already resolved. It is the
// run-scoped cache token Delivery issued for the hosted run's credential: it
// is classed TokenClassRun, so a refusal never re-mints it, and the configured
// source never runs again for this client. The token stays in memory.
func (c *Client) UseRunBearer(token string) {
	run := Bearer{Token: token, Class: TokenClassRun}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.bearerFn = func(context.Context) (Bearer, error) { return run, nil }
	c.tokenGen++
	c.tokenCur = &resolvedBearer{Bearer: run, gen: c.tokenGen}
}

// refreshBearer drops the memoized credential if it is still the generation
// stale was refused on, then resolves a fresh one. Guarding on the generation is
// what bounds the re-mint: the first refusal bumps the generation, and every
// later refusal carrying that same stale generation finds it already superseded
// and reuses the credential that replaced it. A whole build's worth of
// concurrent stores refused at once therefore runs `putnami cloud token` once.
func (c *Client) refreshBearer(ctx context.Context, stale *resolvedBearer) (*resolvedBearer, error) {
	c.tokenMu.Lock()
	if c.tokenCur != nil && c.tokenCur.gen == stale.gen {
		c.tokenGen++
		c.tokenCur = nil
	}
	c.tokenMu.Unlock()
	return c.bearer(ctx)
}

// KeyInput is the per-job data the request builder needs: the precomputed cache
// key plus the metadata the eligibility policy and server-side reporting use.
type KeyInput struct {
	Key        string
	Extension  string
	Task       string
	Project    string
	DurationMs int64
	SizeBytes  int64
}

// BuildRequest constructs a negotiate request from inputs, keeping only keys
// eligible for remote caching (not side-effecting and past the break-even
// guard) and applying canonical ordering. The number of remote-eligible keys is
// typically far smaller than the full plan.
func (c *Client) BuildRequest(inputs []KeyInput) *cache.NegotiateRequest {
	req := &cache.NegotiateRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Mode:            c.mode,
	}
	for _, in := range inputs {
		k := cache.KeyRequest{
			Key:        in.Key,
			Extension:  in.Extension,
			Task:       in.Task,
			Project:    in.Project,
			DurationMs: in.DurationMs,
			SizeBytes:  in.SizeBytes,
		}
		if !cache.EligibleForRemote(k, c.breakEven) {
			continue
		}
		req.Keys = append(req.Keys, k)
	}
	cache.NormalizeRequest(req)
	return req
}

// Negotiate sends the batch request and returns the validated response. A
// request with no eligible keys short-circuits to an empty response without a
// round trip. A non-2xx status is surfaced as an error carrying the server's
// structured code/message when present.
func (c *Client) Negotiate(ctx context.Context, req *cache.NegotiateRequest) (*cache.NegotiateResponse, error) {
	if len(req.Keys) == 0 {
		return &cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion}, nil
	}
	if diags := cache.ValidateRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid negotiate request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "negotiate", req, (*cacheserverclient.CacheClient).CreateV1CacheNegotiate)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid negotiate response: %s", firstError(diags))
	}
	return out, nil
}

// EntryProvenance is the cache-server-authenticated metadata associated with a
// hit. It is carried on the Cloud extension response only, then mapped to the
// provider-RPC v2 restore result; it is never filled from UploadParams.
type EntryProvenance struct {
	Producer         cache.Producer
	ProducerIdentity string
	Channel          cache.Channel
}

func (p EntryProvenance) present() bool {
	return p.Producer != "" || p.ProducerIdentity != "" || p.Channel != ""
}

func (p EntryProvenance) valid() bool {
	return p.Producer.Valid() && p.Channel.Valid() && strings.TrimSpace(p.ProducerIdentity) != ""
}

// ProvenanceNegotiation is the normal shared-protocol negotiation response plus
// server-authenticated metadata for its hit keys. A server that does not
// understand the opt-in header returns an empty Provenance map, which is the
// intentional legacy fallback.
type ProvenanceNegotiation struct {
	Response   *cache.NegotiateResponse
	Provenance map[string]EntryProvenance
}

type keyResultWithProvenance struct {
	cache.KeyResult
	Producer         cache.Producer `json:"producer,omitempty"`
	ProducerIdentity string         `json:"producerIdentity,omitempty"`
	Channel          cache.Channel  `json:"channel,omitempty"`
}

type negotiateResponseWithProvenance struct {
	ProtocolVersion int                       `json:"protocolVersion"`
	Results         []keyResultWithProvenance `json:"results,omitempty"`
}

// NegotiateWithProvenance requests the Cloud metadata extension without
// changing the normal HTTP cache contract. The response is rebuilt and checked
// with the shared strict parser after the extension fields are stripped, so the
// cache's existing validation remains the source of truth.
func (c *Client) NegotiateWithProvenance(ctx context.Context, req *cache.NegotiateRequest) (*ProvenanceNegotiation, error) {
	if len(req.Keys) == 0 {
		return &ProvenanceNegotiation{Response: &cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion}}, nil
	}
	if diags := cache.ValidateRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid negotiate request: %s", firstError(diags))
	}

	// The provenance binding adds the opt-in header; Negotiate's binding never
	// sends it.
	data, err := postControlWith(ctx, c, "negotiate", true, req, (*cacheserverclient.CacheClient).CreateV1CacheNegotiate)
	if err != nil {
		return nil, err
	}

	var extended negotiateResponseWithProvenance
	if err := json.Unmarshal(data, &extended); err != nil {
		return nil, fmt.Errorf("decode provenance negotiate response: %w", err)
	}
	base := &cache.NegotiateResponse{ProtocolVersion: extended.ProtocolVersion, Results: make([]cache.KeyResult, len(extended.Results))}
	for i, result := range extended.Results {
		base.Results[i] = result.KeyResult
	}
	baseData, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal shared negotiate response: %w", err)
	}
	validated, diags := cache.ParseAndValidateResponse(baseData)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid negotiate response: %s", firstError(diags))
	}

	provenance := make(map[string]EntryProvenance, len(extended.Results))
	for _, result := range extended.Results {
		metadata := EntryProvenance{Producer: result.Producer, ProducerIdentity: result.ProducerIdentity, Channel: result.Channel}
		if !metadata.present() {
			continue
		}
		if !metadata.valid() {
			return nil, fmt.Errorf("invalid cache-entry provenance for key %q", result.Key)
		}
		provenance[result.Key] = metadata
	}
	return &ProvenanceNegotiation{Response: validated, Provenance: provenance}, nil
}

// doAuthed performs one bearer-carrying cache exchange and — when a RENEWABLE
// token source is refused with 401 — re-mints the bearer and replays the
// request exactly once. It returns the response body and the received status;
// mapping a non-2xx status to an error stays with the caller, because the
// capabilities probe deliberately degrades on one and the other calls error on
// it.
//
// attempt performs one exchange with the given credential and returns the
// received body and status, or an error for a failure that produced no status.
// It runs under a fresh control-plane timeout. The timeout is per ATTEMPT rather
// than across the retry sequence so a replay gets the same budget the first
// attempt had instead of its remainder, and it is applied here (not via
// http.Client.Timeout) so it caps negotiate/store/commit but never a blob
// transfer.
//
// Replaying is safe for every exchange routed through here because they are all
// idempotent by contract: negotiate, find-missing, download-batch,
// capabilities, upload-grant and run-marker lookup are reads; store and
// find-missing are dedup queries whose only effect is issuing presigned PUTs;
// commit and commit-batch are documented no-ops on an already-present key;
// upload-batch is a content-addressed write; and run-marker publish is a
// compare-and-swap on the same (key, sha) pair. Blob transfers are deliberately
// NOT routed here: a presigned URL carries its own credential and never the
// bearer, so it cannot be refused for an expired token.
func (c *Client) doAuthed(ctx context.Context, op string, attempt func(context.Context, *resolvedBearer) ([]byte, int, error)) ([]byte, int, error) {
	for try := 0; ; try++ {
		cred, err := c.bearer(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: resolve cache token: %w", op, err)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
		data, status, err := attempt(attemptCtx, cred)
		cancel()
		if err != nil {
			return nil, status, err
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return data, status, nil
		}

		// Refused. Decide whether a re-minted bearer earns one replay, then
		// report the refusal (redacted) either way, so a run that gives up still
		// leaves the evidence behind.
		refreshed := false
		if try == 0 && status == http.StatusUnauthorized && cred.Class.Renewable() {
			// A 403 is a SCOPE denial — re-minting yields the same identity with
			// the same grants, so only a 401 is worth replaying. Skipping an
			// unchanged credential matters in practice: the GCP metadata server
			// returns its cached ID token until the token nears expiry, so a
			// replay with identical bytes is a guaranteed second rejection.
			if fresh, ferr := c.refreshBearer(ctx, cred); ferr == nil && fresh.Token != "" && fresh.Token != cred.Token {
				refreshed = true
			}
		}
		c.reportAuthFailure(op, status, data, cred, refreshed)
		if refreshed {
			continue
		}
		return data, status, nil
	}
}

// Index returns the response's per-key results keyed by cache key for O(1)
// lookup during execution.
func Index(resp *cache.NegotiateResponse) map[string]cache.KeyResult {
	if resp == nil {
		return nil
	}
	out := make(map[string]cache.KeyResult, len(resp.Results))
	for _, r := range resp.Results {
		out[r.Key] = r
	}
	return out
}

// statusError builds an error from a non-2xx response, preferring the server's
// structured ErrorResponse body when it parses. op labels the failed exchange
// (e.g. "negotiate", "store", "commit").
type StatusError struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s failed: HTTP %d: %s: %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("%s failed: HTTP %d", e.Op, e.Status)
}

func IsStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

func statusError(op string, status int, body []byte) error {
	var e cache.ErrorResponse
	if err := json.Unmarshal(body, &e); err == nil && e.Code != "" {
		return &StatusError{Op: op, Status: status, Code: e.Code, Message: e.Message}
	}
	return &StatusError{Op: op, Status: status}
}

// maxReasonBytes bounds the server's rejection reason carried into a diagnostic
// so a verbose or misbehaving server cannot flood the run log.
const maxReasonBytes = 200

// AuthFailure is a redacted record of a cache exchange the server refused. It
// carries classification ONLY — the operation, the HTTP status, the server's
// structured code and a bounded reason, and the CLASS of the token source. The
// bearer itself, its claims, and its issuer never appear in it, which is what
// makes it safe to print into a CI log.
type AuthFailure struct {
	// Op is the refused exchange ("negotiate", "store", "commit", …).
	Op string
	// Status is the HTTP status the cache server returned: 401 (unauthenticated
	// or expired) or 403 (authenticated but missing the required cache scope).
	Status int
	// Code is the server's structured error code, when it sent a parseable body.
	Code string
	// Message is the server's rejection reason, bounded and redacted.
	Message string
	// Source is the class of token source that produced the refused bearer.
	Source TokenClass
	// Refreshed reports whether the client re-minted the bearer and replayed the
	// exchange once. Only a renewable source refused with 401 can set it.
	Refreshed bool
}

// String renders the one-line diagnostic the runner logs. The field order is
// stable because this line is the record the cache-401 investigation greps.
func (f AuthFailure) String() string {
	line := fmt.Sprintf("auth refused: op=%s status=%d source=%s refreshed=%t", f.Op, f.Status, f.Source, f.Refreshed)
	if f.Code != "" {
		line += " code=" + f.Code
	}
	if f.Message != "" {
		line += " reason=" + strconv.Quote(f.Message)
	}
	return line
}

// reportAuthFailure hands the observer a redacted record of a refused exchange.
func (c *Client) reportAuthFailure(op string, status int, body []byte, cred *resolvedBearer, refreshed bool) {
	if c.authObserver == nil {
		return
	}
	f := AuthFailure{Op: op, Status: status, Source: cred.Class, Refreshed: refreshed}
	var e cache.ErrorResponse
	if err := json.Unmarshal(body, &e); err == nil {
		f.Code = e.Code
		f.Message = redactReason(e.Message, cred.Token)
	}
	c.authObserver(f)
}

// redactReason bounds the server's rejection reason and refuses to pass through
// a message that echoes the bearer back, so a compromised or misconfigured
// server cannot launder the credential into the run log through its own error
// body.
func redactReason(message, token string) string {
	message = strings.TrimSpace(message)
	if token != "" && strings.Contains(message, token) {
		return "[redacted: server echoed the bearer]"
	}
	if len(message) > maxReasonBytes {
		return message[:maxReasonBytes] + "…"
	}
	return message
}

func firstError(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == diag.Error {
			return d.String()
		}
	}
	if len(diags) > 0 {
		return diags[0].String()
	}
	return "unknown error"
}
