package gocacheprog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The object-cache half of the helper: the provider socket the run's cache
// provider listens on, reached through the two ops in protocols/cache.
//
// Three rules shape everything here:
//
//  1. A get is synchronous, but a PUT is never: the upload happens after the
//     body is in the local store and the go command is told about it.
//  2. Every failure degrades to local-only: the first error disables the
//     remote half for the rest of the process instead of being retried per
//     request.
//  3. Nothing here is authoritative about trust. The accept list is derived
//     from the run's resolved policy and the PROVIDER decides which channel an
//     object carries; a hit on a channel this run does not accept is simply
//     absent from the response.

const (
	// objectNamespace scopes this helper's ids in the object cache. The id is
	// Go's action id, which already hashes the toolchain, the build flags, the
	// target platform and every input source file, so two namespaced ids that
	// match describe the same compilation.
	objectNamespace = "go-build"

	// putBatchSize bounds one object-put request. The protocol allows 5000
	// objects; a smaller batch keeps the first bytes moving early in a build
	// instead of holding everything for one large request.
	putBatchSize = 128

	// putFlushInterval flushes a partial batch, so the last few objects of a
	// small build do not wait for a batch that will never fill.
	putFlushInterval = 100 * time.Millisecond

	// putQueueCapacity bounds the memory the pending queue can hold. A full
	// queue DROPS, because blocking here would make the remote cache slow a
	// build down, which is the one thing it must never do.
	putQueueCapacity = 4096

	// putConcurrency bounds the object-put requests in flight at once.
	putConcurrency = 4

	// maxRemoteObjectBytes caps what this helper offers to the provider. Bytes
	// are handed over by staging a COPY in the exchange directory, so an
	// enormous object costs a full write before the provider ever sees it.
	// Large linked binaries rebuild from cached package archives quickly; the
	// archives are what is worth sharing.
	maxRemoteObjectBytes = 64 << 20

	// getBatchSize bounds one object-get request. The go command issues its
	// lookups in parallel (one per build action it can run at once), so the
	// misses of a cold build arrive in bursts; one request per burst is what
	// the protocol's batch shape is for.
	getBatchSize = 256

	// getCoalesceWindow is how long a lookup waits for siblings before its
	// batch is sent. It is paid only on a LOCAL miss — a local hit never reaches
	// this file — and is small next to the socket round trip it saves, so it
	// costs a lone lookup a few milliseconds and saves a burst most of its.
	getCoalesceWindow = 2 * time.Millisecond

	// maxSocketConns bounds the connections this helper opens. The socket
	// accepts many, and a provider serves one connection at a time, so a small
	// pool is what keeps a wave of misses from queueing behind each other.
	maxSocketConns = 8

	// drainTimeout bounds how long a "close" waits for queued uploads. The go
	// command is blocked on that response, so the wait is generous but finite:
	// losing the tail of a batch costs the next run a few misses, while waiting
	// forever costs every run.
	drainTimeout = 30 * time.Second

	// trustAny is the resolved trust policy core exports for a run that admits
	// developer-produced entries. The vocabulary is core's (--cache-trust) and
	// travels in cache.CacheTrustEnv.
	trustAny = "any"
)

// The three deadlines below bound what a slow provider costs a build. They are
// variables so a test can shorten them; production never changes them.
var (
	// dialTimeout bounds connecting to the socket.
	dialTimeout = 5 * time.Second
	// getTimeout bounds one object-get round trip, including the provider's
	// download of the hit into the exchange directory.
	getTimeout = 15 * time.Second
	// putTimeout bounds one object-put round trip. Puts run behind the build,
	// so the budget is the generous one core's provider session uses.
	putTimeout = 60 * time.Second
)

// getRequest is one lookup waiting to join an object-get batch.
type getRequest struct {
	id    string
	reply chan getReply
}

// getReply answers a getRequest: the hit and the exchange path its bytes are
// staged at, or ok=false for a miss (which every failure also is).
type getReply struct {
	hit  cache.ObjectHit
	path string
	ok   bool
}

// pendingPut is one object waiting to be offered to the provider.
type pendingPut struct {
	// id is the object id: Go's action id in hex.
	id string
	// outputID is the object's metadata: Go's output id in hex, which the
	// provider returns verbatim on a later hit.
	outputID string
	// size is the body's byte length.
	size int64
	// source is the local store path the bytes are staged from.
	source string
}

// remote is the helper's client for the provider's object-cache socket.
type remote struct {
	pool        *connPool
	exchangeDir string
	accept      []cache.Channel

	queue    chan pendingPut
	stop     chan struct{}
	loopDone chan struct{}
	inFlight sync.WaitGroup
	slots    chan struct{}

	gets     chan getRequest
	getsDone chan struct{}
	lookups  sync.WaitGroup

	disabled atomic.Bool
	failOnce sync.Once
	onFail   func(error)

	// served and offered count what this process actually exchanged with the
	// provider, which is what a user asking "is the shared cache working?"
	// needs to see.
	served  atomic.Int64
	offered atomic.Int64
}

// counts reports how many objects the provider served this process and how many
// it accepted from it.
func (r *remote) counts() (served, offered int64) {
	if r == nil {
		return 0, 0
	}
	return r.served.Load(), r.offered.Load()
}

// openRemote connects the helper to the provider's object-cache socket named by
// the job environment, or returns nil when this run has none.
//
// A nil remote is the ordinary case, not a failure: core exports the socket
// variable only for a provider-backed run, so its absence means "local cache
// only" and every call site treats a nil remote that way.
func openRemote(getenv func(string) string, onFail func(error)) *remote {
	if getenv == nil {
		return nil
	}
	socket := strings.TrimSpace(getenv(cache.ObjectCacheSocketEnv))
	if socket == "" {
		return nil
	}
	r := &remote{
		pool: &connPool{socket: socket, conns: make([]*socketConn, maxSocketConns)},
		// The provider creates its socket INSIDE the blob-exchange directory,
		// so the parent of the path is the directory bytes travel through. It
		// is the only derivation a job process can make, and the contract in
		// protocols/cache states it.
		exchangeDir: filepath.Dir(socket),
		accept:      acceptChannels(getenv(cache.CacheTrustEnv)),
		queue:       make(chan pendingPut, putQueueCapacity),
		stop:        make(chan struct{}),
		loopDone:    make(chan struct{}),
		slots:       make(chan struct{}, putConcurrency),
		gets:        make(chan getRequest),
		getsDone:    make(chan struct{}),
		onFail:      onFail,
	}
	go r.putLoop()
	go r.getLoop()
	return r
}

// acceptChannels maps the run's resolved trust policy onto the get filter.
//
// It fails CLOSED. "any" — the value core exports for a policy that admits
// developer-produced entries — is the only value that accepts every channel;
// anything else, including an absent or unknown policy, accepts only the
// provider-authoritative trusted channel. An object this run does not accept is
// a miss, so the cost of being strict is a rebuild and the cost of being lax is
// linking output an untrusted credential produced.
func acceptChannels(trust string) []cache.Channel {
	if strings.TrimSpace(trust) == trustAny {
		return nil
	}
	return []cache.Channel{cache.ChannelTrusted}
}

// get asks the provider for one action id and returns the hit plus the local
// path its bytes were staged at.
//
// The request joins the batch getLoop is assembling, so a burst of parallel
// misses costs one socket round trip. The caller still sees a synchronous
// answer: every request is replied to, including when the helper is closing.
func (r *remote) get(actionID string) (cache.ObjectHit, string, bool) {
	if r == nil || r.disabled.Load() {
		return cache.ObjectHit{}, "", false
	}
	req := getRequest{id: actionID, reply: make(chan getReply, 1)}
	select {
	case r.gets <- req:
	case <-r.stop:
		return cache.ObjectHit{}, "", false
	}
	reply := <-req.reply
	return reply.hit, reply.path, reply.ok
}

// markServed counts one object the go command actually used. It is called by
// the store path AFTER the bytes were verified and ingested, so a hit the
// helper refused never inflates the number a user reads.
func (r *remote) markServed() {
	if r != nil {
		r.served.Add(1)
	}
}

// getLoop assembles lookups into object-get batches: the first request opens
// a window, siblings arriving within it join, and the batch is sent on its own
// goroutine so the next window can open while the socket is busy.
func (r *remote) getLoop() {
	defer close(r.getsDone)
	for {
		var first getRequest
		select {
		case first = <-r.gets:
		case <-r.stop:
			return
		}
		batch := []getRequest{first}
		window := time.NewTimer(getCoalesceWindow)
	collect:
		for len(batch) < getBatchSize {
			select {
			case req := <-r.gets:
				batch = append(batch, req)
			case <-window.C:
				break collect
			case <-r.stop:
				break collect
			}
		}
		window.Stop()
		r.lookups.Add(1)
		go func(batch []getRequest) {
			defer r.lookups.Done()
			r.lookup(batch)
		}(batch)
	}
}

// lookup sends one object-get for a batch and answers every request in it.
func (r *remote) lookup(batch []getRequest) {
	answers := make(map[string]getReply, len(batch))
	defer func() {
		for _, req := range batch {
			req.reply <- answers[req.id]
		}
	}()
	if r.disabled.Load() {
		return
	}
	ids := make([]string, 0, len(batch))
	for _, req := range batch {
		if _, seen := answers[req.id]; seen {
			continue
		}
		answers[req.id] = getReply{}
		ids = append(ids, req.id)
	}
	payload, err := r.call(cache.OpObjectGet, &cache.ObjectGetParams{
		Namespace:      objectNamespace,
		IDs:            ids,
		AcceptChannels: r.accept,
	}, getTimeout)
	if err != nil {
		r.fail(err)
		return
	}
	result, diags := cache.ParseAndValidateObjectGetResult(payload)
	if result == nil {
		r.fail(fmt.Errorf("object-get result: %v", diags))
		return
	}
	for _, hit := range result.Objects {
		if _, asked := answers[hit.ID]; !asked {
			continue
		}
		path, ok := cache.BlobExchangePath(r.exchangeDir, hit.Digest)
		if !ok {
			continue
		}
		answers[hit.ID] = getReply{hit: hit, path: path, ok: true}
	}
}

// queuePut hands an object to the background uploader. It never blocks and
// never fails: the object is already in the local store, so losing the upload
// costs the NEXT run a lookup, not this one a build.
func (r *remote) queuePut(put pendingPut) {
	if r == nil || r.disabled.Load() || put.size > maxRemoteObjectBytes {
		return
	}
	select {
	case r.queue <- put:
	default:
		// The uploader is behind. Dropping is the contract.
	}
}

// close drains the queued uploads within a bounded budget and releases the
// connections. It is called once, from the "close" command.
func (r *remote) close() {
	if r == nil {
		return
	}
	close(r.stop)
	drained := make(chan struct{})
	go func() {
		<-r.getsDone
		r.lookups.Wait()
		<-r.loopDone
		r.inFlight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		// Best effort: the run keeps whatever reached the provider.
		r.disabled.Store(true)
	}
	r.pool.closeAll()
}

// putLoop batches queued objects into object-put requests. It flushes on a full
// batch or on a tick, and drains what is queued when the helper closes.
func (r *remote) putLoop() {
	defer close(r.loopDone)
	ticker := time.NewTicker(putFlushInterval)
	defer ticker.Stop()
	batch := make([]pendingPut, 0, putBatchSize)
	for {
		select {
		case put := <-r.queue:
			batch = append(batch, put)
			if len(batch) >= putBatchSize {
				batch = r.flush(batch)
			}
		case <-ticker.C:
			batch = r.flush(batch)
		case <-r.stop:
			for {
				select {
				case put := <-r.queue:
					batch = append(batch, put)
					if len(batch) >= putBatchSize {
						batch = r.flush(batch)
					}
				default:
					r.flush(batch)
					return
				}
			}
		}
	}
}

// flush sends one batch in the background and returns the empty batch to
// refill. It blocks only when putConcurrency requests are already in flight,
// which throttles the loop, never the go command.
func (r *remote) flush(batch []pendingPut) []pendingPut {
	if len(batch) == 0 {
		return batch
	}
	sending := append([]pendingPut(nil), batch...)
	r.inFlight.Add(1)
	r.slots <- struct{}{}
	go func() {
		defer r.inFlight.Done()
		defer func() { <-r.slots }()
		r.send(sending)
	}()
	return batch[:0]
}

// send stages each object's bytes in the exchange directory and offers the
// batch to the provider.
func (r *remote) send(batch []pendingPut) {
	if r.disabled.Load() {
		return
	}
	objects := make([]cache.ObjectPut, 0, len(batch))
	for _, put := range batch {
		// The object id IS the content address here: the go command derives the
		// output id as the SHA-256 of the body, so the digest the exchange
		// directory addresses the bytes by is that same sum.
		digest := cache.DigestAlgorithm + ":" + put.outputID
		path, ok := cache.BlobExchangePath(r.exchangeDir, digest)
		if !ok {
			continue
		}
		if err := stageBlob(path, put.source, put.size); err != nil {
			continue // one unstageable object is not a reason to drop the batch
		}
		objects = append(objects, cache.ObjectPut{
			ID:     put.id,
			Digest: digest,
			Size:   put.size,
			Meta:   put.outputID,
		})
	}
	if len(objects) == 0 {
		return
	}
	payload, err := r.call(cache.OpObjectPut, &cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   objects,
	}, putTimeout)
	if err != nil {
		r.fail(err)
		return
	}
	result, diags := cache.ParseAndValidateObjectPutResult(payload)
	if result == nil {
		r.fail(fmt.Errorf("object-put result: %v", diags))
		return
	}
	r.offered.Add(int64(result.Accepted))
}

// stageBlob copies an object's bytes to their content address in the exchange
// directory, where the provider reads them.
//
// The destination is content addressed, so a file already there holds these
// bytes and the copy is skipped. The copy itself lands through a sibling temp
// file and a rename, so a provider reading the same path never sees a partial
// blob.
//
// A source that is not size bytes long is refused. The source is the body in
// GOCACHE, which the go command also writes when it runs without this helper,
// and the go command truncates a data file it failed to finish writing. Staging
// such a file would leave a short blob at the digest's address, and the skip
// above would keep it there for every later offer of that object.
func stageBlob(destination, source string, size int64) error {
	if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(destination), "blob-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	written, err := io.Copy(temp, in)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("gocacheprog: staged object is %d bytes, announced %d", written, size)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, destination)
}

// fail disables the remote half for the rest of the process and reports the
// first error once. Retrying a broken socket per request would turn a dead
// provider into a per-lookup timeout on every compile.
func (r *remote) fail(err error) {
	r.disabled.Store(true)
	r.failOnce.Do(func() {
		if r.onFail != nil {
			r.onFail(err)
		}
	})
}

// call issues one op on the socket and returns its payload. timeout bounds
// the whole round trip; a provider that exceeds it is treated as broken.
func (r *remote) call(op cache.ProviderOp, params any, timeout time.Duration) (json.RawMessage, error) {
	raw, err := cache.MarshalPayload(params)
	if err != nil {
		return nil, err
	}
	conn, err := r.pool.acquire()
	if err != nil {
		return nil, err
	}
	resp, err := conn.roundTrip(op, raw, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		code, message := "unknown", "provider reported failure"
		if resp.Error != nil {
			code, message = resp.Error.Code, resp.Error.Message
		}
		return nil, fmt.Errorf("op %s failed (%s): %s", op, code, message)
	}
	return resp.Payload, nil
}

// connPool keeps a bounded set of socket connections. Each one multiplexes: the
// contract says request ids are scoped per connection and responses may come
// back out of order, so a connection is shared rather than leased.
type connPool struct {
	socket string

	mu    sync.Mutex
	conns []*socketConn
	next  int
	shut  bool
}

func (p *connPool) acquire() (*socketConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shut {
		return nil, errors.New("object cache socket is closed")
	}
	index := p.next
	p.next = (p.next + 1) % len(p.conns)
	if conn := p.conns[index]; conn != nil && !conn.broken() {
		return conn, nil
	}
	conn, err := dialSocket(p.socket)
	if err != nil {
		return nil, err
	}
	p.conns[index] = conn
	return conn, nil
}

func (p *connPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shut = true
	for index, conn := range p.conns {
		if conn != nil {
			conn.close()
			p.conns[index] = nil
		}
	}
}

// socketConn is one multiplexed connection to the object-cache socket.
type socketConn struct {
	conn net.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *cache.ProviderResponse
	closed  bool
}

func dialSocket(path string) (*socketConn, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial object cache socket: %w", err)
	}
	c := &socketConn{conn: conn, pending: map[int64]chan *cache.ProviderResponse{}}
	go c.readLoop()
	return c, nil
}

func (c *socketConn) broken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *socketConn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close()
}

// readLoop demultiplexes responses onto the waiting callers. A response for an
// unknown id is dropped: it answers a request that already gave up.
func (c *socketConn) readLoop() {
	reader := bufio.NewReaderSize(c.conn, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if resp, _ := cache.ParseAndValidateProviderResponse(line); resp != nil {
				c.deliver(resp)
			}
		}
		if err != nil {
			break
		}
	}
	c.abort()
}

func (c *socketConn) deliver(resp *cache.ProviderResponse) {
	c.mu.Lock()
	waiter, ok := c.pending[resp.ID]
	delete(c.pending, resp.ID)
	c.mu.Unlock()
	if ok {
		waiter <- resp
	}
}

// abort releases every waiter when the connection dies, so a caller blocked on
// a response fails instead of waiting for a peer that is gone.
func (c *socketConn) abort() {
	c.mu.Lock()
	c.closed = true
	waiters := c.pending
	c.pending = map[int64]chan *cache.ProviderResponse{}
	c.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
}

// roundTrip writes one request and waits for its response, for at most
// timeout. A connection that misses the deadline is CLOSED, not reused: a peer
// that stalled once holds an unknown number of requests it may never answer,
// and the late response, if it ever comes, would answer nobody.
func (c *socketConn) roundTrip(op cache.ProviderOp, payload json.RawMessage, timeout time.Duration) (*cache.ProviderResponse, error) {
	waiter := make(chan *cache.ProviderResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("object cache connection is closed")
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = waiter
	c.mu.Unlock()

	line, err := json.Marshal(&cache.ProviderRequest{
		ProtocolVersion: cache.ProviderProtocolVersion,
		ID:              id,
		Op:              op,
		Payload:         payload,
	})
	if err != nil {
		c.forget(id)
		return nil, err
	}
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err = c.conn.Write(append(line, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		c.close()
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case resp, ok := <-waiter:
		if !ok || resp == nil {
			return nil, errors.New("object cache connection closed before answering")
		}
		return resp, nil
	case <-deadline.C:
		c.forget(id)
		c.close()
		return nil, fmt.Errorf("object cache op %s timed out after %s", op, timeout)
	}
}

func (c *socketConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
