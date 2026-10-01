package transaction

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// pack mirrors conformance/pack.json: the committed pack-manifest convention a
// downstream project references by id to opt into the transaction conformance
// corpus. The shape is deliberately minimal and forward-stable; aggregating
// packs across suites is out of scope for this module.
type pack struct {
	ID              string   `json:"id"`
	Corpus          string   `json:"corpus"`
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

// TestConformancePack pins the committed pack-manifest convention: a stable id,
// the languages both adapters implement, and a corpus pointer that resolves to
// the single source of truth. Downstream aggregation identifies a pack by id,
// so these fields must stay stable.
func TestConformancePack(t *testing.T) {
	p := loadPack(t)

	if p.ID != "putnami.transaction.conformance" {
		t.Errorf("pack id = %q, want %q", p.ID, "putnami.transaction.conformance")
	}

	wantLangs := []string{"go", "typescript"}
	if !reflect.DeepEqual(p.Languages, wantLangs) {
		t.Errorf("pack languages = %v, want %v", p.Languages, wantLangs)
	}

	if p.Corpus != "manifest.json" {
		t.Errorf("pack corpus = %q, want %q", p.Corpus, "manifest.json")
	}
	corpus, err := os.ReadFile(filepath.Join("conformance", p.Corpus))
	if err != nil {
		t.Fatalf("pack corpus %q does not resolve: %v", p.Corpus, err)
	}
	if !bytes.Equal(corpus, ConformanceManifestJSON()) {
		t.Errorf("pack corpus %q does not match the canonical embedded corpus", p.Corpus)
	}

	if len(p.CapabilityKinds) == 0 {
		t.Fatal("pack declares no capabilityKinds")
	}
	valid := validCapabilityKinds(t)
	for _, kind := range p.CapabilityKinds {
		if !valid[kind] {
			t.Errorf("pack capabilityKind %q is not a value in protocols/capabilities", kind)
		}
	}
}

// validCapabilityKinds reads the closed capabilityKind enum straight from the
// capabilities protocol's JSON schema, so a pack that names a capability kind
// the vocabulary does not define — or one renamed upstream — fails here without
// protocols/transaction taking a Go dependency on protocols/capabilities.
func validCapabilityKinds(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "capabilities", "schemas", "capabilities.json"))
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
