// Package objectcachetest serves the object-cache half of the provider RPC
// (cache.OpObjectGet / cache.OpObjectPut) over a real Unix socket, backed by an
// in-memory store.
//
// It exists because the object cache has THREE implementations that must agree
// — the provider extension that serves it, the CLI that negotiates it, and the
// language extension whose helper calls it from a job process — and only one of
// them is in any given repository. A shared fake lets each side test against
// the same server instead of a private stub that drifts from the wire.
//
// It is deliberately a real server, not a mock: a real socket, real JSONL
// framing, real concurrent connections, per-connection request ids, and real
// bytes handed over through the blob-exchange directory. What it does NOT do is
// anything a test should not depend on — no network, no persistence, no
// eviction, no credential. The channel it stamps on stored objects is a
// constructor option, because "an untrusted credential stamps hint" is the rule
// every consumer's trust test needs to exercise.
//
// It lives in a non-test file so consumers in other modules can import it; it
// imports the standard library and its own protocol package only.
package objectcachetest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	cache "go.putnami.dev/protocol/cache"
)

// socketName is the socket's file name inside the exchange directory. It is
// short on purpose: a Unix socket path is capped near 104 bytes on macOS, and a
// provider creates its socket under the exchange directory precisely so the
// whole path stays under that cap.
const socketName = "objects.sock"

// maxSocketPath is the conservative common floor of the platform sun_path
// limits (104 on macOS, 108 on Linux), minus room for the trailing NUL.
const maxSocketPath = 100

// Object is one stored object: the bytes plus the metadata a get returns.
type Object struct {
	// Bytes is the object's content. The server writes it into the exchange
	// directory on a hit, addressed by its digest.
	Bytes []byte
	// Meta is the opaque caller metadata the object was stored with.
	Meta string
	// Channel is the trust channel the SERVER stamped when it accepted the
	// object. A caller never gets to assert one.
	Channel cache.Channel
	// Producer is the producer class the server stamped alongside the channel.
	Producer cache.Producer
}

// Option configures a Server at start time.
type Option func(*Server)

// WithChannel sets the trust channel the server stamps on every object it
// accepts, and the matching producer class. It models the provider's rule that
// trust comes from the credential: an untrusted one stamps cache.ChannelHint
// however the caller asked. The default is cache.ChannelTrusted.
func WithChannel(channel cache.Channel) Option {
	return func(s *Server) { s.channel = channel }
}

// WithSeed preloads one object, so a consumer can test a hit without performing
// a put first. The seeded object carries the server's configured channel.
func WithSeed(namespace, id string, content []byte, meta string) Option {
	return func(s *Server) {
		s.objects[objectKey{namespace: namespace, id: id}] = Object{Bytes: content, Meta: meta}
	}
}

type objectKey struct {
	namespace string
	id        string
}

// Server is a running fake object-cache socket server.
type Server struct {
	exchangeDir string
	path        string
	listener    net.Listener
	channel     cache.Channel

	wg sync.WaitGroup

	mu      sync.Mutex
	objects map[objectKey]Object
	gets    int
	puts    int
	closed  bool
}

// Start listens on a Unix socket serving the two object ops and returns the
// running server. exchangeDir is the blob-exchange directory the caller passed
// as cache.InitializeParams.BlobExchangeDir: the socket is created inside it and
// object bytes are handed over through cache.BlobExchangePath under it, exactly
// as a real provider does.
//
// When the resulting socket path would exceed the platform limit — a real risk
// under a deep temporary directory on macOS — the server binds a short path in
// the system temporary directory instead, and serves object bytes through THAT
// directory. The contract a client depends on is preserved either way: the
// exchange directory is always the socket's parent (see ExchangeDir), which is
// the only thing a job process handed a socket path can derive. Core validates
// that the negotiated path is absolute and is a socket, never where it lives, so
// the fallback keeps a long test path from turning into an unrunnable test.
func Start(exchangeDir string, opts ...Option) (*Server, error) {
	if exchangeDir == "" {
		return nil, errors.New("objectcachetest: an exchange directory is required")
	}
	if err := os.MkdirAll(exchangeDir, 0o755); err != nil {
		return nil, fmt.Errorf("objectcachetest: create exchange dir: %w", err)
	}
	s := &Server{
		exchangeDir: exchangeDir,
		channel:     cache.ChannelTrusted,
		objects:     make(map[objectKey]Object),
	}
	for _, opt := range opts {
		opt(s)
	}
	for key, obj := range s.objects {
		obj.Channel, obj.Producer = s.channel, producerFor(s.channel)
		s.objects[key] = obj
	}

	path, cleanup, err := socketPath(exchangeDir)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("objectcachetest: listen on %s: %w", path, err)
	}
	s.path = path
	// Bytes travel through the socket's own directory, so a client can always
	// derive it from the path it was given.
	s.exchangeDir = filepath.Dir(path)
	s.listener = listener

	s.wg.Add(1)
	go s.acceptLoop(cleanup)
	return s, nil
}

// socketPath picks a bindable socket path under dir, falling back to a short
// temporary directory when dir makes the path too long. The returned cleanup
// removes the fallback directory (it is a no-op for the ordinary path).
func socketPath(dir string) (string, func(), error) {
	path := filepath.Join(dir, socketName)
	if len(path) <= maxSocketPath {
		// A stale socket file from a previous run would make bind fail with
		// EADDRINUSE even though nothing is listening.
		_ = os.Remove(path)
		return path, func() {}, nil
	}
	short, err := os.MkdirTemp("", "putnami-objcache-")
	if err != nil {
		return "", func() {}, fmt.Errorf("objectcachetest: create short socket dir: %w", err)
	}
	return filepath.Join(short, socketName), func() { _ = os.RemoveAll(short) }, nil
}

// Path returns the absolute socket path, for an InitializeResult's
// ObjectCacheSocket.
func (s *Server) Path() string { return s.path }

// ExchangeDir returns the directory object bytes travel through. It is always
// the socket's parent directory — the derivation a client performs — so a test
// driving this server stages its blobs exactly where a job process would.
func (s *Server) ExchangeDir() string { return s.exchangeDir }

// Close stops accepting, waits for the in-flight connections, and removes the
// socket. It is idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	err := s.listener.Close()
	s.wg.Wait()
	return err
}

// Stats returns the number of object-get and object-put requests served so far.
func (s *Server) Stats() (gets, puts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.puts
}

// Object returns the stored object for (namespace, id), and whether it exists.
func (s *Server) Object(namespace, id string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[objectKey{namespace: namespace, id: id}]
	return obj, ok
}

func (s *Server) acceptLoop(cleanup func()) {
	defer s.wg.Done()
	defer cleanup()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // the listener was closed
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { _ = conn.Close() }()
			s.serveConn(conn)
		}()
	}
}

// serveConn runs the JSONL loop for ONE connection. Request ids are scoped to
// this connection, exactly as the socket contract states, so nothing here is
// shared with a sibling client but the object map.
func (s *Server) serveConn(conn net.Conn) {
	in := bufio.NewScanner(conn)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		req, diags := cache.ParseAndValidateProviderRequest(line)
		if req == nil {
			writeResponse(conn, &cache.ProviderResponse{
				ProtocolVersion: cache.ProviderProtocolVersion,
				OK:              false,
				Error:           &cache.ProviderError{Code: "bad-request", Message: fmt.Sprint(diags)},
			})
			continue
		}
		if !req.Op.ValidOnObjectCacheSocket() {
			// The socket is reachable by any job process, so a session op here is
			// refused rather than served.
			writeResponse(conn, &cache.ProviderResponse{
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
		switch req.Op {
		case cache.OpObjectGet:
			s.serveGet(conn, req)
		case cache.OpObjectPut:
			s.servePut(conn, req)
		}
	}
}

func (s *Server) serveGet(w io.Writer, req *cache.ProviderRequest) {
	params, diags := cache.ParseAndValidateObjectGetParams(req.Payload)
	if params == nil {
		writeFailure(w, req, "bad-params", fmt.Sprint(diags))
		return
	}
	result := &cache.ObjectGetResult{}
	s.mu.Lock()
	s.gets++
	for _, id := range params.IDs {
		obj, ok := s.objects[objectKey{namespace: params.Namespace, id: id}]
		if !ok || !acceptsChannel(params.AcceptChannels, obj.Channel) {
			// A filtered object is a MISS, not an error: the caller must not be
			// able to tell a rejected channel from an absent id.
			continue
		}
		digest := cache.DigestOf(obj.Bytes)
		if err := stageBlob(s.exchangeDir, digest, obj.Bytes); err != nil {
			continue // best-effort, exactly like a provider that failed to fetch
		}
		result.Objects = append(result.Objects, cache.ObjectHit{
			ID:       id,
			Digest:   digest,
			Size:     int64(len(obj.Bytes)),
			Meta:     obj.Meta,
			Producer: obj.Producer,
			Channel:  obj.Channel,
		})
	}
	s.mu.Unlock()
	writeSuccess(w, req, result)
}

func (s *Server) servePut(w io.Writer, req *cache.ProviderRequest) {
	params, diags := cache.ParseAndValidateObjectPutParams(req.Payload)
	if params == nil {
		writeFailure(w, req, "bad-params", fmt.Sprint(diags))
		return
	}
	accepted := 0
	s.mu.Lock()
	s.puts++
	for _, obj := range params.Objects {
		path, ok := cache.BlobExchangePath(s.exchangeDir, obj.Digest)
		if !ok {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil || cache.DigestOf(content) != obj.Digest {
			// The caller promised the bytes were staged and that they match the
			// digest. A provider that cannot verify that simply drops the object.
			continue
		}
		s.objects[objectKey{namespace: params.Namespace, id: obj.ID}] = Object{
			Bytes:    content,
			Meta:     obj.Meta,
			Channel:  s.channel,
			Producer: producerFor(s.channel),
		}
		accepted++
	}
	s.mu.Unlock()
	writeSuccess(w, req, &cache.ObjectPutResult{Accepted: accepted})
}

// stageBlob writes an object's bytes into the blob-exchange directory the way a
// provider does: a sibling temp file, then an atomic rename, so a concurrent
// reader of the same content-addressed path never sees a partial blob.
func stageBlob(exchangeDir, digest string, content []byte) error {
	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		return fmt.Errorf("objectcachetest: invalid digest %q", digest)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "blob-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// acceptsChannel implements the filter rule: an EMPTY accept list takes every
// channel, including the legacy empty one.
func acceptsChannel(accept []cache.Channel, channel cache.Channel) bool {
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

func producerFor(channel cache.Channel) cache.Producer {
	if channel == cache.ChannelTrusted {
		return cache.ProducerCI
	}
	return cache.ProducerDeveloper
}

func writeSuccess(w io.Writer, req *cache.ProviderRequest, payload any) {
	raw, err := cache.MarshalPayload(payload)
	if err != nil {
		writeFailure(w, req, "encode-failed", err.Error())
		return
	}
	writeResponse(w, &cache.ProviderResponse{
		ProtocolVersion: req.ProtocolVersion,
		ID:              req.ID,
		OK:              true,
		Payload:         raw,
	})
}

func writeFailure(w io.Writer, req *cache.ProviderRequest, code, message string) {
	writeResponse(w, &cache.ProviderResponse{
		ProtocolVersion: req.ProtocolVersion,
		ID:              req.ID,
		OK:              false,
		Error:           &cache.ProviderError{Code: code, Message: message},
	})
}

func writeResponse(w io.Writer, resp *cache.ProviderResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}
