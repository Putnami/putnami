package database_test

import (
	"testing"

	"go.putnami.dev/database/conformance"
)

// TestConformanceCorpus runs the cross-language transaction / concurrency
// conformance corpus through the exported runner. The execution
// machinery lives in go.putnami.dev/database/conformance so a downstream project
// opts into the full corpus with a single committed line:
//
//	func TestConformance(t *testing.T) { conformance.Run(t) }
//
// It provisions Postgres via the shared testprovider and SKIPS without
// DATABASE_TEST_BINDINGS, so the local unit gate stays green with no database.
func TestConformanceCorpus(t *testing.T) {
	conformance.Run(t)
}
