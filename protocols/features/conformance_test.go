package features_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/conformance"
)

func TestConformanceRunners(t *testing.T) {
	conformance.RunManifestFile(t, filepath.Join("fixtures", "equivalence", "go-typescript-features.golden.json"))
	conformance.RunEvidenceFile(t, filepath.Join("fixtures", "equivalence", "go-typescript-evidence.golden.json"))
	conformance.RunEvidenceFile(t, filepath.Join("fixtures", "equivalence", "generated-evidence.golden.json"))
	conformance.RunEvidenceFile(t, filepath.Join("fixtures", "equivalence", "python-authored-evidence.golden.json"))
	conformance.RunDesignGraphFile(t, filepath.Join("fixtures", "equivalence", "go-typescript-design.golden.json"))
	conformance.RunSpecFile(t, filepath.Join("fixtures", "equivalence", "human-authored-spec.golden.json"))
	conformance.RunManifestFile(t, filepath.Join("fixtures", "equivalence", "human-authored-features-v2.golden.json"))
	conformance.RunVerificationReportFile(t, filepath.Join("fixtures", "equivalence", "go-typescript-verification.golden.json"))
}

func TestConformancePackAndVectors(t *testing.T) {
	packData, err := os.ReadFile(filepath.Join("conformance", "pack.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pack struct {
		ID              string   `json:"id"`
		Corpus          string   `json:"corpus"`
		CapabilityKinds []string `json:"capabilityKinds"`
		Languages       []string `json:"languages"`
	}
	decoder := json.NewDecoder(bytes.NewReader(packData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pack); err != nil {
		t.Fatal(err)
	}
	if pack.ID != "putnami.features.protocol-v1" || pack.Corpus != "vectors.json" {
		t.Fatalf("invalid pack: %#v", pack)
	}
	if len(pack.CapabilityKinds) != 0 || !reflect.DeepEqual(pack.Languages, []string{"go"}) {
		t.Fatalf("invalid pack vocabularies: %#v", pack)
	}

	vectorsData, err := os.ReadFile(filepath.Join("conformance", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		ProtocolVersion int `json:"protocolVersion"`
		Vectors         []struct {
			Canonical string `json:"canonical"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(vectorsData, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.ProtocolVersion != 1 || len(vectors.Vectors) != 8 {
		t.Fatalf("invalid vectors: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		if _, err := os.Stat(filepath.Clean(filepath.Join("conformance", vector.Canonical))); err != nil {
			t.Errorf("missing vector %q: %v", vector.Canonical, err)
		}
	}
}
