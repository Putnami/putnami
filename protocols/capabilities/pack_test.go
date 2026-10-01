package capabilities_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/capabilities/conformance"
)

// pack mirrors conformance/pack.json: the committed pack-manifest convention a
// downstream project references by id to opt into a conformance pack. The shape
// is forward-stable across the protocol packs: `corpus` is OPTIONAL — a pack
// whose corpus is a runtime artifact rather than a committed fixture omits it.
// The agent-context document lists referenced packs by id, so the fields must
// stay stable.
type pack struct {
	ID              string   `json:"id"`
	Corpus          string   `json:"corpus,omitempty"`
	CapabilityKinds []string `json:"capabilityKinds"`
	Languages       []string `json:"languages"`
}

func loadPack(t *testing.T) pack {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("conformance", "pack.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p pack
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("parse pack.json (strict): %v", err)
	}
	return p
}

// TestConformancePack pins the committed manifest-determinism pack manifest: a
// stable id, the language with an exported runner, and capabilityKinds drawn from
// the real capabilities vocabulary. The pack is manifest-scoped — it certifies the
// aggregate manifest artifact, which can carry every capability kind, so it lists
// the full vocabulary rather than one kind — and corpus-less, because its "corpus"
// is the emitted manifest, not a committed fixture, which is what the optional
// `corpus` field in the shared pack convention exists for.
func TestConformancePack(t *testing.T) {
	p := loadPack(t)

	if p.ID != "putnami.capabilities.manifest-determinism" {
		t.Errorf("pack id = %q, want %q", p.ID, "putnami.capabilities.manifest-determinism")
	}

	wantLangs := []string{"go"}
	if !reflect.DeepEqual(p.Languages, wantLangs) {
		t.Errorf("pack languages = %v, want %v", p.Languages, wantLangs)
	}

	// The manifest-determinism pack has no committed corpus file — its corpus is
	// the emitted capabilities manifest re-checked at run time — so pack.json must
	// omit the optional `corpus` pointer.
	if p.Corpus != "" {
		t.Errorf("manifest-determinism pack must omit corpus (its corpus is the emitted manifest), got %q", p.Corpus)
	}

	if len(p.CapabilityKinds) == 0 {
		t.Fatal("pack declares no capabilityKinds")
	}
	valid := validCapabilityKinds(t)
	declared := map[string]bool{}
	for _, kind := range p.CapabilityKinds {
		if !valid[kind] {
			t.Errorf("pack capabilityKind %q is not a value in the capabilities vocabulary", kind)
		}
		declared[kind] = true
	}
	// The manifest-determinism pack certifies the AGGREGATE manifest, which can
	// carry every capability kind, so it must list the FULL vocabulary — not merely
	// a valid subset. Assert exact coverage so adding a new capability kind (a Go
	// const + a schema enum value) fails here until pack.json is extended to match.
	for kind := range valid {
		if !declared[kind] {
			t.Errorf("pack.json is missing capabilityKind %q — the manifest-determinism pack must list the full vocabulary", kind)
		}
	}
	if len(declared) != len(valid) {
		t.Errorf("pack declares %d distinct capabilityKinds, want the full vocabulary of %d", len(declared), len(valid))
	}
}

// validCapabilityKinds reads the closed capabilityKind enum straight from the
// capabilities protocol's JSON schema, so a pack that names a capability kind the
// vocabulary does not define — or one renamed upstream — fails here.
func validCapabilityKinds(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("schemas", "capabilities.json"))
	if err != nil {
		t.Fatalf("read capabilities schema: %v", err)
	}
	var schema struct {
		Defs struct {
			CapabilityKind struct {
				Enum []string `json:"enum"`
			} `json:"capabilityKind"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse capabilities schema: %v", err)
	}
	if len(schema.Defs.CapabilityKind.Enum) == 0 {
		t.Fatal("capabilities schema $defs.capabilityKind.enum is empty")
	}
	out := map[string]bool{}
	for _, k := range schema.Defs.CapabilityKind.Enum {
		out[k] = true
	}
	return out
}

// TestManifestDeterminismRunner runs the exported pack runner against the shared
// cross-language golden — the real emitter output pinned by
// determinism/describe tests. It proves the runner actually executes in the unit
// gate (a PURE pack, no skip) and that the committed golden is byte-deterministic
// and complete, exactly as a downstream's own manifest must be.
func TestManifestDeterminismRunner(t *testing.T) {
	conformance.RunFile(t, filepath.Join("fixtures", "equivalence", "capabilities.golden.json"))
	conformance.RunFile(t, filepath.Join("fixtures", "v2", "equivalence", "capabilities-v2.golden.json"))
}
