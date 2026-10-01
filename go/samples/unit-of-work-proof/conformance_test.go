package uowproof_test

import (
	"testing"

	"go.putnami.dev/database/conformance"
)

// TestTransactionConformance certifies this sample's provisioned Postgres against
// the shared cross-language transaction / concurrency conformance corpus with the
// single committed opt-in line the pack convention prescribes
// (go/framework/database/conformance.Run — the pack whose manifest lives at
// protocols/transaction/conformance). It provisions through the same
// DATABASE_TEST_BINDINGS-gated test provider the sample's own proofs use, so
// `putnami test` (auto mode) auto-provisions a database and runs the full corpus
// live; a run with no binding SKIPS, keeping the unit gate green with no Postgres.
func TestTransactionConformance(t *testing.T) {
	conformance.Run(t)
}
