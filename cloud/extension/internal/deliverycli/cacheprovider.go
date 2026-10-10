package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/procguard"
)

// providerName identifies this provider implementation in InitializeResult. Core
// uses it to disambiguate when several extensions advertise the provider command.
const providerName = "@putnami/cloud"

// maxProviderRequestBytes bounds a single provider-request line. Requests carry
// metadata (params, manifests) — never blob bytes, which travel through the
// blob-exchange directory — but a restore/upload manifest can list many files,
// so the cap matches the framework session harness's response bound.
const maxProviderRequestBytes = 16 * 1024 * 1024

// CacheProvider runs the extension binary as the remote build-cache
// provider: the server half of the provider RPC defined in protocol/cache. Core
// (the build scheduler) spawns this subprocess once per build run and drives it
// with one cache.ProviderRequest per line on stdin, reading one
// cache.ProviderResponse per line from stdout; large blobs move through the
// blob-exchange directory, never the pipe.
//
// Because the protocol owns stdout, every diagnostic goes to stderr (which core
// forwards) — a stray stdout write would corrupt the response stream. The
// session reads raw os.Stdin/os.Stdout directly rather than the IO formatting
// funcs for the same reason.
func CacheProvider(_ map[string]any, _ []string, workspaceRoot string, _ map[string]string, _ clicore.IO) error {
	return runCacheProviderSession(os.Stdin, os.Stdout, os.Stderr, workspaceRoot)
}

// restoreConcurrency bounds the restore transfers running at once: core's
// workers restore in parallel, so serving them one at a time would serialize
// the whole warm rebuild behind single-key network round trips.
// prefetchConcurrency bounds the background blob-warming downloads.
const (
	restoreConcurrency  = 16
	prefetchConcurrency = 8
)

// providerSession holds the per-run state established by initialize and used by
// the data-plane ops: the configured remote-cache client and the blob-exchange
// directory core hands blobs through. Control-plane requests are served from
// the read loop one at a time; prefetch and upload return promptly and do their
// work on background goroutines, and restores run concurrently on a bounded
// pool (all tracked by bg, which summary and shutdown drain) — core's session
// matches responses by ID, so answering out of order is part of the protocol.
type providerSession struct {
	workspaceRoot string
	logw          io.Writer

	exchangeDir string
	mode        cache.Mode
	readOnly    bool
	workspace   string
	branch      string
	// providerProtocolVersion is selected during the v1 initialize bootstrap.
	// v2 enables cache-entry provenance for the rest of the session; v1 keeps
	// old cores and strict channel-less providers byte-compatible.
	providerProtocolVersion int
	known                   map[string]bool     // digests core already has in its CAS — never re-staged
	client                  *remotecache.Client // nil when the cache is not configured (local-only)
	// objects serves the generic object cache to JOB processes over a Unix socket
	// (see cacheobjects.go). It is nil unless initialize negotiated the
	// capability with BOTH core and the cache server, which is the feature's
	// whole off switch: no socket, no capability echo, nothing exported to jobs.
	objects *objectCache

	ctx    context.Context
	cancel context.CancelFunc

	bg         sync.WaitGroup // background prefetch + upload + restore work
	restoreSem chan struct{}  // bounds concurrent restore transfers

	mu    sync.Mutex
	stats cache.SummaryResult
	// authRefusals counts cache exchanges the server refused (401/403) and
	// firstAuthRefusal keeps the first one's redacted record. Without them a run
	// whose every store was rejected reports uploadedCount=0 — byte-identical to
	// a fully deduped warm run — so a permanently cold cache looks healthy.
	// They are reported at summary, never as a build failure.
	authRefusals      int
	authWriteRefusals int
	firstAuthRefusal  string
	// firstObjectAuthRefusal is the same record for the OBJECT cache, kept apart
	// on purpose: object writes are best-effort and are refused on every run
	// until the default-branch write lease is armed, so they must not spend the
	// session's single first-refusal slot that the cache-401 investigation reads.
	// See observeObjectAuthRefusal.
	firstObjectAuthRefusal string
	// tokenCommandFailureLogged collapses the memoized token-command error into
	// one actionable session warning. Without this guard every prefetch,
	// restore, upload, and marker operation repeats the same opaque failure.
	tokenCommandFailureLogged bool
	// negotiateIdx caches the prefetch's batched negotiate outcome for every
	// prefetched key (misses included) so each restore is served from it
	// instead of issuing its own single-key negotiate round trip — before this
	// index a fully warm rebuild spent its entire wall time on serialized
	// per-restore negotiates. prefetchDone is closed once the index is
	// populated (or the batched negotiate failed, leaving the index nil), so a
	// restore racing the batch waits for it rather than duplicating it. The
	// batch is the run's consistency window — the same single build-wide
	// negotiate semantics the in-core client had before the provider split.
	negotiateIdx map[string]providerCacheResult
	prefetchDone chan struct{}

	// runCredentialCaller is set when initialize echoed
	// cache.CapabilityRunCredential: core then sends the hosted run's
	// credential in one authenticate, which exchanges it at Delivery for the
	// run's cache token. authenticated records that the one authenticate ran.
	// The run credential itself is never stored on the session.
	runCredentialCaller *capabilityCaller
	authenticated       bool
}

// cacheProviderDenyInspection marks the process non-dumpable before it accepts
// a run credential. Tests replace it.
var cacheProviderDenyInspection = procguard.DenyInspection

type providerCacheResult struct {
	result     cache.KeyResult
	provenance remotecache.EntryProvenance
}

func newProviderSession(workspaceRoot string, logw io.Writer) *providerSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &providerSession{
		workspaceRoot: workspaceRoot,
		logw:          logw,
		ctx:           ctx,
		cancel:        cancel,
		known:         make(map[string]bool),
		restoreSem:    make(chan struct{}, restoreConcurrency),
	}
}

// close cancels the session context (aborting any in-flight background transfer)
// and waits for the background goroutines to exit, so the process never returns
// while a goroutine still writes to the exchange directory.
func (s *providerSession) close() {
	s.cancel()
	s.objects.close()
	s.bg.Wait()
}

// log writes one diagnostic line to stderr. The protocol owns stdout, so all
// logging is funneled here.
func (s *providerSession) log(msg string) {
	fmt.Fprintln(s.logw, "cache-provider: "+msg) //nolint:gosec // G705: logw is the provider's stderr, never an HTTP response — no XSS sink
}

// logCacheError keeps ordinary cache failures scoped to their operation, but a
// TokenCommandError is session-wide: the client memoizes that failed resolution
// and no remote operation can work until the next run. Say it once, early, with
// its redaction-safe diagnostic and recovery instruction.
func (s *providerSession) logCacheError(scope string, err error) {
	var tokenCommandErr *remotecache.TokenCommandError
	if !errors.As(err, &tokenCommandErr) {
		s.log(fmt.Sprintf("%s: %v", scope, err))
		return
	}

	s.mu.Lock()
	first := !s.tokenCommandFailureLogged
	if first {
		s.tokenCommandFailureLogged = true
	}
	s.mu.Unlock()
	if first {
		s.log(fmt.Sprintf("WARNING: remote cache disabled for this session after %s: %v", scope, tokenCommandErr))
	}
}

func (s *providerSession) addStats(f func(*cache.SummaryResult)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

// cacheWriteOps are the TASK-cache exchanges that persist something. A refusal
// on any of them means this run stored no task entry, which is what makes the
// summary warning worth printing: reads can be refused and the run still just
// runs cold, but refused WRITES leave the next run cold too.
//
// "objects store" is deliberately NOT one of them. A build-cache object is
// best-effort — and refused on every run until the default-branch cache write
// lease is armed — so counting it here would print "this run persisted NO cache
// entries" on a pull-request run whose every task entry was stored. A warning
// that fires on healthy runs is a warning operators learn to ignore, and this
// one exists to catch a genuinely cold cache. Object refusals report
// themselves on the object cache's own summary line instead.
var cacheWriteOps = map[string]bool{
	"store":              true,
	"commit":             true,
	"find-missing":       true,
	"commit-batch":       true,
	"upload-batch":       true,
	"run-marker publish": true,
}

// cacheObjectOps are the exchanges that belong to the object cache alone. They
// are routed away from the run's task-cache verdict entirely; see
// observeObjectAuthRefusal.
var cacheObjectOps = map[string]bool{
	"objects lookup": true,
	"objects store":  true,
}

// observeAuthRefusal records a redacted cache auth refusal. The first one per
// session is logged in full — the record the cache-401 investigation reads:
// which auth source was used, which exchange was refused, whether the bearer was
// re-minted, and the server's own rejection class — and the rest are only
// counted, so a run whose every key is refused does not print one line per key.
//
// It never sees the bearer: AuthFailure carries classification only.
func (s *providerSession) observeAuthRefusal(f remotecache.AuthFailure) {
	if cacheObjectOps[f.Op] {
		s.observeObjectAuthRefusal(f)
		return
	}
	s.mu.Lock()
	s.authRefusals++
	if cacheWriteOps[f.Op] {
		s.authWriteRefusals++
	}
	first := s.firstAuthRefusal == ""
	if first {
		s.firstAuthRefusal = f.String()
	}
	s.mu.Unlock()
	if first {
		s.log(f.String())
	}
}

// observeObjectAuthRefusal records an object-cache refusal WITHOUT touching the
// run's task-cache verdict — not the refusal counts, not the write-refusal
// count, and not the first-refusal slot.
//
// The object cache is an accelerator whose writes are refused by design on every
// run the cache write lease does not cover. Folding those refusals into the task
// counters would make a run whose every task entry was stored announce that it
// "persisted NO cache entries", and would spend the session's single
// first-refusal record — the line the cache-401 investigation reads — on the
// expected refusal instead of the surprising one.
//
// The record itself is not lost: it is logged once, under the object cache's own
// name, through the same redaction path (classification only, never the bearer).
func (s *providerSession) observeObjectAuthRefusal(f remotecache.AuthFailure) {
	s.mu.Lock()
	first := s.firstObjectAuthRefusal == ""
	if first {
		s.firstObjectAuthRefusal = f.String()
	}
	s.mu.Unlock()
	if first {
		s.log("object cache: " + f.String())
	}
}

// runCacheProviderSession runs the request/response loop until stdin closes
// (core went away — the crash/normal-exit path) or a shutdown op is received.
// It is separated from CacheProvider so tests can drive it over buffers.
func runCacheProviderSession(in io.Reader, out io.Writer, logw io.Writer, workspaceRoot string) error {
	sess := newProviderSession(workspaceRoot, logw)
	defer sess.close()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxProviderRequestBytes)
	w := bufio.NewWriter(out)
	// Restores answer from bounded background goroutines (core matches
	// responses by ID), so their writes interleave with the loop's — every
	// response write goes through this one serialized, flushed path.
	var wmu sync.Mutex
	writeResp := func(resp *cache.ProviderResponse) error {
		wmu.Lock()
		defer wmu.Unlock()
		if err := writeProviderResponse(w, resp); err != nil {
			return err
		}
		return w.Flush()
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		req, diags := cache.ParseAndValidateProviderRequest(line)
		if req == nil {
			// No ID to answer on a malformed request; core's per-op timeout turns
			// the silence into a local-build fallback. Log and keep serving.
			sess.log(fmt.Sprintf("dropping malformed request: %v", diags))
			continue
		}
		if req.Op == cache.OpRestore && sess.serving() {
			// Serve restores concurrently: each may move blobs over the network,
			// and core's workers restore in parallel — answering them one at a
			// time from this loop serialized the whole warm rebuild. Params are
			// parsed here, synchronously, so the payload's alias into the read
			// buffer never escapes; the goroutine gets its own copy of the
			// request envelope for the same reason.
			p, pdiags := cache.ParseAndValidateRestoreParams(req.Payload)
			reqCopy := *req
			sess.bg.Add(1)
			go func() {
				defer sess.bg.Done()
				sess.restoreSem <- struct{}{}
				defer func() { <-sess.restoreSem }()
				var resp *cache.ProviderResponse
				if p == nil {
					resp = providerErr(&reqCopy, "invalid_params", fmt.Sprintf("restore: %v", pdiags))
				} else {
					resp = sess.restore(&reqCopy, p)
				}
				if err := writeResp(resp); err != nil {
					sess.log(fmt.Sprintf("restore %d: write response: %v", reqCopy.ID, err))
				}
			}()
			continue
		}
		if req.Op == cache.OpShutdown {
			// Let in-flight restores answer before acking the shutdown; core
			// stops reading once the ack lands.
			sess.bg.Wait()
		}
		resp := sess.handle(req)
		if err := writeResp(resp); err != nil {
			return err // pipe closed: core went away
		}
		if req.Op == cache.OpShutdown {
			return nil // acked the shutdown; exit cleanly
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("cache-provider read: %w", err)
	}
	return nil // EOF: core closed stdin
}

// handle parses the op-specific params (synchronously, before any background
// goroutine, so req.Payload's alias into the read buffer never escapes) and
// dispatches. Every op is best-effort: a non-OK response, or for the data plane
// a clean miss/not-accepted, only tells core to build locally.
func (s *providerSession) handle(req *cache.ProviderRequest) *cache.ProviderResponse {
	switch req.Op {
	case cache.OpInitialize:
		p, diags := cache.ParseAndValidateInitializeParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("initialize: %v", diags))
		}
		return s.initialize(req, p)
	case cache.OpAuthenticate:
		p, diags := cache.ParseAndValidateAuthenticateParams(req.Payload)
		if p == nil {
			// The diagnostics describe the credential's shape; never echo them.
			return providerErr(req, "invalid_params", fmt.Sprintf("authenticate: the params are not one run credential (%d problems)", len(diags)))
		}
		return s.authenticate(req, p)
	case cache.OpPrefetch:
		p, diags := cache.ParseAndValidatePrefetchParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("prefetch: %v", diags))
		}
		return s.prefetch(req, p)
	case cache.OpRestore:
		p, diags := cache.ParseAndValidateRestoreParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("restore: %v", diags))
		}
		return s.restore(req, p)
	case cache.OpUpload:
		p, diags := cache.ParseAndValidateUploadParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("upload: %v", diags))
		}
		return s.upload(req, p)
	case cache.OpMarkerLookup:
		p, diags := cache.ParseAndValidateMarkerLookupParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("marker-lookup: %v", diags))
		}
		return s.markerLookup(req, p)
	case cache.OpMarkerWrite:
		p, diags := cache.ParseAndValidateMarkerWriteParams(req.Payload)
		if p == nil {
			return providerErr(req, "invalid_params", fmt.Sprintf("marker-write: %v", diags))
		}
		return s.markerWrite(req, p)
	case cache.OpSummary:
		return s.summary(req)
	case cache.OpShutdown:
		return providerOK(req, nil)
	default:
		return providerErr(req, "unknown_op", fmt.Sprintf("unsupported provider op %q", req.Op))
	}
}

// initialize opens the session: it records the blob-exchange directory and
// run-marker identity, resolves the materialization mode (core's intent, else the
// configured default), and builds the remote-cache client when the cache is
// configured and enabled. A provider with no client reports Ready=false and
// stays a no-op so core remains local-only without an error.
func (s *providerSession) initialize(req *cache.ProviderRequest, p *cache.InitializeParams) *cache.ProviderResponse {
	s.exchangeDir = p.BlobExchangeDir
	s.workspace = p.Workspace
	s.branch = p.Branch

	// Presence stays core-owned: core owns its CAS, so it hands the provider the
	// digests it already has (KnownDigests). The provider only consumes that set
	// — it skips re-staging those blobs on restore — and never maintains its own
	// presence file (.putnami/cache-present.json). The provider-owned-presence
	// path (returning an updated set in SummaryResult.KnownDigests for large warm
	// sets) is a deferred optimization; the seam is left in place, unused.
	for _, d := range p.KnownDigests {
		s.known[d] = true
	}

	cfg, err := remotecache.LoadConfig(CachePath(s.workspaceRoot))
	if err != nil {
		s.log(fmt.Sprintf("initialize: load cache config: %v", err))
		cfg = &remotecache.Config{}
	}
	if err := cfg.ApplyEnv(); err != nil { // PUTNAMI_CACHE_URL/MODE/READ_ONLY overrides (the CI path)
		s.log(fmt.Sprintf("initialize: apply cache environment: %v", err))
		disabled := false
		cfg = &remotecache.Config{Enabled: &disabled}
	}
	s.applyLinkedCacheDefault(cfg)
	s.readOnly = cfg.ReadOnly

	mode := p.Mode
	if !mode.Valid() {
		mode = cfg.Mode
	}
	if !mode.Valid() {
		mode = cache.DefaultMode
	}
	s.mode = mode
	s.providerProtocolVersion = cache.ProviderProtocolMinVersion
	for _, capability := range p.Capabilities {
		if capability == cache.CapabilityProviderProtocolV2 {
			s.providerProtocolVersion = cache.ProviderProtocolVersion
			break
		}
	}

	if cfg.Active() {
		s.client = remotecache.NewClient(cfg.URL, "",
			remotecache.WithMode(mode),
			// ResolveBearer, not ResolveToken: the client needs the token SOURCE
			// class to know whether a refused bearer can be re-minted. A static
			// PUTNAMI_CACHE_TOKEN is never re-minted; the command and metadata
			// recipes are.
			remotecache.WithBearerFunc(cfg.ResolveBearer),
			remotecache.WithAuthObserver(s.observeAuthRefusal),
		)
	}

	result := &cache.InitializeResult{
		ProtocolVersion: s.providerProtocolVersion,
		ProviderName:    providerName,
		ProviderVersion: providerVersion(),
		Ready:           s.serving(),
	}
	// The object cache is negotiated HERE, synchronously, because core validates
	// the socket while it processes this very response: it requires that the
	// provider echoed the capability, that the path is absolute, and that
	// os.Stat reports a socket at that instant. Binding it lazily, or from a
	// goroutine, would race that check and silently disable the feature.
	if s.objects = s.negotiateObjectCache(p); s.objects != nil {
		result.Capabilities = append(result.Capabilities, cache.CapabilityObjectCache)
		result.ObjectCacheSocket = s.objects.path
	}
	if slices.Contains(p.Capabilities, cache.CapabilityRunCredential) && s.acceptRunCredential() {
		result.Capabilities = append(result.Capabilities, cache.CapabilityRunCredential)
	}
	return providerOK(req, result)
}

// acceptRunCredential decides whether to echo cache.CapabilityRunCredential,
// which makes core send the hosted run's credential in one authenticate right
// after this initialize. It echoes only when the credential can do something
// here: a remote cache is configured, the launcher exported an ingest base
// that may carry the credential, and the process denied inspection before it
// reads the next line. Without the echo core sends nothing, and the configured
// token source serves the session as before.
func (s *providerSession) acceptRunCredential() bool {
	if s.client == nil {
		return false
	}
	caller := newCapabilityCaller(os.Getenv(SessionReporterIngestURLEnv))
	if !caller.configured() {
		s.log(fmt.Sprintf("run credential: %s is not set to an https Delivery ingest base; using the configured cache token source", SessionReporterIngestURLEnv))
		return false
	}
	if err := cacheProviderDenyInspection(); err != nil {
		s.log(fmt.Sprintf("run credential: cannot deny process inspection (%v); using the configured cache token source", err))
		return false
	}
	s.runCredentialCaller = caller
	return true
}

// authenticate exchanges the hosted run's credential at Delivery for the run's
// cache token. A 201 makes that token the session's only cache bearer, never
// re-minted. A 204 keeps the configured token source, and so does every answer
// that says nothing about the run: a 404 (a Delivery without the route), an
// unavailable Delivery (5xx, 408, 429, a transport failure or a timeout), or a
// grant this provider cannot use. Only a refusal (any other 4xx, such as
// run_terminal or run_credential_invalid) refuses the op, which ends the
// session like a failed initialize: core builds without the remote cache. The
// credential is used for that one request and dropped.
func (s *providerSession) authenticate(req *cache.ProviderRequest, p *cache.AuthenticateParams) *cache.ProviderResponse {
	if s.runCredentialCaller == nil {
		return providerErr(req, "unexpected_op", "authenticate: this provider did not echo run-credential")
	}
	if s.authenticated {
		return providerErr(req, "unexpected_op", "authenticate: the run credential was already exchanged")
	}
	s.authenticated = true
	answer := s.runCredentialCaller.call(s.ctx, cacheCapabilityPath, p.Credential)
	switch {
	case answer.outcome == capabilityGranted:
		token, err := runCacheToken(answer.body)
		if err != nil {
			return s.keepConfiguredCacheToken(req, p.Credential, fmt.Sprintf("Delivery answered HTTP %d with a cache token this provider cannot use (%v)", answer.status, err))
		}
		s.client.UseRunBearer(token)
		s.log("run credential: the remote cache uses the run's cache token")
		return providerOK(req, &cache.AuthenticateResult{})
	case answer.outcome == capabilityAbsent:
		s.log("run credential: the run has no cache token; using the configured cache token source")
		return providerOK(req, &cache.AuthenticateResult{})
	case answer.outcome == capabilityRefused && answer.status == http.StatusNotFound:
		return s.keepConfiguredCacheToken(req, p.Credential, "Delivery has no cache capability route (HTTP 404)")
	case answer.outcome == capabilityRefused:
		code, message := capabilityRefusal(answer.body)
		if !registryRefusalCode.MatchString(code) {
			code = "cache_refused"
		}
		message = boundedDiagnosticText(message, cacheRefusalMessageBytes, p.Credential)
		if message == "" {
			message = fmt.Sprintf("Delivery refused the cache capability (HTTP %d)", answer.status)
		}
		s.logRunCredential(p.Credential, fmt.Sprintf("Delivery refused the cache capability (HTTP %d, %s)", answer.status, code))
		return providerErr(req, code, message)
	default:
		message := "Delivery did not answer the cache capability"
		if answer.status != 0 {
			message = fmt.Sprintf("Delivery did not answer the cache capability (last answer HTTP %d)", answer.status)
		}
		return s.keepConfiguredCacheToken(req, p.Credential, message)
	}
}

// keepConfiguredCacheToken answers authenticate ok without a run cache token:
// the session keeps its configured token source, as it did before the run
// credential existed. It logs why on one line.
func (s *providerSession) keepConfiguredCacheToken(req *cache.ProviderRequest, credential, reason string) *cache.ProviderResponse {
	s.logRunCredential(credential, reason+"; using the configured cache token source")
	return providerOK(req, &cache.AuthenticateResult{})
}

// logRunCredential writes one run-credential diagnostic on one stderr line of
// at most cacheRefusalMessageBytes, with the run credential and every
// token-like value replaced.
func (s *providerSession) logRunCredential(credential, text string) {
	line := tokenLike.ReplaceAllString(text, "<redacted>")
	s.log("run credential: " + boundedDiagnosticText(line, cacheRefusalMessageBytes, credential))
}

// cacheRefusalMessageBytes bounds the Delivery text an authenticate refusal
// carries, the same bound the credential-provider wire applies.
const cacheRefusalMessageBytes = 512

// runCacheToken reads Delivery's granted cache capability
// `{protocolVersion, token, expiresAt}` leniently and returns the token when it
// can travel in an Authorization header.
func runCacheToken(body []byte) (string, error) {
	var granted struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Token           string `json:"token"`
	}
	if err := json.Unmarshal(body, &granted); err != nil {
		return "", errors.New("the body is not a JSON object")
	}
	if granted.ProtocolVersion != capabilityProtocolVersion {
		return "", fmt.Errorf("protocolVersion %d is not %d", granted.ProtocolVersion, capabilityProtocolVersion)
	}
	if !registry.ValidCredentialBearer(granted.Token) {
		return "", errors.New("the token is empty, too long, or not visible ASCII")
	}
	return granted.Token, nil
}

// applyLinkedCacheDefault synthesizes the managed remote-cache default when no
// .putnami/cache.json is present but this workspace is linked to a Cloud
// workspace via the committed manifest (putnami.workspace.json).
//
// A fresh git worktree inherits that committed link and the global
// ~/.putnami/auth.json credential, but not the git-ignored cache.json pointer —
// and the install hook that writes it can silently no-op on a cold checkout:
// the @putnami/cloud extension binary is materialized lazily, so the very first
// `putnami install` can run `workspace-install` before the binary exists, its
// cache write never happens, and remote caching stays dark until a later
// install re-writes the file. Rather than depend on that fragile write, resolve
// the same managed endpoint + token source the writer would have persisted, so
// a linked worktree caches from its first build. This mirrors how
// clicore.ReadCloudLink treats the committed manifest link as primary and
// .putnami/cloud-link.json as a regenerable cache.
//
// It only fills a genuinely empty URL, so an explicit config (including a
// `cloud cache disable` that sets enabled:false with the URL kept) and the
// PUTNAMI_CACHE_URL env override both win untouched; an unlinked checkout stays
// local-only.
func (s *providerSession) applyLinkedCacheDefault(cfg *remotecache.Config) {
	if cfg.URL != "" || !workspaceLinked(s.workspaceRoot) {
		return
	}
	cfg.URL = DefaultCacheURL
	if !cfg.Token.Configured() {
		cfg.Token = remotecache.TokenSource{Command: cacheTokenCommand()}
	}
}

// workspaceLinked reports whether this workspace is bound to a Cloud workspace,
// resolving the committed manifest link (primary) then the local
// cloud-link.json cache. It uses the same resolver `cloud token --for cache`
// relies on, so a synthesized cache config is only ever handed a token source
// that can actually mint a bearer.
func workspaceLinked(workspaceRoot string) bool {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	return err == nil && clicore.StringValue(link["workspace_id"]) != ""
}

// serving reports whether the provider can move blobs: it needs a configured
// client and the blob-exchange directory core hands them through.
func (s *providerSession) serving() bool {
	return s.client != nil && s.exchangeDir != ""
}

// restore materializes one key's blobs into the blob-exchange directory and
// returns the cached result on a hit. A miss or an error both tell core to build
// locally; the error status is the restore-failure signal (logged, not a cold
// miss). Eligibility/break-even filtering is core's call at this point — the
// provider restores exactly the key core asked for, so it builds the negotiate
// request directly rather than through the eligibility-filtering BuildRequest.
func (s *providerSession) restore(req *cache.ProviderRequest, p *cache.RestoreParams) *cache.ProviderResponse {
	if !s.serving() {
		return providerOK(req, &cache.RestoreResult{Status: cache.RestoreMiss})
	}
	cacheResult, negotiated := s.negotiatedResult(p.Key)
	if !negotiated {
		// The key was not covered by a prefetch batch (or the batch failed):
		// negotiate it individually. With core handing the whole build's key
		// set to prefetch, this is the exception path, not the per-restore tax.
		nreq := &cache.NegotiateRequest{
			ProtocolVersion: cache.ProtocolVersion,
			Mode:            s.mode,
			Keys:            []cache.KeyRequest{{Key: p.Key}},
		}
		cache.NormalizeRequest(nreq)
		results, err := s.negotiate(nreq)
		if err != nil {
			s.logCacheError(fmt.Sprintf("restore %s negotiate", short(p.Key)), err)
			return providerOK(req, &cache.RestoreResult{Status: cache.RestoreError})
		}
		cacheResult = results[p.Key]
	}
	if !cacheResult.result.Hit {
		return providerOK(req, &cache.RestoreResult{Status: cache.RestoreMiss})
	}
	n, err := s.materializeHit(s.ctx, cacheResult.result)
	if err != nil {
		s.log(fmt.Sprintf("restore %s: materialize: %v", short(p.Key), err))
		return providerOK(req, &cache.RestoreResult{Status: cache.RestoreError})
	}
	s.addStats(func(st *cache.SummaryResult) {
		st.RestoredCount++
		st.RestoredBytes += n
	})
	result := &cache.RestoreResult{
		Status:   cache.RestoreHit,
		Result:   cacheResult.result.Result,
		Manifest: cacheResult.result.Manifest,
	}
	if cache.ProviderProtocolHasProvenance(req.ProtocolVersion) && s.provenanceEnabled() {
		result.Producer = cacheResult.provenance.Producer
		result.ProducerIdentity = cacheResult.provenance.ProducerIdentity
		result.Channel = cacheResult.provenance.Channel
	}
	return providerOK(req, result)
}

// prefetch negotiates a set of keys in one batch and speculatively materializes
// the hits' blobs into the exchange directory in the background, warming them
// for later restores. It returns promptly with the count it accepted; the work
// is drained at summary. The batch's negotiate outcome is kept as the session's
// negotiate index (installed synchronously here, before the response, so a
// restore dispatched right after this op always finds the in-flight batch to
// wait on). Only the first prefetch installs the index — core sends exactly
// one; a hypothetical second batch still warms blobs but leaves the index and
// its already-released waiters untouched.
func (s *providerSession) prefetch(req *cache.ProviderRequest, p *cache.PrefetchParams) *cache.ProviderResponse {
	if !s.serving() || len(p.Keys) == 0 {
		return providerOK(req, &cache.PrefetchResult{Started: 0})
	}
	keys := p.Keys // owned by ParseAndValidatePrefetchParams; safe to hand to the goroutine
	var done chan struct{}
	s.mu.Lock()
	if s.prefetchDone == nil {
		done = make(chan struct{})
		s.prefetchDone = done
	}
	s.mu.Unlock()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.runPrefetch(keys, done)
	}()
	return providerOK(req, &cache.PrefetchResult{Started: len(keys)})
}

// runPrefetch performs the batched negotiate, publishes its outcome as the
// negotiate index (releasing any restores waiting on done), then downloads the
// hits' blobs on a bounded pool. done is nil when a prior prefetch already owns
// the index.
func (s *providerSession) runPrefetch(keys []string, done chan struct{}) {
	nreq := &cache.NegotiateRequest{ProtocolVersion: cache.ProtocolVersion, Mode: s.mode}
	for _, k := range keys {
		nreq.Keys = append(nreq.Keys, cache.KeyRequest{Key: k})
	}
	cache.NormalizeRequest(nreq)
	results, err := s.negotiate(nreq)
	if err != nil {
		s.logCacheError("prefetch negotiate", err)
		if done != nil {
			close(done) // index stays nil: restores fall back to per-key negotiate
		}
		return
	}
	if done != nil {
		// Record an explicit outcome for every batched key — misses too — so a
		// restore for a batched miss is answered from the index instead of
		// re-asking the server for a key this run already knows is absent.
		full := make(map[string]providerCacheResult, len(keys))
		for _, k := range keys {
			full[k] = results[k] // zero result (Hit=false) for misses
		}
		s.mu.Lock()
		s.negotiateIdx = full
		s.mu.Unlock()
		close(done)
	}

	// Warm the hits' blobs concurrently: they are independent transfers, and a
	// restore for a not-yet-warmed key downloads its own blobs anyway (atomic
	// staging makes the duplicate harmless), so the only cost of sequential
	// warming was that it never kept up with the build.
	sem := make(chan struct{}, prefetchConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var warmed int
	for _, k := range keys {
		result, ok := results[k]
		if !ok || !result.result.Hit {
			continue
		}
		wg.Add(1)
		go func(k string, result providerCacheResult) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := s.materializeHit(s.ctx, result.result); err != nil {
				s.log(fmt.Sprintf("prefetch %s: %v", short(k), err))
				return
			}
			mu.Lock()
			warmed++
			mu.Unlock()
		}(k, result)
	}
	wg.Wait()
	s.addStats(func(st *cache.SummaryResult) { st.PrefetchedCount += warmed })
}

// negotiatedResult returns the batched negotiate outcome for key, waiting for
// an in-flight prefetch batch first so a restore racing it never duplicates the
// negotiate. ok=false when no batch covered the key (no prefetch ran, the batch
// failed, or the key was outside it) — the caller then negotiates individually.
func (s *providerSession) negotiatedResult(key string) (providerCacheResult, bool) {
	s.mu.Lock()
	done := s.prefetchDone
	s.mu.Unlock()
	if done == nil {
		return providerCacheResult{}, false
	}
	select {
	case <-done:
	case <-s.ctx.Done():
		return providerCacheResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.negotiateIdx[key]
	return result, ok
}

// negotiate preserves the shared HTTP cache response for v1 sessions and asks
// for Cloud provenance only after the provider-RPC v2 handshake selected it.
func (s *providerSession) negotiate(req *cache.NegotiateRequest) (map[string]providerCacheResult, error) {
	if s.provenanceEnabled() {
		response, err := s.client.NegotiateWithProvenance(s.ctx, req)
		if err != nil {
			return nil, err
		}
		indexed := remotecache.Index(response.Response)
		out := make(map[string]providerCacheResult, len(indexed))
		for key, result := range indexed {
			out[key] = providerCacheResult{result: result, provenance: response.Provenance[key]}
		}
		return out, nil
	}
	response, err := s.client.Negotiate(s.ctx, req)
	if err != nil {
		return nil, err
	}
	indexed := remotecache.Index(response)
	out := make(map[string]providerCacheResult, len(indexed))
	for key, result := range indexed {
		out[key] = providerCacheResult{result: result}
	}
	return out, nil
}

func (s *providerSession) provenanceEnabled() bool {
	return cache.ProviderProtocolHasProvenance(s.providerProtocolVersion)
}

// upload stores a freshly built entry in the background: its blobs are already in
// the exchange directory (core exported them), so a BlobSource reads them from
// there. It returns promptly; the bytes are confirmed durable at summary.
func (s *providerSession) upload(req *cache.ProviderRequest, p *cache.UploadParams) *cache.ProviderResponse {
	if !s.serving() || s.readOnly {
		return providerOK(req, &cache.UploadResult{Accepted: false})
	}
	// UploadParams provenance is deliberately ignored. The cache server derives
	// and persists producer/channel from the bearer it verifies at commit time,
	// so a caller cannot turn a developer upload into a trusted CI entry by
	// forging these optional v2 fields.
	in := remotecache.StoreInput{Key: p.Key, Result: p.Result, Manifest: p.Manifest}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		out, err := s.client.StoreResult(s.ctx, in, blobExchangeSource{dir: s.exchangeDir})
		if err != nil {
			s.logCacheError("upload "+short(p.Key), err)
			return
		}
		s.addStats(func(st *cache.SummaryResult) {
			st.UploadedCount++
			st.UploadedBytes += out.BytesUploaded
		})
	}()
	return providerOK(req, &cache.UploadResult{Accepted: true})
}

func (s *providerSession) markerLookup(req *cache.ProviderRequest, p *cache.MarkerLookupParams) *cache.ProviderResponse {
	if !s.serving() {
		return providerOK(req, &cache.MarkerLookupResult{Found: false})
	}
	mreq := s.client.BuildRunMarkerRequest(p.Workspace, p.Branch, p.Commands, p.ParamsHash)
	resp, err := s.client.LookupRunMarker(s.ctx, mreq)
	if err != nil {
		s.logCacheError("marker-lookup", err)
		return providerOK(req, &cache.MarkerLookupResult{Found: false})
	}
	if resp == nil || resp.Marker == nil {
		return providerOK(req, &cache.MarkerLookupResult{Found: false})
	}
	return providerOK(req, &cache.MarkerLookupResult{Found: true, Marker: resp.Marker})
}

func (s *providerSession) markerWrite(req *cache.ProviderRequest, p *cache.MarkerWriteParams) *cache.ProviderResponse {
	if !s.serving() || s.readOnly {
		return providerOK(req, &cache.MarkerWriteResult{Published: false})
	}
	mreq := s.client.BuildPublishRunMarkerRequest(p.Workspace, p.Branch, p.Commands, p.ParamsHash, p.SHA, p.ObservedSHA)
	resp, err := s.client.PublishRunMarker(s.ctx, mreq)
	if err != nil {
		s.logCacheError("marker-write", err)
		return providerOK(req, &cache.MarkerWriteResult{Published: false})
	}
	if resp == nil {
		return providerOK(req, &cache.MarkerWriteResult{Published: false})
	}
	return providerOK(req, &cache.MarkerWriteResult{Published: resp.Published, Marker: resp.Marker})
}

// summary drains the background prefetch + upload work and returns the run's
// accumulated statistics, reporting any cache-auth refusals alongside them.
//
// Object puts are drained here too — the contract acknowledges a put as QUEUED
// and promises durability at summary — but they are reported on their own line
// rather than folded into SummaryResult: those counters have always meant TASK
// entries, and quietly adding compiler objects to them would change what every
// existing reading of uploadedCount means.
func (s *providerSession) summary(req *cache.ProviderRequest) *cache.ProviderResponse {
	s.objects.drain()
	s.objects.report()
	s.bg.Wait()
	s.mu.Lock()
	stats := s.stats
	refusals, writeRefusals, first := s.authRefusals, s.authWriteRefusals, s.firstAuthRefusal
	s.mu.Unlock()
	s.reportAuthRefusals(stats, refusals, writeRefusals, first)
	return providerOK(req, &stats)
}

// reportAuthRefusals states the run's TASK-cache auth verdict. Remote caching
// stays best-effort — this never fails a build — but a run whose writes were all
// refused persists NOTHING, so the next run starts cold and the loop repeats.
// Reported as uploadedCount=0 alone that is indistinguishable
// from a fully warm run, so the refusal is said out loud instead.
//
// Object-cache refusals are not counted here and never reach this warning: they
// speak on the object cache's own line (see cacheWriteOps and objectCache.report).
func (s *providerSession) reportAuthRefusals(stats cache.SummaryResult, refusals, writeRefusals int, first string) {
	if refusals == 0 {
		return
	}
	s.log(fmt.Sprintf("%d cache request(s) refused by the server this run, %d of them on the write path; first: %s",
		refusals, writeRefusals, first))
	if writeRefusals > 0 && stats.UploadedCount == 0 {
		s.log("WARNING: every remote-cache write was refused — this run persisted NO cache entries, so the next run starts cold")
	}
}

// materializeHit downloads every blob of a negotiate hit into the blob-exchange
// directory, skipping digests already present (warmed by a prefetch or a prior
// restore), and returns the bytes downloaded. Each blob is content-verified by
// the fetcher and written atomically (temp + rename) so core never ingests a
// half-written file.
//
// It materializes the blobs the server returned for the negotiated mode; the
// guarantee that every Manifest digest is present (full vs minimal/toplevel
// materialization) is refined with the mode ownership decision.
func (s *providerSession) materializeHit(ctx context.Context, result cache.KeyResult) (int64, error) {
	fetch := s.client.BatchHitFetcher(ctx, result)
	var total int64
	seen := make(map[string]bool, len(result.Downloads))
	for _, t := range result.Downloads {
		if seen[t.Digest] {
			continue
		}
		seen[t.Digest] = true
		if s.known[t.Digest] {
			continue // core already has this blob in its CAS; it ingests from there, not the exchange dir
		}
		path, ok := cache.BlobExchangePath(s.exchangeDir, t.Digest)
		if !ok {
			return total, fmt.Errorf("invalid digest %s", t.Digest)
		}
		if blobPresent(path, t.SizeBytes) {
			continue
		}
		n, err := writeExchangeBlob(fetch, t.Digest, path)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// writeExchangeBlob opens a verified blob from the hit fetcher and writes it to
// the exchange directory atomically: a temp file in the target directory is
// renamed into place so a concurrent reader (core's ingest, or another op) only
// ever sees the complete blob.
func writeExchangeBlob(fetch func(string) (io.ReadCloser, error), digest, path string) (int64, error) {
	rc, err := fetch(digest)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	n, err := io.Copy(tmp, rc)
	if err != nil {
		_ = tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return n, err
	}
	return n, nil
}

// blobPresent reports whether the exchange directory already holds the blob at
// the expected size (digest-addressed content the fetcher verified when it was
// written), so a prefetch-then-restore does not download it twice.
func blobPresent(path string, size int64) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return size <= 0 || fi.Size() == size
}

// blobExchangeSource reads upload blobs from the blob-exchange directory core
// exported them into. It implements remotecache.BlobSource.
type blobExchangeSource struct{ dir string }

func (s blobExchangeSource) OpenBlob(digest string) (io.ReadCloser, error) {
	path, ok := cache.BlobExchangePath(s.dir, digest)
	if !ok {
		return nil, fmt.Errorf("invalid digest %s", digest)
	}
	return os.Open(path)
}

// short truncates a cache key/digest for a log line.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// providerVersionValue is the @putnami/cloud version reported in
// InitializeResult. The protocol requires a non-empty version, so it carries a
// development placeholder that the channel bump overrides with the
// real published version via -ldflags "-X …providerVersionValue=<v>" or the
// PUTNAMI_CLOUD_VERSION env. The authoritative gate is core comparing the loaded
// extension's declared version at load time; this runtime field confirms it.
var providerVersionValue = "0.0.0-dev"

// providerVersion reports this extension's version for InitializeResult — the
// runtime half of core's capability/version gate.
func providerVersion() string {
	if v := strings.TrimSpace(os.Getenv("PUTNAMI_CLOUD_VERSION")); v != "" {
		return v
	}
	return providerVersionValue
}

// providerOK builds an OK response carrying result as its payload.
func providerOK(req *cache.ProviderRequest, result any) *cache.ProviderResponse {
	payload, err := cache.MarshalPayload(result)
	if err != nil {
		return providerErr(req, "encode_error", fmt.Sprintf("encode %s result: %v", req.Op, err))
	}
	return &cache.ProviderResponse{
		ProtocolVersion: req.ProtocolVersion,
		ID:              req.ID,
		OK:              true,
		Payload:         payload,
	}
}

// providerErr builds a non-OK response. Core falls back to a local build.
func providerErr(req *cache.ProviderRequest, code, message string) *cache.ProviderResponse {
	return &cache.ProviderResponse{
		ProtocolVersion: req.ProtocolVersion,
		ID:              req.ID,
		OK:              false,
		Error:           &cache.ProviderError{Code: code, Message: message},
	}
}

// writeProviderResponse writes one framed response line.
func writeProviderResponse(w *bufio.Writer, resp *cache.ProviderResponse) error {
	line, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal provider response: %w", err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}
