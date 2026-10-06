// Package cacheprovider drives an out-of-process cache provider over the
// provider RPC defined in protocol/cache.
//
// A Session spawns one long-lived provider subprocess per build run and speaks
// JSONL request/response over its stdin/stdout: requests are written one per
// line to stdin, responses are read one per line from stdout, and a single
// reader goroutine demultiplexes responses to waiting callers by correlation
// ID, so a speculative prefetch need not block a restore. Large blobs never
// cross the pipe — they move through the blob-exchange directory named in
// InitializeParams.BlobExchangeDir (see cache.BlobExchangePath), a provider↔core
// handoff that core ingests into / exports from its own CAS.
//
// Every op is best-effort. A per-op timeout, a non-OK response, or a provider
// crash (the pipe closes) surfaces as an error so the caller can fall back to a
// local build — the same "the remote cache can never break a build, only
// accelerate it" guarantee the in-core remote cache already makes. Wiring this
// session into the live build scheduler (replacing the in-core remote cache) is
// deferred to the delegate+remove issue; this package and its fake-provider
// tests are the Wave-1 enablement.
package cacheprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/sdk/extension/proctree"
)

// maxResponseBytes bounds a single provider response line. Responses are
// metadata (manifests, results), never blob bytes, but a hit's manifest can list
// many files, so the cap matches the job-event reader's generous bound.
const maxResponseBytes = 16 * 1024 * 1024

// Default timeouts. The op timeout applies to any op call whose context carries
// no deadline; the shutdown timeout bounds the graceful close before a forced
// kill.
const (
	defaultOpTimeout       = 60 * time.Second
	defaultShutdownTimeout = 5 * time.Second
)

// ErrSessionClosed is returned by ops issued after the session has been closed.
var ErrSessionClosed = errors.New("cache provider session closed")

// LaunchSpec describes how to start the provider subprocess. In production the
// command is resolved from the detected provider extension (see the extension
// package's capability gate); tests point it at a fake provider.
type LaunchSpec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
}

// OpError reports a provider op that completed with a non-OK response. It is
// distinct from a transport failure (timeout, crash): the provider answered, but
// the op failed. Callers fall back to a local build either way.
type OpError struct {
	Op        cache.ProviderOp
	Code      string
	Message   string
	Retryable bool
}

func (e *OpError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "provider reported failure"
	}
	return fmt.Sprintf("provider op %s failed (%s): %s", e.Op, e.Code, msg)
}

// Option configures a Session at spawn time.
type Option func(*Session)

// WithOpTimeout sets the default per-op timeout applied when a call's context
// carries no deadline. A non-positive value disables the default.
func WithOpTimeout(d time.Duration) Option { return func(s *Session) { s.opTimeout = d } }

// WithShutdownTimeout bounds the graceful close before the provider is killed.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Session) { s.shutdownTimeout = d }
}

// WithStderr forwards the provider subprocess's stderr to w (default: discarded).
func WithStderr(w io.Writer) Option { return func(s *Session) { s.stderr = w } }

// WithRunCredential gives Initialize the hosted run's credential. Initialize
// advertises cache.CapabilityRunCredential and, when the provider echoes it,
// sends the credential in one cache.OpAuthenticate. The launch environment
// does not change. An empty bearer is the same as no option.
func WithRunCredential(bearer string) Option {
	return func(s *Session) { s.runCredential = bearer }
}

// Session is a live connection to a provider subprocess. Its methods are safe
// for concurrent use: writes are serialized and responses are demultiplexed by
// correlation ID.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.Writer
	tree   *proctree.Tree

	opTimeout       time.Duration
	shutdownTimeout time.Duration
	// runCredential is the hosted run's bearer, sent only in OpAuthenticate;
	// empty without one. A Session is never formatted.
	runCredential string

	writeMu sync.Mutex

	mu     sync.Mutex
	nextID int64
	// protocolVersion is selected by Initialize. Sessions bootstrap at the
	// oldest supported version so a deployed legacy provider can parse the
	// first envelope, then use the version returned by InitializeResult.
	protocolVersion int
	// objectCacheSocket is the object-cache socket path Initialize accepted from
	// the provider, or "" when the object cache is not available for this
	// session. It is deliberately the CHECKED value, not the advertised one.
	objectCacheSocket string
	// restoreResultOnly reports that the provider echoed
	// cache.CapabilityRestoreResultOnly. Until it does, Prefetch and Restore
	// strip the result-only fields from every request.
	restoreResultOnly bool
	pending           map[int64]chan *cache.ProviderResponse
	dead              bool
	deadErr           error
	closing           bool

	waitCh   chan error
	readDone chan struct{}
}

func newSession(opts ...Option) *Session {
	s := &Session{
		stderr:          io.Discard,
		opTimeout:       defaultOpTimeout,
		shutdownTimeout: defaultShutdownTimeout,
		pending:         make(map[int64]chan *cache.ProviderResponse),
		nextID:          1,
		protocolVersion: cache.ProviderProtocolMinVersion,
		waitCh:          make(chan error, 1),
		readDone:        make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Connect wires a Session over caller-supplied pipes instead of a spawned
// subprocess: requests are written to stdin, responses read from stdout, with
// the same framing, demultiplexing, and death semantics as Spawn. Tests use it
// to exercise the provider protocol without paying subprocess startup, which
// under -race costs ~1s per spawn.
func Connect(stdin io.WriteCloser, stdout io.ReadCloser, opts ...Option) *Session {
	s := newSession(opts...)
	s.stdin = stdin
	s.stdout = stdout
	s.waitCh <- nil // no subprocess to wait for
	go s.readLoop()
	return s
}

// Spawn starts the provider subprocess and begins reading its responses. The
// provided context governs the subprocess lifetime: canceling it terminates the
// provider (SIGTERM, then SIGKILL after a grace period), mirroring the job
// runner. Call Initialize next, then the per-op methods, and Close at the end.
func Spawn(ctx context.Context, spec LaunchSpec, opts ...Option) (*Session, error) {
	s := newSession(opts...)

	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stderr = s.stderr
	// Own process tree so cancellation can reach the whole tree.
	tree := proctree.New(cmd)
	cmd.Cancel = tree.Terminate
	cmd.WaitDelay = defaultShutdownTimeout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provider stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("provider stdout pipe: %w", err)
	}

	if err := tree.Start(); err != nil {
		return nil, fmt.Errorf("start cache provider %s: %w", spec.Command, err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.tree = tree

	go func() { s.waitCh <- cmd.Wait() }()
	go s.readLoop()

	return s, nil
}

// readLoop demultiplexes provider responses until the pipe closes. On EOF, a
// scan error, or a malformed response it marks the session dead so every pending
// and future op fails fast into a local-build fallback.
func (s *Session) readLoop() {
	defer close(s.readDone)
	sc := bufio.NewScanner(s.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		resp, diags := cache.ParseAndValidateProviderResponse(line)
		if resp == nil {
			s.markDead(fmt.Errorf("cache provider sent a malformed response: %v", diags))
			return
		}
		s.deliver(resp)
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	s.markDead(fmt.Errorf("cache provider session ended: %w", err))
}

// deliver routes a response to its waiting caller. An unknown ID (a late
// response to an op that already timed out) is dropped.
func (s *Session) deliver(resp *cache.ProviderResponse) {
	s.mu.Lock()
	ch, ok := s.pending[resp.ID]
	if ok {
		delete(s.pending, resp.ID)
	}
	s.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// markDead records a fatal session error and fails every pending op. It is
// idempotent.
func (s *Session) markDead(err error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead = true
	s.deadErr = err
	pending := s.pending
	s.pending = make(map[int64]chan *cache.ProviderResponse)
	s.mu.Unlock()
	for _, ch := range pending {
		ch <- nil // nil signals death; the caller maps it to deadErr
	}
}

func (s *Session) deathErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadErr != nil {
		return s.deadErr
	}
	return errors.New("cache provider session ended")
}

// call writes one request and waits for its correlated response, the context
// deadline, or session death.
func (s *Session) call(ctx context.Context, op cache.ProviderOp, params any) (*cache.ProviderResponse, error) {
	raw, err := cache.MarshalPayload(params)
	if err != nil {
		return nil, fmt.Errorf("encode %s params: %w", op, err)
	}

	s.mu.Lock()
	if s.dead {
		err := s.deadErr
		s.mu.Unlock()
		return nil, err
	}
	id := s.nextID
	s.nextID++
	protocolVersion := s.protocolVersion
	ch := make(chan *cache.ProviderResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	req := &cache.ProviderRequest{ProtocolVersion: protocolVersion, ID: id, Op: op, Payload: raw}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	if err := s.writeLine(line); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("provider op %s: %w", op, ctx.Err())
	case resp := <-ch:
		if resp == nil {
			return nil, s.deathErr()
		}
		return resp, nil
	}
}

// writeLine writes one framed request. A write failure marks the session dead.
func (s *Session) writeLine(line []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.markDead(fmt.Errorf("cache provider write failed: %w", err))
		return s.deathErr()
	}
	return nil
}

// withTimeout applies the default op timeout when the caller's context has no
// deadline of its own.
func (s *Session) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || s.opTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.opTimeout)
}

func (s *Session) do(ctx context.Context, op cache.ProviderOp, params any) (*cache.ProviderResponse, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return s.call(ctx, op, params)
}

// --- ops ---

// Initialize opens the session and performs the runtime half of the
// capability/version gate. It deliberately bootstraps at the oldest supported
// version so a strict legacy provider can parse the first envelope and payload.
// The bootstrap capability tells a newer provider it may select v2; the version
// in InitializeResult then becomes the version for every later request. The
// caller inspects Ready to decide whether the provider can actually serve the
// cache.
//
// With a run credential (WithRunCredential), the bootstrap also advertises
// cache.CapabilityRunCredential. A provider that echoes it receives the
// credential in one authenticate request, sent before Initialize returns and
// so before any other op; a failed authenticate fails Initialize. A malformed
// run credential fails Initialize before anything is sent. Without the echo
// nothing more is sent.
//
// The bootstrap always advertises cache.CapabilityRestoreResultOnly. Only a
// provider that echoes it receives RestoreParams.ResultOnly and
// PrefetchParams.ResultOnlyKeys; see RestoreResultOnly.
func (s *Session) Initialize(ctx context.Context, p *cache.InitializeParams) (*cache.InitializeResult, error) {
	if p == nil {
		return nil, errors.New("provider initialize params are nil")
	}
	if s.runCredential != "" && !cache.ValidRunCredential(s.runCredential) {
		return nil, fmt.Errorf("the run credential is not 1 to %d bytes of UTF-8 with no whitespace", cache.MaxRunCredentialBytes)
	}

	// Do not mutate the caller's reusable params. A v1 provider rejects both a
	// v2 envelope and a v2 InitializeParams body before it can advertise the
	// version it supports. Capabilities are an existing, forward-tolerant field,
	// so the v2 opt-in is safe for the v1 bootstrap shape.
	bootstrap := *p
	bootstrap.ProtocolVersion = cache.ProviderProtocolMinVersion
	bootstrap.Capabilities = append([]string(nil), p.Capabilities...)
	bootstrap.Capabilities = appendCapability(bootstrap.Capabilities, cache.CapabilityProviderProtocolV2)
	// The object cache is the same kind of opt-in: an unknown capability string
	// is safe for a strict provider to ignore, and a provider that does ignore it
	// must not answer with a socket.
	bootstrap.Capabilities = appendCapability(bootstrap.Capabilities, cache.CapabilityObjectCache)
	// Only a core that holds a run credential asks for it, so a local run's
	// initialize never lists it.
	if s.runCredential != "" {
		bootstrap.Capabilities = appendCapability(bootstrap.Capabilities, cache.CapabilityRunCredential)
	}
	// Result-only restores are an opt-in of the same kind: the request fields
	// that carry them are sent only after the echo.
	bootstrap.Capabilities = appendCapability(bootstrap.Capabilities, cache.CapabilityRestoreResultOnly)

	resp, err := s.do(ctx, cache.OpInitialize, &bootstrap)
	if err != nil {
		return nil, err
	}
	result, err := decodeResult(cache.OpInitialize, resp, cache.ParseAndValidateInitializeResult)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.protocolVersion = result.ProtocolVersion
	s.objectCacheSocket = negotiatedObjectCacheSocket(result)
	s.restoreResultOnly = containsCapability(result.Capabilities, cache.CapabilityRestoreResultOnly)
	s.mu.Unlock()

	if s.runCredential != "" && containsCapability(result.Capabilities, cache.CapabilityRunCredential) {
		if err := s.authenticate(ctx); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// authenticate sends the run credential once. A refusal's message never
// carries the credential into the error.
func (s *Session) authenticate(ctx context.Context) error {
	resp, err := s.do(ctx, cache.OpAuthenticate, &cache.AuthenticateParams{Credential: s.runCredential})
	if err != nil {
		return err
	}
	_, err = decodeResult(cache.OpAuthenticate, resp, cache.ParseAndValidateAuthenticateResult)
	var opErr *OpError
	if errors.As(err, &opErr) {
		opErr.Message = strings.ReplaceAll(opErr.Message, s.runCredential, "<redacted>")
	}
	return err
}

// ObjectCacheSocket returns the object-cache socket path negotiated for this
// session, or "" when the object cache is unavailable. Callers export it to job
// processes; its emptiness is the off switch.
func (s *Session) ObjectCacheSocket() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objectCacheSocket
}

// negotiatedObjectCacheSocket applies the three conditions that make a provider's
// socket usable, and fails closed on each:
//
//  1. the provider ECHOED CapabilityObjectCache, so the path is an answer to the
//     capability core advertised and not an unrelated field;
//  2. the path is ABSOLUTE, because a job subprocess runs in its project
//     directory, not in the provider's;
//  3. the path EXISTS AND IS A SOCKET right now, so a stale or crashed provider
//     cannot make every job wait on a dial that can never connect.
//
// Anything else leaves the object cache off for the run. That is the whole cost
// of the feature failing: jobs keep whatever local cache they already had.
func negotiatedObjectCacheSocket(result *cache.InitializeResult) string {
	if result == nil {
		return ""
	}
	path := strings.TrimSpace(result.ObjectCacheSocket)
	if path == "" || !containsCapability(result.Capabilities, cache.CapabilityObjectCache) {
		return ""
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return path
}

// RestoreResultOnly reports whether the provider echoed
// cache.CapabilityRestoreResultOnly, so a result-only restore downloads no blob.
// Without the echo, Prefetch and Restore send neither result-only field and
// every restore hit places its blobs as before.
func (s *Session) RestoreResultOnly() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoreResultOnly
}

func containsCapability(capabilities []string, capability string) bool {
	for _, got := range capabilities {
		if got == capability {
			return true
		}
	}
	return false
}

// Prefetch asks the provider to speculatively pull the given keys into the CAS.
// It sends PrefetchParams.ResultOnlyKeys only to a provider that echoed
// cache.CapabilityRestoreResultOnly, and never modifies p.
func (s *Session) Prefetch(ctx context.Context, p *cache.PrefetchParams) (*cache.PrefetchResult, error) {
	if p != nil && p.ResultOnlyKeys != nil && !s.RestoreResultOnly() {
		wire := *p
		wire.ResultOnlyKeys = nil
		p = &wire
	}
	resp, err := s.do(ctx, cache.OpPrefetch, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpPrefetch, resp, cache.ParseAndValidatePrefetchResult)
}

// Restore materializes one key's blobs into the CAS and returns the cached
// result on a hit. A miss or an error status both mean "build locally". It
// sends RestoreParams.ResultOnly only to a provider that echoed
// cache.CapabilityRestoreResultOnly, and never modifies p: without the echo a
// hit places its blobs as before.
func (s *Session) Restore(ctx context.Context, p *cache.RestoreParams) (*cache.RestoreResult, error) {
	if p != nil && p.ResultOnly && !s.RestoreResultOnly() {
		wire := *p
		wire.ResultOnly = false
		p = &wire
	}
	resp, err := s.do(ctx, cache.OpRestore, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpRestore, resp, cache.ParseAndValidateRestoreResult)
}

// Upload hands the provider a freshly built entry whose blobs are already in the
// CAS, to store and upload in the background.
func (s *Session) Upload(ctx context.Context, p *cache.UploadParams) (*cache.UploadResult, error) {
	resp, err := s.do(ctx, cache.OpUpload, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpUpload, resp, cache.ParseAndValidateUploadResult)
}

// LookupMarker reads the last successful whole-target run marker.
func (s *Session) LookupMarker(ctx context.Context, p *cache.MarkerLookupParams) (*cache.MarkerLookupResult, error) {
	resp, err := s.do(ctx, cache.OpMarkerLookup, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpMarkerLookup, resp, cache.ParseAndValidateMarkerLookupResult)
}

// WriteMarker publishes a successful whole-target run marker.
func (s *Session) WriteMarker(ctx context.Context, p *cache.MarkerWriteParams) (*cache.MarkerWriteResult, error) {
	resp, err := s.do(ctx, cache.OpMarkerWrite, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpMarkerWrite, resp, cache.ParseAndValidateMarkerWriteResult)
}

// Summary drains pending background uploads and returns the run's statistics.
func (s *Session) Summary(ctx context.Context, p *cache.SummaryParams) (*cache.SummaryResult, error) {
	if p == nil {
		p = &cache.SummaryParams{}
	}
	resp, err := s.do(ctx, cache.OpSummary, p)
	if err != nil {
		return nil, err
	}
	return decodeResult(cache.OpSummary, resp, cache.ParseAndValidateSummaryResult)
}

// Close shuts the session down: it sends a best-effort shutdown op, closes
// stdin so the provider sees EOF, and waits for the process to exit, forcing a
// kill if it overstays the shutdown timeout. Close is idempotent.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	dead := s.dead
	s.mu.Unlock()

	if !dead {
		shutCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		_, _ = s.call(shutCtx, cache.OpShutdown, nil)
		cancel()
	}
	_ = s.stdin.Close()

	select {
	case <-s.waitCh:
	case <-time.After(s.shutdownTimeout):
		_ = s.tree.Kill()
		<-s.waitCh
	}
	_ = s.tree.Close()
	<-s.readDone
	return nil
}

// decodeResult turns a provider response into a typed result. A non-OK response
// becomes an OpError; an empty payload yields a zero-valued result (ops whose
// result carries only optional fields may answer with no body).
func decodeResult[T any](op cache.ProviderOp, resp *cache.ProviderResponse, parse func([]byte) (*T, []diag.Diagnostic)) (*T, error) {
	if !resp.OK {
		return nil, opErrorFrom(op, resp.Error)
	}
	if len(resp.Payload) == 0 {
		return new(T), nil
	}
	v, diags := parse(resp.Payload)
	if v == nil {
		return nil, fmt.Errorf("provider op %s returned an invalid result: %v", op, diags)
	}
	return v, nil
}

func opErrorFrom(op cache.ProviderOp, e *cache.ProviderError) error {
	if e == nil {
		return &OpError{Op: op, Code: "unknown", Message: "provider reported failure without details"}
	}
	return &OpError{Op: op, Code: e.Code, Message: e.Message, Retryable: e.Retryable}
}

// trimSpace trims ASCII whitespace from a byte slice without allocating.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && asciiSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && asciiSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func appendCapability(capabilities []string, capability string) []string {
	for _, got := range capabilities {
		if got == capability {
			return capabilities
		}
	}
	return append(capabilities, capability)
}
