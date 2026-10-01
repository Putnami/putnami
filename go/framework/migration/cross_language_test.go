package migration

import (
	"os"
	"testing"

	"go.putnami.dev/protocol/infra"
)

// equivalenceFixturePath is the shared golden file the TypeScript migration
// producer asserts against too. Both languages writing identical bytes to
// that fixture is the test surface the audit's S1 finding would have caught
// (a silent shape divergence between the two implementations).
const equivalenceFixturePath = "../../../protocols/infra/fixtures/equivalence/migration.golden.json"

// TestCrossLanguageEquivalence_Migration proves the Go migration producer
// emits byte-for-byte the same JSON the TypeScript producer emits for the same
// conceptual inputs. Two sources target the same (primary, postgres) database
// with different schemas so the sorted-union merge is exercised, plus a second
// database registered out of name order to cover the deterministic
// (name, engine) sort.
//
// Unlike the storage/database/events producers, InfraRequirements does not
// stamp $schema — WriteSidecar does, while writing the file — so this test
// compares the bytes WriteInfraRequirements actually emits rather than
// marshaling the builder output directly.
//
// The TypeScript counterpart lives in
// typescript/framework/migration/test/cross-language.test.ts and asserts
// against the same golden file. A change to either implementation that breaks
// byte-equivalence fails both tests.
func TestCrossLanguageEquivalence_Migration(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r,
		schemaSource{
			kind:      KindSQL,
			namespace: "audit",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"audit"}}},
		},
		schemaSource{
			kind:      KindSQL,
			namespace: "iam",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
		schemaSource{
			kind:      KindSQL,
			namespace: "ledger",
			databases: []infra.Database{{Name: "wealth", Engine: infra.EnginePostgres, Schemas: []string{"ledger"}}},
		},
	)

	dir := t.TempDir()
	if err := r.WriteInfraRequirements(dir); err != nil {
		t.Fatalf("WriteInfraRequirements: %v", err)
	}
	got, err := os.ReadFile(infra.SidecarPathIn(dir, sidecarSlug))
	if err != nil {
		t.Fatalf("read emitted sidecar: %v", err)
	}

	want, err := os.ReadFile(equivalenceFixturePath)
	if err != nil {
		t.Fatalf("read fixture %s: %v", equivalenceFixturePath, err)
	}

	if string(got) != string(want) {
		t.Errorf("Go output does not match cross-language fixture.\n"+
			"got:\n%s\nwant:\n%s\n"+
			"If this is an intentional shape change, update the fixture and\n"+
			"the TypeScript test in typescript/framework/migration/test/cross-language.test.ts.",
			string(got), string(want))
	}
}
