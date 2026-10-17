package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// remoteCacheOptions hands the hosted run's credential to the cache provider
// over its RPC. Without a run credential it is empty, and the provider session
// is byte-identical to one without the option.
func remoteCacheOptions() []remotecache.RemoteCacheOption {
	if bearer, ok := runcredential.Current(); ok {
		return []remotecache.RemoteCacheOption{remotecache.WithCacheRunCredential(bearer)}
	}
	return nil
}

// launchCacheProvider starts the cache provider the way the CLI starts every
// provider it owns (jobs.PrepareProviderLaunch). The engine hands it to the
// remote cache loader, so the remote cache client never imports the runner.
func launchCacheProvider(ctx context.Context, workspaceRoot string, ext *extension.ExtensionDescription, command string) (cacheprovider.LaunchSpec, error) {
	launch, err := jobs.PrepareProviderLaunch(ctx, workspaceRoot, ext, command)
	return cacheprovider.LaunchSpec(launch), err
}

// schedulerRemote is the remote cache a run's scheduler reads: remote, or a
// nil interface when remote is nil. A nil *RemoteCache stored in the
// interface is not nil, so the scheduler would treat a local-only run as one
// with a remote cache whose every method must accept a nil receiver.
func schedulerRemote(remote *remotecache.RemoteCache) cachekey.RemoteCache {
	if remote == nil {
		return nil
	}
	return remote
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
	remote  *remotecache.RemoteCache
	err     error
}

// Start starts the provider of the remote cache req reads, once, and returns
// that remote cache, or the refusal to start its provider. A request that
// reads no hosted remote cache (readsHostedRemoteCache) gets nil and leaves
// the start to a later request: a workspace lifecycle job runs under
// --no-cache, and a watch iteration under the trust policy "none". Every
// later request gets what the first start gave, a refusal and nothing
// included: the provider never starts twice.
func (h *HostedRemoteCache) Start(ctx context.Context, req *Request) (*remotecache.RemoteCache, error) {
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
// and prints the notice of a configured cache that cannot serve the run. It
// refuses, before any hook, a run whose CI configuration needs a provider that
// only a path extension declares (requireHostedProviders). A
// workspace that does not load starts nothing here: the run reports it after
// the hooks. A provider that fails to start leaves the run building locally,
// as a lazy start does. The error is the refusal to start the provider:
// repository code already ran in this process, or the provider is not its
// extension's native runtime (runcredential.RequireNativeHolder).
//
// It resolves the workspace and its extensions as the hooks will find them;
// the run resolves them again after the hooks, and reads the remote cache
// through the provider started here (Request.hostedRemote).
func startHostedRemoteCache(ctx context.Context, req *Request) (*remotecache.RemoteCache, error) {
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
	if err := requireHostedProviders(req, discovered); err != nil {
		return nil, err
	}
	setupStart := time.Now()
	remote, notice := remotecache.LoadRemoteCache(ctx, req.WorkspaceRoot, discovered.Extensions, discovered,
		trust, launchCacheProvider, remoteCacheOptions()...)
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
// (remotecache.RemoteCache.StartProvider): the run fails and never builds locally in
// its place. It returns nil without a run credential, under --no-cache, and
// when cache is nil.
func hostedRunRemoteCache(ctx context.Context, req *Request, ws *workspace.Workspace, discovered *extension.DiscoveryResult, cache *store.CacheManager) (*remotecache.RemoteCache, error) {
	if !runcredential.Hosted() || req.Global.NoCache || cache == nil {
		return nil, nil
	}
	if req.hostedRemote != nil {
		return req.hostedRemote, nil
	}
	remote, _ := remotecache.LoadRemoteCache(ctx, req.WorkspaceRoot, discovered.Extensions, discovered,
		store.CacheTrust(req.Global.CacheTrust), launchCacheProvider, remoteCacheOptions()...)
	if err := remote.StartProvider(ctx, ws, cache); err != nil {
		remote.Close()
		return nil, err
	}
	return remote, nil
}

// requireHostedProviders fails a hosted run whose CI configuration needs a
// provider capability that only the workspace's path extensions declare, since
// a hosted run removes it from them
// (extension.DiscoveryResult.OnlyFromPathExtensions). The configuration needs
// the release-set provider when the workspace's CI document declares a rule
// that publishes to at least one channel, and the cache provider when req
// reads the remote cache (readsHostedRemoteCache) and the workspace configures
// one (remotecache.Configured). A CI document that cannot be read or parsed
// declares no rule: only a run that publishes reads it as a failure. The error
// is an invalid configuration that names, for each capability, what needs it
// and each path extension that declares it, then the fix. A run without a run
// credential and a workspace lifecycle run (Request.WorkspaceLifecycle) are
// never refused.
func requireHostedProviders(req *Request, discovered *extension.DiscoveryResult) error {
	if !runcredential.Hosted() || req.WorkspaceLifecycle {
		return nil
	}
	type need struct{ command, neededBy string }
	var needs []need
	if data, err := os.ReadFile(filepath.Join(req.WorkspaceRoot, ciproto.Filename)); err == nil {
		if document, err := ciproto.Parse(data); err == nil && slices.ContainsFunc(document.Rules, func(rule ciproto.Rule) bool {
			return !rule.Publish.Disabled && len(rule.Publish.Channels) > 0
		}) {
			needs = append(needs, need{distribution.ProviderCommandName, ciproto.Filename + " declares a rule that publishes"})
		}
	}
	if readsHostedRemoteCache(req) && remotecache.Configured(req.WorkspaceRoot) {
		needs = append(needs, need{cache.ProviderCommandName, "the workspace configures a remote cache"})
	}
	var unmet, names []string
	for _, need := range needs {
		removed := discovered.OnlyFromPathExtensions(need.command)
		if len(removed) == 0 {
			continue
		}
		causes := make([]string, len(removed))
		for i, capability := range removed {
			causes[i] = capability.Reason().Error()
			if !slices.Contains(names, capability.Extension) {
				names = append(names, capability.Extension)
			}
		}
		unmet = append(unmet, fmt.Sprintf("%s, which needs the %s capability, and no extension of this hosted run serves it: %s",
			need.neededBy, need.command, strings.Join(causes, "; ")))
	}
	if len(unmet) == 0 {
		return nil
	}
	return protocolcli.InvalidConfigf("%s; in the extensions of putnami.workspace.json, declare %s by name instead of by path, "+
		"pin it from the registry in the workspace lock and install it",
		strings.Join(unmet, "; "), strings.Join(names, ", "))
}
