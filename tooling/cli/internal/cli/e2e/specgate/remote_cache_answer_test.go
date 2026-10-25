package specgate

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The run records say why the remote cache gave no hit. A narrowed run over
// the attester fixture, with an empty local store, must ask the remote cache
// for every task, and the spec gate asks it for the library's test task the
// run did not plan. The provider is served in process, so each test decides
// how it answers a restore.

// withRemoteCacheProvider makes the fixture extension declare the reserved
// cache provider command, so the run loads a provider-backed remote cache.
// The command never starts a process: the test serves the provider in process
// (serveRemoteCache).
func withRemoteCacheProvider(t *testing.T, wsRoot string) {
	t.Helper()
	path := filepath.Join(wsRoot, "gate-extension", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, edit := range [][2]string{
		{`"commands": {`, `"commands": {
    "` + cache.ProviderCommandName + `": {
      "description": "Serve the remote cache",
      "activationFiles": ["gate.provider"],
      "run": [{"id": "provider", "task": "provider-exec"}]
    },`},
		{`"tasks": {`, `"tasks": {
    "provider-exec": {
      "description": "Stand in for a cache provider; the test serves it in process.",
      "kind": "command",
      "command": "{extensionRoot}/emit.sh",
      "cwd": "{workspaceRoot}",
      "cache": false
    },`},
	} {
		if strings.Count(manifest, edit[0]) != 1 {
			t.Fatalf("the fixture manifest no longer contains %q exactly once", edit[0])
		}
		manifest = strings.Replace(manifest, edit[0], edit[1], 1)
	}
	clitest.WriteFile(t, path, manifest)
	workspace.InvalidateLoadCache(wsRoot)
}

// serveRemoteCache routes every provider start of the test to an in-process
// provider that answers each restore with status, and returns how many
// restores it answered.
func serveRemoteCache(t *testing.T, status cache.RestoreStatus) *atomic.Int64 {
	t.Helper()
	var restores atomic.Int64
	original := remotecache.SpawnProviderSession
	remotecache.SpawnProviderSession = func(_ context.Context, _ cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		requests, requestWriter := io.Pipe()
		responseReader, responses := io.Pipe()
		go func() {
			serveRestoreStatus(requests, responses, status, &restores)
			_ = responses.Close()
			_ = requests.Close()
		}()
		return cacheprovider.Connect(requestWriter, responseReader, opts...), nil
	}
	t.Cleanup(func() { remotecache.SpawnProviderSession = original })
	return &restores
}

// serveRestoreStatus is a provider that is ready, holds no entry, accepts
// every upload, and answers every restore with status.
func serveRestoreStatus(stdin io.Reader, stdout io.Writer, status cache.RestoreStatus, restores *atomic.Int64) {
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	write := func(response *cache.ProviderResponse) {
		line, _ := json.Marshal(response)
		_, _ = stdout.Write(append(line, '\n'))
	}
	answer := func(request *cache.ProviderRequest, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		write(&cache.ProviderResponse{ProtocolVersion: request.ProtocolVersion, ID: request.ID, OK: true, Payload: raw})
	}
	for in.Scan() {
		request, _ := cache.ParseAndValidateProviderRequest(in.Bytes())
		if request == nil {
			continue
		}
		switch request.Op {
		case cache.OpInitialize:
			negotiated := request.ProtocolVersion
			if params, _ := cache.ParseAndValidateInitializeParams(request.Payload); params != nil &&
				slices.Contains(params.Capabilities, cache.CapabilityProviderProtocolV2) {
				negotiated = cache.ProviderProtocolVersion
			}
			answer(request, &cache.InitializeResult{
				ProtocolVersion: negotiated, ProviderName: "@putnami/gate-fixture", ProviderVersion: "0.1.0", Ready: true,
			})
		case cache.OpPrefetch:
			answer(request, &cache.PrefetchResult{})
		case cache.OpRestore:
			restores.Add(1)
			answer(request, &cache.RestoreResult{Status: status})
		case cache.OpUpload:
			answer(request, &cache.UploadResult{Accepted: true})
		case cache.OpSummary:
			answer(request, &cache.SummaryResult{})
		case cache.OpShutdown:
			answer(request, nil)
			return
		default:
			write(&cache.ProviderResponse{
				ProtocolVersion: request.ProtocolVersion, ID: request.ID, OK: false,
				Error: &cache.ProviderError{Code: "unsupported", Message: string(request.Op)},
			})
		}
	}
}

// latestRemoteCacheAnswer reads the remote member of the cache object of the
// latest session's session.json, or nil when it is absent.
func latestRemoteCacheAnswer(t *testing.T, wsRoot, sessionID string) *jobmodel.RemoteCacheAnswer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsRoot, ".putnami", "sessions", sessionID, "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	var session struct {
		Cache *jobmodel.CacheStatsSnapshot `json:"cache"`
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	if session.Cache == nil {
		t.Fatalf("session.json has no cache object:\n%s", data)
	}
	return session.Cache.Remote
}

// narrowedRunWithRemote runs the attester fixture's narrowed selection with an
// empty local store and a remote cache that answers every restore with
// status, and returns the record's alpha group and the session's remote
// member.
func narrowedRunWithRemote(t *testing.T, status cache.RestoreStatus) (features.SpecVerificationGroup, *jobmodel.RemoteCacheAnswer) {
	t.Helper()
	clitest.RequireShell(t)
	wsRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	// The test runner's environment can turn the remote cache off; this run
	// needs it on.
	t.Setenv("PUTNAMI_CACHE_TRUST", "any")
	attesterFixture(t, wsRoot)
	withRemoteCacheProvider(t, wsRoot)
	restores := serveRemoteCache(t, status)

	code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--projects", "alpha")
	if code != cli.ExitSuccess {
		t.Fatalf("a run whose library could not be consulted exited %d:\n%s", code, output)
	}
	if restores.Load() == 0 {
		t.Fatalf("the run asked the remote cache nothing:\n%s", output)
	}
	if !strings.Contains(output, "/library") {
		t.Errorf("the warning does not name the library:\n%s", output)
	}
	record, sessionID := latestGateRecord(t, wsRoot)
	group := gateState(t, record, "gate/alpha")
	if group.Requirements[0].State != features.RequirementUnobserved || group.Blocked {
		t.Fatalf("group = %+v, want an unblocked unobserved requirement", group)
	}
	return group, latestRemoteCacheAnswer(t, wsRoot, sessionID)
}

// TestRunRecordsSayTheRemoteCacheDidNotAnswer is a run whose remote lookups
// fail: the provider reports every restore as an error, which is how it
// reports a remote cache it cannot reach. session.json says the remote cache
// did not answer, why, and how many lookups failed; the record says the
// unplanned library was not consulted because the remote cache was
// unreachable.
func TestRunRecordsSayTheRemoteCacheDidNotAnswer(t *testing.T) {
	group, remote := narrowedRunWithRemote(t, cache.RestoreError)

	want := []features.UnconsultedCandidate{{Project: "/library", Reason: features.UnconsultedRemoteUnreachable}}
	if !slices.Equal(group.Unconsulted, want) {
		t.Errorf("unconsulted = %+v, want %+v", group.Unconsulted, want)
	}
	if remote == nil {
		t.Fatal("session.json does not say whether the remote cache answered")
	}
	if remote.Answered || remote.Reason != jobmodel.RemoteCacheUnreachable {
		t.Errorf("remote = %+v, want unanswered and unreachable", remote)
	}
	if remote.FailedLookups == 0 || remote.FailedLookups != remote.Lookups {
		t.Errorf("remote = %+v, want every lookup failed", remote)
	}
}

// TestRunRecordsReadAColdRemoteCacheAsNoEntry is a run whose remote cache
// answers every lookup with no entry. That is a cold cache, not a failure:
// session.json says the remote cache answered, and the record says the
// library has no entry for these inputs.
func TestRunRecordsReadAColdRemoteCacheAsNoEntry(t *testing.T) {
	group, remote := narrowedRunWithRemote(t, cache.RestoreMiss)

	want := []features.UnconsultedCandidate{{Project: "/library", Reason: features.UnconsultedNoEntry}}
	if !slices.Equal(group.Unconsulted, want) {
		t.Errorf("unconsulted = %+v, want %+v", group.Unconsulted, want)
	}
	if remote == nil {
		t.Fatal("session.json does not say whether the remote cache answered")
	}
	if !remote.Answered || remote.Lookups == 0 || remote.FailedLookups != 0 || remote.Reason != "" {
		t.Errorf("remote = %+v, want every lookup answered", remote)
	}
}
