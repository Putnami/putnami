package sdd

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// The cloud-pilot conformance fixtures, authored in Go.
//
// They are not this repository's domains — they are
// `protocols/architecture/fixtures/valid/cloud-pilot/`, the documents the
// protocol's own conformance suite validates — and they are pinned here for one
// reason: they carry shapes an adopting repository needs and this repository's
// five manifests do not.
//
//   - a PLANNED import, with planned carriers, which the protocol forbids from
//     claiming any current project binding;
//   - a projection with the whole consistency block: a freshness bound, ordering
//     with an idempotency key, a late-event rule, tombstone deletion, and a local
//     model whose projected fields are separate from a locally authoritative one.
//
// A builder that could not express those would fail an adopter, and the gap
// would only surface in that adopter's repository. Holding the fixtures to the
// same authoring proves the coverage here instead.
//
// # Why here
//
// A pin belongs in a project that owns the file, and `protocols/architecture`
// cannot host one: it is a published Go module and the authoring runs through the
// unpublished extension SDK. The same reason keeps the `protocols` domain's own
// pin in this file's sibling — see architecture_pin_test.go.

func authoredCloudPilotRuntime() *arch.Builder {
	return arch.NewDomain("runtime", "runtime").
		Projects(
			"/runtime/libs/workspace-runtime",
		).
		Owns(
			arch.Concept("runtime.workspace-telemetry-binding", archproto.OwnershipModel,
				"Authoritative placement facts needed to bind workspace telemetry to runtime resources."),
		).
		Export(archproto.Export{
			ID:          "runtime.workspace-telemetry-binding.v1",
			Version:     1,
			Status:      archproto.StatusPlanned,
			Description: "Minimal runtime placement facts Observability may project locally.",
			Facts: []archproto.Fact{
				arch.Fact("workspace_id", "runtime",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
				arch.Fact("runtime_project", "runtime",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
				arch.Fact("region", "runtime",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
				arch.Fact("logging_scope", "runtime",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
				arch.Fact("source_version", "runtime",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeProjection, archproto.ModeQuery},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import()
}

func authoredCloudPilotObservability() *arch.Builder {
	return arch.NewDomain("observability", "observability").
		Projects(
			"/observability/workloads/telemetry-api",
		).
		Owns(
			arch.Concept("observability.workspace-context", archproto.OwnershipModel,
				"Read-only local telemetry context plus separately identified Observability-owned fields."),
			arch.Concept("observability.retention-policy", archproto.OwnershipFact,
				"Observability-owned retention configuration; never projected from Runtime."),
		).
		Import(
			arch.Projection("observability.runtime-workspace-context.v1", 1, arch.From("runtime", "runtime.workspace-telemetry-binding.v1"), arch.Carried(archproto.TransportAPI, "runtime.workspace-telemetry-bindings.v1", archproto.StatusPlanned), arch.Carried(archproto.TransportEvent, "runtime.workspace-telemetry-binding-changed.v1", archproto.StatusPlanned),
				arch.As("observability.workspace-context"),
				arch.Status(archproto.StatusPlanned),
				arch.Facts(
					"workspace_id",
					"runtime_project",
					"region",
					"logging_scope",
					"source_version",
				),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "5m",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureFailClosed,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "source_version",
					IdempotencyKey: "event_id",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Tombstones("deleted_at"),
				arch.Model(archproto.LocalModel{
					Name:            "observability.workspace-context",
					Kind:            archproto.LocalModelProjection,
					SourceIdentity:  "workspace_id",
					ProjectedFields: []string{"workspace_id", "runtime_project", "region", "logging_scope", "source_version"},
					LocalFields:     []string{"retention_policy"},
					ProvenanceField: "source_contract",
					ObservedAtField: "observed_at",
					FreshnessField:  "freshness_state",
					Writer:          "observability.workspace-context-projector",
					Rebuildable:     true,
					Rebuild:         archproto.RebuildBootstrapAndReplay,
				}),
				arch.Justification("Observability needs local telemetry routing facts without making Control or Runtime a per-request lookup service."),
			),
		)
}

// TestCloudPilotFixturesAreAuthorable proves the builder covers the cloud-pilot
// shapes. The committed fixtures are unchanged: the pin compares both documents
// in the protocol's canonical form, so a conformance corpus keeps the byte layout
// its own tests were written against.
func TestCloudPilotFixturesAreAuthorable(t *testing.T) {
	const root = "../../protocols/architecture/fixtures/valid/cloud-pilot/"
	arch.Pin(t, root+"runtime.json", authoredCloudPilotRuntime)
	arch.Pin(t, root+"observability.json", authoredCloudPilotObservability)
}

// TestCloudPilotAuthoringCarriesTheShapesThisRepositoryLacks states what the pin
// above is FOR. Without it the two fixtures would be pinned bytes and nobody
// would notice if the builder silently stopped covering a mode.
func TestCloudPilotAuthoringCarriesTheShapesThisRepositoryLacks(t *testing.T) {
	observability, err := authoredCloudPilotObservability().Build()
	if err != nil {
		t.Fatal(err)
	}
	projection := observability.Imports[0]
	if projection.Mode != archproto.ModeProjection || projection.Status != archproto.StatusPlanned {
		t.Fatalf("import = mode %q status %q, want a planned projection", projection.Mode, projection.Status)
	}
	if len(projection.Bindings) != 0 {
		t.Errorf("bindings = %+v; a planned target cannot claim a current project binding", projection.Bindings)
	}
	if projection.Bootstrap.Availability != archproto.StatusPlanned || projection.Updates.Availability != archproto.StatusPlanned {
		t.Errorf("carriers = %+v/%+v, want both planned", projection.Bootstrap, projection.Updates)
	}
	if projection.Consistency == nil || projection.Consistency.IdempotencyKey == "" ||
		projection.Consistency.Ordering == archproto.OrderingNone {
		t.Errorf("consistency = %+v, want the full ordered block a projection requires", projection.Consistency)
	}
	if projection.Deletion == nil || projection.Deletion.Strategy != archproto.DeletionTombstone {
		t.Errorf("deletion = %+v, want tombstone with its marker field", projection.Deletion)
	}
	if model := projection.LocalModel; model == nil || len(model.LocalFields) == 0 {
		t.Errorf("local model = %+v, want locally authoritative fields kept separate from the projected ones", model)
	}
}
