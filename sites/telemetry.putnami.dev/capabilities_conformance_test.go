package main

import (
	"testing"

	"go.putnami.dev/protocol/capabilities/conformance"
)

// TestCapabilityManifestDeterministic opts this workload into the exported
// capability-manifest determinism pack (putnami.capabilities.manifest-determinism).
// It certifies that this project's committed
// schema/capabilities.json is byte-canonical, complete, and round-trip
// idempotent — the determinism invariant every describe-emitting project shares.
func TestCapabilityManifestDeterministic(t *testing.T) {
	conformance.RunFile(t, "schema/capabilities.json")
}
