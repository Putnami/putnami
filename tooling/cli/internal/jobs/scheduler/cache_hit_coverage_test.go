package scheduler

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestCacheHitReportsTheCoverageItsProducingRunMeasured pins coverage parity
// across every way a hit is served. A verification task's coverage summary
// travels in its result data, not in an event of its own, so a hit reports the
// producing run's summary whatever it kept of that run's events: the local
// entry keeps them, a provider without the action-events capability stores
// none, and a result-only restore reads no file at all. The session report,
// the machine report and the stream all read the summary from that data, or
// from the result event a hit replays or rebuilds from it.
//
// Each first-party producer's shape is checked: Go's statement summary with
// its per-file detail, and TypeScript's line summary.
func TestCacheHitReportsTheCoverageItsProducingRunMeasured(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-verification-producers",
		"a-cache-hit-reports-the-coverage-its-producing-run-measured")
	producers := []struct {
		name     string
		coverage string
		want     runtimeproto.CoverageSummary
	}{
		{
			name: "go statements",
			coverage: `{"percentage":87.5,"granularity":"statements","covered":7,"total":8,"threshold":70,` +
				`"totalStatements":8,"coveredStatements":7,"fileCount":1,"uncoveredFiles":0,` +
				`"files":[{"file":"lib.go","totalStatements":8,"coveredStatements":7,"percentage":87.5}]}`,
			want: runtimeproto.CoverageSummary{Percentage: 87.5, Granularity: "statements", Covered: 7, Total: 8, Threshold: 70},
		},
		{
			name:     "typescript lines",
			coverage: `{"percentage":75,"granularity":"lines","covered":30,"total":40,"threshold":70}`,
			want:     runtimeproto.CoverageSummary{Percentage: 75, Granularity: "lines", Covered: 30, Total: 40, Threshold: 70},
		},
	}
	for _, producer := range producers {
		t.Run(producer.name, func(t *testing.T) {
			server := newFakeCacheServer()
			useSharedFakeProvider(t, server)
			job := coverageTestJob()

			// The producing run: the test task's stream parsed as an execution
			// parses it, then stored and shared as an execution stores and
			// shares it.
			producing := newCaptureFixture(t, job)
			producingRemote := attachRemote(t, producing, cache.ModeFull)
			hash := matrixKey(t, producing, job)
			cold := coverageRunResult(t, producer.coverage)
			if got := TaskResultOf(job, cold).Coverage; got == nil || *got != producer.want {
				t.Fatalf("producing run coverage = %+v, want %+v", got, producer.want)
			}
			if !producing.sched.storeDeclaredCapture(job, cold, hash) {
				t.Fatal("the producing run published no entry")
			}
			producingRemote.UploadTaskEntry(t.Context(), hash, job, producing.cache)
			producingRemote.DrainUploads()
			assertProviderKeptNoEvents(t, server, store.RemoteTaskEntryKey(hash))
			recorded := coverageJSON(t, cold.Data["coverageSummary"])

			local := producing.runDeclaredLookup(t, job, hash)

			consuming := newCaptureFixture(t, job)
			attachRemote(t, consuming, cache.ModeFull)
			remote := consuming.runDeclaredLookup(t, job, hash)

			server.echoResultOnly()
			statusOnly, statusOnlyRemote := resultOnlyConsumer(t, server, cache.ModeMinimal)
			statusOnlyRemote.Negotiate(t.Context(), statusOnly.ws, []*ScheduledJob{job}, nil, nil, statusOnly.cache, cachekey.CacheBypass{})
			if got := server.prefetchedResultOnly(); !slices.Equal(got, []string{store.RemoteTaskEntryKey(hash)}) {
				t.Fatalf("result-only prefetch keys = %v, want the task's one key", got)
			}
			resultOnly := statusOnly.runDeclaredLookup(t, job, hash)

			for _, hit := range []struct {
				name   string
				f      *captureFixture
				result *JobResult
				reuse  ReuseKind
			}{
				{"local hit", producing, local, ReuseLocalCache},
				{"remote hit", consuming, remote, ReuseRemoteCache},
				{"result-only remote hit", statusOnly, resultOnly, ReuseRemoteCache},
			} {
				if hit.result == nil || hit.result.Reuse != hit.reuse {
					t.Fatalf("%s: result = %+v, want a %q hit", hit.name, hit.result, hit.reuse)
				}
				if got := TaskResultOf(job, hit.result).Coverage; got == nil || *got != producer.want {
					t.Errorf("%s: coverage = %+v, want the producing run's %+v", hit.name, got, producer.want)
				}
				// The whole recorded block, producer detail included, is what the
				// hit carries: a reader of the per-file breakdown sees the same rows.
				if got := coverageJSON(t, hit.result.Data["coverageSummary"]); !bytes.Equal(got, recorded) {
					t.Errorf("%s: result data coverage = %s, want the producing run's %s", hit.name, got, recorded)
				}
				events := hit.f.sched.renderer.(*mockRenderer).eventsOfType(job.Key(), EventTypeResult)
				if len(events) != 1 {
					t.Fatalf("%s: replayed %d result events, want 1", hit.name, len(events))
				}
				if got := replayedCoverageJSON(t, events[0]); !bytes.Equal(got, recorded) {
					t.Errorf("%s: replayed result event coverage = %s, want the producing run's %s", hit.name, got, recorded)
				}
			}
		})
	}
}

// coverageTestJob is a task-owned test task of a verification command, the
// shape both first-party test tasks have.
func coverageTestJob() *ScheduledJob {
	job := declaredJob("test~run", "test-run", &extension.TaskDeclaration{})
	job.JobDef.Traits = extension.CommandTraits{Kind: extension.CommandKindVerification}
	return job
}

// coverageRunResult is the result an execution of the test task derives from
// its stream: one result line carrying a test summary and the given coverage
// summary, parsed and attached as the runner does.
func coverageRunResult(t *testing.T, coverage string) *JobResult {
	t.Helper()
	line := `{"v":2,"type":"result","data":{"status":"success","data":{` +
		`"testSummary":{"total":4,"passed":4,"failed":0},"coverageSummary":` + coverage + `}}}` + "\n"
	events, result := readJSONLEvents(strings.NewReader(line), nil, nil)
	if result == nil || len(events) != 1 {
		t.Fatalf("parsed %d events and result %+v from the producing stream, want one result", len(events), result)
	}
	result.Events = events
	return result
}

// coverageJSON is a recorded coverage block in canonical JSON, so two blocks
// compare by value and a mismatch prints readably.
func coverageJSON(t *testing.T, coverage any) []byte {
	t.Helper()
	if coverage == nil {
		return nil
	}
	data, err := json.Marshal(coverage)
	if err != nil {
		t.Fatalf("marshal coverage: %v", err)
	}
	return data
}

// replayedCoverageJSON is the coverage block a replayed result event carries,
// in the same canonical JSON as coverageJSON.
func replayedCoverageJSON(t *testing.T, event RawJobEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(event.Data)
	if err != nil {
		t.Fatalf("marshal replayed result event: %v", err)
	}
	var payload struct {
		Data struct {
			CoverageSummary json.RawMessage `json:"coverageSummary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode replayed result event: %v", err)
	}
	return payload.Data.CoverageSummary
}

// assertProviderKeptNoEvents fails unless the provider holds the entry at key
// and that entry carries no events: the remote legs must exercise a provider
// without the action-events capability, whose hit rebuilds the result event
// from the result data alone.
func assertProviderKeptNoEvents(t *testing.T, server *fakeCacheServer, key string) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	entry, ok := server.entries[key]
	if !ok || entry.result == nil {
		t.Fatalf("provider holds no result at %s", key)
	}
	if len(entry.result.Events) != 0 {
		t.Fatalf("provider entry %s carries %d events; the remote legs need a provider that keeps none",
			key, len(entry.result.Events))
	}
}
