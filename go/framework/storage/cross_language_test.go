package storage

import (
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// equivalenceFixturePath is the shared golden file the TypeScript storage
// producer asserts against too. Both languages writing identical bytes
// to that fixture is the test surface the audit's S1 finding would have
// caught (TypeScript stamped protocolVersion 1 while Go required 2).
const equivalenceFixturePath = "../../../protocols/infra/fixtures/equivalence/storage.golden.json"

// TestCrossLanguageEquivalence_Storage proves the Go storage producer
// emits byte-for-byte the same JSON the TypeScript producer emits for
// the same conceptual inputs. Inputs are intentionally unsorted so the
// test also covers the deterministic name sort, and they mix
// with-retention and without-retention buckets so the omitempty handling
// is exercised on both sides.
//
// The TypeScript counterpart lives in
// typescript/framework/storage/test/cross-language.test.ts and asserts
// against the same golden file. A change to either implementation that
// breaks byte-equivalence fails both tests.
func TestCrossLanguageEquivalence_Storage(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "cross-runtime-backend-conformance")
	buckets := []*BucketDefinition{
		{Name: "uploads"},
		{Name: "audit-logs", Options: BucketOptions{Retention: "30d"}},
		{Name: "avatars", Options: BucketOptions{Retention: "90d"}},
	}

	manifest, diags := buildInfraManifest(buckets)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if manifest == nil {
		t.Fatal("expected manifest, got nil")
	}

	got, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n') // match WriteSidecar's terminator

	want, err := os.ReadFile(equivalenceFixturePath)
	if err != nil {
		t.Fatalf("read fixture %s: %v", equivalenceFixturePath, err)
	}

	if string(got) != string(want) {
		t.Errorf("Go output does not match cross-language fixture.\n"+
			"got:\n%s\nwant:\n%s\n"+
			"If this is an intentional shape change, update the fixture and\n"+
			"the TypeScript test in typescript/framework/storage/test/cross-language.test.ts.",
			string(got), string(want))
	}
}
