package main

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/protocol/capabilities/conformance"
)

// matrixFeature is the feature this sample owns in putnami.features.json. The
// tests below bind the checks it declares, so a result becomes evidence through
// the verification wire rather than through this file.
const matrixFeature = "samples/go-first-party-client-matrix"

// TestCapabilityManifestDeterministic opts this sample into the exported
// capability-manifest determinism pack
// (putnami.capabilities.manifest-determinism). It certifies that the committed
// schema/capabilities.json is byte-canonical, complete, and round-trip
// idempotent.
//
// The canonical form also SCOPES packages[] to the project's own capability
// surface, so this one line additionally certifies the single-project stability
// contract: a manifest that enumerated the reachable dependency closure would no
// longer re-emit to its committed bytes and fails here.
func TestCapabilityManifestDeterministic(t *testing.T) {
	spectest.Proves(t, matrixFeature, "generation-is-deterministic-and-a-break-is-detected", "the-capability-manifest-is-canonical")
	conformance.RunFile(t, "schema/capabilities.json")
}
