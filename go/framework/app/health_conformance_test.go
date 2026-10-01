package app_test

import (
	"testing"

	"go.putnami.dev/app/conformance"
)

// TestHealthConformance runs the exported health-probe conformance pack through
// the same one-line opt-in a downstream project uses. The
// machinery lives in go.putnami.dev/app/conformance; the TypeScript counterpart is
// @putnami/application/conformance. It is a PURE pack — no external service, no
// skip gate — so the probe/discovery/projection assertions run in the normal unit
// gate.
func TestHealthConformance(t *testing.T) {
	conformance.Run(t)
}
