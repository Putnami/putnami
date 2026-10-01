package conformance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// pack mirrors pack.json: the committed pack-manifest convention a downstream
// project references by id to opt into a conformance pack. The shape
// is a forward-stable superset of the transaction pack: `corpus` is OPTIONAL
// because the health pack asserts in-process probe behavior and has no
// committed corpus file, so it omits it. Packs aggregate by id, so the
// fields must stay stable.
type pack struct {
	ID              string   `json:"id"`
	Corpus          string   `json:"corpus,omitempty"`
	CapabilityKinds []string `json:"capabilityKinds"`
	Languages       []string `json:"languages"`
}

func loadPack(t *testing.T) pack {
	t.Helper()
	data, err := os.ReadFile("pack.json")
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

// TestConformancePack pins the committed health pack manifest: a stable id, the
// languages both runtimes implement a runner for, and the health-probe capability
// kinds it certifies (health/liveness/readiness). The pack is corpus-less — it
// certifies in-process behavior, not a committed fixture — so pack.json omits the
// optional `corpus` pointer defined by the shared convention.
func TestConformancePack(t *testing.T) {
	p := loadPack(t)

	if p.ID != "putnami.health.conformance" {
		t.Errorf("pack id = %q, want %q", p.ID, "putnami.health.conformance")
	}

	wantLangs := []string{"go", "typescript"}
	if !reflect.DeepEqual(p.Languages, wantLangs) {
		t.Errorf("pack languages = %v, want %v", p.Languages, wantLangs)
	}

	// The health pack asserts liveness/readiness/version behavior in-process, so
	// it has no committed corpus file — pack.json must omit the optional pointer.
	if p.Corpus != "" {
		t.Errorf("health pack must omit corpus (it certifies in-process behavior), got %q", p.Corpus)
	}

	wantKinds := []string{"health", "liveness", "readiness"}
	if !reflect.DeepEqual(p.CapabilityKinds, wantKinds) {
		t.Errorf("pack capabilityKinds = %v, want %v", p.CapabilityKinds, wantKinds)
	}
	valid := validCapabilityKinds(t)
	for _, kind := range p.CapabilityKinds {
		if !valid[kind] {
			t.Errorf("pack capabilityKind %q is not a value in the capabilities vocabulary", kind)
		}
	}
}

// validCapabilityKinds reads the closed capabilityKind enum straight from the
// capabilities protocol's JSON schema (by relative path, no module dependency), so
// a pack that names a capability kind the vocabulary does not define — or one
// renamed upstream — fails here.
func validCapabilityKinds(t *testing.T) map[string]bool {
	t.Helper()
	schemaPath := filepath.Join("..", "..", "..", "..", "protocols", "capabilities", "schemas", "capabilities.json")
	data, err := os.ReadFile(schemaPath)
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
