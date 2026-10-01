package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestPlannedFileLayer_HitAvoidsSourceAndMissVerifiesSource(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	source := writeFile(t, dir, "payload", "original")
	spec := Spec{Layers: []Layer{{Files: []File{{Source: source, Path: "/payload", Mode: 0o644}}}}}

	plan, err := NewImagePlan(spec, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &plan.Layers[0], filepath.Join(dir, "first.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Fatal("first planned build unexpectedly hit cache")
	}

	// A trusted upstream fingerprint can recreate the plan without reopening
	// the source. The cache hit is then fully source-free.
	fingerprint := *plan.Layers[0].Files[0].Fingerprint
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	trusted, err := NewImagePlan(spec, PlanOptions{TrustedFingerprints: map[string]SourceFingerprint{source: fingerprint}})
	if err != nil {
		t.Fatalf("planning from trusted fingerprint: %v", err)
	}
	if _, hit, err := loadOrBuildPlannedFileLayer(spec.Layers[0], &trusted.Layers[0], filepath.Join(dir, "hit.tar.gz"), cacheRoot, types.DockerLayer); err != nil {
		t.Fatalf("source-free cache hit: %v", err)
	} else if !hit {
		t.Fatal("trusted plan unexpectedly missed cache")
	}

	// On a miss, the builder hashes bytes as they flow into the tar and fails
	// closed if they no longer match the plan.
	changedSource := writeFile(t, dir, "changed", "before")
	changedSpec := Spec{Layers: []Layer{{Files: []File{{Source: changedSource, Path: "/changed", Mode: 0o644}}}}}
	changedPlan, err := NewImagePlan(changedSpec, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedSource, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = loadOrBuildPlannedFileLayer(changedSpec.Layers[0], &changedPlan.Layers[0], filepath.Join(dir, "changed.tar.gz"), filepath.Join(dir, "empty-cache"), types.DockerLayer)
	if err == nil || !strings.Contains(err.Error(), "changed since image plan") {
		t.Fatalf("changed source error = %v", err)
	}
}

func TestImagePlanV2_TarballMutationFailsClosed(t *testing.T) {
	dir := t.TempDir()
	tarball := writeFile(t, dir, "layer.tar", "before")
	plan, err := NewImagePlan(Spec{Layers: []Layer{{Tarball: tarball}}}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarball, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = AssemblePlan(plan, syntheticBase(t), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "changed since image plan") {
		t.Fatalf("changed tarball error = %v", err)
	}
}
