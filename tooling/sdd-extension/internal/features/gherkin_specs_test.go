package features

import (
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/sdd/extension/internal/gittest"
)

func TestTrackedGherkinSpecIsReadThroughTheCandidateCut(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "gherkin-specs-reach-the-gate", "a-tracked-gherkin-spec-is-read-through-the-candidate-cut")
	ws := externalWorkspace(t, "", "gherkin")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"),
		strings.NewReplacer(`"format":"openspec"`, `"format":"gherkin"`, "app/openspec/invoice", "app/features").Replace(externalManifest))
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "features", "invoice.feature"), "Feature: Invoice export\n  Scenario: It runs\n")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "features", "notes.md"), "Feature: Not read\n")
	if output, err := gittest.Command(ws.Root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	native, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	result := discoverSpecs(ws, nil, native, func() (Reader, error) { return NewTaskReader(ws.Root) }, featureproto.SpecReaderFor)
	// The manifest declares no check, so the one scenario matches none.
	mismatch := featureproto.WarningCodeSpecScenarioMismatch + "@app/features/invoice.feature#L2"
	if findingCodes(result.Diagnostics) != mismatch || len(result.Specs) != 1 {
		t.Fatalf("specs = %#v, findings = %s", result.Specs, findingCodes(result.Diagnostics))
	}
	if got := result.Specs[0].Spec.Requirements[0].ID; got != "invoice-export" || len(result.Specs[0].Spec.Requirements) != 1 {
		t.Fatalf("requirements = %+v", result.Specs[0].Spec.Requirements)
	}
}
