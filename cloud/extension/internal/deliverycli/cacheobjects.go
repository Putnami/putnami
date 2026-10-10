package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// The object cache: the generic (namespace, id) cache the provider serves to JOB
// processes over a local Unix socket, whose first consumer is Go's GOCACHEPROG.
//
// Every other provider op is driven by core over stdin/stdout. This one is not:
// the process that wants a compiler-cache entry is a job subprocess, so the
// provider also listens on a socket, returns its path in
// InitializeResult.ObjectCacheSocket, and core exports that path to every job it
// spawns. The framing is identical to stdin/stdout — one ProviderRequest JSON
// line in, one ProviderResponse line out — with three differences that shape
// this file:
//
//   - Request IDs are scoped PER CONNECTION. Each client numbers from 1, so
//     nothing here is shared between connections but the session's client.
//   - ONLY object-get and object-put are valid. Every other op is refused, so a
//     job process can never drive the session's lifecycle, restore a task entry,
//     or publish a run marker — the socket is reachable by anything the build
//     spawned, and it is the only provider surface that is.
//   - The socket lives DIRECTLY under BlobExchangeDir, never in a subdirectory: a
//     job is handed the socket path and nothing else, so it derives the directory
//     bytes travel through as the socket's PARENT. Nesting it would break every
//     put. (It also keeps the path under the ~104-byte sun_path cap — and when
//     BlobExchangeDir is itself too long, objectSocketPath relocates the socket
//     and object bytes together, preserving that parent-directory contract.)
//
// TRUST. object-get filters hits by the caller's AcceptChannels, and the channel
// on a hit is the SERVER's, derived from the credential that stored the object —
// never something a caller asserted. Under --cache-trust ci a run sends
// [ChannelTrusted], so a developer- or pull-request-produced object is omitted:
// indistinguishable from an unknown id, by design. object-put carries no channel
// at all; the server stamps it.

const (
	// objectSocketName is the socket's file name inside the exchange directory.
	// It matches the framework conformance fixture's name, and it is short on
	// purpose: a Unix socket path is capped near 104 bytes on macOS.
	objectSocketName = "objects.sock"
	// maxObjectSocketPath is the conservative common floor of the platform
	// sun_path limits (104 on macOS, 108 on Linux), minus room for the trailing
	// NUL. A path over it moves the socket to a short directory rather than
	// disabling the object cache (see objectSocketPath).
	maxObjectSocketPath = 100
)

// objectWriteScope is the scope the cache-server's object-store route requires
// (its own ScopeCacheObjectWrite, in an internal package this client cannot
// import). It appears here only to NAME the missing grant in a refusal the run
// log has to be readable about; nothing in this client gates on it.
const objectWriteScope = "cache.object.write"

const (
	// objectConnRequests bounds the requests ONE connection can have in flight.
	// It is acquired on the read loop, before the handling goroutine is spawned,
	// which is what makes it backpressure rather than bookkeeping: a client that
	// pipelines past it stops being read until a slot frees, so a peer cannot
	// turn a socket it controls into unbounded goroutines. It matches the
	// consumer's own socket pool, so a legitimate client never waits.
	objectConnRequests = 8
)

// The three deadlines below are what keeps a peer this provider does not trust —
// a job process, i.e. on a pull-request run the code under test — from turning
// the socket into a stall rather than a cache. They are variables so a test can
// shorten them; production never changes them.
var (
	// objectFirstRequestTimeout bounds how long a connection may sit silent
	// before its FIRST request. Without it, a job that dials and never speaks
	// holds a connection slot for the whole run, and enough of them stop the
	// accept loop for every well-behaved job. It is deliberately not an idle
	// timeout: the consumer pools its connections across a whole build, so
	// closing a healthy idle one would cost that process its remote cache.
	objectFirstRequestTimeout = 30 * time.Second
	// objectWriteTimeout bounds one response write. A peer that stops reading
	// would otherwise park a serving goroutine — and the session-wide request
	// slot it holds — forever, which is how one job starves every other job's
	// object cache.
	objectWriteTimeout = 30 * time.Second
	// objectDrainTimeout bounds the end-of-run upload drain. Core answers
	// summary under a 60s budget and discards the WHOLE SummaryResult when it
	// expires, so an object backlog must never spend the budget the run's task
	// entries need. Whatever is still in flight is said out loud instead.
	objectDrainTimeout = 20 * time.Second
	// objectWriteProbeWait bounds how long a queued batch waits for the run's
	// write probe before it uploads anyway (see enterWriteProbe). The probe saves
	// three batches of CAS bytes on a refused run, but the batch that holds it
	// performs a full presigned PUT first — a stream with NO deadline of its own:
	// the client applies its control-plane timeout per authed round trip and
	// deliberately not to blob transfers, and sets no client-wide Timeout. So an
	// unbounded wait would let ONE first put — a linked test binary, or a stalled
	// storage endpoint — park every other upload worker until the drain gives up
	// at summary and the run persists nothing. The bound keeps the trade at three
	// batches of wasted bytes instead of the whole pipeline.
	objectWriteProbeWait = 5 * time.Second
	// objectAuthRefusalGrace is how long 401s must keep coming before the object
	// write path latches closed; see observeWriteRefusal. It is several breaker
	// windows wide, so the ~30s the auth server's introspection breaker spends
	// fail-closed under a publish burst cannot cost a 45-minute run its object
	// cache, while a bearer that is genuinely dead still latches.
	objectAuthRefusalGrace = 2 * time.Minute
	// objectReadBreakerLookups is how many ANSWERED lookups must have served
	// zero objects before the read path closes on a run whose writes are already
	// latched shut; see observeLookup. It is the evidence threshold, so it is
	// deliberately generous: 512 independent ids that the namespace does not
	// hold, on top of a credential that has been told it may not write, is not a
	// slow start. Paying it costs the run 512 round trips once; not paying it
	// costs one per compilation for the whole run (one observed run made 9812
	// lookups and was served 0 objects).
	objectReadBreakerLookups = 512
)

const (
	// objectConnections bounds concurrently served socket connections. The socket
	// is reachable by every job process, so the bound is what keeps a wide build
	// (or a misbehaving job) from turning connections into unbounded goroutines
	// and file descriptors. A connection over the bound waits for a slot rather
	// than being refused: the client would read the refusal as a cold cache.
	//
	// The ceiling has to clear the widest legitimate build: one connection pool
	// per concurrent job process (the consumer pools up to 8), so a 16-lane run
	// already wants 128. Sized at the limit, a wide build would spend the run
	// queueing behind itself.
	objectConnections = 256
	// objectRequests bounds in-flight object requests across every connection.
	// Each get is a network round trip plus local staging; each put is a hash and
	// a stat. The bound is per session, not per connection, so 64 clients cannot
	// multiply into 64 waves.
	objectRequests = 16
	// objectUploadWorkers is how many put batches upload concurrently in the
	// background. Puts are fire-and-forget by contract, so the compiler never
	// waits on these; the bound keeps the drain at summary predictable.
	objectUploadWorkers = 4
	// objectUploadQueue bounds the queued put batches. A full queue DROPS the
	// batch (logged, counted) rather than blocking the compiler that offered it:
	// a dropped object is a miss on the next run, while a blocked put is a
	// stalled build.
	objectUploadQueue = 256
)

// objectProbeTimeout bounds the capability probe initialize makes before it
// opens the socket. The client's own control-plane budget is 30s, which is the
// right bound for a negotiate but not for a discovery probe that sits in front
// of the FIRST op of every run: an unreachable or wedged cache server must cost
// the build a few seconds, not half a minute, and the only thing lost is the
// compiler cache.
const objectProbeTimeout = 5 * time.Second

// objectCache is the session's object-cache half: the listener, its bounded
// serving pool, and the background upload queue that summary drains. It is nil
// on a session that did not negotiate the capability, which is the whole off
// switch — no socket, no capability echo, and core exports nothing to its jobs.
type objectCache struct {
	s        *providerSession
	path     string
	listener net.Listener
	// exchangeDir is the directory OBJECT bytes travel through. It is always the
	// socket's PARENT, because that is the only thing a job process handed a
	// socket path can derive — never s.exchangeDir, which is the same directory
	// on the ordinary path but not on the short-path fallback.
	exchangeDir string
	// cleanup removes the fallback socket directory, and is a no-op when the
	// socket sits under the run's own exchange directory.
	cleanup   func()
	writePath remotecache.WritePath

	serveWG sync.WaitGroup
	connSem chan struct{}
	reqSem  chan struct{}

	// conns holds the live connections so shutdown can close them. Closing the
	// LISTENER only stops new dials: a job process that left a connection open
	// leaves its serving goroutine parked in a blocking read, and close() would
	// then wait for a client that is never going to speak again.
	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	// queue carries put batches to the upload workers. It is closed exactly once,
	// by drain, and queueClosed guards the send: a put racing the end-of-run
	// drain must be dropped, never panic on a closed channel.
	queue      chan objectPutBatch
	workersWG  sync.WaitGroup
	queueMu    sync.Mutex
	queueClose sync.Once
	queueOpen  bool

	mu       sync.Mutex
	gets     int
	hits     int
	puts     int
	accepted int
	stored   int
	dropped  int
	// refused counts the objects the server would not take, and writeRefused is
	// the latch: once set, every later put is answered accepted=0 and every
	// queued batch is discarded without uploading its bytes.
	//
	// The latch is what keeps a refusal costing ONE batch instead of the whole
	// run. Without it every batch re-walks the write path — and that path uploads
	// the object bytes into the CAS BEFORE it asks for the index row — so a run
	// whose every store is refused still pushes its entire compiler cache into
	// cas_blobs as rows no object row will ever name, counted against the byte
	// quotas whose sweep then evicts real action-cache blobs to make room for it.
	//
	// What sets it is NOT simply "the server said no": see observeWriteRefusal.
	// A 403 latches at once; a 401 has to persist, because a transient one is a
	// blip this run can survive.
	//
	// It disables WRITES only. Lookups need cache.read, which every runner class
	// holds, so a refused writer keeps serving its jobs from the shared cache —
	// until readClosed decides that cache has nothing for it either.
	writeRefused bool
	refused      int
	// readClosed latches the LOOKUP path shut, and answered/unserved are what
	// decide it and what it costs; see observeLookup.
	//
	// A lookup is a synchronous round trip on the compiler's critical path, so
	// negotiateObjectCache already refuses to open the socket at all against a
	// server with no object index rather than "make every compilation pay a
	// round trip to be told 404". The same reasoning has to survive first
	// contact: a run whose writes are refused stores nothing, so nothing it
	// compiles can ever populate the namespace, and once several hundred
	// answered lookups have served it zero objects the namespace holds nothing
	// for this build either. Every later lookup is then a round trip whose
	// expected value is zero, paid per compilation.
	//
	// It needs BOTH facts. Zero hits alone is an ordinary cold run that may warm
	// mid-build; a refused write path alone still reads a namespace some trusted
	// run filled. Together they describe exactly the failing configuration —
	// the object cache is on, the write lease is not armed, so the feature can
	// only cost. Arming the lease clears writeRefused and this
	// breaker becomes unreachable by construction.
	//
	// answered counts lookups the SERVER answered: a lookup that errored proves
	// nothing about the namespace's contents, so it is not evidence. unserved
	// counts the ids the run stopped asking about, so the summary can say what
	// the breaker saved instead of hiding it.
	//
	// The threshold is a floor, not an exact bound: up to objectRequests lookups
	// are already in flight when it is crossed, so a real run closes at a few
	// over it. Nothing depends on the exact number.
	readClosed bool
	answered   int
	unserved   int
	// refusedSince is when the current unbroken streak of 401s began; a store
	// that succeeds clears it. It is what makes the 401 rule a duration rather
	// than a count (observeWriteRefusal explains why a count cannot work).
	refusedSince time.Time

	// The write probe. ONE batch at a time walks the write path until a store has
	// actually returned, because a refusal discovered by four workers in parallel
	// has already cost four batches of CAS bytes — the bytes go up BEFORE the
	// index row is asked for. probeRunning marks the slot taken; probeAnswered
	// records that a store round trip returned, the only thing that ends the
	// probe for the run; and probeWake is closed and replaced every time the slot
	// changes hands, so a batch parked behind a probe that ended WITHOUT an
	// answer takes the slot instead of proceeding blind. See enterWriteProbe.
	probeRunning  bool
	probeAnswered bool
	probeWake     chan struct{}
}

// objectPutBatch is one queued put: the namespace and the objects whose bytes
// are already staged in the exchange directory.
type objectPutBatch struct {
	namespace string
	objects   []remotecache.ObjectOffer
}

// negotiateObjectCache decides whether this session serves the object cache and,
// when it does, binds the socket BEFORE initialize answers.
//
// Both halves of the capability must hold, and a third condition the protocol
// leaves to the provider:
//
//  1. Core advertised CapabilityObjectCache in the initialize params (it only
//     does so for a live provider-backed run, so --no-cache and trust "none" are
//     already excluded).
//  2. This provider is serving — a configured client and an exchange directory.
//  3. The SERVER advertises object-cache. An older cache-server has no object
//     index, so opening the socket would make every compilation pay a round trip
//     to be told 404 on its critical path. The probe also selects the blob upload
//     mechanism, which the object write path shares with the batched task path.
//
// Core validates the returned path synchronously, while it processes the
// initialize result: it requires an absolute path that os.Stat reports as a
// socket at that instant. So the listener is bound here, synchronously, never in
// a goroutine that might not have bound yet.
//
// Every failure disables the object cache and returns nil. None of them fails
// initialize: an unavailable compiler cache is a local build, not a broken run.
func (s *providerSession) negotiateObjectCache(p *cache.InitializeParams) *objectCache {
	if !advertisesObjectCache(p.Capabilities) || !s.serving() {
		return nil
	}

	probeCtx, cancelProbe := context.WithTimeout(s.ctx, objectProbeTimeout)
	defer cancelProbe()
	caps, err := s.client.Capabilities(probeCtx)
	if err != nil {
		s.logCacheError("object cache capabilities", err)
		return nil
	}
	writePath, supported := remotecache.ObjectWritePath(caps)
	if !supported {
		s.log("object cache: the cache server does not advertise object-cache; jobs keep their local compiler cache")
		return nil
	}

	if !filepath.IsAbs(s.exchangeDir) {
		s.log(fmt.Sprintf("object cache: exchange directory %q is not absolute; object cache disabled", s.exchangeDir))
		return nil
	}
	path, cleanup, err := objectSocketPath(s.exchangeDir)
	if err != nil {
		s.log(fmt.Sprintf("object cache: %v", err))
		return nil
	}
	// A stale socket file from a crashed previous run makes bind fail with
	// EADDRINUSE even though nothing is listening.
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		cleanup()
		s.log(fmt.Sprintf("object cache: listen on %s: %v", path, err))
		return nil
	}

	oc := &objectCache{
		s:        s,
		path:     path,
		listener: listener,
		// The contract a client depends on is that the exchange directory is the
		// socket's parent, so derive it rather than assuming s.exchangeDir.
		exchangeDir: filepath.Dir(path),
		cleanup:     cleanup,
		writePath:   writePath,
		connSem:     make(chan struct{}, objectConnections),
		reqSem:      make(chan struct{}, objectRequests),
		conns:       make(map[net.Conn]struct{}),
		queue:       make(chan objectPutBatch, objectUploadQueue),
		queueOpen:   true,
		probeWake:   make(chan struct{}),
	}
	for i := 0; i < objectUploadWorkers; i++ {
		oc.workersWG.Add(1)
		go func() {
			defer oc.workersWG.Done()
			oc.uploadWorker()
		}()
	}
	oc.serveWG.Add(1)
	go func() {
		defer oc.serveWG.Done()
		oc.acceptLoop()
	}()
	return oc
}

// objectSocketPath picks a bindable socket path for the run and returns it with
// a cleanup that removes the directory it created (a no-op on the ordinary
// path).
//
// The protocol says the socket sits DIRECTLY under BlobExchangeDir, and that is
// the default here. It cannot be the only option, because the exchange directory
// is a per-run temp directory under the machine-global store
// ($HOME/.putnami/store/<repo-id>/cache-provider-exchange-<random>), which on a
// default macOS install already measures 93 of the platform's ~104 bytes: a
// home directory a dozen characters longer than this author's would silently
// disable the compiler cache for every developer on that machine.
//
// So a too-long path falls back to a short directory of its own, exactly as the
// protocol's own conformance fixture does. What the fallback must preserve is
// the only thing a client can derive — the exchange directory is the socket's
// PARENT — and it does: object bytes travel through filepath.Dir(path). Core
// validates that the negotiated path is absolute and is a socket, never where it
// lives, and object bytes are exchanged only with job processes, never with core.
func objectSocketPath(exchangeDir string) (string, func(), error) {
	path := filepath.Join(exchangeDir, objectSocketName)
	if len(path) <= maxObjectSocketPath {
		if err := os.MkdirAll(exchangeDir, 0o755); err != nil {
			return "", func() {}, fmt.Errorf("create exchange directory: %w", err)
		}
		return path, func() {}, nil
	}
	short, err := os.MkdirTemp("", "putnami-objects-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create short socket directory: %w", err)
	}
	path = filepath.Join(short, objectSocketName)
	if len(path) > maxObjectSocketPath {
		_ = os.RemoveAll(short)
		return "", func() {}, fmt.Errorf("socket path %q exceeds the %d-byte platform limit", path, maxObjectSocketPath)
	}
	return path, func() { _ = os.RemoveAll(short) }, nil
}

// advertisesObjectCache reports whether a capability list carries the
// object-cache string. Both halves of the gate read it: core's
// InitializeParams.Capabilities (does core want the object cache?) and the
// provider's own InitializeResult.Capabilities echo (which is what makes
// ObjectCacheSocket meaningful to core at all).
func advertisesObjectCache(capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == cache.CapabilityObjectCache {
			return true
		}
	}
	return false
}

// acceptLoop serves the socket until it is closed. Each connection gets its own
// goroutine and its own request-id space; a connection over the bound waits for
// a slot instead of being refused.
func (oc *objectCache) acceptLoop() {
	for {
		conn, err := oc.listener.Accept()
		if err != nil {
			return // the listener was closed: the session is shutting down
		}
		select {
		case oc.connSem <- struct{}{}:
		case <-oc.s.ctx.Done():
			_ = conn.Close()
			return
		}
		oc.track(conn)
		oc.serveWG.Add(1)
		go func() {
			defer oc.serveWG.Done()
			defer func() { <-oc.connSem }()
			defer oc.untrack(conn)
			oc.serveConn(conn)
		}()
	}
}

// track registers a live connection. untrack closes it and forgets it, so the
// two are idempotent with the shutdown sweep in close.
func (oc *objectCache) track(conn net.Conn) {
	oc.connMu.Lock()
	oc.conns[conn] = struct{}{}
	oc.connMu.Unlock()
}

func (oc *objectCache) untrack(conn net.Conn) {
	oc.connMu.Lock()
	delete(oc.conns, conn)
	oc.connMu.Unlock()
	_ = conn.Close()
}

// closeConns closes every live connection, which is what unblocks the parked
// reads in serveConn so close can wait on a bounded amount of work.
func (oc *objectCache) closeConns() {
	oc.connMu.Lock()
	live := make([]net.Conn, 0, len(oc.conns))
	for conn := range oc.conns {
		live = append(live, conn)
	}
	oc.connMu.Unlock()
	for _, conn := range live {
		_ = conn.Close()
	}
}

// serveConn runs the JSONL loop for ONE connection. Request ids are scoped to
// this connection, so the demultiplexing is the client's; all this side promises
// is one response per request, carrying the request's id, in any order.
//
// Requests are dispatched to bounded goroutines rather than served inline, so a
// batched get that is waiting on the network never delays a concurrent put on
// the same connection — and nothing here can delay the stdin/stdout loop, which
// shares no lock with this path.
//
// Three bounds make this safe against the peer, which is a job process and
// therefore, on a pull-request run, code the repository under test wrote:
// connRequests is taken HERE, on the read loop, so a client that pipelines
// faster than the provider serves stops being read instead of spawning
// goroutines; each response write carries a deadline, so a peer that stops
// reading cannot park a serving goroutine and the session-wide slot it holds;
// and a connection that never sends a first request is closed rather than
// holding a connection slot for the run.
func (oc *objectCache) serveConn(conn net.Conn) {
	in := bufio.NewScanner(conn)
	in.Buffer(make([]byte, 0, 64*1024), maxProviderRequestBytes)
	var writeMu sync.Mutex
	var pending sync.WaitGroup
	defer pending.Wait() // never close the connection under an in-flight response

	// Bounds this ONE connection's in-flight requests. Acquired synchronously
	// below: that is what makes it backpressure on the peer's socket.
	connRequests := make(chan struct{}, objectConnRequests)

	write := func(resp *cache.ProviderResponse) {
		line, err := json.Marshal(resp)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(objectWriteTimeout))
		_, _ = conn.Write(append(line, '\n'))
	}

	// Only the FIRST request is deadlined. A pooled connection is idle between
	// the compiler's bursts for as long as the build lasts, so an idle timeout
	// would cost a healthy client its remote cache mid-build.
	_ = conn.SetReadDeadline(time.Now().Add(objectFirstRequestTimeout))
	first := true

	for in.Scan() {
		if first {
			first = false
			_ = conn.SetReadDeadline(time.Time{})
		}
		line := in.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		req, diags := cache.ParseAndValidateProviderRequest(line)
		if req == nil {
			// No id to answer on: report it on the envelope the client can still
			// parse, so a malformed line is not silent.
			write(&cache.ProviderResponse{
				ProtocolVersion: cache.ProviderProtocolVersion,
				OK:              false,
				Error: &cache.ProviderError{
					Code:    "bad-request",
					Message: fmt.Sprintf("malformed object-cache request: %v", diags),
				},
			})
			continue
		}
		if !req.Op.ValidOnObjectCacheSocket() {
			// The socket is reachable by any job process, so a session op here is
			// refused rather than served. This is the boundary that keeps a job
			// from restoring a task entry or publishing a run marker.
			write(&cache.ProviderResponse{
				ProtocolVersion: req.ProtocolVersion,
				ID:              req.ID,
				OK:              false,
				Error: &cache.ProviderError{
					Code:    "unsupported-op",
					Message: fmt.Sprintf("op %q is not valid on the object-cache socket", req.Op),
				},
			})
			continue
		}
		// Params are parsed HERE, synchronously, so the payload's alias into the
		// scanner's buffer never escapes into the goroutine; the goroutine gets
		// its own copy of the envelope for the same reason.
		reqCopy := *req
		var resp *cache.ProviderResponse
		var handle func() *cache.ProviderResponse
		switch req.Op {
		case cache.OpObjectGet:
			params, pdiags := cache.ParseAndValidateObjectGetParams(req.Payload)
			if params == nil {
				resp = providerErr(&reqCopy, "invalid_params", fmt.Sprintf("object-get: %v", pdiags))
			} else {
				handle = func() *cache.ProviderResponse { return oc.get(&reqCopy, params) }
			}
		case cache.OpObjectPut:
			params, pdiags := cache.ParseAndValidateObjectPutParams(req.Payload)
			if params == nil {
				resp = providerErr(&reqCopy, "invalid_params", fmt.Sprintf("object-put: %v", pdiags))
			} else {
				handle = func() *cache.ProviderResponse { return oc.put(&reqCopy, params) }
			}
		}
		if handle == nil {
			write(resp)
			continue
		}
		// Taken before the goroutine exists, so this connection can never have
		// more than objectConnRequests of them: past the bound the loop stops
		// reading and the peer's own socket buffer becomes the queue.
		select {
		case connRequests <- struct{}{}:
		case <-oc.s.ctx.Done():
			write(providerErr(&reqCopy, "shutting_down", "the cache provider session is shutting down"))
			return
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer func() { <-connRequests }()
			select {
			case oc.reqSem <- struct{}{}:
			case <-oc.s.ctx.Done():
				write(providerErr(&reqCopy, "shutting_down", "the cache provider session is shutting down"))
				return
			}
			defer func() { <-oc.reqSem }()
			write(handle())
		}()
	}
}

// get answers object-get: resolve the batch in one round trip, drop every hit on
// a channel the caller does not accept, stage the survivors' bytes in the
// exchange directory, and return them.
//
// Every failure is an empty result, never an error: a miss makes the compiler
// build locally, which is the correct outcome for an unavailable cache. A hit is
// only reported once its bytes are on disk, because the contract promises the
// caller they are there when the response is written.
func (oc *objectCache) get(req *cache.ProviderRequest, p *cache.ObjectGetParams) *cache.ProviderResponse {
	s := oc.s
	result := &cache.ObjectGetResult{}
	if len(p.IDs) == 0 {
		return providerOK(req, result)
	}
	// The read path is latched closed. Answer the miss from here rather than
	// spending a round trip on a namespace this run has already proven holds
	// nothing for it; see the readClosed field.
	if oc.readClosedLatched() {
		oc.observe(func(o *objectCache) {
			o.gets++
			o.unserved += len(p.IDs)
		})
		return providerOK(req, result)
	}
	records, err := s.client.LookupObjects(s.ctx, p.Namespace, p.IDs)
	if err != nil {
		s.logCacheError("object-get "+p.Namespace, err)
		oc.observe(func(o *objectCache) { o.gets++ })
		return providerOK(req, result)
	}
	for _, record := range records {
		channel := cache.Channel(record.Channel)
		if !objectChannelAccepted(p.AcceptChannels, channel) {
			// A filtered object is a MISS, not an error: a caller must not be able
			// to tell a rejected channel from an absent id.
			continue
		}
		if err := oc.stage(record); err != nil {
			s.log(fmt.Sprintf("object-get %s: stage %s: %v", p.Namespace, short(record.ID), err))
			continue // best-effort: an object we could not materialize is a miss
		}
		result.Objects = append(result.Objects, cache.ObjectHit{
			ID:       record.ID,
			Digest:   record.Digest,
			Size:     record.SizeBytes,
			Meta:     record.Meta,
			Producer: cache.Producer(record.Producer),
			Channel:  channel,
		})
	}
	oc.observeLookup(len(result.Objects))
	return providerOK(req, result)
}

// observeLookup records one ANSWERED lookup and decides the read breaker.
//
// The rule is stated on the readClosed field: a run that may not write, and
// whose answered lookups have served it zero objects, is reading a namespace
// that has nothing for this build and cannot acquire anything either. It closes
// the lookup path and says so once, naming the grant whose absence caused it —
// the run log otherwise carries only a trailing summary counter, which a
// reader cannot act on.
//
// Reads are never failed, only declined: a closed path answers the same empty
// result a miss answers, so every job keeps its local compiler cache and the
// build is byte-identical apart from the round trips it no longer pays.
func (oc *objectCache) observeLookup(hits int) {
	oc.mu.Lock()
	oc.gets++
	oc.hits += hits
	oc.answered++
	closing := !oc.readClosed && oc.writeRefused && oc.hits == 0 &&
		oc.answered >= objectReadBreakerLookups
	if closing {
		oc.readClosed = true
	}
	answered := oc.answered
	oc.mu.Unlock()
	if closing {
		oc.s.log(fmt.Sprintf("object cache: %d lookup(s) served 0 object(s) on a run whose writes the server refuses — "+
			"this run's credential carries no %s, the scope only a per-run cache write lease holds "+
			"(arm delivery.ci.cacheWriteLease). "+
			"No further lookups are issued this run; jobs keep their local compiler cache",
			answered, objectWriteScope))
	}
}

// readClosedLatched reports whether the object lookup path is closed.
func (oc *objectCache) readClosedLatched() bool {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return oc.readClosed
}

// stage writes one served object's bytes into the object exchange directory at
// their content address, from whichever channel the server used: inline bytes
// for a small object, its presigned GET for a large one.
//
// Already-present bytes are re-used only when they actually content-address to
// the digest. A stat-and-size check would be enough for a directory only core
// and the provider write to, which is what the task-restore path has; this
// directory is one every JOB PROCESS writes into by contract, so "a file is
// already there at the right size" is a claim a peer can manufacture. The
// provider tells its caller those bytes match the digest it reports, so it
// verifies them rather than passing on a peer's word — and a mismatch re-stages
// over it instead of failing.
func (oc *objectCache) stage(record remotecache.ObjectRecord) error {
	path, ok := cache.BlobExchangePath(oc.exchangeDir, record.Digest)
	if !ok {
		return fmt.Errorf("invalid digest %s", record.Digest)
	}
	src := blobExchangeSource{dir: oc.exchangeDir}
	if blobPresent(path, record.SizeBytes) && verifyExchangeDigest(src, record.Digest) == nil {
		return nil
	}
	if record.Blob != nil {
		// The inline form may be gzipped (the digest stays the UNCOMPRESSED
		// content hash), and it is verified against that digest before it lands,
		// so a mislabeled object never reaches a compiler's cache.
		data, err := remotecache.VerifiedInlineBlob(*record.Blob, record.SizeBytes)
		if err != nil {
			return err
		}
		return writeExchangeBytes(data, path)
	}
	if record.Download == nil {
		return errors.New("server served neither inline bytes nor a download")
	}
	// DownloadBlob content-verifies the stream against the TRANSFER's digest,
	// while the bytes land at the RECORD's digest. Those are the same address on
	// every correct response, so requiring it here is what keeps a cross-wired
	// presign from parking object B's verified bytes at object A's content
	// address — the one place this file would otherwise take a digest on trust.
	if record.Download.Digest != record.Digest {
		return fmt.Errorf("download digest %s does not address object %s", short(record.Download.Digest), short(record.Digest))
	}
	transfer := *record.Download
	_, err := writeExchangeBlob(func(string) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			_, err := oc.s.client.DownloadBlob(oc.s.ctx, transfer, pw)
			_ = pw.CloseWithError(err)
		}()
		return pr, nil
	}, record.Digest, path)
	return err
}

// put answers object-put: accept the batch, then upload and index it in the
// background. It returns promptly — the contract acknowledges QUEUEING, and the
// bytes are durable at summary — so a compiler never waits on the network.
//
// Acceptance is a cheap local check (the bytes the caller promised to stage are
// there, at the promised size). The expensive integrity check — hashing the
// content against the offered digest — happens on the upload worker, before the
// bytes reach the CAS, because a content-addressed store must never be handed
// bytes that do not match their address and a presigned PUT verifies nothing.
func (oc *objectCache) put(req *cache.ProviderRequest, p *cache.ObjectPutParams) *cache.ProviderResponse {
	// A read-only session (PUTNAMI_CACHE_READ_ONLY, the pinned-runner posture)
	// serves reads and writes nothing, objects included.
	if oc.s.readOnly {
		oc.observe(func(o *objectCache) { o.puts++ })
		return providerOK(req, &cache.ObjectPutResult{Accepted: 0})
	}
	// The write path is latched closed. Answer accepted=0 from here rather than
	// queueing a batch whose upload would push bytes into the CAS only to be
	// refused the index row again. Only a verdict that cannot change inside this
	// run sets the latch — a 403 scope denial, or 401s that outlasted
	// objectAuthRefusalGrace — so this never turns a blip into a run-long loss.
	if oc.writeRefusedLatched() {
		oc.observe(func(o *objectCache) {
			o.puts++
			o.refused += len(p.Objects)
		})
		return providerOK(req, &cache.ObjectPutResult{Accepted: 0})
	}
	offers := make([]remotecache.ObjectOffer, 0, len(p.Objects))
	for _, obj := range p.Objects {
		path, ok := cache.BlobExchangePath(oc.exchangeDir, obj.Digest)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || (obj.Size > 0 && info.Size() != obj.Size) {
			// The caller promised the bytes were staged at this address and size.
			// A provider that cannot see them drops the object rather than
			// offering the server an index row for content it cannot upload.
			continue
		}
		// Provenance is deliberately absent from the offer. The cache server
		// derives producer/channel from the bearer it verifies, so a caller
		// cannot turn a developer object into a trusted CI one — the same rule
		// the task-entry upload path states.
		offers = append(offers, remotecache.ObjectOffer{
			ID:        obj.ID,
			Digest:    obj.Digest,
			SizeBytes: info.Size(),
			Meta:      obj.Meta,
		})
	}
	accepted := len(offers)
	if accepted > 0 && !oc.enqueue(objectPutBatch{namespace: p.Namespace, objects: offers}) {
		// The queue is full or the run is draining: report the drop honestly
		// rather than promising a durability the summary will not deliver.
		accepted = 0
	}
	oc.observe(func(o *objectCache) {
		o.puts++
		o.accepted += accepted
	})
	return providerOK(req, &cache.ObjectPutResult{Accepted: accepted})
}

// enqueue hands a batch to the upload workers, reporting whether it was taken.
// The lock plus queueOpen is what makes a put racing the end-of-run drain safe:
// a send on a closed channel panics, and this is the one place that sends.
//
// A refusal is counted and logged OUTSIDE the lock, so a slow stderr can never
// hold up the next compiler's put.
func (oc *objectCache) enqueue(batch objectPutBatch) bool {
	reason := ""
	oc.queueMu.Lock()
	if !oc.queueOpen {
		reason = "the run is draining"
	} else {
		select {
		case oc.queue <- batch:
		default:
			reason = "the upload queue is full"
		}
	}
	oc.queueMu.Unlock()
	if reason == "" {
		return true
	}
	oc.observe(func(o *objectCache) { o.dropped += len(batch.objects) })
	oc.s.log(fmt.Sprintf("object cache: dropped %d object(s) — %s", len(batch.objects), reason))
	return false
}

// uploadWorker uploads queued batches until the queue is closed and drained.
func (oc *objectCache) uploadWorker() {
	for batch := range oc.queue {
		oc.upload(batch)
	}
}

// upload verifies the batch's staged bytes, uploads them, and records their
// index rows. Bytes first, index row second: the server refuses to index a
// digest it does not hold, so a row can never name absent bytes.
//
// That order is also why the refusal latch is read HERE, before any of the work.
// The bytes are already in the CAS by the time the store call is refused, so a
// latch checked after the round trip would still have paid for it; a run whose
// credential the object write gate rejects must spend one batch learning that,
// not one per batch for the whole compiler cache.
//
// One batch is that probe and the others wait for its verdict (bounded — see
// enterWriteProbe) instead of racing it. The capability probe cannot answer this
// question: GET capabilities is authorized at cache.read, and it advertises what
// the SERVER serves, not what the CALLER may write — nothing in the response
// distinguishes a credential the object write gate will admit from one it will
// refuse.
func (oc *objectCache) upload(batch objectPutBatch) {
	s := oc.s
	if s.ctx.Err() != nil {
		return
	}
	// answered records that a store round trip actually returned. It stays false
	// on every path that gives up before one — a session cancel, or a batch whose
	// every staged digest failed verification — and leaveWriteProbe then hands
	// the slot to the next batch rather than spending the run's one-batch
	// guarantee on a probe that learned nothing.
	answered := false
	if oc.enterWriteProbe() {
		defer func() { oc.leaveWriteProbe(answered) }()
	}
	if oc.writeRefusedLatched() {
		oc.observe(func(o *objectCache) { o.refused += len(batch.objects) })
		return
	}
	src := blobExchangeSource{dir: oc.exchangeDir}
	verified := make([]remotecache.ObjectOffer, 0, len(batch.objects))
	for _, obj := range batch.objects {
		if err := verifyExchangeDigest(src, obj.Digest); err != nil {
			// Refusing here is the point: a presigned PUT verifies nothing, so
			// mislabeled bytes would sit in the CAS under a digest that is not
			// their content address and poison every later reader of it.
			s.log(fmt.Sprintf("object-put %s: %v", short(obj.ID), err))
			continue
		}
		verified = append(verified, obj)
	}
	if len(verified) == 0 {
		return
	}
	stored, err := s.client.StoreObjects(s.ctx, batch.namespace, verified, src, oc.writePath)
	// This batch has walked the whole write path, so whatever it found is the
	// verdict the parked batches are waiting for. The deferred leaveWriteProbe
	// publishes it AFTER the latch below is decided, so a waiter never wakes to a
	// write path this batch already knows is closed.
	answered = true
	if err != nil {
		if status, refused := cacheAuthRefusal(err); refused {
			oc.observeWriteRefusal(len(verified), status)
			return
		}
		s.logCacheError("object-put "+batch.namespace, err)
		return
	}
	oc.observe(func(o *objectCache) {
		o.stored += len(stored)
		// A store the server took ends any 401 streak: this credential works, so
		// the next 401 starts its own grace window rather than inheriting one.
		o.refusedSince = time.Time{}
	})
}

// cacheAuthRefusal reports the status of an exchange the server refused for the
// CREDENTIAL, rather than one that failed for a transient reason. 401 and 403 are
// the only two: everything else (a timeout, a 5xx, a malformed body) can differ
// on the next batch, and latching on one of those would turn a blip into a
// run-long outage of the compiler cache.
func cacheAuthRefusal(err error) (int, bool) {
	var status *remotecache.StatusError
	if !errors.As(err, &status) {
		return 0, false
	}
	if status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
		return status.Status, true
	}
	return 0, false
}

// observeWriteRefusal records one refused object-write exchange and closes the
// write path for the rest of the run when — and only when — the refusal is
// conclusive.
//
// A 403 latches AT ONCE. It is a scope verdict on a credential that cannot
// change inside one run — the client does not even re-mint on it, because a
// re-mint yields the same identity with the same grants — and it is the refusal
// that costs CAS bytes: the run's cache.write is good enough for find-missing
// and the presigned PUTs, so the object bytes are already in the CAS by the time
// objects store refuses the index row. Every later batch would add more blobs no
// object row will ever name.
//
// A 401 does NOT latch on its own. It says the BEARER was not accepted, which a
// run can survive and which usually costs no CAS bytes at all: find-missing and
// upload-batch carry the same bearer, so a refused credential is refused there,
// one round trip ahead of any presigned PUT. It is also a known TRANSIENT — a
// publish burst once drove uncached delegation introspection past the auth
// server's rate limit and its breaker failed closed with 401 for about 30
// seconds — and a run that latched
// on one of those would spend the remaining 33 minutes of a 45-minute run
// persisting no objects, while its log pointed the operator at a lease scope
// instead of a breaker.
//
// A COUNT of consecutive 401s cannot separate the two: 30 seconds of breaker
// easily produces hundreds, four workers deep. Time can. So 401s latch only once
// they have kept coming for objectAuthRefusalGrace with no store succeeding in
// between — a bearer that is genuinely dead still latches, and a blip costs a
// few batches of control round trips.
//
// It never fails the run: the object cache is an accelerator by contract, and a
// refused writer still serves its jobs every object the shared cache already
// holds.
//
// It logs the STATUS and nothing the server wrote. The classification record —
// op, status, token-source class, and the server's own reason, bounded and
// checked for an echoed bearer — is the auth observer's line, printed once per
// session under the object cache's own name by the same redaction path the
// task-cache refusals use. These lines state the consequence, so they
// must not be the place a server's error body reaches the run log unredacted.
func (oc *objectCache) observeWriteRefusal(objects, status int) {
	now := time.Now()
	oc.mu.Lock()
	latch := status == http.StatusForbidden
	streakStarted := false
	if !latch {
		if oc.refusedSince.IsZero() {
			oc.refusedSince = now
			streakStarted = true
		}
		latch = now.Sub(oc.refusedSince) >= objectAuthRefusalGrace
	}
	first := latch && !oc.writeRefused
	if latch {
		oc.writeRefused = true
	}
	oc.refused += objects
	oc.mu.Unlock()

	switch {
	case first && status == http.StatusForbidden:
		// A 403 is a verdict on the credential's CLASS, so it names the grant.
		// Without the name the run says only that "the server refused", and the
		// reader has to know that the object-store route is gated on a scope no
		// service-account identity may hold and that a per-run lease is the only
		// thing that carries it.
		oc.s.log(fmt.Sprintf("object cache: the cache server refused this run's object write (HTTP %d) — "+
			"this run's credential carries no %s, the scope only a per-run cache write lease holds "+
			"(arm delivery.ci.cacheWriteLease); "+
			"no further object bytes are uploaded this run and jobs keep their local compiler cache — reads continue",
			status, objectWriteScope))
	case first:
		oc.s.log(fmt.Sprintf("object cache: the cache server refused this run's object write (HTTP %d); "+
			"no further object bytes are uploaded this run and jobs keep their local compiler cache — reads continue", status))
	case streakStarted:
		// Once per streak, not once per batch: a breaker window refuses every
		// batch four workers can offer it.
		oc.s.log(fmt.Sprintf("object cache: the cache server refused an object write (HTTP %d); "+
			"the write path stays OPEN — a 401 can be a transient auth failure, so it closes the path only if refusals keep coming for %s",
			status, objectAuthRefusalGrace))
	}
}

// writeRefusedLatched reports whether the object write path is closed.
func (oc *objectCache) writeRefusedLatched() bool {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return oc.writeRefused
}

// enterWriteProbe admits this batch to the write path and reports whether it
// holds the run's probe slot (see upload). A batch that does not hold it waits
// for the probe to publish a verdict — but only for objectWriteProbeWait, and
// then it uploads anyway.
//
// That bound is the whole point. The probe's own batch performs a full presigned
// PUT of its blobs, with no deadline of its own, before the store call that
// yields the verdict; an unbounded wait would let one large or one stalled
// upload park every other worker until the end-of-run drain gave up and the run
// persisted nothing. Proceeding unprobed costs at most objectUploadWorkers
// batches of CAS bytes on a refused run — bounded, and paid only when the probe
// is already in trouble.
func (oc *objectCache) enterWriteProbe() bool {
	probe, wake := oc.tryWriteProbe()
	if wake == nil {
		return probe
	}
	timer := time.NewTimer(objectWriteProbeWait)
	defer timer.Stop()
	for {
		select {
		case <-wake:
		case <-timer.C:
			return false
		case <-oc.s.ctx.Done():
			return false
		}
		if probe, wake = oc.tryWriteProbe(); wake == nil {
			return probe
		}
	}
}

// tryWriteProbe takes one look at the write path. It returns probe=true when
// this batch now HOLDS the probe slot, wake=nil when the batch may proceed
// without it (a probe already answered, or the path is latched closed), and a
// non-nil wake channel when it must wait for the current probe to hand the slot
// on. The channel is read under the lock that replaces it, so a waiter can never
// park on a channel that was already closed.
func (oc *objectCache) tryWriteProbe() (probe bool, wake chan struct{}) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.probeAnswered || oc.writeRefused {
		return false, nil
	}
	if !oc.probeRunning {
		oc.probeRunning = true
		return true, nil
	}
	return false, oc.probeWake
}

// leaveWriteProbe hands the probe slot back and wakes the batches parked on it.
//
// answered reports whether a store round trip actually returned — the only thing
// that ends the probe for the run. Releasing the slot without one would degrade
// the "a refusal costs ONE batch" guarantee to "one WORKER WAVE": a batch whose
// every staged digest failed verification never reaches a store, so the waiting
// workers would proceed with no verdict and upload objectUploadWorkers batches
// of object bytes into the CAS before the first 403 landed. So the slot goes to
// the next batch instead, and that batch probes.
func (oc *objectCache) leaveWriteProbe(answered bool) {
	oc.mu.Lock()
	oc.probeRunning = false
	if answered {
		oc.probeAnswered = true
	}
	wake := oc.probeWake
	oc.probeWake = make(chan struct{})
	oc.mu.Unlock()
	// Closed outside the lock, after it has been replaced: the waiters re-read
	// the state through tryWriteProbe, so they must never see this channel again.
	close(wake)
}

// drain closes the queue and waits for the in-flight uploads, so summary can
// promise that every accepted object is durable. It is idempotent.
//
// The wait is BOUNDED. Core answers summary under a 60s budget and discards the
// whole SummaryResult when it expires, so an object backlog that outlives
// objectDrainTimeout would cost the run its task-entry upload confirmations
// too — trading the thing the cache exists for against a compiler cache that is
// only ever an accelerator. Whatever is still uploading is left to the session
// context's cancel and said out loud instead.
func (oc *objectCache) drain() {
	if oc == nil {
		return
	}
	oc.queueClose.Do(func() {
		oc.queueMu.Lock()
		oc.queueOpen = false
		oc.queueMu.Unlock()
		close(oc.queue)
	})
	done := make(chan struct{})
	go func() {
		oc.workersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-oc.s.ctx.Done():
	case <-time.After(objectDrainTimeout):
		oc.s.log(fmt.Sprintf("object cache: upload drain did not finish within %s; the objects still in flight are not durable this run",
			objectDrainTimeout))
	}
}

// close stops serving the socket, drains the uploads, and removes the socket
// file. The session's context cancel has already aborted any in-flight transfer
// by the time close is reached on the shutdown path.
func (oc *objectCache) close() {
	if oc == nil {
		return
	}
	_ = oc.listener.Close()
	// Closing the listener stops new dials; closing the live connections is what
	// lets the parked readers return. Without it, one job process that exited
	// without closing its socket would hold the provider open until core's
	// force-terminate deadline.
	oc.closeConns()
	oc.serveWG.Wait()
	oc.drain()
	_ = os.Remove(oc.path)
	// Removes the fallback socket directory; a no-op when the socket sat under
	// the run's own exchange directory, which core removes itself.
	if oc.cleanup != nil {
		oc.cleanup()
	}
}

// report logs the object cache's end-of-run line. It is deliberately separate
// from cache.SummaryResult: those counters are the run's TASK entries, and
// folding compiler objects into them would silently change what "uploadedCount"
// has always meant.
func (oc *objectCache) report() {
	if oc == nil {
		return
	}
	oc.mu.Lock()
	gets, hits, puts, accepted, stored, dropped := oc.gets, oc.hits, oc.puts, oc.accepted, oc.stored, oc.dropped
	refused, writeRefused := oc.refused, oc.writeRefused
	readClosed, answered, unserved := oc.readClosed, oc.answered, oc.unserved
	oc.mu.Unlock()
	if gets == 0 && puts == 0 {
		return
	}
	line := fmt.Sprintf("object cache: %d get(s) served %d object(s); %d put(s) accepted %d, stored %d",
		gets, hits, puts, accepted, stored)
	if dropped > 0 {
		line += fmt.Sprintf("; DROPPED %d (see the warnings above)", dropped)
	}
	// Say it on the summary line too. A refused writer is otherwise
	// indistinguishable from a fully deduped one — stored 0, no error — which is
	// the same disguise the task-cache path no longer has. This line
	// is also the ONLY place the run speaks about object-write refusals: they are
	// best-effort by contract, so they never enter the task cache's verdict (see
	// cacheWriteOps).
	switch {
	case writeRefused:
		line += fmt.Sprintf("; REFUSED %d (the server rejected this run's object writes, so the next run rebuilds them)", refused)
	case refused > 0:
		line += fmt.Sprintf("; REFUSED %d (the server refused these writes; the write path stayed open)", refused)
	}
	// The breaker has to be on the summary line too, for the same reason the
	// refusal is: a run that stopped asking reports the same 0 objects served as
	// one that asked ten thousand times, and the two have different causes.
	if readClosed {
		line += fmt.Sprintf("; LOOKUPS CLOSED after %d answered served 0 (%d id(s) not asked; the run may not write, "+
			"so the namespace cannot warm — grant %s via the per-run cache write lease)", answered, unserved, objectWriteScope)
	}
	oc.s.log(line)
}

func (oc *objectCache) observe(f func(*objectCache)) {
	oc.mu.Lock()
	f(oc)
	oc.mu.Unlock()
}

// objectChannelAccepted implements the filter rule: an EMPTY accept list takes
// every channel, INCLUDING the legacy empty one a channel-less server reports. A
// run under an authoritative policy sends [ChannelTrusted], and that is what
// keeps a developer-produced object out of it.
func objectChannelAccepted(accept []cache.Channel, channel cache.Channel) bool {
	if len(accept) == 0 {
		return true
	}
	for _, want := range accept {
		if want == channel {
			return true
		}
	}
	return false
}

// writeExchangeBytes writes already-verified bytes into the exchange directory
// atomically — a temp file in the target directory renamed into place — so a
// concurrent reader of the content-addressed path never sees a partial object.
// It reuses the same atomic writer the restore path uses, handing it a reader
// over the in-memory bytes.
func writeExchangeBytes(data []byte, path string) error {
	_, err := writeExchangeBlob(func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}, "", path)
	return err
}

// verifyExchangeDigest reads a staged blob and checks it content-addresses to
// digest. It is the pre-upload integrity gate for objects: the bytes come from a
// job process, and the CAS is content-addressed, so offering it content that
// does not match its address would poison the digest for every later reader.
func verifyExchangeDigest(src remotecache.BlobSource, digest string) error {
	rc, err := src.OpenBlob(digest)
	if err != nil {
		return fmt.Errorf("open staged object %s: %w", short(digest), err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return fmt.Errorf("read staged object %s: %w", short(digest), err)
	}
	got := cache.DigestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("staged object content-addresses to %s, offered as %s", short(got), short(digest))
	}
	return nil
}
