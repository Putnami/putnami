package migration

import (
	"os"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// goldenBundleDigest pins the bundle digest of fixtures/equivalence/bundle.golden.json.
//
// The TypeScript counterpart (typescript/framework/migration/test/cross-language.test.ts)
// asserts computeBundleDigest of the same fixture equals this exact value, so a
// change to either implementation that breaks byte-equivalence fails both
// suites. When you touch the digest algorithm or the golden fixture, update this
// constant and the TS test in lockstep.
const goldenBundleDigest = "6533cedd09aa95f9622ec420f675782c4ad34a7646702ed7ba9a0918d1feb86e"

// goldenCompatibleBundleDigest pins the bundle digest of
// fixtures/equivalence/bundle-compatible.golden.json, whose operations all set
// the Compatible capability.
//
// It exists because the canonical form is rebuilt field by field on the
// TypeScript side: a capability Go writes and TypeScript does not know silently
// splits the two digests. The TypeScript counterpart asserts computeBundleDigest
// of the same fixture equals this exact value.
const goldenCompatibleBundleDigest = "2884a81ce4de0e7e28f776b46c370c3ec1c7bbd1cb1bf2c8149ce61e2c23a2bc"

func TestCrossLanguage_BundleDigest(t *testing.T) {
	data, err := os.ReadFile("fixtures/equivalence/bundle.golden.json")
	if err != nil {
		t.Fatal(err)
	}

	b, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("golden bundle is invalid: %v", diags)
	}

	got := ComputeBundleDigest(*b)
	if got != goldenBundleDigest {
		t.Fatalf("bundle digest = %q, want %q (cross-language golden)", got, goldenBundleDigest)
	}
}

func TestCrossLanguage_CompatibleBundleDigest(t *testing.T) {
	data, err := os.ReadFile("fixtures/equivalence/bundle-compatible.golden.json")
	if err != nil {
		t.Fatal(err)
	}

	b, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("golden compatible bundle is invalid: %v", diags)
	}

	for _, op := range b.Operations {
		if !op.Capabilities.Compatible {
			t.Fatalf("operation %q does not set compatible; the fixture must exercise the marker", op.Name)
		}
	}
	if !RollbackAllowed(b.Operations) {
		t.Fatal("RollbackAllowed = false, want true for a bundle whose operations are all compatible")
	}

	got := ComputeBundleDigest(*b)
	if got != goldenCompatibleBundleDigest {
		t.Fatalf("compatible bundle digest = %q, want %q (cross-language golden)", got, goldenCompatibleBundleDigest)
	}
}
