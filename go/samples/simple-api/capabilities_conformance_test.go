package main

import (
	"testing"

	"go.putnami.dev/protocol/capabilities/conformance"
)

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
	conformance.RunFile(t, "schema/capabilities.json")
}
