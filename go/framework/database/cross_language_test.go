package database

import (
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/infra"
)

// equivalenceFixturePath is the shared golden file the TypeScript database
// producer asserts against too. Both languages writing identical bytes to
// that fixture is the test surface the audit's S1 finding would have caught
// (a silent shape divergence between the two implementations).
const equivalenceFixturePath = "../../../protocols/infra/fixtures/equivalence/database.golden.json"

// TestCrossLanguageEquivalence_Database proves the Go database producer emits
// byte-for-byte the same JSON the TypeScript producer emits for the same
// conceptual inputs. The decls are intentionally unsorted and mix a database
// declared twice (so the sorted-union schema merge is exercised), a database
// with no schemas (the omitempty path), and three distinct names (the
// deterministic name sort).
//
// The TypeScript counterpart lives in
// typescript/framework/database/test/cross-language.test.ts and asserts
// against the same golden file. A change to either implementation that breaks
// byte-equivalence fails both tests.
func TestCrossLanguageEquivalence_Database(t *testing.T) {
	decls := []databaseDecl{
		{name: "primary", schemas: []string{"iam"}},
		{name: "analytics", schemas: []string{"reporting"}},
		{name: "analytics", schemas: []string{"events"}},
		{name: "cache"},
	}

	manifest, diags := buildInfraManifest(infra.EnginePostgres, decls)
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
			"the TypeScript test in typescript/framework/database/test/cross-language.test.ts.",
			string(got), string(want))
	}
}
