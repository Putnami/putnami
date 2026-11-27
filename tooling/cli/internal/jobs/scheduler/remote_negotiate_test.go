package scheduler

import (
	"context"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/store"
)

// uncachedProbe is a job with no cache key: it executes here on every run,
// so a negotiation starts the provider for it whatever the keys are.
func uncachedProbe() *ScheduledJob {
	job := declaredJob("package~probe", "probe", &extension.TaskDeclaration{})
	job.JobDef.Cache = false
	return job
}

// negotiateFixture is a plan of a job that always executes here, a producer
// and a check whose key the producer's writes may move, against the shared
// fake provider.
type negotiateFixture struct {
	server  *fakeCacheServer
	f       *captureFixture
	planned []*ScheduledJob
}

func newNegotiateFixture(t *testing.T, notReady bool) *negotiateFixture {
	t.Helper()
	server := newFakeCacheServer()
	server.notReady = notReady
	useSharedFakeProvider(t, server)
	planned := []*ScheduledJob{uncachedProbe(), layersProducer(), layersCheck()}
	f := newCaptureFixture(t, planned...)
	writeFileAt(t, layerPath(f), "layer\n")
	return &negotiateFixture{server: server, f: f, planned: planned}
}

func (n *negotiateFixture) remote(t *testing.T) *remotecache.RemoteCache {
	t.Helper()
	remote, notice := remotecache.LoadRemoteCache(context.Background(), n.f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny, fakeProviderLaunch)
	if remote == nil || !remote.IsProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	return remote
}

// A provider that serves nothing (signed out, disabled) answers initialize
// not ready. The negotiation of a run that starts it anyway, because one of
// its jobs executes here, then computes no key and sends nothing but that
// initialize.
func TestNegotiateComputesNoKeyWhenTheProviderServesNothing(t *testing.T) {
	n := newNegotiateFixture(t, true)
	remote := n.remote(t)
	stats := &cachekey.CacheStats{}
	remote.SetCacheStats(stats)

	remote.Negotiate(t.Context(), n.f.ws, n.planned, nil, nil, n.f.cache, cachekey.CacheBypass{})

	if remote.Keys != nil {
		t.Fatalf("the negotiation computed keys %v for a provider that serves nothing", remote.Keys)
	}
	if snapshot := stats.Snapshot(); snapshot.KeysRequested != 0 {
		t.Fatalf("the negotiation recorded %d keys for a provider that serves nothing", snapshot.KeysRequested)
	}
	for _, op := range providerOps(n.server) {
		if op != cache.OpInitialize && op != cache.OpShutdown {
			t.Fatalf("the provider received %s; a provider that serves nothing receives only initialize", op)
		}
	}
	if len(n.server.prefetched()) != 0 {
		t.Fatalf("the provider was asked to prefetch %v", n.server.prefetched())
	}
}

// providerOps lists the operations the shared fake provider received, in
// arrival order.
func providerOps(server *fakeCacheServer) []cache.ProviderOp {
	var ops []cache.ProviderOp
	for _, line := range server.requestLines() {
		if request, _ := cache.ParseAndValidateProviderRequest([]byte(line)); request != nil {
			ops = append(ops, request.Op)
		}
	}
	return ops
}
