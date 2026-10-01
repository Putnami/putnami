package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The object cache is the one provider surface a JOB process talks to rather
// than core: the provider listens on a Unix socket and core hands every job its
// path plus the run's trust policy. These tests pin what keeps that honest —
// the socket is negotiated and checked, the provider starts whenever a job will
// run here, the two variables reach a real subprocess, and they stay out of
// every cache key.

// objectCacheClientEnv makes the re-exec'd test binary act as a job that uses
// the object cache instead of running the suite (see TestMain).
const objectCacheClientEnv = "PUTNAMI_JOBS_OBJECT_CACHE_CLIENT"

// objectCacheJobResultEnv is where that job writes what it observed.
const objectCacheJobResultEnv = "PUTNAMI_JOBS_OBJECT_CACHE_OUT"

// runObjectCacheClientJob is the body of the job subprocess. It reads the two
// variables core exported, drives one put and one get over the socket it was
// given, and reports what it saw. It is deliberately the shape a language
// extension's cache helper has: read the environment, dial, batch, and fall
// back to local behavior on any failure.
func runObjectCacheClientJob(out string) {
	socket := os.Getenv(cache.ObjectCacheSocketEnv)
	trust := os.Getenv(cache.CacheTrustEnv)
	report := func(text string) {
		if out != "" {
			_ = os.WriteFile(out, []byte(text), 0o644)
		}
		// The job itself always succeeds: the object cache is an accelerator, so
		// a job that cannot reach it must still do its work.
		_, _ = os.Stdout.WriteString(`{"v":2,"type":"result","data":{"status":"success"}}` + "\n")
	}
	if socket == "" {
		report("socket=|trust=" + trust)
		return
	}
	client, err := objectcachetest.Dial(socket)
	if err != nil {
		report("dial-failed=" + err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	// The provider creates the socket inside the blob-exchange directory, which
	// is also where the bytes travel.
	exchangeDir := filepath.Dir(socket)
	content := []byte("object written by a job")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		report("stage-failed=" + err.Error())
		return
	}
	id := strings.Repeat("f", 64)
	if _, err := client.Put(&cache.ObjectPutParams{
		Namespace: "go-build",
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content)), Meta: "output-id"}},
	}); err != nil {
		report("put-failed=" + err.Error())
		return
	}
	// AcceptChannels is derived from the trust policy exactly as a language
	// extension derives it: an authoritative run reads only trusted objects.
	var accept []cache.Channel
	if trust == string(store.CacheTrustCI) {
		accept = []cache.Channel{cache.ChannelTrusted}
	}
	got, err := client.Get(&cache.ObjectGetParams{Namespace: "go-build", IDs: []string{id}, AcceptChannels: accept})
	if err != nil {
		report("get-failed=" + err.Error())
		return
	}
	channel := ""
	if len(got.Objects) == 1 {
		channel = string(got.Objects[0].Channel)
	}
	report("socket=" + socket + "|trust=" + trust + "|hits=" + strconv.Itoa(len(got.Objects)) + "|channel=" + channel)
}

// objectCacheFixture is a workspace with one job that runs the object-cache
// client, wired to a provider whose fake serves the socket.
type objectCacheFixture struct {
	ws      *workspace.Workspace
	planned []*ScheduledJob
	cache   *store.CacheManager
	remote  *RemoteCache
	out     string
}

// newObjectCacheFixture builds the fixture. useClient selects the job body: a
// /bin/sh one-liner that only REPORTS the two variables (cheap, enough for the
// export/absence rules) or the re-exec'd test binary that actually drives the
// socket (needed only where the round trip itself is the subject).
func newObjectCacheFixture(t *testing.T, providerEnv map[string]string, cacheable, useClient bool) *objectCacheFixture {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil && !useClient {
		t.Skip("/bin/sh unavailable")
	}
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)

	root := t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "app", "src.txt", "one")
	ws := testWorkspace(root, project)

	out := filepath.Join(root, "object-cache-observed.txt")
	command := os.Args[0]
	jobEnv := map[string]string{objectCacheClientEnv: "1", objectCacheJobResultEnv: out}
	if !useClient {
		command = filepath.Join(root, "report-env.sh")
		writeExecutable(t, command, "#!/bin/sh\n"+
			"printf 'socket=%s|trust=%s' \"${PUTNAMI_CACHE_OBJECT_SOCKET-}\" \"${PUTNAMI_CACHE_TRUST-}\" > "+
			shellQuote(out)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\"}}'\n")
		jobEnv = nil
	}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.0.0", Path: filepath.Join(root, "extension")}
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "build",
			CommandName:   "build",
			Command:       command,
			Cwd:           "{projectRoot}",
			Cache:         cacheable,
			FilePatterns:  []string{"src.txt"},
			TimeoutMs:     unboundedJobTimeoutMs,
			Env:           jobEnv,
		},
	}
	if cacheable {
		// Only a task-owned DECLARATION makes a job cacheable (CanUseCache), and
		// a status-only declaration is the smallest one: this job produces no
		// output, it is here to be a cache HIT on the second run.
		ext.Tasks = map[string]extension.TaskDefinition{
			"build-run": {Declares: &extension.TaskDeclaration{}},
		}
		job.Step = &extension.PipelineStep{ID: "run", Task: "build-run"}
		job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true, NoOutput: true}
	}

	localStore := store.NewLocalStore(filepath.Join(root, "store"))
	cm := store.NewCacheManager(localStore)

	f := &objectCacheFixture{ws: ws, planned: []*ScheduledJob{job}, cache: cm, out: out}
	f.remote = f.loadRemote(t, providerEnv)
	return f
}

// loadRemote resolves a provider-backed remote cache for the fixture's
// workspace. Each call is a fresh provider SESSION, so it also produces a fresh
// exchange directory and socket path.
func (f *objectCacheFixture) loadRemote(t *testing.T, providerEnv map[string]string) *RemoteCache {
	t.Helper()
	provider := map[string]string{"FAKE_OBJECT_CACHE": "1"}
	for k, v := range providerEnv {
		provider[k] = v
	}
	remote, notice := LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, provider)}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	return remote
}

func (f *objectCacheFixture) run(t *testing.T, cfg SchedulerConfig) *SchedulerResult {
	t.Helper()
	sched := newScheduler(f.ws, f.planned, nil, cfg, &mockRenderer{}, f.cache)
	sched.setRemoteCache(f.remote)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return sched.Run(ctx)
}

func (f *objectCacheFixture) observed(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatalf("the job wrote no observation: %v", err)
	}
	return string(data)
}

// TestObjectCacheSocketReachesAJobProcess is the end-to-end proof: a REAL job
// subprocess receives the socket path and the trust policy, dials the socket,
// and completes a put/get round trip against the provider's object cache.
func TestObjectCacheSocketReachesAJobProcess(t *testing.T) {
	f := newObjectCacheFixture(t, nil, false, true)
	result := f.run(t, SchedulerConfig{MaxParallel: 1})
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}

	socket := f.remote.ObjectCacheSocket()
	if socket == "" {
		t.Fatal("the run negotiated no object-cache socket")
	}
	observed := f.observed(t)
	for _, want := range []string{"socket=" + socket, "trust=any", "hits=1", "channel=trusted"} {
		if !strings.Contains(observed, want) {
			t.Fatalf("job observed %q, want it to contain %q", observed, want)
		}
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("exported path is not a live socket: %v (%v)", err, info)
	}
}

// TestObjectCacheTrustPolicyReachesAJobProcess pins the channel rule end to
// end: under an authoritative policy the job asks for trusted objects only, and
// a provider stamping hint serves it nothing.
func TestObjectCacheTrustPolicyReachesAJobProcess(t *testing.T) {
	f := newObjectCacheFixture(t, map[string]string{"FAKE_OBJECT_CHANNEL": string(cache.ChannelHint)}, false, true)
	// The run's resolved policy is what the job must see; LoadRemoteCache took
	// it from the CLI flag, so restate it here as an authoritative run would.
	f.remote.trust = store.CacheTrustCI

	result := f.run(t, SchedulerConfig{MaxParallel: 1})
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	observed := f.observed(t)
	if !strings.Contains(observed, "trust=ci") {
		t.Fatalf("job observed %q, want the resolved trust policy", observed)
	}
	if !strings.Contains(observed, "hits=0") {
		t.Fatalf("job observed %q, want a hint object withheld from an authoritative run", observed)
	}
}

// TestObjectCacheEnvIsAbsentUnderNoCache: a run that consults no cache must not
// hand its jobs one either.
func TestObjectCacheEnvIsAbsentUnderNoCache(t *testing.T) {
	f := newObjectCacheFixture(t, nil, false, false)
	result := f.run(t, SchedulerConfig{MaxParallel: 1, NoCache: true})
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	if observed := f.observed(t); !strings.HasPrefix(observed, "socket=|") {
		t.Fatalf("job observed %q, want no socket under --no-cache", observed)
	}
	if got := f.remote.ObjectCacheSocket(); got != "" {
		t.Fatalf("--no-cache spun up a provider: socket %q", got)
	}
}

// TestObjectCacheEnvIsAbsentWithoutAProvider covers the ordinary local-only
// run: no provider, no variables, and the job still succeeds.
func TestObjectCacheEnvIsAbsentWithoutAProvider(t *testing.T) {
	f := newObjectCacheFixture(t, nil, false, false)
	f.remote = nil
	if result := f.run(t, SchedulerConfig{MaxParallel: 1}); !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	if observed := f.observed(t); !strings.HasPrefix(observed, "socket=|") {
		t.Fatalf("job observed %q, want no socket without a provider", observed)
	}
}

// TestObjectCacheEnvOfAnOuterRunNeverReachesAJob is the nested-run half of the
// two absence rules above. Withholding a variable is not the same as its being
// absent: a nested run — a test suite under a CI gate, an extension calling the
// CLI back — inherits the OUTER run's exported pair through os.Environ, and the
// job would read that socket as if this run had handed it one (a CI-only
// red on exactly the two tests above). The pair the job sees must be this run's
// statement alone: absent when this run consults no cache, and this run's own
// socket — never the inherited one — when it does.
func TestObjectCacheEnvOfAnOuterRunNeverReachesAJob(t *testing.T) {
	const outerSocket = "/tmp/putnami-outer-run/objects.sock"
	t.Setenv(cache.ObjectCacheSocketEnv, outerSocket)
	t.Setenv(cache.CacheTrustEnv, string(store.CacheTrustCI))

	t.Run("no-cache", func(t *testing.T) {
		f := newObjectCacheFixture(t, nil, false, false)
		if result := f.run(t, SchedulerConfig{MaxParallel: 1, NoCache: true}); !result.Success {
			t.Fatalf("run failed: %+v", result.Results)
		}
		if observed := f.observed(t); observed != "socket=|trust=" {
			t.Fatalf("job observed %q, want the outer run's pair stripped under --no-cache", observed)
		}
	})

	t.Run("no-provider", func(t *testing.T) {
		f := newObjectCacheFixture(t, nil, false, false)
		f.remote = nil
		if result := f.run(t, SchedulerConfig{MaxParallel: 1}); !result.Success {
			t.Fatalf("run failed: %+v", result.Results)
		}
		if observed := f.observed(t); observed != "socket=|trust=" {
			t.Fatalf("job observed %q, want the outer run's pair stripped without a provider", observed)
		}
	})

	t.Run("own-provider", func(t *testing.T) {
		f := newObjectCacheFixture(t, nil, false, false)
		if result := f.run(t, SchedulerConfig{MaxParallel: 1}); !result.Success {
			t.Fatalf("run failed: %+v", result.Results)
		}
		socket := f.remote.ObjectCacheSocket()
		if socket == "" {
			t.Fatal("the run negotiated no object-cache socket")
		}
		if observed, want := f.observed(t), "socket="+socket+"|trust=any"; observed != want {
			t.Fatalf("job observed %q, want this run's own pair %q", observed, want)
		}
	})
}

// TestObjectCacheEnvStaysOutOfCacheKeys is the determinism invariant: the
// socket path is a per-run temporary path, so if it reached a cache key every
// warm run would miss. The proof is behavioral — a second run with a DIFFERENT
// socket (a new provider session) is still a cache hit.
func TestObjectCacheEnvStaysOutOfCacheKeys(t *testing.T) {
	f := newObjectCacheFixture(t, nil, true, false)
	cold := f.run(t, SchedulerConfig{MaxParallel: 1})
	if !cold.Success {
		t.Fatalf("cold run failed: %+v", cold.Results)
	}
	coldSocket := f.remote.ObjectCacheSocket()
	if coldSocket == "" {
		t.Fatal("the cold run negotiated no object-cache socket")
	}

	// A fresh provider session for the same workspace: a new exchange directory,
	// therefore a new socket path.
	f.remote.Close()
	f.remote = f.loadRemote(t, nil)

	warm := f.run(t, SchedulerConfig{MaxParallel: 1})
	if !warm.Success {
		t.Fatalf("warm run failed: %+v", warm.Results)
	}
	if warmSocket := f.remote.ObjectCacheSocket(); warmSocket == coldSocket {
		t.Fatal("the second run reused the first socket; the test cannot prove the key ignores it")
	}
	for key, result := range warm.Results {
		if !result.CacheHit {
			t.Fatalf("%s missed on the warm run: the object-cache environment moved its cache key", key)
		}
	}
}

// TestNegotiateSpawnsTheProviderForALocallyExecutingJob pins the fix for the
// early return: a run whose jobs are all uncacheable warms no key, but its jobs
// still execute here and still need the object cache.
func TestNegotiateSpawnsTheProviderForALocallyExecutingJob(t *testing.T) {
	f := newObjectCacheFixture(t, nil, false, false)
	keys, err := PrecomputeKeys(f.ws, f.planned, nil, nil, f.cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("the fixture's job is cacheable (%d keys); this test needs a run with nothing to warm", len(keys))
	}
	f.remote.Negotiate(context.Background(), f.ws, f.planned, nil, nil, f.cache, CacheBypass{})
	if got := f.remote.ObjectCacheSocket(); got == "" {
		t.Fatal("a run with a locally executing job did not start the provider")
	}
}

// TestNegotiateDoesNotSpawnTheProviderForAFullyWarmRun keeps the property the
// early return protected: when every planned job is a local hit there is
// nothing to warm and nothing to execute, so the provider stays down.
func TestNegotiateDoesNotSpawnTheProviderForAFullyWarmRun(t *testing.T) {
	f := newObjectCacheFixture(t, nil, true, false)
	if cold := f.run(t, SchedulerConfig{MaxParallel: 1}); !cold.Success {
		t.Fatalf("cold run failed: %+v", cold.Results)
	}

	warmRemote := f.loadRemote(t, nil)
	warmRemote.Negotiate(context.Background(), f.ws, f.planned, nil, nil, f.cache, CacheBypass{})
	if got := warmRemote.ObjectCacheSocket(); got != "" {
		t.Fatalf("a fully warm rebuild spun up the provider: socket %q", got)
	}
}

// TestNegotiateAsksForTheObjectCacheCapability pins the bootstrap: core lists
// the capability, so a provider knows it may answer with a socket.
func TestNegotiateAsksForTheObjectCacheCapability(t *testing.T) {
	capabilitiesOut := filepath.Join(t.TempDir(), "capabilities.txt")
	f := newObjectCacheFixture(t, map[string]string{"FAKE_CAPABILITIES_OUT": capabilitiesOut}, false, false)
	f.remote.Negotiate(context.Background(), f.ws, f.planned, nil, nil, f.cache, CacheBypass{})

	data, err := os.ReadFile(capabilitiesOut)
	if err != nil {
		t.Fatalf("the provider recorded no capabilities: %v", err)
	}
	if !strings.Contains(string(data), cache.CapabilityObjectCache) {
		t.Fatalf("advertised capabilities = %q, want %q", data, cache.CapabilityObjectCache)
	}
}

// TestObjectCacheJobEnvFailsClosed covers, at the accessor, every case where
// the two variables must not be exported at all.
func TestObjectCacheJobEnvFailsClosed(t *testing.T) {
	var nilCache *RemoteCache
	if got := nilCache.objectCacheJobEnv(); got != nil {
		t.Fatalf("nil cache env = %v, want none", got)
	}
	if got := nilCache.ObjectCacheSocket(); got != "" {
		t.Fatalf("nil cache socket = %q, want empty", got)
	}

	// A provider-backed cache that has not negotiated a socket exports nothing.
	local := newProviderRemoteCache(t.TempDir(), cacheprovider.LaunchSpec{Command: "/bin/true"}, cache.ModeFull, store.CacheTrustAny)
	if got := local.objectCacheJobEnv(); got != nil {
		t.Fatalf("un-negotiated env = %v, want none", got)
	}

	// A socket with a disabled trust policy stays unexported: the object cache
	// follows the same fail-closed rule the task cache does.
	local.provider.ready = true
	local.provider.objectCacheSocket = "/tmp/objects.sock"
	local.trust = store.CacheTrustNone
	if got := local.objectCacheJobEnv(); got != nil {
		t.Fatalf("trust none env = %v, want none", got)
	}
	local.trust = store.CacheTrustCI
	want := []string{
		cache.ObjectCacheSocketEnv + "=/tmp/objects.sock",
		cache.CacheTrustEnv + "=ci",
	}
	got := local.objectCacheJobEnv()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("env = %v, want %v", got, want)
	}
}
