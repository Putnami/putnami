package oci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImagePlanV2_StableIdentityAndLayerKey(t *testing.T) {
	dir := t.TempDir()
	first := writeFile(t, dir, "first", "first payload")
	second := writeFile(t, dir, "second", "second payload")
	spec := testSpec(first)
	spec.Layers[0].Files = append(spec.Layers[0].Files,
		File{Source: second, Path: "/app/second", Mode: 0o644},
	)

	plan, err := NewImagePlan(spec, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != ImagePlanVersion {
		t.Fatalf("plan version = %d, want %d", plan.Version, ImagePlanVersion)
	}
	if len(plan.Identity) != 64 {
		t.Fatalf("plan identity = %q, want full sha256", plan.Identity)
	}
	const wantIdentity = "52fb9ffa2f2158f5ac01ebc19d7827495ad3c9027307c6752a2814aec267f5e8"
	if plan.Identity != wantIdentity {
		t.Fatalf("plan identity = %s, want v2 compatibility identity %s", plan.Identity, wantIdentity)
	}
	if len(plan.Layers) != 1 {
		t.Fatalf("layer plans = %d, want 1", len(plan.Layers))
	}
	key, err := plan.Layers[0].Key()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 {
		t.Fatalf("layer key = %q, want full sha256", key)
	}
	const wantLayerKey LayerKey = "2e51fddedaf4d7866f000e3e17a487360ae6ff62afd5fe92de31b445b18c94ed"
	if key != wantLayerKey {
		t.Fatalf("layer key = %s, want v1 compatibility key %s", key, wantLayerKey)
	}

	// Host paths do not shape either identity. Equivalent bytes staged from a
	// different checkout must reuse both the image plan and the layer blob.
	otherDir := t.TempDir()
	otherSpec := testSpec(writeFile(t, otherDir, "renamed-first", "first payload"))
	otherSpec.Layers[0].Files = append(otherSpec.Layers[0].Files,
		File{Source: writeFile(t, otherDir, "renamed-second", "second payload"), Path: "/app/second", Mode: 0o644},
	)
	otherPlan, err := NewImagePlan(otherSpec, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := otherPlan.Layers[0].Key()
	if err != nil {
		t.Fatal(err)
	}
	if otherPlan.Identity != plan.Identity || otherKey != key {
		t.Fatalf("machine paths changed identities: image %s/%s, layer %s/%s", plan.Identity, otherPlan.Identity, key, otherKey)
	}
}

func TestImagePlanV2_DerivedFileBindsAfterIdentity(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "app", "binary")
	stamp := filepath.Join(dir, "stamp.json") // deliberately absent at plan time
	spec := testSpec(binary)
	spec.Layers = append(spec.Layers, Layer{Files: []File{{Source: stamp, Path: "/app/.gen/version.json", Mode: 0o644}}})
	options := PlanOptions{DerivedFiles: map[string]string{
		"/app/.gen/version.json": "putnami/typescript/content-stamp/v1:example",
	}}

	plan, err := NewImagePlan(spec, options)
	if err != nil {
		t.Fatal(err)
	}
	identity := plan.Identity
	if _, err := plan.Layers[1].Key(); err == nil {
		t.Fatal("unbound derived file unexpectedly produced a layer key")
	}
	if err := os.WriteFile(stamp, []byte(`{"contentHash":"`+identity[:12]+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plan.BindDerivedFiles(); err != nil {
		t.Fatal(err)
	}
	if plan.Identity != identity {
		t.Fatal("binding self-derived content changed the image identity")
	}
	if _, err := plan.Layers[1].Key(); err != nil {
		t.Fatalf("bound derived layer key: %v", err)
	}
}
