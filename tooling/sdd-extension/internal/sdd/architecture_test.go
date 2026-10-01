package sdd

import (
	"errors"
	"path/filepath"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The three builders below were extracted from core's ArchitectureValidate /
// Snapshot / InspectCommand when the engines moved into this extension.
// Core's command tests assert the same facts through captured
// stdout and a decoded ResultV2 envelope; those stay with the command layer
// that owns rendering. What is pinned here is what the extraction created: the
// report each builder returns and the error class it fails with.

func TestArchitectureBuildersShareTheValidatedSnapshot(t *testing.T) {
	ws := architectureBuildersWorkspace(t, true)

	validation, err := BuildArchitectureValidationResult(ws, "")
	if err != nil {
		t.Fatalf("architecture validate: %v", err)
	}
	if !validation.Valid || validation.Summary.Domains != 2 || validation.Summary.ObservedEdges != 1 || len(validation.Findings) != 0 {
		t.Fatalf("validation data = %+v", validation)
	}
	if validation.Coverage == nil || validation.Coverage.ProjectDependencies != "enforced-for-mapped-projects" || validation.Coverage.Events != "not-detected" {
		t.Fatalf("validation coverage = %+v", validation.Coverage)
	}

	inspection, err := BuildArchitectureInspectionResult(ws, "consumer", "")
	if err != nil {
		t.Fatalf("architecture inspect: %v", err)
	}
	if inspection.Domain == nil || inspection.Domain.ID != "consumer" || len(inspection.Inbound) != 1 || len(inspection.Outbound) != 0 || len(inspection.Observed) != 1 {
		t.Fatalf("inspection data = %+v", inspection)
	}

	snapshot, err := BuildArchitectureSnapshotResult(ws, "")
	if err != nil {
		t.Fatalf("architecture snapshot: %v", err)
	}
	if snapshot.Snapshot == nil || len(snapshot.Snapshot.Graph.Domains) != 2 || len(snapshot.Snapshot.Graph.Edges) != 1 {
		t.Fatalf("snapshot data = %+v", snapshot)
	}
	if snapshot.Snapshot.Coverage.ProjectDependencies != "enforced-for-mapped-projects" || snapshot.Snapshot.Coverage.HTTP != "not-detected" {
		t.Fatalf("snapshot coverage = %+v", snapshot.Snapshot.Coverage)
	}
}

func TestArchitectureValidateCarriesRatchetedFailureData(t *testing.T) {
	ws := architectureBuildersWorkspace(t, false)

	report, err := BuildArchitectureValidationResult(ws, "")
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("architecture validate error = %v, want ErrInvalidConfig", err)
	}
	attached, ok := ResultData(err).(ArchitectureValidationReport)
	if !ok {
		t.Fatalf("failure data = %T, want ArchitectureValidationReport", ResultData(err))
	}
	if report.Valid || len(report.Findings) != 1 || report.Findings[0].Code != archproto.ErrorCodeUndeclaredProjectDependency {
		t.Fatalf("failure report = %+v", report)
	}
	if len(attached.Findings) != len(report.Findings) || attached.Findings[0].ID != report.Findings[0].ID {
		t.Fatalf("attached report = %+v, want the returned one", attached)
	}
}

func TestArchitectureInspectRequiresAnExactDeclaredDomain(t *testing.T) {
	ws := architectureBuildersWorkspace(t, true)

	report, err := BuildArchitectureInspectionResult(ws, "missing", "")
	if !errors.Is(err, protocolcli.ErrNoMatch) {
		t.Fatalf("architecture inspect error = %v, want ErrNoMatch", err)
	}
	attached, ok := ResultData(err).(ArchitectureInspectionReport)
	if !ok || attached.Domain != nil || len(attached.Diagnostics) != 1 || attached.Diagnostics[0].Code != archproto.ErrorCodeUnknownDomain {
		t.Fatalf("inspection failure = %+v", ResultData(err))
	}
	if report.Domain != nil {
		t.Fatalf("inspection report = %+v, want no domain", report)
	}
}

// TestArchitectureBuildersRequireAWorkspace pins the wire boundary: an
// evaluation with no membership reports that, rather than discovering one.
func TestArchitectureBuildersRequireAWorkspace(t *testing.T) {
	report, err := BuildArchitectureValidationResult(nil, "")
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("validate without a workspace = %v, want ErrInvalidConfig", err)
	}
	if report.Valid || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != ErrorCodeReadFailure {
		t.Fatalf("report = %+v, want one read-failure diagnostic", report)
	}
}

func architectureBuildersWorkspace(t *testing.T, binding bool) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", archproto.ManifestFilename), architectureConsumerManifest(binding))
	// Core read this membership out of the two putnami.json files through
	// workspace.Load. An extension is handed it on the wire, so the fixture
	// states the same two members and the same declared edge directly.
	return workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Type: "library", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Type: "application", Path: "consumer", Dependencies: []string{"producer"}},
	})
}

func architectureProducerManifest() string {
	return `{
  "protocolVersion": 1,
  "domain": "producer",
  "owner": "producer-team",
  "projects": ["/producer"],
  "exports": [{
    "id": "producer.reference.v1",
    "version": 1,
    "status": "active",
    "description": "Stable producer reference.",
    "facts": [{"name":"id","authority":"producer","classification":"internal","personalData":"none"}],
    "modes": ["reference"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": []
}`
}

func architectureConsumerManifest(binding bool) string {
	bindings := ""
	if binding {
		bindings = `,
    "bindings": [{"kind":"project-dependency","consumerProject":"/consumer","producerProject":"/producer"}]`
	}
	return `{
  "protocolVersion": 1,
  "domain": "consumer",
  "owner": "consumer-team",
  "projects": ["/consumer"],
  "exports": [],
  "imports": [{
    "id": "consumer.producer-reference.v1",
    "version": 1,
    "from": {"domain":"producer","export":"producer.reference.v1"},
    "as": "consumer.producer-reference",
    "mode": "reference",
    "status": "active",
    "facts": ["id"],
    "justification": "The consumer stores the authoritative producer identity only"` + bindings + `
  }]
}`
}
