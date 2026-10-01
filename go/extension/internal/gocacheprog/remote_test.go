package gocacheprog

import (
	"bytes"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
	"go.putnami.dev/protocol/features/spectest"
)

// startObjectCache runs the shared fake provider socket from protocols/cache.
//
// It is the SAME server the CLI and a provider implementation test against, on
// a real Unix socket with real JSONL framing and real bytes handed over through
// the blob-exchange directory. Testing this helper against a private stub would
// prove agreement with the stub, not with the contract.
func startObjectCache(t *testing.T, opts ...objectcachetest.Option) *objectcachetest.Server {
	t.Helper()
	server, err := objectcachetest.Start(filepath.Join(t.TempDir(), "exchange"), opts...)
	if err != nil {
		t.Fatalf("start object cache: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// remoteEnv is the job environment core exports for a provider-backed run.
func remoteEnv(t *testing.T, server *objectcachetest.Server, trust string) map[string]string {
	t.Helper()
	return map[string]string{
		"PUTNAMI_GO_CACHE_DIR":     t.TempDir(),
		cache.ObjectCacheSocketEnv: server.Path(),
		cache.CacheTrustEnv:        trust,
		"PUTNAMI_DEBUG":            "1",
	}
}

// TestRemoteHitIsIngestedIntoTheLocalStore is the cold-machine path: nothing is
// in the local directory, the object cache has the object, and the go command
// gets a hit whose DiskPath holds the right bytes.
func TestRemoteHitIsIngestedIntoTheLocalStore(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "a-cold-machine-is-served-by-the-provider-and-keeps-what-it-received")
	body := []byte("archive built on another machine")
	action := actionID(0x11)
	server := startObjectCache(t, objectcachetest.WithSeed(
		"go-build", hex.EncodeToString(action), body, sumOf(body),
	))
	env := remoteEnv(t, server, "any")

	h := startHelper(t, env)
	got := h.get(action)
	if got.Miss {
		t.Fatal("a seeded object was not served from the object cache")
	}
	stored, err := os.ReadFile(got.DiskPath)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("ingested content = %q (%v), want %q", stored, err, body)
	}
	if hex.EncodeToString(got.OutputID) != sumOf(body) {
		t.Fatalf("ingested OutputID = %x, want %s", got.OutputID, sumOf(body))
	}
	// A served object lands where a go command running without the helper
	// looks for it too, so it is not fetched or compiled a second time.
	sharedPath := filepath.Join(toolchain.GoBuildCacheDir(env["PUTNAMI_GO_CACHE_DIR"]), sumOf(body)[:2], sumOf(body)+dataSuffix)
	if got.DiskPath != sharedPath {
		t.Fatalf("ingested DiskPath = %q, want the go command's own path %q", got.DiskPath, sharedPath)
	}
	h.close()

	// The ingest is what makes the SECOND lookup local: a helper started with
	// the socket removed must still find the object.
	local := startHelper(t, map[string]string{"PUTNAMI_GO_CACHE_DIR": env["PUTNAMI_GO_CACHE_DIR"]})
	again := local.get(action)
	if again.Miss {
		t.Fatal("an ingested object was not stored locally")
	}
	local.close()

	gets, _ := server.Stats()
	if gets == 0 {
		t.Fatal("the object cache served no get")
	}
}

// TestRemoteObjectWithMismatchedMetadataIsAMiss refuses the one thing the go
// command cannot check itself. The digest addresses the bytes and Meta carries
// the output id; a provider whose two answers disagree is not serving this
// action's output, and a helper that trusted it would have the go command link
// content it never produced.
func TestRemoteObjectWithMismatchedMetadataIsAMiss(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "served-bytes-are-verified-against-the-output-id-they-were-announced-under")
	body := []byte("bytes that do not match their metadata")
	action := actionID(0x12)
	server := startObjectCache(t, objectcachetest.WithSeed(
		// The metadata announces a different output id than the content hashes
		// to, which is exactly the case the digest check catches.
		"go-build", hex.EncodeToString(action), body, strings.Repeat("9", idHexLength),
	))
	h := startHelper(t, remoteEnv(t, server, "any"))
	got := h.get(action)
	if !got.Miss {
		t.Fatalf("an object whose metadata contradicts its digest was served: %+v", got)
	}
	h.close()
	// A refused object was never used, so it must not be counted as served.
	if !strings.Contains(h.stderr.String(), "object cache served 0 objects") {
		t.Fatalf("stderr = %q, want a refused object counted as not served", h.stderr.String())
	}
}

// TestTrustPolicyFiltersTheChannel is the security half of the object cache: a
// run under the authoritative policy reads only what the provider stamped
// trusted, so a developer-produced object can never reach it. The SAME object
// and the SAME server answer differently for the two policies.
func TestTrustPolicyFiltersTheChannel(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "the-run-trust-policy-decides-which-channels-a-lookup-accepts")
	body := []byte("object a developer credential produced")
	action := actionID(0x13)
	seed := objectcachetest.WithSeed("go-build", hex.EncodeToString(action), body, sumOf(body))

	authoritative := startObjectCache(t, objectcachetest.WithChannel(cache.ChannelHint), seed)
	strict := startHelper(t, remoteEnv(t, authoritative, "ci"))
	if got := strict.get(action); !got.Miss {
		t.Fatalf("a hint-stamped object reached a ci run: %+v", got)
	}
	strict.close()

	permissive := startObjectCache(t, objectcachetest.WithChannel(cache.ChannelHint), seed)
	relaxed := startHelper(t, remoteEnv(t, permissive, "any"))
	if got := relaxed.get(action); got.Miss {
		t.Fatal("a hint-stamped object was refused by a run that accepts any channel")
	}
	relaxed.close()
}

// TestAcceptChannelsFailsClosed pins the derivation itself, including the
// values core never exports: only the explicit "any" opens the filter.
func TestAcceptChannelsFailsClosed(t *testing.T) {
	if got := acceptChannels("any"); len(got) != 0 {
		t.Fatalf("acceptChannels(any) = %v, want an empty filter", got)
	}
	for _, trust := range []string{"ci", "", "none", "ANY", "unknown"} {
		got := acceptChannels(trust)
		if len(got) != 1 || got[0] != cache.ChannelTrusted {
			t.Errorf("acceptChannels(%q) = %v, want [trusted]", trust, got)
		}
	}
}

// TestPutsReachTheProviderByCloseTime pins the asynchronous upload and the
// drain that closes it: the go command is never blocked on a put, and nothing
// queued is lost when the helper exits.
func TestPutsReachTheProviderByCloseTime(t *testing.T) {
	server := startObjectCache(t)
	h := startHelper(t, remoteEnv(t, server, "any"))

	const count = 5
	bodies := map[string][]byte{}
	for i := 0; i < count; i++ {
		action := actionID(byte(0x20 + i))
		body := []byte(strings.Repeat("object ", i+1))
		bodies[hex.EncodeToString(action)] = body
		if put := h.put(action, body); put.Err != "" {
			t.Fatalf("put %d failed: %s", i, put.Err)
		}
	}
	h.close()

	_, puts := server.Stats()
	if puts == 0 {
		t.Fatal("no object-put reached the provider")
	}
	if puts > count {
		t.Fatalf("%d object-put requests for %d objects; they are not batched", puts, count)
	}
	for id, body := range bodies {
		object, ok := server.Object("go-build", id)
		if !ok {
			t.Fatalf("object %s never reached the provider", id)
		}
		if !bytes.Equal(object.Bytes, body) {
			t.Fatalf("object %s stored %q, want %q", id, object.Bytes, body)
		}
		if object.Meta != sumOf(body) {
			t.Fatalf("object %s stored meta %q, want the output id %q", id, object.Meta, sumOf(body))
		}
	}
}

// TestAPutIsNotBlockedByTheProvider pins the ordering the compiler depends on:
// the answer carries a usable DiskPath before anything is offered to the
// provider, so a slow upload can never slow a build down.
func TestAPutIsNotBlockedByTheProvider(t *testing.T) {
	server := startObjectCache(t)
	h := startHelper(t, remoteEnv(t, server, "any"))
	body := []byte("answered before it is uploaded")
	put := h.put(actionID(0x30), body)
	if put.Err != "" {
		t.Fatalf("put failed: %s", put.Err)
	}
	stored, err := os.ReadFile(put.DiskPath)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("DiskPath content = %q (%v), want the body", stored, err)
	}
	h.close()
}

// TestAnUnreachableSocketDegradesToLocalOnly is the failure mode that matters
// most: a provider that is not there must cost the build nothing but a line on
// stderr, and the local cache must keep working for the whole process.
func TestAnUnreachableSocketDegradesToLocalOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "a-provider-failure-degrades-to-the-local-cache-for-the-rest-of-the-process")
	env := map[string]string{
		"PUTNAMI_GO_CACHE_DIR":     t.TempDir(),
		cache.ObjectCacheSocketEnv: filepath.Join(t.TempDir(), "not-a-socket"),
		cache.CacheTrustEnv:        "any",
		"PUTNAMI_DEBUG":            "1",
	}
	h := startHelper(t, env)
	action := actionID(0x40)
	body := []byte("stored while the provider is gone")

	if got := h.get(action); !got.Miss {
		t.Fatalf("an unreachable provider produced a hit: %+v", got)
	}
	if put := h.put(action, body); put.Err != "" {
		t.Fatalf("put failed with an unreachable provider: %s", put.Err)
	}
	got := h.get(action)
	if got.Miss {
		t.Fatal("the local cache stopped working after a socket failure")
	}
	h.close()

	if !strings.Contains(h.stderr.String(), "local cache only") {
		t.Fatalf("stderr = %q, want one line reporting the degradation", h.stderr.String())
	}
	if count := strings.Count(h.stderr.String(), "local cache only"); count != 1 {
		t.Fatalf("the degradation was reported %d times, want once", count)
	}
}

// TestTheDegradationIsSilentWithoutDebug keeps the report off the output of
// every job: the helper is a child of every go invocation, so an unconditional
// line would appear dozens of times per build for a condition it recovers from.
func TestTheDegradationIsSilentWithoutDebug(t *testing.T) {
	h := startHelper(t, map[string]string{
		"PUTNAMI_GO_CACHE_DIR":     t.TempDir(),
		cache.ObjectCacheSocketEnv: filepath.Join(t.TempDir(), "not-a-socket"),
		cache.CacheTrustEnv:        "any",
	})
	h.get(actionID(0x41))
	h.close()
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want silence without PUTNAMI_DEBUG", h.stderr.String())
	}
}

// TestAProviderThatHangsUpMidRunStopsBeingAsked pins the "disable once" rule.
// A provider that dies in the middle of a build must cost the rest of it
// nothing: the first failure turns the remote half off instead of charging
// every later lookup another failed exchange.
//
// The peer here is a raw listener rather than the shared fake, because what is
// under test is a provider that HANGS UP — a behavior a conformant server does
// not have and the fake therefore cannot produce.
func TestAProviderThatHangsUpMidRunStopsBeingAsked(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgc-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "objects.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan int, 8)
	go func() {
		count := 0
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count++
			accepted <- count
			// Read the request, then hang up without answering it.
			buffer := make([]byte, 1)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()

	h := startHelper(t, map[string]string{
		"PUTNAMI_GO_CACHE_DIR":     t.TempDir(),
		cache.ObjectCacheSocketEnv: socket,
		cache.CacheTrustEnv:        "any",
		"PUTNAMI_DEBUG":            "1",
	})
	for i := 0; i < 4; i++ {
		if got := h.get(actionID(byte(0x60 + i))); !got.Miss {
			t.Fatalf("lookup %d against a provider that hangs up did not miss", i)
		}
	}
	h.close()

	close(accepted)
	connections := 0
	for range accepted {
		connections++
	}
	if connections != 1 {
		t.Fatalf("the helper opened %d connections; the first failure must disable the remote half", connections)
	}
	if count := strings.Count(h.stderr.String(), "local cache only"); count != 1 {
		t.Fatalf("the degradation was reported %d times, want once", count)
	}
}

// TestObjectsLargerThanTheCapStayLocal pins the one policy the upload path
// carries: bytes travel by staging a COPY in the exchange directory, so an
// enormous object costs a full write before a provider ever sees it.
func TestObjectsLargerThanTheCapStayLocal(t *testing.T) {
	server := startObjectCache(t)
	client := openRemote(func(key string) string {
		switch key {
		case cache.ObjectCacheSocketEnv:
			return server.Path()
		case cache.CacheTrustEnv:
			return "any"
		default:
			return ""
		}
	}, nil)
	if client == nil {
		t.Fatal("openRemote returned no client for a live socket")
	}
	client.queuePut(pendingPut{
		id:       strings.Repeat("7", idHexLength),
		outputID: strings.Repeat("8", idHexLength),
		size:     maxRemoteObjectBytes + 1,
		source:   filepath.Join(t.TempDir(), "absent"),
	})
	client.close()
	if _, puts := server.Stats(); puts != 0 {
		t.Fatalf("%d object-put requests for an oversized object, want none", puts)
	}
}

// TestStageBlobRefusesASourceOfTheWrongSize pins the guard on the upload
// source. The source is a body in GOCACHE, which a go command running without
// the helper also writes, and that go command truncates a data file it failed
// to finish. A short source must not be staged: the exchange path is content
// addressed and stageBlob never rewrites it, so a short blob there would stay.
func TestStageBlobRefusesASourceOfTheWrongSize(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	body := []byte("a body another writer truncated")
	if err := os.WriteFile(source, body[:4], 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	destination := filepath.Join(dir, "exchange", "cd", "cdef01")
	if err := stageBlob(destination, source, int64(len(body))); err == nil {
		t.Fatal("stageBlob staged a source shorter than the object's announced size")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("a refused source left a blob at the exchange address: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatalf("read exchange dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused source left %d files in the exchange directory", len(entries))
	}
}

// TestStageBlobIsAtomicAndIdempotent covers the handoff directory: a provider
// reading a content-addressed path must never see a partial blob, and bytes
// already there are already the right bytes.
func TestStageBlobIsAtomicAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	body := []byte("staged body")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	destination := filepath.Join(dir, "exchange", "ab", "abcdef")
	if err := stageBlob(destination, source, int64(len(body))); err != nil {
		t.Fatalf("stageBlob: %v", err)
	}
	staged, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(staged, body) {
		t.Fatalf("staged %q (%v), want %q", staged, err, body)
	}
	before, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := stageBlob(destination, filepath.Join(dir, "absent"), int64(len(body))); err != nil {
		t.Fatalf("re-staging an existing blob failed: %v", err)
	}
	after, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("re-staging rewrote a blob that was already at its content address")
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatalf("read exchange dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("exchange directory holds %d files, want only the blob", len(entries))
	}
}

// TestTheExchangeIsReportedUnderDebug pins the observable that makes a shared
// build cache diagnosable at all: a run served entirely from the provider and a
// run that recompiled everything are otherwise indistinguishable — same output,
// same local cache, same exit code.
func TestTheExchangeIsReportedUnderDebug(t *testing.T) {
	body := []byte("object that is both served and offered")
	served := actionID(0x70)
	server := startObjectCache(t, objectcachetest.WithSeed(
		"go-build", hex.EncodeToString(served), body, sumOf(body),
	))
	h := startHelper(t, remoteEnv(t, server, "any"))
	if got := h.get(served); got.Miss {
		t.Fatal("the seeded object was not served")
	}
	h.put(actionID(0x71), []byte("object this run produced"))
	h.close()

	if !strings.Contains(h.stderr.String(), "object cache served 1 objects, offered 1") {
		t.Fatalf("stderr = %q, want the exchange summary", h.stderr.String())
	}
}

// TestTheExchangeIsNotReportedWithoutAProvider keeps the summary off a run that
// has no object cache at all: there is nothing to report, and the line would
// otherwise appear in the output of every local build.
func TestTheExchangeIsNotReportedWithoutAProvider(t *testing.T) {
	h := startHelper(t, map[string]string{
		"PUTNAMI_GO_CACHE_DIR": t.TempDir(),
		"PUTNAMI_DEBUG":        "1",
	})
	h.put(actionID(0x72), []byte("local only"))
	h.close()
	if strings.Contains(h.stderr.String(), "object cache") {
		t.Fatalf("stderr = %q, want no object-cache report", h.stderr.String())
	}
}

// TestAStalledProviderIsAbandonedWithinTheDeadline is the failure mode a dead
// socket does not cover: a provider that ACCEPTS the connection and then never
// answers. The go command puts no timeout on a lookup, so without a deadline
// here that action — and the build — would wait forever. The lookup must come
// back a miss within the deadline, the helper must open no second connection,
// and every later lookup must miss immediately.
func TestAStalledProviderIsAbandonedWithinTheDeadline(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "a-provider-failure-degrades-to-the-local-cache-for-the-rest-of-the-process")
	previous := getTimeout
	getTimeout = 200 * time.Millisecond
	t.Cleanup(func() { getTimeout = previous })

	dir, err := os.MkdirTemp("", "pgc-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "objects.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	release := make(chan struct{})
	accepted := make(chan struct{}, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() {
				// Read the request and then stall: no answer, no hang-up.
				buffer := make([]byte, 4096)
				_, _ = conn.Read(buffer)
				<-release
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() { close(release) })

	h := startHelper(t, map[string]string{
		"PUTNAMI_GO_CACHE_DIR":     t.TempDir(),
		cache.ObjectCacheSocketEnv: socket,
		cache.CacheTrustEnv:        "any",
		"PUTNAMI_DEBUG":            "1",
	})
	start := time.Now()
	if got := h.get(actionID(0x80)); !got.Miss {
		t.Fatalf("a lookup against a stalled provider did not miss: %+v", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the lookup took %s; the deadline did not bound it", elapsed)
	}
	start = time.Now()
	if got := h.get(actionID(0x81)); !got.Miss {
		t.Fatalf("a lookup after the stall did not miss: %+v", got)
	}
	if elapsed := time.Since(start); elapsed > getTimeout {
		t.Fatalf("a lookup after the stall took %s; the remote half was not disabled", elapsed)
	}
	h.close()

	if len(accepted) != 1 {
		t.Fatalf("the helper opened %d connections; a stalled provider must not be retried", len(accepted))
	}
	if !strings.Contains(h.stderr.String(), "timed out") {
		t.Fatalf("stderr = %q, want the timeout reported", h.stderr.String())
	}
}

// TestParallelLookupsShareOneObjectGet pins the batch shape: the go command
// issues its lookups in parallel, and the misses of one burst must reach the
// provider as one object-get, not one request per action.
func TestParallelLookupsShareOneObjectGet(t *testing.T) {
	const lookups = 32
	seeds := make([]objectcachetest.Option, 0, lookups/2)
	ids := make([]string, lookups)
	for i := range ids {
		ids[i] = hex.EncodeToString(actionID(byte(0x90 + i)))
		if i%2 == 0 {
			body := []byte("object " + ids[i])
			seeds = append(seeds, objectcachetest.WithSeed("go-build", ids[i], body, sumOf(body)))
		}
	}
	server := startObjectCache(t, seeds...)
	r := openRemote(func(key string) string {
		return map[string]string{
			cache.ObjectCacheSocketEnv: server.Path(),
			cache.CacheTrustEnv:        "any",
		}[key]
	}, nil)
	t.Cleanup(r.close)

	var wg sync.WaitGroup
	hits := make([]bool, lookups)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, hits[i] = r.get(ids[i])
		}(i)
	}
	wg.Wait()
	for i, hit := range hits {
		if hit != (i%2 == 0) {
			t.Fatalf("lookup %d: hit=%t, want %t", i, hit, i%2 == 0)
		}
	}
	if gets, _ := server.Stats(); gets > lookups/4 {
		t.Fatalf("%d parallel lookups cost %d object-get requests; they must be batched", lookups, gets)
	}
}
