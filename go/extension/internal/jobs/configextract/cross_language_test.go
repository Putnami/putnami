package configextract

import (
	"os"
	"testing"

	protocfg "go.putnami.dev/protocol/config"
	"go.putnami.dev/protocol/infra"
)

// equivalenceFixturePath is the shared golden file the TypeScript secrets
// producer (runtime config) asserts against too. Both languages writing
// identical bytes to that fixture is the test surface the audit's S1 finding
// would have caught (a silent shape divergence between the two
// implementations).
const equivalenceFixturePath = "../../../../../protocols/infra/fixtures/equivalence/secrets.golden.json"

// TestCrossLanguageEquivalence_Secrets proves the Go secrets producer emits
// byte-for-byte the same JSON the TypeScript producer emits for the same
// conceptual inputs: a top-level "database.password" and a nested camelCase
// "integrations.stripe.apiKey" that canonicalizes to "...api_key", emitted
// sorted.
//
// Neither field uses an env binding on purpose. The Go producer names an
// env-bound secret after its env var while the TypeScript producer always uses
// the config path, so an env binding is the one input shape the two producers
// name differently by design — not a shape divergence — and would make this
// equivalence assertion meaningless.
//
// The producer stamps $schema while writing the sidecar (WriteSidecar), so the
// test compares the bytes writeInfraRequirements actually emits.
//
// The TypeScript counterpart lives in
// typescript/framework/runtime/test/config/cross-language.test.ts and asserts
// against the same golden file. A change to either implementation that breaks
// byte-equivalence fails both tests.
func TestCrossLanguageEquivalence_Secrets(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "database", Fields: []protocfg.FieldSchema{
			{Name: "password", Type: protocfg.FieldTypeString, Sensitive: true},
		}},
		{Path: "integrations", Fields: []protocfg.FieldSchema{
			{Name: "stripe", Type: protocfg.FieldTypeObject, Fields: []protocfg.FieldSchema{
				{Name: "apiKey", Type: protocfg.FieldTypeString, Sensitive: true},
			}},
		}},
	}

	dir := t.TempDir()
	if err := writeInfraRequirements(dir, blocks); err != nil {
		t.Fatalf("writeInfraRequirements: %v", err)
	}
	got, err := os.ReadFile(infra.SidecarPath(dir, sidecarSlug))
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
			"the TypeScript test in typescript/framework/runtime/test/config/cross-language.test.ts.",
			string(got), string(want))
	}
}
