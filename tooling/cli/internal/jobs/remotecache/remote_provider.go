package remotecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

type providerRemote struct {
	wsRoot string
	launch cacheprovider.LaunchSpec
	// mode is the materialization mode the provider resolved at initialize
	// (providerMode), cache.ModeFull until then. It is guarded by mu.
	mode cache.Mode
	// notices receives the one line that says a declared provider failed to
	// start or to answer initialize. Nil is os.Stderr.
	notices io.Writer
	// extensionName names the extension that serves the provider command, or
	// is empty when unknown.
	extensionName string
	// runtime is the runtime executable of that extension, prepared with the
	// launch, or empty when it has none. A hosted run starts the provider only
	// when launch.Command is this file (runcredential.RequireNativeHolder).
	runtime string
	// runCredential is the hosted run's bearer, handed to the provider over
	// its RPC and never through launch; empty without one. A providerRemote is
	// never formatted.
	runCredential string

	mu          sync.Mutex
	session     *cacheprovider.Session
	exchangeDir string
	// exchangeOwner is the flock on exchangeDir's owner.lock that marks the
	// directory live for other runs' sweeps (see remote_exchange.go). It is
	// released only after the directory is deleted.
	exchangeOwner *flock.Lock
	// sweep tracks the stale-sibling sweep a session start launches.
	// closeProvider stops it and waits for it, so it never outlives the cache.
	sweep sync.WaitGroup
	// sweepStop asks that sweep to stop before its next directory. The
	// directory it is deleting finishes; the next run's sweep takes the rest.
	sweepStop atomic.Bool
	// objectCacheSocket is the object-cache socket path this session negotiated,
	// or "" when the provider does not serve one. Jobs receive it through their
	// environment; it never enters a cache key.
	objectCacheSocket string
	ready             bool
	closed            bool
	initErr           error
}

// SpawnProviderSession is swapped in tests to run the fake provider in-process:
// under -race, each spawned subprocess pays ~1s of race-runtime startup, and the
// orchestration under test here never needs real process semantics.
var SpawnProviderSession = cacheprovider.Spawn

// sweepExchangeDirs is the stale-sibling sweep a session start launches. Tests
// swap it to hold a sweep open and prove closeProvider stops it before waiting.
var sweepExchangeDirs = sweepStaleExchangeDirs

func newProviderRemoteCache(wsRoot string, launch cacheprovider.LaunchSpec, trust store.CacheTrust) *RemoteCache {
	if !trust.Valid() || trust == store.CacheTrustNone {
		trust = store.CacheTrustCI
	}
	return &RemoteCache{
		uploadSem: make(chan struct{}, uploadConcurrency),
		stats:     &cachekey.CacheStats{},
		Trust:     trust,
		provider: &providerRemote{
			wsRoot: wsRoot,
			launch: launch,
			mode:   cache.ModeFull,
		},
	}
}

// loadProviderRemoteCache resolves the cache-provider extension for this
// workspace. The core reads no remote cache configuration, so the cache is
// provider-backed whenever an extension declares the provider command: the
// provider's initialize answer says whether it serves the run. It returns
// (nil, "", nil) when no extension declares the command, and (nil, notice,
// nil) when a hosted run removed it from a path extension
// (removedProviderNotice), which the caller says once. A non-nil error is a
// hard resolution failure (two extensions claiming the provider command, or a
// provider runtime that cannot be prepared) the caller turns into a notice too.
//
// Resolution is by the protocol's reserved command name only. An earlier
// cleanup deleted the product name and the extension-version floor that used
// to sit beside it: compatibility is established by the extension contract
// version at discovery and by provider-RPC negotiation at the initialize
// handshake, neither of which needs to know which vendor ships the provider.
func loadProviderRemoteCache(ctx context.Context, wsRoot string, exts []*extension.ExtensionDescription, discovered *extension.DiscoveryResult, trust store.CacheTrust, launcher Launcher) (*RemoteCache, string, error) {
	provider, err := extension.ResolveReservedProvider(exts, cache.ProviderCommandName)
	if err != nil {
		return nil, "", err
	}
	if provider == nil {
		return nil, removedProviderNotice(discovered), nil
	}

	launch, err := providerLaunchSpec(ctx, wsRoot, exts, provider, launcher)
	if err != nil {
		return nil, "", err
	}

	remote := newProviderRemoteCache(wsRoot, launch, trust)
	remote.provider.extensionName = provider.ExtensionName
	remote.provider.runtime = findExtensionByName(exts, provider.ExtensionName).RuntimeExecutable
	return remote, "", nil
}

// removedProviderNotice is the notice of a hosted run that removed the cache
// provider command from every path extension that declares it, and that no
// other extension serves (DiscoveryResult.OnlyFromPathExtensions). It returns
// "" otherwise, so an extension skipped for any other reason adds no notice.
func removedProviderNotice(discovered *extension.DiscoveryResult) string {
	removed := discovered.OnlyFromPathExtensions(cache.ProviderCommandName)
	if len(removed) == 0 {
		return ""
	}
	causes := make([]string, len(removed))
	for i, capability := range removed {
		causes[i] = capability.Reason().Error()
	}
	return fmt.Sprintf("putnami: no cache provider serves this run (%s); building locally", strings.Join(causes, "; "))
}

// Launcher resolves how to start the provider command of ext: the runtime
// preparation a task gets, the command's templates, toolchains and
// environment. The engine passes the runner's provider launch
// (scheduler.PrepareProviderLaunch), so this package never imports the runner.
type Launcher func(ctx context.Context, workspaceRoot string, ext *extension.ExtensionDescription, command string) (cacheprovider.LaunchSpec, error)

// providerLaunchSpec resolves how to start the cache provider. The provider
// command gets the runtime preparation a task gets (see Launcher), so an error
// also means a runtime that cannot be prepared, or no launcher at all.
func providerLaunchSpec(ctx context.Context, wsRoot string, exts []*extension.ExtensionDescription, provider *extension.ResolvedProvider, launcher Launcher) (cacheprovider.LaunchSpec, error) {
	ext := findExtensionByName(exts, provider.ExtensionName)
	if ext == nil {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("cache provider extension %q was not loaded", provider.ExtensionName)
	}
	job := ext.Jobs[provider.Command]
	if job == nil {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("cache provider command %q was not resolved for %s", provider.Command, provider.ExtensionName)
	}
	if strings.TrimSpace(job.Command) == "" {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("cache provider command %q has no executable", provider.Command)
	}
	if launcher == nil {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("cache provider %s has no launcher", provider.ExtensionName)
	}
	launch, err := launcher(ctx, wsRoot, ext, provider.Command)
	if err != nil {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("prepare cache provider %s: %w", provider.ExtensionName, err)
	}
	return launch, nil
}

func findExtensionByName(exts []*extension.ExtensionDescription, name string) *extension.ExtensionDescription {
	for _, ext := range exts {
		if ext != nil && ext.Name == name {
			return ext
		}
	}
	return nil
}

func (r *RemoteCache) IsProviderBacked() bool {
	return r != nil && r.provider != nil
}

func (r *RemoteCache) ensureProvider(ctx context.Context, ws *workspace.Workspace, cm *store.CacheManager) (*cacheprovider.Session, bool) {
	if !r.IsProviderBacked() {
		return nil, false
	}
	p := r.provider
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, false
	}
	if p.session != nil {
		return p.session, p.ready
	}
	if p.initErr != nil {
		return nil, false
	}

	exchangeParent := filepath.Join(p.wsRoot, ".putnami")
	if cm != nil && cm.StoreRoot() != "" {
		exchangeParent = cm.StoreRoot()
	}
	if err := os.MkdirAll(exchangeParent, 0o755); err != nil {
		p.initErr = err
		slog.Warn("remote cache: create provider exchange parent failed; building locally", "error", err)
		return nil, false
	}
	// Reclaim the directories killed runs left under this parent. The sweep runs
	// beside the build, because the first one after a long leak can take a
	// while; closeProvider stops it between directories and waits for it.
	sweep, sweepNow, sweepGrace := sweepExchangeDirs, time.Now(), exchangeSweepGrace()
	p.sweep.Add(1)
	go func() {
		defer p.sweep.Done()
		if n := sweep(exchangeParent, sweepNow, sweepGrace, p.sweepStop.Load); n > 0 {
			slog.Debug("remote cache: removed stale provider exchange dirs", "count", n, "parent", exchangeParent)
		}
	}()

	exchangeDir, exchangeOwner, err := newExchangeDir(exchangeParent)
	if err != nil {
		p.initErr = err
		slog.Warn("remote cache: create provider exchange dir failed; building locally", "error", err)
		return nil, false
	}
	p.exchangeDir = exchangeDir
	p.exchangeOwner = exchangeOwner

	// The provider receives the run credential of a hosted run, so it starts
	// only before repository code, and only as its extension's native runtime;
	// a refusal stays initErr, which StartProvider returns.
	var sess *cacheprovider.Session
	err = runcredential.StartHolder(p.holder(), func() error {
		if err := runcredential.RequireNativeHolder(p.holder(), p.launch.Command, p.runtime); err != nil {
			return err
		}
		var spawnErr error
		sess, spawnErr = SpawnProviderSession(ctx, p.launch, cacheprovider.WithStderr(os.Stderr), cacheprovider.WithRunCredential(p.runCredential))
		return spawnErr
	})
	if err != nil {
		p.initErr = err
		removeExchangeDir(exchangeDir, exchangeOwner)
		p.exchangeDir, p.exchangeOwner = "", nil
		if !hostedRefusal(err) {
			p.notice(ctx, "failed to start", err)
		}
		return nil, false
	}

	workspaceID := ""
	if ws != nil {
		workspaceID = RunMarkerWorkspaceID(ws)
	}
	// No Mode: the provider resolves it (cache.CapabilityProviderConfiguration,
	// which the session lists) and returns it in the result.
	init, err := sess.Initialize(ctx, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: exchangeDir,
		Workspace:       workspaceID,
	})
	if err == nil && !init.Ready {
		err = errProviderNotReady
	}
	if err != nil {
		p.initErr = err
		_ = sess.Close()
		removeExchangeDir(exchangeDir, exchangeOwner)
		p.exchangeDir, p.exchangeOwner = "", nil
		if errors.Is(err, errProviderNotReady) {
			// The unconfigured default: the provider serves nothing, so its
			// idle process is released now rather than at the end of the run.
			slog.Debug("remote cache: provider is not ready; building locally")
		} else {
			p.notice(ctx, "failed its initialize handshake", err)
		}
		return nil, false
	}

	r.actionEvents = containsString(init.Capabilities, cache.CapabilityActionEvents)
	p.mode = providerMode(init)
	// The session checked the socket the provider offered (echoed capability,
	// absolute path, really a socket); record only what it accepted.
	p.objectCacheSocket = sess.ObjectCacheSocket()
	p.ready = true
	p.session = sess
	return sess, true
}

// errProviderNotReady is the initErr of a provider that answered initialize
// with Ready=false: the run builds locally and never starts it again.
var errProviderNotReady = errors.New("cache provider is not ready")

// providerMode is the materialization mode the provider resolved, which it
// returns only with the cache.CapabilityProviderConfiguration echo, else
// cache.ModeFull.
func providerMode(init *cache.InitializeResult) cache.Mode {
	if init.Mode.Valid() && containsString(init.Capabilities, cache.CapabilityProviderConfiguration) {
		return init.Mode
	}
	return cache.ModeFull
}

// notice says, once per run, that the declared provider cannot serve it and
// the run builds locally. ensureProvider calls it at most once, because the
// failure stays initErr. A run whose context ended says nothing: the failure
// is the cancellation, not the provider.
func (p *providerRemote) notice(ctx context.Context, what string, err error) {
	if ctx.Err() != nil {
		slog.Debug("remote cache: provider start canceled", "error", err)
		return
	}
	w := p.notices
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "putnami: %s %s (%v); building locally\n", p.holder(), what, err)
}

// holder names the provider process in a custody refusal.
func (p *providerRemote) holder() string {
	if p.extensionName == "" {
		return "the cache provider"
	}
	return "the cache provider of " + p.extensionName
}

// StartProvider starts the provider session now, as the first operation that
// needs it would, and returns nil once it runs or when the cache has no
// provider. A provider that fails to start or to initialize leaves the run
// building locally, as a lazy start does. The one error it returns is the
// refusal of a hosted run to start the provider, because the provider would
// receive the run credential: repository code ran first
// (runcredential.StartHolder), or the provider is not its extension's native
// runtime (runcredential.RequireNativeHolder). Every later call returns it
// too, and no operation starts the provider.
func (r *RemoteCache) StartProvider(ctx context.Context, ws *workspace.Workspace, cm *store.CacheManager) error {
	if !r.IsProviderBacked() {
		return nil
	}
	r.ensureProvider(ctx, ws, cm)
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	if hostedRefusal(r.provider.initErr) {
		return r.provider.initErr
	}
	return nil
}

// hostedRefusal reports whether err is a hosted run's refusal to start the
// provider.
func hostedRefusal(err error) bool {
	return errors.As(err, new(*runcredential.CustodyError)) || errors.As(err, new(*runcredential.NativeHolderError))
}

func (r *RemoteCache) closeProvider() {
	if !r.IsProviderBacked() {
		return
	}
	p := r.provider
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	sess := p.session
	exchangeDir := p.exchangeDir
	exchangeOwner := p.exchangeOwner
	p.mu.Unlock()

	// Stop first, so shutdown waits for at most the directory the sweep is on.
	p.sweepStop.Store(true)
	if sess != nil {
		_ = sess.Close()
	}
	// The last resort for this run's blobs. A killed run never gets here, and
	// the next session's sweep reclaims what it left.
	removeExchangeDir(exchangeDir, exchangeOwner)
	p.sweep.Wait()
}

// ObjectCacheSocket returns the object-cache socket the provider negotiated for
// this run, or "" when there is none — no provider, a provider that does not
// serve the object cache, or a provider that has not been spawned yet. Safe on a
// nil cache (the local-only path), so callers need no guard.
func (r *RemoteCache) ObjectCacheSocket() string {
	if !r.IsProviderBacked() {
		return ""
	}
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	if !r.provider.ready {
		return ""
	}
	return r.provider.objectCacheSocket
}

// ObjectCacheJobEnv returns the execution-only environment a job subprocess
// needs to reach the provider's object cache: the socket path and the run's
// resolved trust policy.
//
// These two values are facts about HOW this process is spawning the job, like
// PUTNAMI_INTERACTIVE — never task inputs. They must stay out of cache keys, run
// markers, and taskParams hashing: the socket path is a per-run temporary
// directory, so folding it into a key would make every key unique per run, and
// the trust policy already decides which entries may be READ, not what the task
// computes.
//
// It returns nil unless a live provider-backed run negotiated a socket AND the
// trust policy admits remote entries at all, so the object cache fails closed.
func (r *RemoteCache) ObjectCacheJobEnv() []string {
	if r == nil {
		return nil
	}
	socket := r.ObjectCacheSocket()
	if socket == "" {
		return nil
	}
	if !r.Trust.Valid() || r.Trust == store.CacheTrustNone {
		return nil
	}
	return []string{
		cache.ObjectCacheSocketEnv + "=" + socket,
		cache.CacheTrustEnv + "=" + string(r.Trust),
	}
}

// resultOnlyMode returns the session's materialization mode and true when a
// ready provider session echoed cache.CapabilityRestoreResultOnly, so a
// restore can skip the entry's blobs. It is false before the session starts
// and for a provider without the echo. The session is the only record of the
// echo: the same session gates the wire fields, so this answer and the request
// core sends never disagree. The mode is read under the same lock.
func (r *RemoteCache) resultOnlyMode() (cache.Mode, bool) {
	if !r.IsProviderBacked() {
		return "", false
	}
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	if !r.provider.ready || !r.provider.session.RestoreResultOnly() {
		return "", false
	}
	return r.provider.mode, true
}

func (r *RemoteCache) providerExchangeDir() string {
	if !r.IsProviderBacked() {
		return ""
	}
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	return r.provider.exchangeDir
}

func exchangeBlobFetcher(exchangeDir string) store.BlobFetcher {
	return func(digest string) (io.ReadCloser, error) {
		path, ok := cache.BlobExchangePath(exchangeDir, digest)
		if !ok {
			return nil, fmt.Errorf("invalid digest %q", digest)
		}
		return os.Open(path)
	}
}

func ManifestBytes(manifest *cache.Manifest) int64 {
	if manifest == nil {
		return 0
	}
	var total int64
	for _, f := range manifest.Files {
		total += f.Size
	}
	return total
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *RemoteCache) providerDrain(ctx context.Context) {
	if !r.IsProviderBacked() {
		return
	}
	p := r.provider
	p.mu.Lock()
	sess := p.session
	ready := p.ready
	p.mu.Unlock()
	if sess == nil || !ready {
		return
	}
	sum, err := sess.Summary(ctx, nil)
	if err != nil {
		r.stats.RecordUploadFailure()
		slog.Warn("remote cache: provider summary failed", "error", err)
		return
	}
	r.stats.RecordProviderSummary(sum)
}

func providerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}
