package main

import (
	"path"
	"slices"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
)

// The schema artifact the test steps fold leaves the emitted feature evidence
// out of its digest: the evidence names the project's source binding, which
// every file of the project moves, a README included, and no test reads it.
// The rest of the converged tree stays in the digest (ADR 0017).
func TestTheSchemaArtifactLeavesTheFeatureEvidenceOut(t *testing.T) {
	m := loadExtensionManifest(t)
	describe := m.Tasks["build-describe"]
	id, output, err := proto.ArtifactOutputFor(describe, "schema")
	if err != nil || output.Kind != proto.OutputKindDirectory {
		t.Fatalf("describe's schema port names %q %+v (%v), want a directory artifact", id, output, err)
	}
	evidence, ok := relativeTo(output.Path, features.EmitEvidenceDirectory)
	if !ok {
		t.Fatalf("the emitted evidence %s is not inside the schema artifact %s", features.EmitEvidenceDirectory, output.Path)
	}
	if got := proto.ArtifactExcludes(describe, "schema"); !slices.Equal(got, []string{evidence}) {
		t.Fatalf("schema artifact excludes %v, want the emitted evidence %q alone", got, evidence)
	}
	for _, port := range []string{"migrationBundle", "migrations", "design", "httpRoutes"} {
		if got := proto.ArtifactExcludes(describe, port); len(got) != 0 {
			t.Errorf("describe's %s artifact excludes %v", port, got)
		}
	}
}

// relativeTo returns the slash path of child relative to parent, when child is
// inside it.
func relativeTo(parent, child string) (string, bool) {
	parent, child = path.Clean(parent), path.Clean(child)
	if len(child) <= len(parent)+1 || child[:len(parent)] != parent || child[len(parent)] != '/' {
		return "", false
	}
	return child[len(parent)+1:], true
}

// An unresolved identity is reported as a skip, which the engine stores only
// for a deterministic task: test-identity-exec caches its listings, and is not
// deterministic, so no unresolved manifest is ever replayed (ADR 0017).
func TestAnUnresolvedIdentityIsNeverStored(t *testing.T) {
	policy := loadExtensionManifest(t).Tasks["test-identity-exec"].Cache
	if policy == nil || (policy.Enabled != nil && !*policy.Enabled) {
		t.Fatalf("test-identity-exec cache = %+v, want it cached", policy)
	}
	if policy.Deterministic {
		t.Fatal("test-identity-exec is deterministic, so the engine would store the skip of an unresolved identity")
	}
}
