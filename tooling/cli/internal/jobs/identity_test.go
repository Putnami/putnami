package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestTypedIdentity_KeyIsTheDerivedView pins B2a's central promise: the string
// Key() every map, cache lookup and session record uses is byte-identical to
// the typed identity's derived key, on every node of a REAL plan. If this
// moves, a v2 consumer's join string and the v1 plan key have forked.
func TestTypedIdentity_KeyIsTheDerivedView(t *testing.T) {
	t.Parallel()
	ws, ext := makeIntegrationWorkspace(t)
	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(planned) == 0 {
		t.Fatal("fixture planned nothing")
	}
	for _, job := range planned {
		id := job.TypedIdentity()
		if id.Key != job.Key() {
			t.Errorf("identity key %q != plan key %q", id.Key, job.Key())
		}
		if id.Key != id.DerivedKey() {
			t.Errorf("identity key %q disagrees with its own structured fields (%q)", id.Key, id.DerivedKey())
		}
		if job.Identity == nil {
			t.Errorf("%s: Plan did not stamp the identity", job.Key())
		}
	}
}

// TestTypedIdentity_StampedEqualsDerived pins that stamping changes nothing: a
// node's stamped identity is exactly what the pure derivation returns, so
// nodes assembled outside Plan (tests, replans) cannot disagree with planned
// ones.
func TestTypedIdentity_StampedEqualsDerived(t *testing.T) {
	t.Parallel()
	ws, ext := makeIntegrationWorkspace(t)
	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, job := range planned {
		stamped := job.TypedIdentity()
		job.Identity = nil // force the lazy derivation
		if derived := job.TypedIdentity(); stamped != derived {
			t.Errorf("%s: stamped %+v != derived %+v", job.Key(), stamped, derived)
		}
	}
}

// TestPlan_RejectsAmbiguousProviders pins the B2a plan-time guard: two jobs
// claiming one identity is an error naming both providers, never a silent
// last-wins collapse in the DAG and result maps. The positive control is every
// other test in this package planning successfully.
func TestPlan_RejectsAmbiguousProviders(t *testing.T) {
	t.Parallel()
	proj := &workspace.Project{ID: "/app", Name: "app"}
	mk := func(extName string) *ScheduledJob {
		return &ScheduledJob{
			Project:   proj,
			Extension: &extension.ExtensionDescription{Name: extName},
			JobDef:    &extension.JobDefinition{Name: "build"},
		}
	}
	err := validatePlanDAG([]*ScheduledJob{mk("@a/ext"), mk("@b/ext")})
	if err == nil {
		t.Fatal("two jobs sharing key /app:build validated cleanly")
	}
	for _, want := range []string{"ambiguous", "/app:build", "@a/ext", "@b/ext"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestPlan_DeterministicIdentitySnapshot pins plan compilation determinism:
// two Plans over the same workspace produce identical identity+edge JSON, in
// identical order. A nondeterministic plan would move cache keys, session
// records and the v2 plan document run to run.
func TestPlan_DeterministicIdentitySnapshot(t *testing.T) {
	t.Parallel()
	ws, ext := makeIntegrationWorkspace(t)
	snapshot := func() string {
		planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		type row struct {
			Identity  protocolcli.TaskIdentity `json:"identity"`
			DependsOn []string                 `json:"dependsOn"`
			After     []string                 `json:"after"`
		}
		rows := make([]row, 0, len(planned))
		for _, job := range planned {
			rows = append(rows, row{job.TypedIdentity(), job.DependsOn, job.SerializeAfter})
		}
		data, err := json.MarshalIndent(rows, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	first := snapshot()
	for i := 0; i < 5; i++ {
		if next := snapshot(); next != first {
			t.Fatalf("plan snapshot is nondeterministic:\n--- first ---\n%s\n--- run %d ---\n%s", first, i+2, next)
		}
	}
}
