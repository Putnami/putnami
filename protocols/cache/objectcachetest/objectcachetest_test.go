package objectcachetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// shortExchangeDir returns an exchange directory whose path is short enough for
// a Unix socket on every platform, so these tests exercise the ordinary
// in-exchange-dir socket rather than the long-path fallback.
func shortExchangeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oc")
	if err != nil {
		t.Fatalf("create exchange dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startServer(t *testing.T, dir string, opts ...Option) *Server {
	t.Helper()
	server, err := Start(dir, opts...)
	if err != nil {
		t.Fatalf("start object cache server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func dial(t *testing.T, server *Server) *Client {
	t.Helper()
	client, err := Dial(server.Path())
	if err != nil {
		t.Fatalf("dial %s: %v", server.Path(), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestObjectRoundTrip drives the whole contract the way a job process will: a
// miss, a put whose bytes travel through the blob-exchange directory, then a hit
// whose bytes are back in that directory at their content address.
func TestObjectRoundTrip(t *testing.T) {
	dir := shortExchangeDir(t)
	server := startServer(t, dir)
	client := dial(t, server)

	id := strings.Repeat("a", 64)
	if got, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: []string{id}}); err != nil {
		t.Fatalf("get: %v", err)
	} else if len(got.Objects) != 0 {
		t.Fatalf("cold get = %+v, want a miss", got.Objects)
	}

	content := []byte("compiled object")
	digest, err := StageBlob(server.ExchangeDir(), content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: "go-build",
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content)), Meta: "output-id"}},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", put.Accepted)
	}

	// Remove the staged blob so the hit can only be served by bytes the server
	// stages itself.
	path, ok := cache.BlobExchangePath(server.ExchangeDir(), digest)
	if !ok {
		t.Fatalf("blob exchange path for %q", digest)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove staged blob: %v", err)
	}

	hit, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: []string{id}})
	if err != nil {
		t.Fatalf("warm get: %v", err)
	}
	if len(hit.Objects) != 1 {
		t.Fatalf("warm get = %+v, want one hit", hit.Objects)
	}
	object := hit.Objects[0]
	if object.ID != id || object.Meta != "output-id" || object.Size != int64(len(content)) {
		t.Fatalf("hit = %+v, want the stored object", object)
	}
	if object.Channel != cache.ChannelTrusted || object.Producer != cache.ProducerCI {
		t.Fatalf("hit provenance = %q/%q, want the server's stamp", object.Producer, object.Channel)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hit blob: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("hit bytes = %q, want %q", got, content)
	}
	if cache.DigestOf(got) != object.Digest {
		t.Fatalf("hit bytes do not match the advertised digest %q", object.Digest)
	}
}

// TestChannelFilterHidesUntrustedObjects is the security rule the whole design
// rests on: under an authoritative policy a hint object is a MISS, and is
// indistinguishable from an absent one.
func TestChannelFilterHidesUntrustedObjects(t *testing.T) {
	dir := shortExchangeDir(t)
	id := strings.Repeat("b", 64)
	server := startServer(t, dir,
		WithChannel(cache.ChannelHint),
		WithSeed("go-build", id, []byte("developer build"), "output-id"),
	)
	client := dial(t, server)

	// The stamp is the server's, not the caller's: the seeded object carries the
	// configured channel.
	if obj, ok := server.Object("go-build", id); !ok || obj.Channel != cache.ChannelHint {
		t.Fatalf("seeded object = %+v, want a hint stamp", obj)
	}

	trustedOnly, err := client.Get(&cache.ObjectGetParams{
		Namespace:      "go-build",
		IDs:            []string{id},
		AcceptChannels: []cache.Channel{cache.ChannelTrusted},
	})
	if err != nil {
		t.Fatalf("trusted get: %v", err)
	}
	if len(trustedOnly.Objects) != 0 {
		t.Fatalf("a hint object was served to a trusted-only caller: %+v", trustedOnly.Objects)
	}

	anyChannel, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: []string{id}})
	if err != nil {
		t.Fatalf("unfiltered get: %v", err)
	}
	if len(anyChannel.Objects) != 1 {
		t.Fatalf("unfiltered get = %+v, want the hint object", anyChannel.Objects)
	}
}

// TestNamespacesAreDisjoint pins that ids never collide across namespaces.
func TestNamespacesAreDisjoint(t *testing.T) {
	dir := shortExchangeDir(t)
	id := strings.Repeat("c", 64)
	server := startServer(t, dir, WithSeed("go-build", id, []byte("go object"), ""))
	client := dial(t, server)

	other, err := client.Get(&cache.ObjectGetParams{Namespace: "other-cache", IDs: []string{id}})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(other.Objects) != 0 {
		t.Fatalf("an id leaked across namespaces: %+v", other.Objects)
	}
}

// TestSessionOpsAreRefusedOnTheSocket pins the socket's op boundary: a job
// process cannot drive the session through the socket it was handed.
func TestSessionOpsAreRefusedOnTheSocket(t *testing.T) {
	server := startServer(t, shortExchangeDir(t))
	client := dial(t, server)

	for _, op := range []cache.ProviderOp{cache.OpInitialize, cache.OpAuthenticate, cache.OpRestore, cache.OpShutdown, cache.OpSummary} {
		resp, err := client.Call(op, nil)
		if err != nil {
			t.Fatalf("call %s: %v", op, err)
		}
		if resp.OK {
			t.Fatalf("op %s was served on the object-cache socket", op)
		}
		if resp.Error == nil || resp.Error.Code != "unsupported-op" {
			t.Fatalf("op %s error = %+v, want an unsupported-op refusal", op, resp.Error)
		}
	}
	// The session op did not kill the connection: object ops still work.
	if _, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: nil}); err != nil {
		t.Fatalf("get after a refused op: %v", err)
	}
}

// TestConcurrentConnections pins the contract's "many concurrent connections"
// clause with real parallel clients sharing one store.
func TestConcurrentConnections(t *testing.T) {
	server := startServer(t, shortExchangeDir(t))

	const clients = 8
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := Dial(server.Path())
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = client.Close() }()
			content := []byte(strings.Repeat("x", i+1))
			digest, err := StageBlob(server.ExchangeDir(), content)
			if err != nil {
				errs <- err
				return
			}
			id := strings.Repeat("d", 60) + string(rune('0'+i))
			if _, err := client.Put(&cache.ObjectPutParams{
				Namespace: "go-build",
				Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content))}},
			}); err != nil {
				errs <- err
				return
			}
			got, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: []string{id}})
			if err != nil {
				errs <- err
				return
			}
			if len(got.Objects) != 1 {
				errs <- fmt.Errorf("unexpected miss for %s", id)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent client: %v", err)
	}
	if gets, puts := server.Stats(); gets != clients || puts != clients {
		t.Fatalf("stats = %d gets / %d puts, want %d each", gets, puts, clients)
	}
}

// TestSocketLivesUnderTheExchangeDir pins the placement rule a real provider
// follows, and TestLongExchangeDirFallsBackToAShortPath covers the escape hatch
// that keeps a deep temporary directory from making a socket unbindable.
func TestSocketLivesUnderTheExchangeDir(t *testing.T) {
	dir := shortExchangeDir(t)
	server := startServer(t, dir)
	if got := filepath.Dir(server.Path()); got != dir {
		t.Fatalf("socket dir = %q, want the exchange dir %q", got, dir)
	}
	info, err := os.Stat(server.Path())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, want a socket", info.Mode())
	}
}

func TestLongExchangeDirFallsBackToAShortPath(t *testing.T) {
	base := shortExchangeDir(t)
	deep := filepath.Join(base, strings.Repeat("deep-directory-name/", 8))
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("create deep exchange dir: %v", err)
	}
	server := startServer(t, deep)
	if len(server.Path()) > maxSocketPath {
		t.Fatalf("socket path %q is %d bytes, over the platform limit", server.Path(), len(server.Path()))
	}
	// The derivation a client performs still holds: bytes travel through the
	// socket's own directory, whichever one that turned out to be.
	if got := server.ExchangeDir(); got != filepath.Dir(server.Path()) {
		t.Fatalf("exchange dir = %q, want the socket's parent %q", got, filepath.Dir(server.Path()))
	}
	client := dial(t, server)
	content := []byte("compiled over a fallback socket")
	digest, err := StageBlob(server.ExchangeDir(), content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	id := strings.Repeat("a", 8)
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: "go-build",
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content))}},
	})
	if err != nil || put.Accepted != 1 {
		t.Fatalf("put over the fallback socket = %+v, err %v", put, err)
	}
}

// TestPutRejectsUnstagedOrMismatchedBytes pins the content-addressing rule: the
// server stores only bytes it could read AND verify, so a caller cannot make an
// id resolve to content that does not hash to the digest it announced.
func TestPutRejectsUnstagedOrMismatchedBytes(t *testing.T) {
	server := startServer(t, shortExchangeDir(t))
	client := dial(t, server)

	id := strings.Repeat("e", 64)
	unstaged := cache.DigestOf([]byte("never staged"))
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: "go-build",
		Objects:   []cache.ObjectPut{{ID: id, Digest: unstaged, Size: 12}},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Accepted != 0 {
		t.Fatalf("accepted = %d, want 0 for bytes that were never staged", put.Accepted)
	}

	// Stage one content, announce another digest: the announced address is what
	// the server verifies against, so the object is dropped.
	staged := []byte("real bytes")
	if _, err := StageBlob(server.ExchangeDir(), staged); err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	lying := cache.DigestOf([]byte("other bytes"))
	path, _ := cache.BlobExchangePath(server.ExchangeDir(), lying)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create blob dir: %v", err)
	}
	if err := os.WriteFile(path, staged, 0o644); err != nil {
		t.Fatalf("write mismatched blob: %v", err)
	}
	put, err = client.Put(&cache.ObjectPutParams{
		Namespace: "go-build",
		Objects:   []cache.ObjectPut{{ID: id, Digest: lying, Size: int64(len(staged))}},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Accepted != 0 {
		t.Fatalf("accepted = %d, want 0 for bytes that do not match their digest", put.Accepted)
	}
	if _, ok := server.Object("go-build", id); ok {
		t.Fatal("an unverifiable object was stored")
	}
}

// TestCloseIsIdempotent keeps the handle safe for a deferred cleanup that a
// test may also call explicitly.
func TestCloseIsIdempotent(t *testing.T) {
	server := startServer(t, shortExchangeDir(t))
	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestStartRequiresAnExchangeDir keeps the handle from starting half-configured.
func TestStartRequiresAnExchangeDir(t *testing.T) {
	if _, err := Start(""); err == nil {
		t.Fatal("Start(\"\") should fail")
	}
}
