package events

import (
	"encoding/json"
	"os"
	"testing"
)

// equivalenceFixturePath is the shared golden file the TypeScript events
// producer asserts against too. Both languages writing identical bytes to
// that fixture is the test surface the audit's S1 finding would have caught
// (a silent shape divergence between the two implementations).
const equivalenceFixturePath = "../../../protocols/infra/fixtures/equivalence/events.golden.json"

// TestCrossLanguageEquivalence_Events proves the Go events producer emits
// byte-for-byte the same JSON the TypeScript producer emits for the same
// conceptual inputs. The publish list is unsorted with a duplicate so the
// dedupe-and-sort path is exercised on both sides; the subscribe list is a
// single distinct topic.
//
// The TypeScript counterpart lives in
// typescript/framework/events/test/cross-language.test.ts and asserts against
// the same golden file. A change to either implementation that breaks
// byte-equivalence fails both tests.
func TestCrossLanguageEquivalence_Events(t *testing.T) {
	publishes := []string{"order.shipped", "order.created", "order.created"}
	subscribes := []string{"payment.settled"}

	manifest, diags := buildInfraManifest(publishes, subscribes, DeliveryPull)
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
			"the TypeScript test in typescript/framework/events/test/cross-language.test.ts.",
			string(got), string(want))
	}
}
