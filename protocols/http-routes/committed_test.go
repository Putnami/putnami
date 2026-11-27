package httproutes

import (
	"bytes"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const committedRequirement = "the-committed-form-derives-its-digest"

func TestCommittedJSON_IsTheArtifactWithoutItsDigest(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", committedRequirement, "the-committed-form-is-the-artifact-without-its-digest")
	artifact, err := os.ReadFile("fixtures/equivalence/http-routes.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("fixtures/equivalence/http-routes.committed.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	got, diags := CommittedForm(artifact)
	if diag.HasErrors(diags) {
		t.Fatalf("committed form: %v", diags)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("committed bytes differ from the committed golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if bytes.Contains(got, []byte(`"digest"`)) {
		t.Fatalf("the committed form carries a digest:\n%s", got)
	}
	again, diags := CommittedForm(got)
	if diag.HasErrors(diags) || !bytes.Equal(again, got) {
		t.Fatalf("the committed form of a committed inventory changed: %v\n%s", diags, again)
	}
	withDigest := strings.Replace(string(want), "\n  ]\n}\n", "\n  ],\n  \"digest\": \""+goldenDigest+"\"\n}\n", 1)
	if withDigest != string(artifact) {
		t.Fatalf("the artifact is not the committed form plus its digest\n--- got ---\n%s", withDigest)
	}
}

func TestParseAndValidateManifest_DerivesAMissingDigest(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", committedRequirement, "a-parser-derives-a-missing-digest")
	committed, err := os.ReadFile("fixtures/equivalence/http-routes.committed.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := ParseAndValidateManifest(committed)
	if diag.HasErrors(diags) {
		t.Fatalf("parse the committed form: %v", diags)
	}
	if manifest.Digest != goldenDigest {
		t.Fatalf("derived digest = %q, want the artifact's %q", manifest.Digest, goldenDigest)
	}
	artifact, diags := CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	golden, err := os.ReadFile("fixtures/equivalence/http-routes.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifact, golden) {
		t.Fatalf("the artifact form of the committed form differs from the golden\n%s", artifact)
	}

	mismatched := strings.Replace(string(golden), goldenDigest, "sha256:"+strings.Repeat("0", 64), 1)
	if _, diags := ParseAndValidateManifest([]byte(mismatched)); !hasCode(diags, ErrorCodeDigestMismatch) {
		t.Fatalf("a present wrong digest: diagnostics = %v, want %s", diags, ErrorCodeDigestMismatch)
	}
	for _, name := range loadExpectations(t).Valid {
		data, err := os.ReadFile("fixtures/valid/" + name)
		if err != nil {
			t.Fatal(err)
		}
		artifactManifest, diags := ParseAndValidateManifest(data)
		if diag.HasErrors(diags) {
			t.Fatalf("%s: %v", name, diags)
		}
		committedForm, diags := CommittedForm(data)
		if diag.HasErrors(diags) {
			t.Fatalf("%s: committed form: %v", name, diags)
		}
		committedManifest, diags := ParseAndValidateManifest(committedForm)
		if diag.HasErrors(diags) || committedManifest.Digest != artifactManifest.Digest {
			t.Fatalf("%s: committed form digest = %v (%v), want %q", name, committedManifest, diags, artifactManifest.Digest)
		}
	}

	empty := strings.Replace(string(golden), goldenDigest, "", 1)
	if _, diags := ParseAndValidateManifest([]byte(empty)); !hasCode(diags, ErrorCodeInvalidDigest) {
		t.Fatalf("an empty digest: diagnostics = %v, want %s", diags, ErrorCodeInvalidDigest)
	}
}
