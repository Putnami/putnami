package features

import (
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/sdd/extension/internal/gittest"
)

func TestTrackedKiroSpecIsReadThroughTheCandidateCut(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "kiro-specs-reach-the-gate", "a-tracked-kiro-spec-is-read-through-the-candidate-cut")
	ws := externalWorkspace(t, "", "kiro")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"),
		strings.NewReplacer(`"format":"openspec"`, `"format":"kiro"`, "app/openspec/invoice", ".kiro/specs/invoice").Replace(externalManifest))
	writeDiscoveryFixture(t, filepath.Join(ws.Root, ".kiro", "specs", "invoice", "requirements.md"),
		"## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n")
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		command := gittest.Command(ws.Root, arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	native, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	result := discoverSpecs(ws, nil, native, func() (Reader, error) { return NewTaskReader(ws.Root) }, featureproto.SpecReaderFor)
	if len(result.Diagnostics) != 0 || len(result.Specs) != 1 {
		t.Fatalf("specs = %#v, findings = %s", result.Specs, findingCodes(result.Diagnostics))
	}
	if got := result.Specs[0].Spec.Requirements[0].ID; got != "requirement-1-1" {
		t.Fatalf("id = %s", got)
	}
}
