package architecture

import (
	"go.putnami.dev/protocol/features/spectest"

	"strings"
	"testing"
)

func TestClosedVocabulariesAndDiagnosticCodes(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "finding-ids-and-vocabularies-are-closed-and-stable")
	for _, status := range []LifecycleStatus{StatusPlanned, StatusActive, StatusLegacy, StatusDeprecated} {
		if !validLifecycle(status) {
			t.Fatalf("lifecycle %q is not accepted", status)
		}
	}
	for _, mode := range []AccessMode{ModeReference, ModeQuery, ModeSnapshot, ModeProjection, ModeCommand} {
		if !validMode(mode) {
			t.Fatalf("mode %q is not accepted", mode)
		}
	}
	if got, want := len(ValidDiagnosticCodes), 34; got != want {
		t.Fatalf("diagnostic code count = %d, want %d; review the public automation vocabulary", got, want)
	}
}

func TestContractIDValidatorsShareTheWireLengthBound(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "contract-id-validators-share-one-wire-bound")
	oversized := "a." + strings.Repeat("b", 252) + ".v1"
	if len(oversized) <= 256 {
		t.Fatalf("test contract ID is not oversized: %d bytes", len(oversized))
	}
	if validContractID(oversized, 1) || validContractIDAnyVersion(oversized) {
		t.Fatalf("oversized contract ID was accepted")
	}
}
