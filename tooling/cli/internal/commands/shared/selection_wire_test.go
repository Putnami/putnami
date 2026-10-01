package shared

import (
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	protocoljob "go.putnami.dev/protocol/job"
)

// TestResolvedSelectionMarshalsLikeTheWireContract is the DRIFT PIN between the
// CLI's resolved selection and the job context contract's `selection` member.
//
// The two types are declared separately on purpose: ResolvedSelection carries
// resolved *workspace.Project values, so protocols/job cannot import it without
// taking on the CLI's workspace loader. What must not diverge is the BYTES —
// same member names, same order, same omitempty behavior — because an extension
// reading `selection` off the wire and a first-party command reading
// ResolvedSelection are answering one question with one resolver. A renamed
// tag, a reordered field or a dropped omitempty fails here instead of shipping
// a wire block no consumer can read the same way twice.
//
// The matrix is exhaustive over the omitempty members rather than one happy
// case: those are exactly the branches where two independently declared structs
// can agree on every populated document and still disagree on an empty one.
func TestResolvedSelectionMarshalsLikeTheWireContract(t *testing.T) {
	cases := []struct {
		name     string
		resolved ResolvedSelection
	}{
		{
			name:     "zero value",
			resolved: ResolvedSelection{},
		},
		{
			name: "unscoped whole workspace",
			resolved: ResolvedSelection{
				Mode:       SelectionModeAll,
				ProjectIDs: []string{"/apps/console", "/libs/widget"},
			},
		},
		{
			name: "empty project list is not a nil one",
			resolved: ResolvedSelection{
				Mode:       SelectionModeAll,
				ProjectIDs: []string{},
			},
		},
		{
			name: "nil project list",
			resolved: ResolvedSelection{
				Mode:       SelectionModeAll,
				ProjectIDs: nil,
			},
		},
		{
			name: "explicit selector",
			resolved: ResolvedSelection{
				Mode:       SelectionModeProjects,
				Scoped:     true,
				ProjectIDs: []string{"/apps/console"},
			},
		},
		{
			name: "impacted with baseline and tier",
			resolved: ResolvedSelection{
				Mode:           SelectionModeImpacted,
				Scoped:         true,
				Baseline:       "origin/main",
				BaselineSource: "upstream",
				ProjectIDs:     []string{"/apps/console"},
			},
		},
		{
			name: "impacted with a baseline but no tier",
			resolved: ResolvedSelection{
				Mode:       SelectionModeImpacted,
				Scoped:     true,
				Baseline:   "HEAD~1",
				ProjectIDs: []string{"/apps/console"},
			},
		},
		{
			name: "impacted with a tier but no baseline",
			resolved: ResolvedSelection{
				Mode:           SelectionModeImpacted,
				Scoped:         true,
				BaselineSource: "local-trunk",
				ProjectIDs:     []string{"/apps/console"},
			},
		},
		{
			name: "the impacted no-op",
			resolved: ResolvedSelection{
				Mode:        SelectionModeImpacted,
				Scoped:      true,
				Baseline:    "origin/main",
				ProjectIDs:  []string{},
				EmptyImpact: true,
			},
		},
		{
			name: "emptyImpact false is omitted",
			resolved: ResolvedSelection{
				Mode:        SelectionModeImpacted,
				Scoped:      true,
				Baseline:    "origin/main",
				ProjectIDs:  []string{"/apps/console"},
				EmptyImpact: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.resolved)
			if err != nil {
				t.Fatalf("marshal ResolvedSelection: %v", err)
			}
			got, err := json.Marshal(tc.resolved.Wire())
			if err != nil {
				t.Fatalf("marshal job.Selection: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("wire block drifted from the resolved selection:\n job.Selection      = %s\n ResolvedSelection  = %s",
					got, want)
			}
		})
	}
}

// TestSelectionModesMatchTheWireContract pins the vocabulary itself. The mode
// strings are what a consumer switches on, so a rename on either side must fail
// here rather than silently produce a mode no extension recognizes.
func TestSelectionModesMatchTheWireContract(t *testing.T) {
	for _, pair := range []struct{ local, wire string }{
		{SelectionModeAll, protocoljob.SelectionModeAll},
		{SelectionModeProjects, protocoljob.SelectionModeProjects},
		{SelectionModeImpacted, protocoljob.SelectionModeImpacted},
	} {
		if pair.local != pair.wire {
			t.Errorf("selection mode %q, wire contract says %q", pair.local, pair.wire)
		}
	}
}

// TestResolvedSelectionWireIsAcceptedByTheContract closes the loop the byte
// comparison above leaves open: identical bytes are only useful if the contract
// ACCEPTS them. Every shape ResolveProjectSelection can produce must validate,
// or a first-party command would resolve a selection the orchestrator could not
// put on the wire.
func TestResolvedSelectionWireIsAcceptedByTheContract(t *testing.T) {
	cases := []ResolvedSelection{
		{Mode: SelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
		{Mode: SelectionModeAll, ProjectIDs: []string{}},
		{Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"}},
		{
			Mode: SelectionModeImpacted, Scoped: true,
			Baseline: "origin/main", BaselineSource: "upstream",
			ProjectIDs: []string{"/a", "/b"},
		},
		{
			Mode: SelectionModeImpacted, Scoped: true, Baseline: "origin/main",
			ProjectIDs: []string{}, EmptyImpact: true,
		},
	}
	for _, resolved := range cases {
		wire := resolved.Wire()
		ctx := &protocoljob.Context{
			ProtocolVersion: protocoljob.ProtocolVersion2,
			WorkspaceRoot:   "/ws",
			OutputPath:      "/ws/out",
			CacheRoot:       "/ws/cache",
			Workspace:       protocoljob.Workspace{Name: "putnami"},
			Project:         protocoljob.Project{Name: "p", Path: "p", FullPath: "/ws/p"},
			Extension:       protocoljob.Extension{Name: "@putnami/sdd", Root: "/ws/e"},
			Job:             protocoljob.Job{Name: "validate"},
			Identity: &protocoljob.TaskIdentity{
				Key:      "/p:validate",
				Scope:    protocoljob.TaskScopeProject,
				Project:  protocoljob.ProjectIdentity{ID: "/p", Name: "p"},
				Task:     protocoljob.TaskRef{Name: "validate", Command: "validate", Kind: "validate"},
				Provider: protocoljob.ProviderIdentity{Extension: "@putnami/sdd"},
			},
			Params:    protocoljob.Params{},
			Selection: &wire,
		}
		if diags := protocoljob.Validate(ctx); diag.HasErrors(diags) {
			t.Errorf("the contract rejects a resolvable selection %+v: %v", resolved, diags)
		}
	}
}
