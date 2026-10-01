package machine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestWarnRetiredSelection_OnlyTheV1SpellingWarns pins the retired variable's
// two properties after the v1 emitters were deleted: it selects NOTHING
// (there is no emitter left to select), and the one spelling whose meaning
// changed — "v1", in any case, padded or not — says so on stderr instead of
// being swallowed. Every other value, including the historical "v2", already
// gets what it asked for and stays silent.
//
// The old predicate this replaces answered "does this value roll the run back
// to v1?". Keeping the value table is deliberate: the notice must fire on
// exactly the spellings the rollback used to accept, or a runner carrying
// " V1 " would silently start parsing v2 documents.
func TestWarnRetiredSelection_OnlyTheV1SpellingWarns(t *testing.T) {
	for _, tc := range []struct {
		value    string
		wantWarn bool
	}{
		{"", false},
		{"v1", true},
		{" V1 ", true},
		{"1", false},
		{"true", false},
		{"json", false},
		{"v2", false},
		{" v2 ", false},
		{"V2", false},
		{"v2,v1", false},
		{"v1,v2", false},
		{"latest", false},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			if got := requestsRetiredV1(tc.value); got != tc.wantWarn {
				t.Errorf("requestsRetiredV1(%q) = %v, want %v", tc.value, got, tc.wantWarn)
			}
		})
	}
}

// TestWarnRetiredSelection_WritesOneNoticePerProcess pins the notice's content
// and its once-per-process budget. The variable is consulted from a single
// startup site, but the sync.Once is what keeps a future second caller from
// turning a legacy environment into repeated stderr noise on every run.
func TestWarnRetiredSelection_WritesOneNoticePerProcess(t *testing.T) {
	var first, second, quiet strings.Builder

	WarnRetiredSelection(&quiet, "v2")
	if quiet.Len() != 0 {
		t.Errorf("a non-v1 value warned: %q", quiet.String())
	}

	WarnRetiredSelection(&first, " V1 ")
	notice := first.String()
	if notice == "" {
		t.Fatal("the retired v1 selection produced no notice")
	}
	for _, want := range []string{RetiredSelectionEnv, "ignored", "protocolVersion", "putnami pin"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice does not mention %q:\n%s", want, notice)
		}
	}

	WarnRetiredSelection(&second, "v1")
	if second.Len() != 0 {
		t.Errorf("the notice repeated within one process:\n%s", second.String())
	}
}

// TestErrorClasses_MatchTheProtocolVocabulary pins the two class spellings this
// package writes against the protocol's own derivation. protocols/cli exports
// the sentinel errors and computes the class in ErrorCode, so a rename there
// would otherwise leave these documents carrying a class the validator rejects.
func TestErrorClasses_MatchTheProtocolVocabulary(t *testing.T) {
	if got := protocolcli.ErrorCode(errors.New("boom")); got != errorClassFailure {
		t.Errorf("ErrorCode(plain error) = %q, want %q", got, errorClassFailure)
	}
	signal := protocolcli.Classify(errors.New("boom"), protocolcli.ErrSignal)
	if got := protocolcli.ErrorCode(signal); got != errorClassSignal {
		t.Errorf("ErrorCode(signal) = %q, want %q", got, errorClassSignal)
	}
}

func TestIdentity_IsDerivedFromThePlanNode(t *testing.T) {
	job := &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: "/tooling/cli", Name: "@putnami/cli"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef: &extension.JobDefinition{
			Name:         "build",
			InternalName: "build~compile",
			CommandName:  "build",
			StepID:       "compile",
		},
		Step: &extension.PipelineStep{Task: "go-build"},
	}

	id := Identity(job)
	if id.Key != job.Key() {
		t.Errorf("key = %q, want the v1 plan key %q", id.Key, job.Key())
	}
	if id.Key != id.DerivedKey() {
		t.Errorf("key %q is not the derived key %q", id.Key, id.DerivedKey())
	}
	if id.Scope != protocolcli.TaskScopeProject {
		t.Errorf("scope = %q, want %q", id.Scope, protocolcli.TaskScopeProject)
	}
	// task.name is the PLAN name, never the display name: two steps of one
	// command share "build" as their display name and would collide.
	if id.Task.Name != "build~compile" {
		t.Errorf("task.name = %q, want the plan name build~compile", id.Task.Name)
	}
	if id.Task.Command != "build" || id.Task.Step != "compile" || id.Task.Kind != "go-build" {
		t.Errorf("task = %+v, want command=build step=compile kind=go-build", id.Task)
	}
	if id.Project.ID != "/tooling/cli" || id.Project.Name != "@putnami/cli" {
		t.Errorf("project = %+v, want the id and the name apart", id.Project)
	}
	if id.Provider.Extension != "@putnami/go" {
		t.Errorf("provider.extension = %q, want @putnami/go", id.Provider.Extension)
	}
}

// TestIdentity_WorkspaceScope pins the structural reading of scope: a task that
// runs once for the whole workspace carries the resolved selection, which v1
// expressed as a synthetic project a reader had to recognize.
func TestIdentity_WorkspaceScope(t *testing.T) {
	job := &jobs.ScheduledJob{
		Project:          &workspace.Project{ID: "/", Name: "workspace"},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef:           &extension.JobDefinition{Name: "install"},
		SelectedProjects: []*workspace.Project{{ID: "/a"}, {ID: "/b"}},
	}
	if got := Identity(job).Scope; got != protocolcli.TaskScopeWorkspace {
		t.Errorf("scope = %q, want %q", got, protocolcli.TaskScopeWorkspace)
	}
}

// TestIdentity_FallsBackToTheResultKey covers the task the plan does not name —
// the case v1's JSON renderer spelled by writing the map key into both the
// package and the job field. Every member is still required, and the key must
// still be the derived one.
func TestIdentity_FallsBackToTheResultKey(t *testing.T) {
	id := identityOfKey("/services/api:test~unit")
	if id.Project.ID != "/services/api" || id.Task.Name != "test~unit" {
		t.Errorf("identity = %+v, want the key split at the first colon", id)
	}
	if id.Task.Command != "test" {
		t.Errorf("task.command = %q, want test", id.Task.Command)
	}
	if id.Key != id.DerivedKey() || id.Key != "/services/api:test~unit" {
		t.Errorf("key = %q, want the derived key back", id.Key)
	}
	if id.Provider.Extension == "" || id.Project.Name == "" || id.Task.Kind == "" {
		t.Errorf("identity = %+v: every member is required and non-empty", id)
	}
}

// TestSummary_VerdictPrecedence walks the three outcomes and the exit code each
// must carry. It is the machine-checked half of decision 2: abort > failure >
// success, with the exit code agreeing on every surface.
func TestSummary_VerdictPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		session  *jobs.SessionResult
		outcome  string
		exitCode int
	}{
		{
			name:     "clean run",
			session:  &jobs.SessionResult{Status: jobs.TaskCounts{Succeeded: 2}},
			outcome:  protocolcli.RunOutcomeSuccess,
			exitCode: protocolcli.ExitSuccess,
		},
		{
			name:     "failure",
			session:  &jobs.SessionResult{Status: jobs.TaskCounts{Succeeded: 1, Failed: 1}},
			outcome:  protocolcli.RunOutcomeFailure,
			exitCode: protocolcli.ExitFailure,
		},
		{
			name: "abort wins over failure",
			session: &jobs.SessionResult{
				Status:    jobs.TaskCounts{Failed: 1, Canceled: 1},
				Aborted:   true,
				AbortedBy: jobs.AbortUser,
			},
			outcome:  protocolcli.RunOutcomeAborted,
			exitCode: protocolcli.ExitSignal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := Run{Session: tc.session}.Summary(12)
			if summary.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", summary.Outcome, tc.outcome)
			}
			if summary.ExitCode != tc.exitCode {
				t.Errorf("exitCode = %d, want %d", summary.ExitCode, tc.exitCode)
			}
			if summary.Counts.Total != summary.Counts.Sum() {
				t.Errorf("counts.total = %d, want the sum of the buckets (%d)",
					summary.Counts.Total, summary.Counts.Sum())
			}
			if summary.DurationMs != 12 {
				t.Errorf("durationMs = %d, want 12", summary.DurationMs)
			}
		})
	}
}

// TestSummary_AbortAlwaysNamesItsSource pins the contract rule that
// outcome=="aborted" implies abortedBy: an abort whose source was never
// recorded still has to name one.
func TestSummary_AbortAlwaysNamesItsSource(t *testing.T) {
	summary := Run{Session: &jobs.SessionResult{Aborted: true}}.Summary(0)
	if summary.AbortedBy != protocolcli.AbortedBySignal {
		t.Errorf("abortedBy = %q for an unattributed abort, want %q",
			summary.AbortedBy, protocolcli.AbortedBySignal)
	}
}

// TestSummary_OmitsAnIncompleteFailureList covers the one case where the list
// and the count cannot be reconciled: the contract requires them to agree, so
// the member is dropped rather than emitted short.
func TestSummary_OmitsAnIncompleteFailureList(t *testing.T) {
	summary := Run{Session: &jobs.SessionResult{Status: jobs.TaskCounts{Failed: 2}}}.Summary(0)
	if summary.Failures != nil {
		t.Errorf("failures = %+v, want it omitted when it cannot be complete", summary.Failures)
	}
}

// TestEnvelope_ReportsTheVerdictOnce pins that the envelope's status, its exit
// code and its run summary are one verdict — the agreement the contract checks
// with outcome_mismatch and exit_code_mismatch.
func TestEnvelope_ReportsTheVerdictOnce(t *testing.T) {
	run := Run{Session: &jobs.SessionResult{Aborted: true, AbortedBy: jobs.AbortSignal}}
	envelope := run.Envelope("build,test", 3)

	if envelope.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d",
			envelope.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if envelope.Status != protocolcli.StatusAborted {
		t.Errorf("status = %q, want %q", envelope.Status, protocolcli.StatusAborted)
	}
	if envelope.Run == nil || envelope.Run.Outcome != envelope.Status {
		t.Fatalf("run summary does not report the envelope's verdict: %+v", envelope.Run)
	}
	if envelope.ExitCode != envelope.Run.ExitCode {
		t.Errorf("envelope exitCode %d disagrees with run.exitCode %d",
			envelope.ExitCode, envelope.Run.ExitCode)
	}
	if envelope.Error == nil || envelope.Error.Code != errorClassSignal {
		t.Fatalf("aborted envelope error = %+v, want the signal class", envelope.Error)
	}
	if envelope.Command != "build,test" {
		t.Errorf("command = %q, want the invoked command path", envelope.Command)
	}
}

// TestEnvelope_SuccessCarriesNoError is the other half of the same rule: an
// error member on a successful document is a violation, not extra detail.
func TestEnvelope_SuccessCarriesNoError(t *testing.T) {
	envelope := Run{Session: &jobs.SessionResult{}}.Envelope("build", 0)
	if envelope.Error != nil {
		t.Errorf("successful envelope carries error %+v", envelope.Error)
	}
	if envelope.ExitCode != protocolcli.ExitSuccess {
		t.Errorf("exitCode = %d, want 0", envelope.ExitCode)
	}
}

// TestTaskRecord_KeepsStatusAndReuseOrthogonal is decision 1 at the task level:
// reuse says how a result was obtained and never rewrites the verdict, so a
// restored failure stays failed and keeps its error.
func TestTaskRecord_KeepsStatusAndReuseOrthogonal(t *testing.T) {
	record := TaskRecord(Task{
		Identity: identityOfKey("/a:test"),
		Result: jobs.TaskResult{
			Status: jobs.TaskStatusFailed,
			Reuse:  jobs.ReuseRemoteCache,
			Error:  &jobs.JobError{Message: "2 assertions failed", Code: "TEST_FAILED"},
			Timing: jobs.TaskTiming{Duration: 850 * time.Millisecond},
		},
	})
	if record.Status != protocolcli.TaskStatusFailed {
		t.Errorf("status = %q, want failed", record.Status)
	}
	if record.Reuse != protocolcli.TaskReuseRemoteCache {
		t.Errorf("reuse = %q, want remote-cache", record.Reuse)
	}
	if record.Error == nil || record.Error.Message != "2 assertions failed" {
		t.Fatalf("error = %+v, want the task's own message", record.Error)
	}
	// The class vocabulary is closed: an extension's own code cannot leak into it.
	if record.Error.Code != errorClassFailure {
		t.Errorf("error.code = %q, want %q", record.Error.Code, errorClassFailure)
	}
	if record.DurationMs != 850 {
		t.Errorf("durationMs = %d, want 850", record.DurationMs)
	}
	if record.SpawnToFirstEventMs != 0 {
		t.Errorf("spawnToFirstEventMs = %d, want it absent for a task that never spawned",
			record.SpawnToFirstEventMs)
	}
}

// TestTaskRecord_ExecutedTaskSpellsReuseExplicitly: v2 has no empty-string
// reuse, so "did it run?" is answerable without knowing which absence means
// what.
func TestTaskRecord_ExecutedTaskSpellsReuseExplicitly(t *testing.T) {
	record := TaskRecord(Task{
		Identity: identityOfKey("/a:build"),
		Result: jobs.TaskResult{
			Status: jobs.TaskStatusSuccess,
			Timing: jobs.TaskTiming{SpawnToFirstEvent: 41 * time.Millisecond, FirstEventObserved: true},
		},
	})
	if record.Reuse != protocolcli.TaskReuseNone {
		t.Errorf("reuse = %q, want %q", record.Reuse, protocolcli.TaskReuseNone)
	}
	if record.Error != nil {
		t.Errorf("successful record carries error %+v", record.Error)
	}
	if record.SpawnToFirstEventMs != 41 {
		t.Errorf("spawnToFirstEventMs = %d, want 41", record.SpawnToFirstEventMs)
	}
}

// TestTaskRecord_CarriesTheInputDigestOnEveryKeyedRecord pins the projection of
// the canonical input digest: every reuse kind carries it, and a skipped record
// never does, even when a hand-built result names one.
func TestTaskRecord_CarriesTheInputDigestOnEveryKeyedRecord(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "task-input-digest", "absent-on-skipped-present-on-every-reuse-kind")
	digest := "sha256:" + strings.Repeat("4d", 32)
	for _, reuse := range []jobs.ReuseKind{jobs.ReuseNone, jobs.ReuseLocalCache, jobs.ReuseRemoteCache, jobs.ReuseCoalesced} {
		record := TaskRecord(Task{
			Identity: identityOfKey("/a:build"),
			Result:   jobs.TaskResult{Status: jobs.TaskStatusSuccess, Reuse: reuse, InputDigest: digest},
		})
		if record.InputDigest != digest {
			t.Errorf("reuse %q: inputDigest = %q, want %q", reuse, record.InputDigest, digest)
		}
	}
	for _, status := range []jobs.TaskStatus{jobs.TaskStatusSkipped, jobs.TaskStatusUnknown} {
		record := TaskRecord(Task{
			Identity: identityOfKey("/a:build"),
			Result:   jobs.TaskResult{Status: status, InputDigest: digest},
		})
		if record.InputDigest != "" {
			t.Errorf("status %q projects as %q and carries inputDigest %q, want it absent",
				status, record.Status, record.InputDigest)
		}
	}
}

func TestSummary_DockerPublicationFactsPreserveColdWarmDigestProvenance(t *testing.T) {
	coldDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	warmDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	coldPackage := identityOfKey("/services/cold:package~docker")
	coldPublish := identityOfKey("/services/cold:publish~docker")
	warmPackage := identityOfKey("/services/warm:package~docker")
	warmPublish := identityOfKey("/services/warm:publish~docker")

	run := Run{
		Session: &jobs.SessionResult{Status: jobs.TaskCounts{Succeeded: 4}},
		Tasks: []Task{
			{Identity: coldPackage, Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Timing: jobs.TaskTiming{TaskWall: 125 * time.Millisecond}}},
			{Identity: warmPackage, Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Timing: jobs.TaskTiming{TaskWall: 9 * time.Millisecond}}},
			{Identity: coldPublish, Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Publications: []jobs.Publication{{
				Registry: "docker", TargetRegistry: "ghcr.io", Name: "ghcr.io/acme/cold", Version: "2026.07.29-1",
				Tags: []string{"edge"}, ContentStatus: "pushed", CacheOutcome: "miss", ImageDigest: coldDigest, ImmutableRef: "ghcr.io/acme/cold@" + coldDigest,
				DigestVerified: true, PublishTimings: &runtimeproto.PublishTimings{CacheLookupMs: 4, CacheTransferMs: 11, RegistryPushMs: 11, ReferencePublishMs: 3, DigestResolveMs: 2},
			}}}},
			{Identity: warmPublish, Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Publications: []jobs.Publication{{
				Registry: "docker", TargetRegistry: "ghcr.io", Name: "ghcr.io/acme/warm", Version: "2026.07.29-1",
				Tags: []string{"edge"}, ContentStatus: "reused", CacheOutcome: "hit", ImageDigest: warmDigest, ImmutableRef: "ghcr.io/acme/warm@" + warmDigest,
				DigestVerified: true, DigestReused: true, PublishTimings: &runtimeproto.PublishTimings{CacheLookupMs: 2, ReferencePublishMs: 1, DigestResolveMs: 1},
			}}}},
		},
	}.WithPublishConcurrency(100, 2)

	summary := run.Summary(200)
	if len(summary.Publications) != 2 {
		t.Fatalf("publications = %+v, want cold and warm records", summary.Publications)
	}
	cold, warm := summary.Publications[0], summary.Publications[1]
	if cold.Identity.Key != coldPublish.Key || cold.ImageDigest != coldDigest || cold.CacheOutcome != "miss" || cold.DigestReused {
		t.Errorf("cold publication = %+v, want distinct cold digest/push facts", cold)
	}
	if cold.Timings.BuildMs != 125 || cold.Timings.RegistryPushMs != 11 {
		t.Errorf("cold timings = %+v, want package and registry timings", cold.Timings)
	}
	if warm.Identity.Key != warmPublish.Key || warm.ImageDigest != warmDigest || warm.CacheOutcome != "hit" || !warm.DigestReused {
		t.Errorf("warm publication = %+v, want explicit cache and digest reuse", warm)
	}
	if warm.Timings.BuildMs != 9 || warm.Timings.CacheTransferMs != 0 {
		t.Errorf("warm timings = %+v, want cached build and no layer transfer", warm.Timings)
	}
	if got := cold.Concurrency; got.ConfiguredCap != 100 || got.Effective != 2 {
		t.Errorf("concurrency = %+v, want configured 100 and observed 2", got)
	}

	for kind, document := range map[protocolcli.DocumentKind]any{
		protocolcli.DocumentResultEnvelope:      run.Envelope("publish", 200),
		protocolcli.DocumentSessionStreamRecord: run.SessionEnd(time.Now(), 200),
	} {
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal %s: %v", kind, err)
		}
		if violations := protocolcli.ValidateDocument(kind, encoded); len(violations) != 0 {
			t.Errorf("%s validation violations = %+v", kind, violations)
		}
	}
}

func TestSessionEnd_CarriesLocalCacheServingBreakdown(t *testing.T) {
	run := Run{Session: &jobs.SessionResult{
		Status: jobs.TaskCounts{Succeeded: 2},
		Reuse:  jobs.ReuseCounts{LocalCache: 2},
		Cache: &jobs.CacheStatsSnapshot{
			LocalHits: 2, LocalMisses: 0, LocalServedMs: 11_200,
			LocalKeysMs: 3_100, LocalBindingsMs: 7_400,
			LocalRestoreVerifyMs: 700, LocalSpawnedProcesses: 12,
		},
	}}
	record := run.SessionEnd(time.Now(), 12_000)
	if record.Run == nil || record.Run.Cache == nil || record.Run.Cache.Local == nil {
		t.Fatalf("session:end cache breakdown missing: %+v", record.Run)
	}
	local := record.Run.Cache.Local
	if local.Hits != 2 || local.ServedMs != 11_200 || local.BindingsMs != 7_400 || local.SpawnedProcesses != 12 {
		t.Fatalf("session:end local cache breakdown = %+v", local)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, encoded); len(violations) != 0 {
		t.Fatalf("session:end validation violations = %+v", violations)
	}
}

func TestSummary_DockerPublicationNeverPromotesATagToDigest(t *testing.T) {
	run := Run{
		Session: &jobs.SessionResult{Status: jobs.TaskCounts{Succeeded: 1}},
		Tasks: []Task{{
			Identity: identityOfKey("/services/api:publish~docker"),
			Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Publications: []jobs.Publication{{
				Registry: "docker", TargetRegistry: "ghcr.io", Name: "ghcr.io/acme/api", Version: "2026.07.29-1",
				ContentStatus: "pushed", CacheOutcome: "miss", ImageDigest: "latest", DigestVerified: true,
			}}},
		}},
	}.WithPublishConcurrency(4, 1)
	if got := run.Summary(1).Publications; len(got) != 0 {
		t.Errorf("publications = %+v, want a mutable tag omitted", got)
	}
}

func TestTerminalDockerPublication_RequiresConsistentCacheProvenance(t *testing.T) {
	publication := jobs.Publication{
		Registry:       "docker",
		TargetRegistry: "ghcr.io",
		Name:           "ghcr.io/acme/api",
		ImageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImmutableRef:   "ghcr.io/acme/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DigestVerified: true,
	}
	for _, tc := range []struct {
		name          string
		contentStatus string
		cacheOutcome  string
		digestReused  bool
		want          bool
	}{
		{name: "cache hit reuses a retagged digest", contentStatus: "retagged", cacheOutcome: "hit", digestReused: true, want: true},
		{name: "cache hit reuses exact immutable content", contentStatus: "reused", cacheOutcome: "hit", digestReused: true, want: true},
		{name: "cache miss pushes new content", contentStatus: "pushed", cacheOutcome: "miss", want: true},
		{name: "cache hit cannot claim a fresh push", contentStatus: "pushed", cacheOutcome: "hit", digestReused: true},
		{name: "cache hit requires digest reuse", contentStatus: "retagged", cacheOutcome: "hit"},
		{name: "cache miss cannot claim a retag", contentStatus: "retagged", cacheOutcome: "miss", digestReused: true},
		{name: "cache miss cannot reuse a digest", contentStatus: "pushed", cacheOutcome: "miss", digestReused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publication.ContentStatus = tc.contentStatus
			publication.CacheOutcome = tc.cacheOutcome
			publication.DigestReused = tc.digestReused
			if got := terminalDockerPublication(publication); got != tc.want {
				t.Errorf("terminalDockerPublication(%+v) = %v, want %v", publication, got, tc.want)
			}
		})
	}
}
