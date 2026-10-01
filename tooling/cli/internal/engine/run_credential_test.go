package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The tests below that hold a run credential change it for the whole process
// (runcredential.SetForTest), so they do not run in parallel.

func TestRemoteCacheOptionsCarryOnlyAHeldRunCredential(t *testing.T) {
	restore := runcredential.SetForTest("")
	if options := remoteCacheOptions(); len(options) != 0 {
		t.Errorf("without a run credential remoteCacheOptions = %d options, want none", len(options))
	}
	restore()
	defer runcredential.SetForTest("prc_engine_cache_option")()
	if options := remoteCacheOptions(); len(options) != 1 {
		t.Errorf("with a run credential remoteCacheOptions = %d options, want 1", len(options))
	}
}

// custodyProviderName is the extension that serves the cache provider in the
// tests below.
const custodyProviderName = "@fixture/cache"

// configureRemoteCache writes the remote-cache configuration of wsRoot.
func configureRemoteCache(t *testing.T, wsRoot string) {
	t.Helper()
	path := jobs.RemoteCacheConfigPath(wsRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"url":"https://cache.invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hostedCacheWorkspace is a workspace on disk with a remote cache configured
// and one project, whose one extension serves the cache provider and the
// build, and is installed in the artifact store, as a hosted run requires. The
// provider is the extension's native runtime, as a hosted run requires too:
// it records each start in providerStarts and exits at once, so a started
// provider serves nothing and the run builds locally. The build creates
// builtMarker.
func hostedCacheWorkspace(t *testing.T) (wsRoot string, cfg *wsproto.Config) {
	t.Helper()
	wsRoot = t.TempDir()
	extensions := filepath.Join(store.ResolveArtifactStoreRoot(wsRoot), "extensions")
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		t.Fatal(err)
	}
	extRoot, err := os.MkdirTemp(extensions, "cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(extRoot) })
	files := map[string]string{
		filepath.Join(extRoot, "putnami.extension.json"): fmt.Sprintf(`{
  "name": %q, "version": "0.1.0", "cliContract": %d,
  "runtime": { "executable": "compiled/provider" },
  "commands": {
    %q: { "description": "Serve the remote cache.", "run": [{ "id": "serve", "task": "serve" }] },
    "build": { "description": "Build.", "activationFiles": ["app.project"], "run": [{ "id": "build", "task": "build" }] }
  },
  "tasks": {
    "serve": { "kind": "command", "command": "{extensionRuntime}", "args": ["cache-provider"], "cache": false },
    "build": { "kind": "command", "command": "sh", "args": ["-c", %q], "cache": false }
  }
}`, custodyProviderName, protocolcli.CurrentContract, cache.ProviderCommandName, ": > "+shPath(builtMarker(wsRoot))),
		filepath.Join(wsRoot, "putnami.workspace.json"): fmt.Sprintf(
			`{"name":"custody-ws","includes":["app"],"extensions":{%q:"0.1.0"}}`, custodyProviderName),
		filepath.Join(wsRoot, "app", "putnami.json"): fmt.Sprintf(`{"name":"app","extensions":[%q]}`, custodyProviderName),
		filepath.Join(wsRoot, "app", "app.project"):  "",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := json.Marshal(runtimeproto.Info{
		Extension:       custodyProviderName,
		Version:         "0.1.0",
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixtureproc.Write(t, filepath.Join(extRoot, "compiled", "provider"), fixtureproc.Program{
		Record: providerStarts(wsRoot),
		Exit:   1,
		On:     map[string]fixtureproc.Outcome{"__putnami runtime-info": {Stdout: string(info) + "\n"}},
	})
	link := layout.StableDir(wsRoot, layout.Extensions, custodyProviderName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extRoot, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	configureRemoteCache(t, wsRoot)
	return wsRoot, wsproto.Load(wsRoot)
}

// builtMarker is the file the build of the hostedCacheWorkspace at wsRoot
// creates.
func builtMarker(wsRoot string) string {
	return filepath.Join(wsRoot, "built")
}

// providerStarts is the file the runtime of the hostedCacheWorkspace at
// wsRoot records each of its runs in (fixtureproc.Runs).
func providerStarts(wsRoot string) string {
	return filepath.Join(wsRoot, "provider-starts")
}

// countProviderStarts is how many times the cache provider of the
// hostedCacheWorkspace at wsRoot started: the runs of its runtime other than
// the runtime-info handshake.
func countProviderStarts(t *testing.T, wsRoot string) int {
	t.Helper()
	starts := 0
	for _, run := range fixtureproc.Runs(t, providerStarts(wsRoot)) {
		if strings.Join(run.Args, " ") == "cache-provider" {
			starts++
		}
	}
	return starts
}

// wantRefusal fails t unless err refuses the fixture's cache provider because
// reason ran repository code.
func wantRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *runcredential.CustodyError
	if !errors.As(err, &refusal) || refusal.Holder != "the cache provider of "+custodyProviderName || refusal.Reason != reason {
		t.Fatalf("error = %v, want the refusal of the cache provider of %s after %s", err, custodyProviderName, reason)
	}
}

// Engine.Run starts a hosted run's cache provider before the first hook, and
// hands it to the run.
func TestEngineRunStartsTheHostedCacheProviderBeforeTheHooks(t *testing.T) {
	t.Parallel()
	parsed, err := parser.ParseFile(token.NewFileSet(), "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]token.Pos{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				name = callee.Name
			case *ast.SelectorExpr:
				name = callee.Sel.Name
			}
			if _, seen := calls[name]; name != "" && !seen {
				calls[name] = call.Pos()
			}
			return true
		})
	}
	start, ok := calls["Start"]
	if !ok {
		t.Fatal("Engine.Run never calls HostedRemoteCache.Start")
	}
	for _, later := range []string{"runBeforeHooks", "run"} {
		if offset, ok := calls[later]; !ok || offset < start {
			t.Errorf("Engine.Run calls %s before HostedRemoteCache.Start", later)
		}
	}
}

// The first-use bootstrap of a hosted run starts the cache provider before
// the implicit install's first repository code, and the run that follows
// reads the remote cache through it: it starts no second provider, which the
// custody rule would refuse, and builds.
func TestAHostedRunReadsTheRemoteCacheTheBootstrapStarted(t *testing.T) {
	requireSh(t)
	wsRoot, cfg := hostedCacheWorkspace(t)
	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	global := GlobalFlags{Projects: "app", CacheTrust: string(store.CacheTrustAny)}

	shared := &HostedRemoteCache{}
	defer shared.Close()
	if remote, err := shared.Start(context.Background(), &Request{WorkspaceRoot: wsRoot, Config: cfg, Global: global}); remote == nil || err != nil {
		t.Fatalf("before repository code: Start = %v, %v; want the remote cache", remote, err)
	}
	runcredential.MarkRepositoryCodeStarted("job workspace-install~/app")

	var result SessionResult
	var runErr error
	stderr := captureStderr(t, func() {
		result, runErr = New().Run(context.Background(), Request{
			WorkspaceRoot:     wsRoot,
			Config:            cfg,
			Commands:          []string{"build"},
			Global:            global,
			HostedRemoteCache: shared,
		}, nil)
	})
	if result.ExitCode != ExitSuccess || runErr != nil {
		t.Fatalf("exit code = %d, error %v; want success\n%s", result.ExitCode, runErr, stderr)
	}
	if _, err := os.Stat(builtMarker(wsRoot)); err != nil {
		t.Errorf("the build did not run: %v", err)
	}
	if starts := countProviderStarts(t, wsRoot); starts != 1 {
		t.Errorf("the cache provider started %d times, want once", starts)
	}
}

// HostedRemoteCache starts the provider once, at the first request that reads
// the remote cache: a lifecycle job under --no-cache and a watch iteration
// under trust "none" leave the start to the run. Every later request gets the
// same remote cache, and none after Close.
func TestHostedRemoteCacheStartsItsProviderOnce(t *testing.T) {
	requireSh(t)
	wsRoot, cfg := hostedCacheWorkspace(t)
	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	ctx := context.Background()
	shared := &HostedRemoteCache{}

	for name, global := range map[string]GlobalFlags{
		"under --no-cache": {CacheTrust: string(store.CacheTrustAny), NoCache: true},
		"under trust none": {CacheTrust: string(store.CacheTrustNone)},
	} {
		if remote, err := shared.Start(ctx, &Request{WorkspaceRoot: wsRoot, Config: cfg, Global: global}); remote != nil || err != nil {
			t.Errorf("%s: Start = %v, %v; want nothing", name, remote, err)
		}
	}
	if starts := countProviderStarts(t, wsRoot); starts != 0 {
		t.Fatalf("the cache provider started %d times before a request that reads the cache", starts)
	}

	reads := &Request{WorkspaceRoot: wsRoot, Config: cfg, Global: GlobalFlags{CacheTrust: string(store.CacheTrustAny)}}
	first, err := shared.Start(ctx, reads)
	if first == nil || err != nil {
		t.Fatalf("Start = %v, %v; want the remote cache", first, err)
	}
	if again, err := shared.Start(ctx, reads); again != first || err != nil {
		t.Errorf("second Start = %p, %v; want the first remote cache %p", again, err, first)
	}
	if starts := countProviderStarts(t, wsRoot); starts != 1 {
		t.Errorf("the cache provider started %d times, want once", starts)
	}

	shared.Close()
	shared.Close()
	if remote, err := shared.Start(ctx, reads); remote != nil || err != nil {
		t.Errorf("Start after Close = %v, %v; want nothing", remote, err)
	}
	if starts := countProviderStarts(t, wsRoot); starts != 1 {
		t.Errorf("the cache provider started %d times, want once", starts)
	}
	(*HostedRemoteCache)(nil).Close()
}

// A refusal is what every later request gets: the provider never starts after
// repository code, not even on a second try.
func TestHostedRemoteCacheKeepsItsRefusal(t *testing.T) {
	requireSh(t)
	wsRoot, cfg := hostedCacheWorkspace(t)
	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	runcredential.MarkRepositoryCodeStarted("hook hooks.cli.before")
	shared := &HostedRemoteCache{}
	defer shared.Close()
	reads := &Request{WorkspaceRoot: wsRoot, Config: cfg, Global: GlobalFlags{CacheTrust: string(store.CacheTrustAny)}}
	for range 2 {
		remote, err := shared.Start(context.Background(), reads)
		if remote != nil {
			t.Errorf("Start after repository code = %v, want no remote cache", remote)
		}
		wantRefusal(t, err, "hook hooks.cli.before")
	}
	if starts := countProviderStarts(t, wsRoot); starts != 0 {
		t.Errorf("the cache provider started %d times after repository code", starts)
	}
}

// A hosted run whose cache provider cannot start before repository code
// fails before its first hook: here the implicit install ran a job first, so
// the provider would start after it.
func TestAHostedRunWhoseCacheProviderCannotStartRunsNoHook(t *testing.T) {
	requireSh(t)
	wsRoot, cfg := hostedCacheWorkspace(t)
	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	runcredential.MarkRepositoryCodeStarted("job workspace-install~/app")
	hookRan := filepath.Join(t.TempDir(), "hook-ran")

	var result SessionResult
	var runErr error
	_ = captureStderr(t, func() {
		result, runErr = New().Run(context.Background(), Request{
			WorkspaceRoot: wsRoot,
			Config:        cfg,
			Commands:      []string{"build"},
			Global:        GlobalFlags{CacheTrust: string(store.CacheTrustAny)},
			Hooks:         &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{": > " + shPath(hookRan)}}},
		}, nil)
	})
	if result.ExitCode != ExitError {
		t.Errorf("exit code = %d, want %d", result.ExitCode, ExitError)
	}
	wantRefusal(t, runErr, "job workspace-install~/app")
	if _, err := os.Stat(hookRan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a hook ran after the refusal: %v", err)
	}
}

// A hosted run whose before-hook configures the remote cache fails before it
// records a session or runs a task: the provider of that cache would start
// after the hook, which is repository code, and would receive the run
// credential.
func TestAHostedRunWhoseHookConfiguresTheCacheRecordsNoSession(t *testing.T) {
	requireSh(t)
	wsRoot, cfg := hostedCacheWorkspace(t)
	configPath := jobs.RemoteCacheConfigPath(wsRoot)
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	configure := "printf '%s' '{\"url\":\"https://cache.invalid\"}' > " + shPath(configPath)

	var result SessionResult
	var runErr error
	stderr := captureStderr(t, func() {
		result, runErr = New().Run(context.Background(), Request{
			WorkspaceRoot: wsRoot,
			Config:        cfg,
			Commands:      []string{"build"},
			Global:        GlobalFlags{Projects: "app", CacheTrust: string(store.CacheTrustAny)},
			Hooks:         &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{configure}}},
		}, nil)
	})
	if result.ExitCode != ExitError {
		t.Errorf("exit code = %d, want %d (error %v)\n%s", result.ExitCode, ExitError, runErr, stderr)
	}
	if want := "the cache provider of " + custodyProviderName + " starts after hook "; !strings.Contains(stderr, want) {
		t.Errorf("stderr does not name the refusal %q (error %v):\n%s", want, runErr, stderr)
	}
	if _, err := os.Stat(builtMarker(wsRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the build ran after the refusal: %v", err)
	}
	if ids, _ := workspace_state.NewSessionStore(wsRoot).List(); len(ids) != 0 {
		t.Errorf("sessions recorded = %v, want none", ids)
	}
}

// startHostedRemoteCache starts nothing without a run credential, under
// --no-cache, under the trust policy "none" of a watch iteration, or for a
// workspace that does not load: the run then reads the remote cache as it
// always did, reads none, or reports the workspace after the hooks.
func TestStartHostedRemoteCacheStartsNothingOutsideAHostedCachedRun(t *testing.T) {
	wsRoot, cfg := hostedCacheWorkspace(t)
	anyTrust := GlobalFlags{CacheTrust: string(store.CacheTrustAny)}
	for name, c := range map[string]struct {
		bearer string
		req    Request
	}{
		"without a run credential": {"", Request{WorkspaceRoot: wsRoot, Config: cfg, Global: anyTrust}},
		"under --no-cache": {"prc_engine_custody", Request{WorkspaceRoot: wsRoot, Config: cfg,
			Global: GlobalFlags{CacheTrust: string(store.CacheTrustAny), NoCache: true}}},
		"under trust none": {"prc_engine_custody", Request{WorkspaceRoot: wsRoot, Config: cfg,
			Global: GlobalFlags{CacheTrust: string(store.CacheTrustNone)}}},
		"without a workspace": {"prc_engine_custody", Request{WorkspaceRoot: t.TempDir(), Config: cfg, Global: anyTrust}},
	} {
		restore := runcredential.SetForTest(c.bearer)
		runcredential.MarkRepositoryCodeStarted("hook hooks.cli.before")
		remote, err := startHostedRemoteCache(context.Background(), &c.req)
		restore()
		if remote != nil || err != nil {
			remote.Close()
			t.Errorf("%s: startHostedRemoteCache = %v, %v; want nothing", name, remote, err)
		}
	}
}

// hostedRunRemoteCache hands execute the remote cache started before the
// hooks. A remote cache configured only after them fails the run, because its
// provider would start after repository code; without a run credential, or
// with the build cache off, it gives nothing.
func TestHostedRunRemoteCacheNeverStartsAProviderAfterRepositoryCode(t *testing.T) {
	wsRoot := t.TempDir()
	configureRemoteCache(t, wsRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{
		Name:     custodyProviderName,
		Version:  "0.1.0",
		Path:     t.TempDir(),
		Commands: map[string]string{cache.ProviderCommandName: "serve"},
		Jobs: map[string]*extension.JobDefinition{cache.ProviderCommandName: {
			ExtensionName: custodyProviderName, Name: cache.ProviderCommandName, Command: executable,
		}},
	}}}
	ws := workspace.NewWorkspace(wsRoot, nil, nil)
	cm := jobs.NewRunCacheManager(wsRoot, "")
	req := &Request{WorkspaceRoot: wsRoot, Global: GlobalFlags{CacheTrust: string(store.CacheTrustAny)}}

	t.Cleanup(runcredential.SetForTest("prc_engine_custody"))
	started, _ := jobs.LoadRemoteCache(context.Background(), wsRoot, discovered.Extensions, nil, store.CacheTrustAny)
	if started == nil {
		t.Fatal("the fixture configures no provider-backed remote cache")
	}
	t.Cleanup(started.Close)
	req.hostedRemote = started
	if remote, err := hostedRunRemoteCache(context.Background(), req, ws, discovered, cm); remote != started || err != nil {
		t.Errorf("with a started provider: hostedRunRemoteCache = %p, %v; want the started cache %p", remote, err, started)
	}
	if remote, err := hostedRunRemoteCache(context.Background(), req, ws, discovered, nil); remote != nil || err != nil {
		t.Errorf("without a cache manager: hostedRunRemoteCache = %v, %v; want nothing", remote, err)
	}

	req.hostedRemote = nil
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")
	req.Global.NoCache = true
	if remote, err := hostedRunRemoteCache(context.Background(), req, ws, discovered, cm); remote != nil || err != nil {
		t.Errorf("under --no-cache: hostedRunRemoteCache = %v, %v; want nothing", remote, err)
	}
	req.Global.NoCache = false
	remote, err := hostedRunRemoteCache(context.Background(), req, ws, discovered, cm)
	if remote != nil {
		remote.Close()
		t.Error("a cache configured after repository code was handed to the run")
	}
	wantRefusal(t, err, "hook hooks.commands.build.before")

	restore := runcredential.SetForTest("")
	defer restore()
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")
	if remote, err := hostedRunRemoteCache(context.Background(), req, ws, discovered, cm); remote != nil || err != nil {
		remote.Close()
		t.Errorf("without a run credential: hostedRunRemoteCache = %v, %v; want nothing", remote, err)
	}
}
