package distribution

import (
	"bytes"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestEmbeddedConformance(t *testing.T) {
	spectest.Proves(t, "distribution/release-set-contract", "the-embedded-corpus-is-the-conformance-suite", "the-embedded-corpus-gets-its-verdict")
	results := RunEmbeddedConformance()
	if len(results) < 10 {
		t.Fatalf("embedded corpus unexpectedly small: %d", len(results))
	}
	for _, result := range results {
		if !result.Passed {
			t.Errorf("%s failed: %s", result.Name, result.Detail)
		}
	}
}

func TestEmbeddedFixturesReturnFreshCopies(t *testing.T) {
	spectest.Proves(t, "distribution/release-set-contract", "the-embedded-corpus-is-the-conformance-suite", "the-embedded-fixtures-are-fresh-copies")
	first := EmbeddedConformanceFixtures()
	second := EmbeddedConformanceFixtures()
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("fixture lengths differ: %d/%d", len(first), len(second))
	}
	first[0].JSON[0] ^= 0xff
	if bytes.Equal(first[0].JSON, second[0].JSON) {
		t.Fatal("fixture callers share mutable bytes")
	}
	canonical, ref := EmbeddedGolden()
	if len(canonical) == 0 || ref.ID == "" || ref.Digest == "" {
		t.Fatalf("embedded golden missing: %q %#v", canonical, ref)
	}
}
