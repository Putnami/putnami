package sessions

import (
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

func v2Identity(projectID, projectName, task, kind, ext string) protocolcli.TaskIdentity {
	id := protocolcli.TaskIdentity{
		Scope:    protocolcli.TaskScopeProject,
		Project:  protocolcli.ProjectIdentity{ID: projectID, Name: projectName},
		Task:     protocolcli.TaskRef{Name: task, Command: "build", Kind: kind},
		Provider: protocolcli.ProviderIdentity{Extension: ext},
	}
	id.Key = id.DerivedKey()
	return id
}

// TestSessionMetaFromV2_ProjectsTheV1View pins the B1a reader
// projection: a v2 session document must satisfy the same consumer (inspect) the
// v1 shape satisfied — counts folded strictly, reuse
// folded into the v1 "cached" bucket, and per-task rows recovering the v1
// outcome vocabulary.
func TestSessionMetaFromV2_ProjectsTheV1View(t *testing.T) {
	doc := &protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       "20260728-000000-test",
		StartTime:       "2026-07-28T00:00:00Z",
		EndTime:         "2026-07-28T00:00:09Z",
		Commands:        []string{"build"},
		Git:             &protocolcli.SessionGit{Branch: "main", Baseline: "abc"},
		Scheduler:       map[string]any{"parallelism": float64(4)},
		Run: protocolcli.RunSummary{
			Outcome:  protocolcli.RunOutcomeFailure,
			ExitCode: protocolcli.ExitFailure,
			Counts:   protocolcli.RunCounts{Total: 3, Succeeded: 1, Failed: 2},
			Reuse:    protocolcli.RunReuse{LocalCache: 1, RemoteCache: 1},

			DurationMs: 9000,
		},
		Tasks: []protocolcli.TaskRecord{
			{Identity: v2Identity("/app", "app", "build", "build", "@putnami/go"),
				Status: protocolcli.TaskStatusSuccess, Reuse: protocolcli.TaskReuseLocalCache, DurationMs: 5, TaskWallMs: 7},
			{Identity: v2Identity("/lib", "lib", "build", "build", "@putnami/go"),
				Status: protocolcli.TaskStatusFailed, Reuse: protocolcli.TaskReuseNone, ExitCode: 1, DurationMs: 6000, SpawnToFirstEventMs: 3},
			{Identity: v2Identity("/x", "x", "build", "build", "@putnami/go"),
				Status: protocolcli.TaskStatusFailed, Reuse: protocolcli.TaskReuseNone, ExitCode: 1, DurationMs: 4000},
		},
	}
	meta := sessionMetaFromV2(doc)

	if meta.ID != doc.SessionID || meta.Duration != 9000 || meta.Git == nil || meta.Git.Baseline != "abc" {
		t.Fatalf("meta header = %+v", meta)
	}
	// Parallel execution can make the session wall time shorter than the sum of
	// executed task durations. Reused tasks are excluded from the v1 aggregate.
	wantStats := &workspace_state.SessionStats{Total: 3, Succeeded: 1, Failed: 2, Cached: 2, DurationMs: 10000}
	if !reflect.DeepEqual(meta.Stats, wantStats) {
		t.Fatalf("stats = %+v, want %+v (reuse folded into the v1 cached bucket)", meta.Stats, wantStats)
	}
	if len(meta.Jobs) != 3 {
		t.Fatalf("jobs = %+v, want 3 rows", meta.Jobs)
	}
	if meta.Jobs[0].Outcome != "cached" || meta.Jobs[0].Status != "success" || meta.Jobs[0].Key != "/app:build" {
		t.Errorf("reused row = %+v, want the v1 cached outcome over the stored status", meta.Jobs[0])
	}
	if meta.Jobs[1].Outcome != "failed" || meta.Jobs[1].SpawnToFirstEventMs == nil || *meta.Jobs[1].SpawnToFirstEventMs != 3 {
		t.Errorf("executed row = %+v, want status-as-outcome and the spawn overhead preserved", meta.Jobs[1])
	}

}
