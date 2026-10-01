package sdd

import (
	"os"
	"path/filepath"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The framework-evidence detector: committed capability manifests joined to the
// declarations they claim to implement.
//
// The reads are the same shape as every other input of `architecture-validate` —
// exact committed files under a project root, no git and no network — which is
// what keeps the cached verdict honest.

// capabilityManifestWith writes a project's committed capability manifest
// carrying one domain-access row.
func capabilityManifestWith(t *testing.T, root, project, importID, mode string) {
	capabilityManifestOwnedBy(t, root, project, project, importID, mode)
}

func capabilityManifestOwnedBy(t *testing.T, root, containerProject, ownerProject, importID, mode string) {
	t.Helper()
	writeArchitectureTestFile(t, filepath.Join(root, containerProject, "schema", "capabilities.json"), `{
  "protocolVersion": 2,
  "project": "`+containerProject+`",
  "domainAccess": [
    {
      "identity": { "ownerProject": "`+ownerProject+`", "kind": "domainAccess", "subkind": "`+mode+`", "key": "`+importID+`" },
      "import": "`+importID+`",
      "mode": "`+mode+`",
      "status": "active",
      "provenance": {
        "project": "`+ownerProject+`",
        "sourceKind": "framework",
        "declaration": { "root": "project", "path": "darc.go" }
      }
    }
  ]
}
`)
}

// TestDetectFrameworkEvidenceAttributesAggregatedRowsToTheirIdentityOwner pins
// that a dependency contribution copied into a workload manifest keeps its own
// project and domain. The container is discovery metadata, never semantic
// ownership. Unknown owners stay outside coverage rather than inheriting the
// container's domain.
func TestDetectFrameworkEvidenceAttributesAggregatedRowsToTheirIdentityOwner(t *testing.T) {
	for _, testCase := range []struct {
		name, owner string
		wantRecords int
	}{
		{name: "resolved project name", owner: "producer", wantRecords: 1},
		{name: "canonical project ID", owner: "/producer", wantRecords: 1},
		{name: "unknown owner", owner: "missing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			sources := evidenceSources(t, root)
			ws := evidenceWorkspace(t, root)
			capabilityManifestOwnedBy(t, root, "consumer", testCase.owner,
				"producer.aggregated-reference.v1", "reference")

			records, diagnostics := DetectFrameworkEvidence(ws, sources)
			if len(diagnostics) != 0 {
				t.Fatalf("diagnostics = %+v, want none", diagnostics)
			}
			if len(records) != testCase.wantRecords {
				t.Fatalf("records = %+v, want %d", records, testCase.wantRecords)
			}
			if testCase.wantRecords == 0 {
				return
			}
			if records[0].ConsumerDomain != "producer" || records[0].ConsumerProject != "/producer" {
				t.Errorf("record = %+v, want the identity owner's project and domain", records[0])
			}
		})
	}
}

func evidenceWorkspace(t *testing.T, root string) *workspace.Workspace {
	t.Helper()
	return workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer"}},
		{ID: "/unmapped", Name: "unmapped", Path: "unmapped"},
	})
}

func evidenceSources(t *testing.T, root string) []archproto.ManifestSource {
	t.Helper()
	writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", archproto.ManifestFilename), architectureConsumerManifest(true))
	return Discover(root).Sources
}

// TestDetectFrameworkEvidenceReadsCommittedManifests pins what the detector
// reads and what it refuses to guess: a mapped project's committed manifest
// becomes a record attributed to its domain, and a project no domain maps stays
// outside coverage rather than being attributed by path.
func TestDetectFrameworkEvidenceReadsCommittedManifests(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"evidence-is-read-from-committed-files-and-unmapped-projects-stay-outside")
	root := t.TempDir()
	sources := evidenceSources(t, root)
	ws := evidenceWorkspace(t, root)

	if records, diagnostics := DetectFrameworkEvidence(ws, sources); len(records) != 0 || len(diagnostics) != 0 {
		t.Fatalf("records = %+v, diagnostics = %+v; a project with no manifest implements nothing", records, diagnostics)
	}

	capabilityManifestWith(t, root, "consumer", "consumer.producer-reference.v1", "reference")
	// A manifest under a project no domain maps must not contribute: attributing
	// it would mean guessing the domain from a path.
	capabilityManifestWith(t, root, "unmapped", "unmapped.something.v1", "reference")

	records, diagnostics := DetectFrameworkEvidence(ws, sources)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diagnostics)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want only the mapped project's row", records)
	}
	record := records[0]
	if record.Kind != archproto.EvidenceFrameworkPrimitive || record.ConsumerDomain != "consumer" ||
		record.ConsumerProject != "/consumer" || record.Import != "consumer.producer-reference.v1" ||
		record.Mode != archproto.ModeReference {
		t.Errorf("record = %+v, want the row attributed to the domain that maps the project", record)
	}
}

// TestDetectFrameworkEvidenceReportsAnUnreadableManifest pins that a broken
// producer artifact is a diagnostic, not an absence. Treating it as "implements
// nothing" would turn a failed build into a clean architecture verdict.
func TestDetectFrameworkEvidenceReportsAnUnreadableManifest(t *testing.T) {
	root := t.TempDir()
	sources := evidenceSources(t, root)
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", "schema", "capabilities.json"), `{"protocolVersion": 2`)

	records, diagnostics := DetectFrameworkEvidence(evidenceWorkspace(t, root), sources)
	if len(records) != 0 {
		t.Errorf("records = %+v, want none from a manifest that does not parse", records)
	}
	if len(diagnostics) == 0 {
		t.Fatal("an unparseable capability manifest produced no diagnostic")
	}
	if diagnostics[0].Field == "" {
		t.Errorf("diagnostic = %+v, want the file it came from named", diagnostics[0])
	}
}

// TestEvaluateWorkspaceJoinsEvidenceToDeclarations is the detector end to end:
// the two findings and the coverage tier, over a real workspace.
func TestEvaluateWorkspaceJoinsEvidenceToDeclarations(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"the-evaluation-reports-both-evidence-findings-and-the-framework-tier")
	root := t.TempDir()
	evidenceSources(t, root)
	ws := evidenceWorkspace(t, root)

	// Nothing implemented: the domain is outside evidence coverage, so its
	// declaration is not held to an implementation and the tier stays absent.
	result := EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
	if result.Snapshot == nil || len(result.Snapshot.Findings) != 0 {
		t.Fatalf("findings = %+v, want none before any project implements anything", result.Snapshot)
	}
	if got := result.Snapshot.Coverage.DomainAccess; got != archproto.CoverageNotDetected {
		t.Errorf("coverage.domainAccess = %q, want %q", got, archproto.CoverageNotDetected)
	}

	// The declaration implemented: still clean, and the tier moves.
	capabilityManifestWith(t, root, "consumer", "consumer.producer-reference.v1", "reference")
	result = EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
	if result.Snapshot == nil || len(result.Snapshot.Findings) != 0 {
		t.Fatalf("findings = %+v, want none when the implementation matches the declaration", result.Snapshot.Findings)
	}
	if got := result.Snapshot.Coverage.DomainAccess; got != archproto.CoverageFrameworkEvidence {
		t.Errorf("coverage.domainAccess = %q, want %q", got, archproto.CoverageFrameworkEvidence)
	}
	if len(result.Snapshot.Evidence) != 1 {
		t.Errorf("snapshot evidence = %+v, want the record it compared", result.Snapshot.Evidence)
	}

	// An implementation nobody declared always fails, whatever else is true.
	capabilityManifestWith(t, root, "consumer", "consumer.invented.v1", "projection")
	result = EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
	codes := map[string]int{}
	for _, finding := range result.Snapshot.Findings {
		codes[finding.Code]++
	}
	if codes[archproto.ErrorCodeEvidenceWithoutDeclaration] != 1 {
		t.Errorf("findings = %+v, want one %s", result.Snapshot.Findings, archproto.ErrorCodeEvidenceWithoutDeclaration)
	}
	// The declared import is now unimplemented — the manifest was replaced —
	// inside a domain that implements something, so it is expected too.
	if codes[archproto.ErrorCodeDeclaredWithoutEvidence] != 1 {
		t.Errorf("findings = %+v, want one %s", result.Snapshot.Findings, archproto.ErrorCodeDeclaredWithoutEvidence)
	}
	if !archproto.HasBlockingFindings(result.Snapshot.Findings) {
		t.Error("evidence findings do not block; an unreviewed contract in code must fail the gate")
	}
}

// TestEvaluateWorkspaceReadsNoGitForEvidence pins that the evidence half obeys
// the same rule the rest of the cached task does: everything it reads is a
// committed file the key already names.
func TestEvaluateWorkspaceReadsNoGitForEvidence(t *testing.T) {
	root := t.TempDir()
	evidenceSources(t, root)
	capabilityManifestWith(t, root, "consumer", "consumer.producer-reference.v1", "reference")
	ws := evidenceWorkspace(t, root)

	// No repository at all: a detector that shelled out to git would fail here.
	if _, err := os.Stat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("the fixture root has a git repository; this test is no longer proving anything")
	}
	result := EvaluateWorkspace(ws, EvaluationOptions{WorktreeOnly: true, Today: architectureEngineTestDate()})
	if result.Snapshot == nil || len(result.Snapshot.Evidence) != 1 {
		t.Fatalf("result = %+v, want the evidence read without git", result)
	}
}

// TestDetectFrameworkEvidenceReadsTheTypeScriptProducersOutput is the Go half of
// a cross-language fixture. The bytes it reads are what
// `@putnami/application`'s capability producer actually emitted for a workload
// enforcing one contract of each shape, and the TypeScript half
// (`typescript/framework/application/test/darc/evidence.test.ts`) asserts the
// producer still emits exactly them.
//
// It is the only test that proves the loop closes. Everything else on either
// side could pass while the two languages disagreed about the manifest that
// carries the evidence between them, and the gate would then read nothing from
// a TypeScript project that enforces its contracts perfectly.
func TestDetectFrameworkEvidenceReadsTheTypeScriptProducersOutput(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"evidence-is-read-from-committed-files-and-unmapped-projects-stay-outside")
	root := t.TempDir()
	sources := evidenceSources(t, root)
	ws := evidenceWorkspace(t, root)

	emitted, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "protocols", "architecture",
		"fixtures", "conformance", "typescript-emitted-capabilities.json"))
	if err != nil {
		t.Fatalf("read the TypeScript-emitted fixture: %v", err)
	}
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", "schema", "capabilities.json"), string(emitted))

	records, diagnostics := DetectFrameworkEvidence(ws, sources)
	if len(diagnostics) != 0 {
		t.Fatalf("the detector refused the TypeScript producer's own output: %+v", diagnostics)
	}
	want := []struct {
		importID string
		mode     archproto.AccessMode
	}{
		{"consumer.producer-command.v1", archproto.ModeCommand},
		{"consumer.producer-projection.v1", archproto.ModeProjection},
		{"consumer.producer-reference.v1", archproto.ModeReference},
	}
	if len(records) != len(want) {
		t.Fatalf("records = %+v, want %d rows from the TypeScript manifest", records, len(want))
	}
	for index, expected := range want {
		record := records[index]
		if record.Import != expected.importID || record.Mode != expected.mode {
			t.Errorf("records[%d] = (%s, %s), want (%s, %s)", index, record.Import, record.Mode, expected.importID, expected.mode)
		}
		if record.ConsumerDomain != "consumer" || record.ConsumerProject != "/consumer" {
			t.Errorf("records[%d] = %+v, want the row attributed to the domain that maps the project", index, record)
		}
		if record.Kind != archproto.EvidenceFrameworkPrimitive {
			t.Errorf("records[%d].Kind = %q, want %q", index, record.Kind, archproto.EvidenceFrameworkPrimitive)
		}
	}
}
