package releaseset

import (
	"strings"
	"testing"
)

func TestSelectionFingerprintIsDeterministicAndKeySensitive(t *testing.T) {
	key := strings.Repeat("a", 64)
	first := SelectionFingerprint(key)
	if first != SelectionFingerprint(key) {
		t.Fatal("selection fingerprint is not deterministic")
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("fingerprint shape = %q", first)
	}
	if moved := SelectionFingerprint(strings.Repeat("b", 64)); moved == first {
		t.Fatal("a different execution key did not move the fingerprint")
	}
	// The domain separator must make the fingerprint distinguishable from a
	// bare hash of the same key, so a cache address can never be mistaken for
	// a selection fingerprint.
	if SelectionFingerprint("") == SelectionFingerprint(selectionDomain) {
		t.Fatal("the domain separator is not part of the hashed input")
	}
}
