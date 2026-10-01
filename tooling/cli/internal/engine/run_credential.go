package engine

import (
	"context"
	"os"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// remoteCacheOptions hands the hosted run's credential to the cache provider
// over its RPC. Without a run credential it is empty, and the provider session
// is byte-identical to one without the option.
func remoteCacheOptions() []jobs.RemoteCacheOption {
	if bearer, ok := runcredential.Current(); ok {
		return []jobs.RemoteCacheOption{jobs.WithCacheRunCredential(bearer)}
	}
	return nil
}

// HostedRemoteCache is the remote cache of a hosted invocation, whose
// provider starts once: at the first Start that reads the remote cache. The
// provider receives the run credential, and a hosted run hands its credential
// to no process that starts after repository code (runcredential.StartHolder).
// So the first-use bootstrap starts it after the implicit install's
// credentialed workspace-fetch and before the install's first repository code
// (lifecycle.LifecycleEnv.BeforeRepositoryCode), and the run that follows
// reads the remote cache through the same provider
// (Request.HostedRemoteCache). The zero value is ready to use. Whoever creates
// it closes it, after the last run that reads it.
type HostedRemoteCache struct {
	mu      sync.Mutex
	started bool
	remote  *jobs.RemoteCache
	err     error
}

// Start starts the provider of the remote cache req reads, once, and returns
// that remote cache, or the refusal to start its provider. A request that
// reads no hosted remote cache (readsHostedRemoteCache) gets nil and leaves
// the start to a later request: a workspace lifecycle job runs under
// --no-cache, and a watch iteration under the trust policy "none". Every
// later request gets what the first start gave, a refusal and nothing
// included: the provider never starts twice.
func (h *HostedRemoteCache) Start(ctx context.Context, req *Request) (*jobs.RemoteCache, error) {
	if !readsHostedRemoteCache(req) {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started {
		h.started = true
		h.remote, h.err = startHostedRemoteCache(ctx, req)
	}
	return h.remote, h.err
}

// Close stops the provider Start started, if any. A later Start gets no
// remote cache, so a run then loads it again and is refused
// (hostedRunRemoteCache). A nil HostedRemoteCache does nothing.
func (h *HostedRemoteCache) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = true
	h.remote.Close()
	h.remote = nil
}

// readsHostedRemoteCache reports whether req is a hosted run that reads the
// remote cache: it holds a run credential, runs with the build cache on, and
// trusts the remote cache.
func readsHostedRemoteCache(req *Request) bool {
	return runcredential.Hosted() && !req.Global.NoCache && store.CacheTrust(req.Global.CacheTrust) != store.CacheTrustNone
}

// startHostedRemoteCache loads the remote cache of a hosted run and starts its
// provider, before the run's first hook. The provider receives the run
// credential, and a hosted run hands its credential to no process that starts
// after repository code (runcredential.StartHolder), so a provider started at
// the first operation that needs it, after the hooks, would be refused.
//
// It returns nil without a run credential, under --no-cache, under the trust
// policy "none" (a watch iteration), and when no remote cache is configured,
// and prints the notice of a configured cache that cannot serve the run. A
// workspace that does not load starts nothing here: the run reports it after
// the hooks. A provider that fails to start leaves the run building locally,
// as a lazy start does. The error is the refusal to start the provider:
// repository code already ran in this process, or the provider is not its
// extension's native runtime (runcredential.RequireNativeHolder).
//
// It resolves the workspace and its extensions as the hooks will find them;
// the run resolves them again after the hooks, and reads the remote cache
// through the provider started here (Request.hostedRemote).
func startHostedRemoteCache(ctx context.Context, req *Request) (*jobs.RemoteCache, error) {
	if !readsHostedRemoteCache(req) {
		return nil, nil
	}
	trust := store.CacheTrust(req.Global.CacheTrust)
	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		return nil, nil
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(req.WorkspaceRoot, req.Config, projectPaths)
	if err != nil {
		return nil, nil
	}
	setupStart := time.Now()
	remote, notice := jobs.LoadRemoteCache(ctx, req.WorkspaceRoot, discovered.Extensions, discovered.Skipped,
		trust, remoteCacheOptions()...)
	if notice != "" {
		iox.Fprintln(os.Stderr, notice)
	}
	var storeRootOverride string
	if req.CacheVerification != nil {
		storeRootOverride = req.CacheVerification.StoreRoot
	}
	if err := remote.StartProvider(ctx, ws, jobs.NewRunCacheManager(req.WorkspaceRoot, storeRootOverride)); err != nil {
		remote.Close()
		return nil, err
	}
	remote.RecordSetup(time.Since(setupStart))
	return remote, nil
}

// hostedRunRemoteCache is the remote cache a hosted run executes with: the one
// startHostedRemoteCache started before the hooks. When that found none, the
// remote cache is loaded again, silently, because startHostedRemoteCache
// already printed its notice. A cache configured only now has a provider that
// would start after repository code, and the error is its refusal
// (jobs.RemoteCache.StartProvider): the run fails and never builds locally in
// its place. It returns nil without a run credential, under --no-cache, and
// when cache is nil.
func hostedRunRemoteCache(ctx context.Context, req *Request, ws *workspace.Workspace, discovered *extension.DiscoveryResult, cache *store.CacheManager) (*jobs.RemoteCache, error) {
	if !runcredential.Hosted() || req.Global.NoCache || cache == nil {
		return nil, nil
	}
	if req.hostedRemote != nil {
		return req.hostedRemote, nil
	}
	remote, _ := jobs.LoadRemoteCache(ctx, req.WorkspaceRoot, discovered.Extensions, discovered.Skipped,
		store.CacheTrust(req.Global.CacheTrust), remoteCacheOptions()...)
	if err := remote.StartProvider(ctx, ws, cache); err != nil {
		remote.Close()
		return nil, err
	}
	return remote, nil
}
