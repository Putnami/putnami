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
	"time"

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
const restoreConcurrency = 16

// providerSession holds the per-run state established by initialize and used by
// the data-plane ops: the configured remote-cache client and the blob-exchange
// directory core hands blobs through. Control-plane requests are served from
// the read loop one at a time; prefetch and upload return promptly and do their
// work on background goroutines, and restores run concurrently on a bounded
// pool (all tracked by bg, which summary and shutdown drain within a bound, see
// cachedrain.go) — core's session matches responses by ID, so answering out of
// order is part of the protocol.
type providerSession struct {
	workspaceRoot string
	logw          io.Writer
	// managedCacheURL is the cache URL a linked workspace with no
	// .putnami/cache.json reads (see linkedCacheDefault): DefaultCacheURL,
	// which a test replaces with its own server.
	managedCacheURL string

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
	// uploads queues the entries upload accepted until the uploader stores them
	// a window at a time (see cacheupload.go).
	uploads uploadWindow
	// capsProbe holds the session's capabilities probe, shared by the object
	// cache at initialize and the uploader's choice of write path.
	capsProbe capabilitiesProbe

	ctx    context.Context
	cancel context.CancelFunc

	bg         background    // background prefetch + upload + restore work
	restoreSem chan struct{} // bounds concurrent restore transfers
	// limits are the run-end drain bounds, read once here (see cachedrain.go).
	limits drainLimits
	// now is the session's clock and transferMaxAge the age past which a
	// restore negotiates a key again before it downloads (see
	// restoreTransferMaxAge). A test replaces them.
	now            func() time.Time
	transferMaxAge time.Duration

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
	// writesRefused latches the TASK-cache write path shut once the server
	// refused a write with 403: a scope verdict that cannot change inside one
	// run, so every later upload and marker write is answered locally, as on a
	// read-only session, and nothing more is sent. developerRefusal records that
	// the refusal is the expected one for a developer credential (see
	// expectedDeveloperRefusal), which the summary states plainly instead of
	// warning. hosted is set at initialize when a hosted launcher started the
	// run (see hostedLaunch).
	writesRefused    bool
	developerRefusal bool
	hosted           bool
	// tokenCommandFailureLogged collapses the memoized token-command error into
	// one actionable session warning. Without this guard every prefetch,
	// restore, upload, and marker operation repeats the same opaque failure.
	tokenCommandFailureLogged bool
	// negotiateIdx caches the prefetch's batched negotiate outcome for every
	// prefetched key (misses included) so each restore is served from it
	// instead of issuing its own single-key negotiate round trip — before this
	// index a fully warm rebuild spent its entire wall time on serialized
	// per-restore negotiates. prefetchDone is closed once the index is
	// populated (a negotiate that failed leaves its keys out of it), so a
	// restore racing the batch waits for it rather than duplicating it. The
	// batch is the run's consistency window — the same single build-wide
	// negotiate semantics the in-core client had before the provider split.
	// Each outcome records the mode it was negotiated in: a result-only key is
	// negotiated in minimal mode and carries no download (see runPrefetch).
	negotiateIdx map[string]providerCacheResult
	prefetchDone chan struct{}
	// inline records the download-batch requests the prefetch and the late
	// negotiates send for the small files of their hits (see cacheinline.go).
	// The prefetch records every request before prefetchDone is closed.
	inline inlineRequests
	// late gathers the restores that negotiate their own key into shared
	// negotiates (see cachenegotiate.go).
	late lateNegotiates

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

// providerCacheResult is one key's negotiate outcome. mode is the mode the
// server answered in: a minimal answer carries the result and the manifest but
// no download. at is when the negotiate was sent, which is before the server
// signed the downloads: a download is good for at least its lifetime counted
// from at.
type providerCacheResult struct {
	result     cache.KeyResult
	provenance remotecache.EntryProvenance
	mode       cache.Mode
	at         time.Time
}

// restoreTransferMaxAge is how old a negotiated download may be when a restore
// starts to use it. A download does not last: the cache server signs a URL for
// 15 minutes, or hands out a storage token that lasts 20 to 60 minutes. The
// prefetch negotiates every key when the run starts, and a restore can come
// much later. Past 5 minutes a restore that needs a download negotiates its key
// again first, one small request, which leaves at least 10 minutes for its
// downloads to start. It is a variable so a test can change it; a session reads
// it once, when it is created.
var restoreTransferMaxAge = 5 * time.Minute

// lacksDownloads reports whether the outcome cannot serve a restore of the
// files in a session of the given mode: it is a hit negotiated in minimal mode,
// which names no download, while the session's mode names them.
func (r providerCacheResult) lacksDownloads(session cache.Mode) bool {
	return r.result.Hit && r.mode == cache.ModeMinimal && session != cache.ModeMinimal
}

func newProviderSession(workspaceRoot string, logw io.Writer) *providerSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &providerSession{
		workspaceRoot:   workspaceRoot,
		logw:            logw,
		managedCacheURL: DefaultCacheURL,
		ctx:             ctx,
		cancel:          cancel,
		known:           make(map[string]bool),
		restoreSem:      make(chan struct{}, restoreConcurrency),
		limits:          readDrainLimits(),
		now:             time.Now,
	}
	s.transferMaxAge = restoreTransferMaxAge
	s.uploads.init()
	s.late.init()
	return s
}

// capabilitiesProbeTimeout bounds the capabilities probe. The client's own
// control-plane budget is 30s, which is the right bound for a negotiate but not
// for a discovery probe that sits in front of the first op of a run (the object
// cache probes at initialize) or of the first upload window: an unreachable or
// wedged cache server must cost the build a few seconds, not half a minute.
const capabilitiesProbeTimeout = 5 * time.Second

// capabilitiesProbe memoizes the cache server's capabilities for the session,
// so the object cache and the uploader share one probe.
type capabilitiesProbe struct {
	mu   sync.Mutex
	done bool
	caps *cache.CapabilitiesResponse
	err  error
}

// serverCapabilities returns the session's capabilities probe, sending it on
// the first call. The client degrades a refused probe (any non-2xx) to an empty
// capability set, the baseline write path, so only a transport fault or an
// invalid answer is an error.
func (s *providerSession) serverCapabilities() (*cache.CapabilitiesResponse, error) {
	p := &s.capsProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.done {
		ctx, cancel := context.WithTimeout(s.ctx, capabilitiesProbeTimeout)
		p.caps, p.err = s.client.Capabilities(ctx)
		cancel()
		p.done = true
	}
	return p.caps, p.err
}

// forgetServerCapabilities drops the memoized probe when the session's bearer
// changes: initialize probed with the configured token, which a hosted run
// replaces with its own cache token at authenticate, and a probe refused to
// that token source says nothing about what the run's token may do.
func (s *providerSession) forgetServerCapabilities() {
	p := &s.capsProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done, p.caps, p.err = false, nil, nil
}

// close cancels the session context (aborting any in-flight background transfer)
// and waits for the background goroutines to exit, so the process never returns
// while a goroutine still writes to the exchange directory. It is the hard
// stop: what the run-end drain did not store is dropped here.
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
	// A 403 on a write is a verdict on the credential's scope; the client does
	// not even re-mint on it. Latch so the run stops sending writes the server
	// will refuse again. A 401 does not latch: it can be a transient auth
	// failure, and a renewable bearer is re-minted once on it.
	latched := cacheWriteOps[f.Op] && f.Status == http.StatusForbidden && !s.writesRefused
	if latched {
		s.writesRefused = true
		s.developerRefusal = s.expectedDeveloperRefusal(f)
	}
	first := s.firstAuthRefusal == ""
	if first {
		s.firstAuthRefusal = f.String()
	}
	s.mu.Unlock()
	if first {
		s.log(f.String())
	}
	if latched {
		s.log(fmt.Sprintf("the cache server refused a %s with HTTP 403; this run sends no further cache writes, reads continue", f.Op))
	}
}

// expectedDeveloperRefusal reports whether a refused write is the expected
// refusal of a developer credential: the cache server lets only hosted CI
// write, so a laptop's credential reads the shared cache and writes nothing.
// Any sign of a hosted run makes it unexpected, so a hosted run whose writes
// are refused keeps the summary warning: a launcher-exported ingest
// base, the run's own cache token, or a service-account identity, which only a
// cloud machine has. Callers hold s.mu.
func (s *providerSession) expectedDeveloperRefusal(f remotecache.AuthFailure) bool {
	if s.hosted {
		return false
	}
	switch f.Source {
	case remotecache.TokenClassRun, remotecache.TokenClassMetadata:
		return false
	default:
		return true
	}
}

// hostedLaunch reports whether a hosted CI launcher started this run. The
// launcher exports the Delivery ingest base the run reports to and exchanges
// its run credential at (SessionReporterIngestURLEnv); a developer machine
// never sets it.
func hostedLaunch() bool {
	return hostedLaunchIn(nil)
}

// hostedLaunchIn is hostedLaunch read from env. A nil env is the process
// environment.
func hostedLaunchIn(env map[string]string) bool {
	return strings.TrimSpace(clicore.EnvGet(env, SessionReporterIngestURLEnv)) != ""
}

// writesLatched reports whether a refused write closed the task-cache write
// path for the rest of the run.
func (s *providerSession) writesLatched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writesRefused
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
//
// Every way out stores the queued uploads first, within a bound. The shutdown
// drains before its acknowledgement. When stdin closes, or the response pipe
// breaks, without a shutdown, the entries already accepted are still stored
// before the process exits: core was told they were accepted. Only a signal
// that kills the process drops them.
func runCacheProviderSession(in io.Reader, out io.Writer, logw io.Writer, workspaceRoot string) error {
	sess := newProviderSession(workspaceRoot, logw)
	shutdown := false
	defer func() {
		if !shutdown {
			sess.drainUploader(time.Now().Add(sess.limits.budget), "they are dropped")
		}
		sess.close()
	}()

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
			// Let in-flight restores answer and queued uploads store before
			// acking the shutdown; core stops reading once the ack lands. Core
			// waits 5 s for the ack, so the wait is bounded under it. The
			// summary already drained, so this wait is over at once unless
			// core sent no summary.
			shutdown = true
			sess.drainBackground(time.Now().Add(sess.limits.shutdown), "they are dropped")
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

	built := buildCacheConfig(s.workspaceRoot, s.managedCacheURL)
	if built.loadErr != nil {
		s.log(fmt.Sprintf("initialize: load cache config: %v", built.loadErr))
	}
	if built.envErr != nil {
		s.log(fmt.Sprintf("initialize: apply cache environment: %v", built.envErr))
	}
	cfg := built.cfg
	s.readOnly = cfg.ReadOnly
	s.hosted = hostedLaunch()
	s.applyMachineToken(cfg)

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
			// PUTNAMI_CACHE_TOKEN and a machine token from PUTNAMI_CLOUD_TOKEN are
			// never re-minted; the command and metadata recipes are.
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
	// Core lists restore-result-only when it can serve a hit from its result
	// alone. After the echo it names the keys whose files it will not read:
	// prefetch negotiates them without downloads and restore places no blob for
	// them. Without core's offer nothing is echoed, and core sends neither field.
	if slices.Contains(p.Capabilities, cache.CapabilityRestoreResultOnly) {
		result.Capabilities = append(result.Capabilities, cache.CapabilityRestoreResultOnly)
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
		s.forgetServerCapabilities()
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

// cacheURLOrigin names the step that set the URL of a built cache config.
type cacheURLOrigin int

const (
	cacheURLUnset cacheURLOrigin = iota
	cacheURLFromFile
	cacheURLFromEnv
	cacheURLFromLink
)

// builtCacheConfig is the remote-cache config of a build and what the steps
// that built it found.
type builtCacheConfig struct {
	cfg *remotecache.Config
	// fileExists reports whether .putnami/cache.json exists.
	fileExists bool
	origin     cacheURLOrigin
	// loadErr is a .putnami/cache.json that cannot be read: the build goes on
	// with an empty config. envErr is a malformed PUTNAMI_CACHE_* override: it
	// turns the remote cache off for the build.
	loadErr error
	envErr  error
}

// buildCacheConfig builds the remote-cache config of a build in workspaceRoot:
// .putnami/cache.json, then the PUTNAMI_CACHE_* overrides from the process
// environment (the CI path), then the workspace link default with managedURL.
// providerSession.initialize and the cache status both call it, so the status
// reports the config a build uses.
func buildCacheConfig(workspaceRoot, managedURL string) builtCacheConfig {
	path := CachePath(workspaceRoot)
	_, statErr := os.Stat(path)
	b := builtCacheConfig{fileExists: statErr == nil}
	cfg, err := remotecache.LoadConfig(path)
	if err != nil {
		b.loadErr = err
		cfg = &remotecache.Config{}
	}
	fileURL := cfg.URL
	if err := cfg.ApplyEnv(); err != nil {
		b.envErr = err
		disabled := false
		cfg = &remotecache.Config{Enabled: &disabled}
	}
	envURL := cfg.URL
	linkedCacheDefault(cfg, workspaceRoot, managedURL)
	switch {
	case cfg.URL == "":
		b.origin = cacheURLUnset
	case envURL == "":
		b.origin = cacheURLFromLink
	case envURL != fileURL:
		b.origin = cacheURLFromEnv
	default:
		b.origin = cacheURLFromFile
	}
	b.cfg = cfg
	return b
}

// linkedCacheDefault synthesizes the managed remote-cache default, managedURL
// with the canonical token recipe, when no .putnami/cache.json is present but
// this workspace is linked to a Cloud workspace via the committed manifest
// (putnami.workspace.json).
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
func linkedCacheDefault(cfg *remotecache.Config, workspaceRoot, managedURL string) {
	if cfg.URL != "" || !workspaceLinked(workspaceRoot) {
		return
	}
	cfg.URL = managedURL
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
//
// A result-only restore (p.ResultOnly) places no blob: core reads the entry's
// result and none of its files. It is answered from the prefetch index, or from
// one minimal negotiate of the key. Core cannot verify such a hit, so the answer
// is the entry the server reported under exactly this key, and a miss otherwise.
// A restore of the files needs the downloads, which a minimal answer lacks: it
// then negotiates the key once more, in the session's mode.
//
// The downloads of the index do not last (see restoreTransferMaxAge), so a
// restore of the files negotiates its key again, once, in two cases: before it
// downloads, when the indexed downloads are too old and it needs one of them;
// and after a download the storage refused the way it refuses an expired one.
// It reports an error only when the download fails with downloads this restore
// negotiated itself. A key the server no longer holds is a miss.
//
// A restore that negotiates its key shares the request with the restores that
// arrive with it (see cachenegotiate.go).
func (s *providerSession) restore(req *cache.ProviderRequest, p *cache.RestoreParams) *cache.ProviderResponse {
	if !s.serving() {
		return providerOK(req, &cache.RestoreResult{Status: cache.RestoreMiss})
	}
	cacheResult, negotiated := s.negotiatedResult(p.Key)
	if negotiated && !p.ResultOnly && cacheResult.lacksDownloads(s.mode) {
		negotiated = false
	}
	// renegotiated is set once this restore asked the server for the key
	// itself: its downloads are then as fresh as they can be, and a refused
	// download is final.
	renegotiated := !negotiated
	if !negotiated {
		// The key was not covered by a prefetch batch (or the batch failed), or
		// the batch answered it for its result only: negotiate it, with the
		// other restores that need a negotiate now (see cachenegotiate.go).
		mode := s.mode
		if p.ResultOnly {
			mode = cache.ModeMinimal
		}
		result, err := s.negotiateKey(mode, p.Key)
		if err != nil {
			s.logCacheError(fmt.Sprintf("restore %s negotiate", short(p.Key)), err)
			return providerOK(req, &cache.RestoreResult{Status: cache.RestoreError})
		}
		cacheResult = result
	} else if !p.ResultOnly && s.transfersStale(cacheResult) {
		// Too old to rely on: ask for downloads signed now. When that negotiate
		// fails, the indexed downloads are still tried; they may have a longer
		// life than the bound assumes.
		renegotiated = true
		result, err := s.negotiateKey(s.mode, p.Key)
		if err != nil {
			s.logCacheError(fmt.Sprintf("restore %s negotiate again", short(p.Key)), err)
		} else {
			cacheResult = result
		}
	}
	if !cacheResult.result.Hit {
		return providerOK(req, &cache.RestoreResult{Status: cache.RestoreMiss})
	}
	var n int64
	if !p.ResultOnly {
		var err error
		n, err = s.materializeHit(s.ctx, cacheResult.result)
		if err != nil && !renegotiated && remotecache.DownloadRefused(err) {
			// The storage refused a download of the index. Blobs already placed
			// stay placed; the second pass fetches the rest.
			again, nerr := s.negotiateKey(s.mode, p.Key)
			if nerr != nil {
				s.logCacheError(fmt.Sprintf("restore %s negotiate again", short(p.Key)), nerr)
				return providerOK(req, &cache.RestoreResult{Status: cache.RestoreError})
			}
			cacheResult = again
			if !cacheResult.result.Hit {
				return providerOK(req, &cache.RestoreResult{Status: cache.RestoreMiss})
			}
			var more int64
			more, err = s.materializeHit(s.ctx, cacheResult.result)
			n += more
		}
		if err != nil {
			s.log(fmt.Sprintf("restore %s: materialize: %v", short(p.Key), err))
			return providerOK(req, &cache.RestoreResult{Status: cache.RestoreError})
		}
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

// transfersStale reports whether a restore of r must negotiate again before it
// downloads: r is older than the session's bound, and the restore would use one
// of its downloads. A blob core already has, or one an earlier restore placed,
// is not downloaded. A blob of at most cache.MaxInlineBlobBytes comes inline
// through download-batch, which uses the session's own credential and no
// download of r; if the server leaves it out of that answer, the refused
// download is caught after the fact.
func (s *providerSession) transfersStale(r providerCacheResult) bool {
	if !r.result.Hit || r.at.IsZero() || s.now().Sub(r.at) < s.transferMaxAge {
		return false
	}
	for _, t := range r.result.Downloads {
		if s.known[t.Digest] {
			continue
		}
		if t.SizeBytes > 0 && t.SizeBytes <= cache.MaxInlineBlobBytes {
			continue
		}
		if path, ok := cache.BlobExchangePath(s.exchangeDir, t.Digest); ok && blobPresent(path, t.SizeBytes) {
			continue
		}
		return true
	}
	return false
}

// prefetch negotiates a set of keys ahead of their restores. Then it fetches
// the small files of the hits in a few download-batch requests (see
// cacheinline.go). A restore downloads the other files of its key, and only
// when core asks for them. It returns promptly with the count it accepted; the
// work is drained at summary. The outcome is kept as the session's negotiate
// index. The first prefetch installs prefetchDone synchronously here, before
// the response, so a restore dispatched right after this op always finds the
// in-flight batch to wait on. Core sends exactly one prefetch; a hypothetical
// second one adds its keys to the index and leaves the already-released
// waiters untouched.
func (s *providerSession) prefetch(req *cache.ProviderRequest, p *cache.PrefetchParams) *cache.ProviderResponse {
	if !s.serving() || len(p.Keys) == 0 {
		return providerOK(req, &cache.PrefetchResult{Started: 0})
	}
	// The keys are owned by ParseAndValidatePrefetchParams; safe to hand to the goroutine.
	batches := prefetchBatches(p.Keys, p.ResultOnlyKeys, s.mode)
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
		s.runPrefetch(batches, done)
	}()
	return providerOK(req, &cache.PrefetchResult{Started: len(p.Keys)})
}

// prefetchBatch is one negotiate of a prefetch: its mode, its keys, and the
// outcome runPrefetch fills in.
type prefetchBatch struct {
	mode    cache.Mode
	keys    []string
	results map[string]providerCacheResult
	err     error
}

// prefetchBatches splits a prefetch into its negotiates, two at most: the
// result-only keys in minimal mode, so the server checks and signs no download
// for files core will not read, and the other keys in the session's mode. A
// session already in minimal mode, or a prefetch without result-only keys, is
// one batch.
func prefetchBatches(keys, resultOnly []string, mode cache.Mode) []prefetchBatch {
	if mode == cache.ModeMinimal || len(resultOnly) == 0 {
		return []prefetchBatch{{mode: mode, keys: keys}}
	}
	only := make(map[string]bool, len(resultOnly))
	for _, k := range resultOnly {
		only[k] = true
	}
	var minimal, rest []string
	for _, k := range keys {
		if only[k] {
			minimal = append(minimal, k)
		} else {
			rest = append(rest, k)
		}
	}
	var batches []prefetchBatch
	if len(minimal) > 0 {
		batches = append(batches, prefetchBatch{mode: cache.ModeMinimal, keys: minimal})
	}
	if len(rest) > 0 {
		batches = append(batches, prefetchBatch{mode: mode, keys: rest})
	}
	return batches
}

// runPrefetch negotiates the batches side by side and publishes their outcomes
// as the negotiate index, releasing any restores waiting on done. A batch whose
// negotiate failed leaves its keys out of the index, so each of their restores
// negotiates its own key. done is nil when a prior prefetch already owns the
// waiters; the outcomes then join the index without replacing an earlier one.
//
// Before it publishes the outcomes it records the download-batch requests for
// the hits' small files, so a restore that reads the index always finds the
// request that carries its files and waits for it. It sends them after it
// releases the waiters.
//
// PrefetchedCount counts the prefetched keys the server holds an entry for. It
// does not count the files the prefetch fetched.
func (s *providerSession) runPrefetch(batches []prefetchBatch, done chan struct{}) {
	var wg sync.WaitGroup
	for i := range batches {
		wg.Add(1)
		go func(b *prefetchBatch) {
			defer wg.Done()
			b.results, b.err = s.negotiateKeys(b.mode, b.keys)
		}(&batches[i])
	}
	wg.Wait()

	inline := s.planInlinePrefetch(batches)

	found := 0
	s.mu.Lock()
	for _, b := range batches {
		if b.err != nil {
			continue
		}
		if s.negotiateIdx == nil {
			s.negotiateIdx = make(map[string]providerCacheResult, len(b.keys))
		}
		// Record an explicit outcome for every batched key — misses too — so a
		// restore for a batched miss is answered from the index instead of
		// re-asking the server for a key this run already knows is absent.
		for _, k := range b.keys {
			if _, held := s.negotiateIdx[k]; held {
				continue
			}
			result := b.results[k] // zero result (Hit=false) for misses
			s.negotiateIdx[k] = result
			if result.result.Hit {
				found++
			}
		}
	}
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
	for _, b := range batches {
		if b.err != nil {
			s.logCacheError("prefetch negotiate", b.err)
		}
	}
	s.addStats(func(st *cache.SummaryResult) { st.PrefetchedCount += found })
	s.fetchInline("prefetch", inline)
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

// negotiateKeys negotiates keys in the given mode. The outcomes are indexed by
// the key the server reported, so a key it did not answer reads as a miss.
func (s *providerSession) negotiateKeys(mode cache.Mode, keys []string) (map[string]providerCacheResult, error) {
	nreq := &cache.NegotiateRequest{ProtocolVersion: cache.ProtocolVersion, Mode: mode}
	for _, k := range keys {
		nreq.Keys = append(nreq.Keys, cache.KeyRequest{Key: k})
	}
	cache.NormalizeRequest(nreq)
	return s.negotiate(nreq)
}

// negotiate preserves the shared HTTP cache response for v1 sessions and asks
// for Cloud provenance only after the provider-RPC v2 handshake selected it.
// Each outcome carries the time the request was sent.
func (s *providerSession) negotiate(req *cache.NegotiateRequest) (map[string]providerCacheResult, error) {
	at := s.now()
	if s.provenanceEnabled() {
		response, err := s.client.NegotiateWithProvenance(s.ctx, req)
		if err != nil {
			return nil, err
		}
		indexed := remotecache.Index(response.Response)
		out := make(map[string]providerCacheResult, len(indexed))
		for key, result := range indexed {
			out[key] = providerCacheResult{result: result, provenance: response.Provenance[key], mode: req.Mode, at: at}
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
		out[key] = providerCacheResult{result: result, mode: req.Mode, at: at}
	}
	return out, nil
}

func (s *providerSession) provenanceEnabled() bool {
	return cache.ProviderProtocolHasProvenance(s.providerProtocolVersion)
}

// upload queues a freshly built entry for the uploader (see cacheupload.go):
// its blobs are already in the exchange directory (core exported them), so a
// BlobSource reads them from there. It returns promptly; the entries are stored
// a window at a time and confirmed durable at summary.
func (s *providerSession) upload(req *cache.ProviderRequest, p *cache.UploadParams) *cache.ProviderResponse {
	if !s.serving() || s.readOnly || s.writesLatched() {
		return providerOK(req, &cache.UploadResult{Accepted: false})
	}
	// UploadParams provenance is deliberately ignored. The cache server derives
	// and persists producer/channel from the bearer it verifies at commit time,
	// so a caller cannot turn a developer upload into a trusted CI entry by
	// forging these optional v2 fields.
	s.enqueueUpload(remotecache.StoreInput{Key: p.Key, Result: p.Result, Manifest: p.Manifest})
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
	if !s.serving() || s.readOnly || s.writesLatched() {
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
//
// The drain is bounded (see cachedrain.go): core discards a summary that takes
// more than 60 s, with every count in it. The uploader starts to flush before
// the object drain, so the two run side by side. Uploads the drain gives up on
// are not canceled here; they go on until the shutdown, and the counts leave
// them out.
func (s *providerSession) summary(req *cache.ProviderRequest) *cache.ProviderResponse {
	deadline := time.Now().Add(s.limits.budget)
	s.drainUploads()
	s.objects.drain()
	s.objects.report()
	s.drainBackground(deadline, "the summary does not count them")
	s.mu.Lock()
	stats := s.stats
	refusals, writeRefusals, first := s.authRefusals, s.authWriteRefusals, s.firstAuthRefusal
	developer := s.writesRefused && s.developerRefusal
	s.mu.Unlock()
	s.reportAuthRefusals(stats, refusals, writeRefusals, first, developer)
	return providerOK(req, &stats)
}

// reportAuthRefusals states the run's TASK-cache auth verdict. Remote caching
// stays best-effort — this never fails a build — but a run whose writes were all
// refused persists NOTHING, so the next run starts cold and the loop repeats.
// Reported as uploadedCount=0 alone that is indistinguishable
// from a fully warm run, so the refusal is said out loud instead.
//
// A developer credential is the exception: the cache server lets only hosted CI
// write, so its refused write is the expected outcome, not a cold cache to
// investigate. developer says so, and the summary states it plainly instead of
// warning. Read refusals are still reported in full.
//
// Object-cache refusals are not counted here and never reach this warning: they
// speak on the object cache's own line (see cacheWriteOps and objectCache.report).
func (s *providerSession) reportAuthRefusals(stats cache.SummaryResult, refusals, writeRefusals int, first string, developer bool) {
	if refusals == 0 {
		return
	}
	if developer {
		if refusals > writeRefusals {
			s.log(fmt.Sprintf("%d cache read request(s) refused by the server this run; first refusal of the run: %s", refusals-writeRefusals, first))
		}
		s.log("this credential reads the shared cache and writes nothing to it: only hosted CI runs write, so the tasks built here stay in the local cache")
		return
	}
	s.log(fmt.Sprintf("%d cache request(s) refused by the server this run, %d of them on the write path; first: %s",
		refusals, writeRefusals, first))
	if writeRefusals > 0 && stats.UploadedCount == 0 {
		s.log("WARNING: every remote-cache write was refused — this run persisted NO cache entries, so the next run starts cold")
	}
}

// materializeHit downloads every blob of a negotiate hit into the blob-exchange
// directory, skipping digests already present (placed by an earlier restore or
// by the prefetch), and returns the bytes it placed for this restore. Each blob
// is content-verified by the fetcher and written atomically (temp + rename) so
// core never ingests a half-written file.
//
// A blob that a download-batch of the prefetch or of a late negotiate asks for
// is not fetched here until that request ends (see cacheinline.go): the
// restore downloads its other blobs first, then waits, then fetches what the
// request did not place.
//
// It materializes the blobs the server returned for the negotiated mode; the
// guarantee that every Manifest digest is present (full vs minimal/toplevel
// materialization) is refined with the mode ownership decision.
func (s *providerSession) materializeHit(ctx context.Context, result cache.KeyResult) (int64, error) {
	var own, carried []cache.BlobTransfer
	seen := make(map[string]bool, len(result.Downloads))
	for _, t := range result.Downloads {
		if seen[t.Digest] {
			continue
		}
		seen[t.Digest] = true
		if s.known[t.Digest] {
			continue // core already has this blob in its CAS; it ingests from there, not the exchange dir
		}
		if _, ok := cache.BlobExchangePath(s.exchangeDir, t.Digest); !ok {
			return 0, fmt.Errorf("invalid digest %s", t.Digest)
		}
		if s.inline.batch(t.Digest) != nil {
			carried = append(carried, t)
		} else {
			own = append(own, t)
		}
	}
	total, err := s.placeBlobs(ctx, own, s.client.BatchHitFetcher)
	if err != nil || len(carried) == 0 {
		return total, err
	}
	retry, direct, counted, err := s.awaitInline(ctx, carried)
	total += counted
	if err != nil {
		return total, err
	}
	n, err := s.placeBlobs(ctx, retry, s.client.BatchHitFetcher)
	total += n
	if err != nil {
		return total, err
	}
	n, err = s.placeBlobs(ctx, direct, s.client.HitFetcher)
	return total + n, err
}

// placeBlobs downloads the blobs the exchange directory does not hold yet,
// through one opener newFetcher builds for them, and returns the bytes it
// wrote. Building the opener for those blobs alone keeps a download-batch from
// asking for a blob already placed.
func (s *providerSession) placeBlobs(ctx context.Context, blobs []cache.BlobTransfer, newFetcher func(context.Context, cache.KeyResult) func(string) (io.ReadCloser, error)) (int64, error) {
	var need []cache.BlobTransfer
	for _, t := range blobs {
		path, _ := cache.BlobExchangePath(s.exchangeDir, t.Digest) // materializeHit checked the digest
		if !blobPresent(path, t.SizeBytes) {
			need = append(need, t)
		}
	}
	if len(need) == 0 {
		return 0, nil
	}
	fetch := newFetcher(ctx, cache.KeyResult{Downloads: need})
	var total int64
	for _, t := range need {
		path, _ := cache.BlobExchangePath(s.exchangeDir, t.Digest)
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
// written), so two restores that share a blob do not download it twice.
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
