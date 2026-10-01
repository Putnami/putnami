package infra

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	pstorage "go.putnami.dev/protocol/storage"
)

// TestStoragesFromManifest_DerivesProjection proves the infra storage entries
// are derived from the canonical storage protocol: the output is exactly the
// access-mapped image of Manifest.Project(), sorted by resource name, carrying
// name/access/public/retention.
func TestStoragesFromManifest_DerivesProjection(t *testing.T) {
	m := &pstorage.Manifest{
		ProtocolVersion: pstorage.ProtocolVersion,
		Resources: []pstorage.Resource{
			{Name: "uploads", Access: pstorage.AccessReadWrite, Scope: pstorage.ScopeRuntime, SignedUrls: true, Retention: "30d"},
			{Name: "avatars", Access: pstorage.AccessRead, Public: true},
			{Name: "audit-logs", Access: pstorage.AccessWrite, Retention: "90d"},
		},
	}

	got, diags := StoragesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	want := []StorageBucket{
		{Name: "audit-logs", Access: StorageAccessWrite, Retention: "90d"},
		{Name: "avatars", Access: StorageAccessRead, Public: true},
		{Name: "uploads", Access: StorageAccessReadWrite, Retention: "30d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StoragesFromManifest = %#v, want %#v", got, want)
	}

	// The result must line up one-to-one with the protocol projection it is
	// built on, so infra cannot diverge from the storage protocol's view.
	projected := m.Project()
	if len(got) != len(projected) {
		t.Fatalf("len(got) = %d, len(Project()) = %d", len(got), len(projected))
	}
	for i := range projected {
		if got[i].Name != projected[i].Name || got[i].Public != projected[i].Public || got[i].Retention != projected[i].Retention {
			t.Errorf("entry %d = %#v, projection = %#v", i, got[i], projected[i])
		}
	}

	// The derived entries are a valid per-project manifest body.
	manifest := &PerProjectManifest{
		Schema:          PerProjectSchemaURL,
		ProtocolVersion: ProtocolVersion,
		Storage:         got,
	}
	if diags := ValidatePerProjectManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("derived manifest failed validation: %v", diags)
	}
}

// TestStoragesFromManifest_NilAndEmpty covers the no-op inputs.
func TestStoragesFromManifest_NilAndEmpty(t *testing.T) {
	if got, diags := StoragesFromManifest(nil); got != nil || diags != nil {
		t.Errorf("nil manifest: got %v, diags %v; want nil, nil", got, diags)
	}
	empty := &pstorage.Manifest{ProtocolVersion: pstorage.ProtocolVersion}
	if got, diags := StoragesFromManifest(empty); got != nil || diags != nil {
		t.Errorf("empty manifest: got %v, diags %v; want nil, nil", got, diags)
	}
}

// TestStoragesFromManifest_NoAccess maps a resource that declares no access to
// an entry with no access — a valid "no grant declared" state, never invented.
func TestStoragesFromManifest_NoAccess(t *testing.T) {
	m := &pstorage.Manifest{
		ProtocolVersion: pstorage.ProtocolVersion,
		Resources:       []pstorage.Resource{{Name: "scratch"}},
	}
	got, diags := StoragesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []StorageBucket{{Name: "scratch"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StoragesFromManifest = %#v, want %#v", got, want)
	}
}

// TestStoragesFromManifest_UnknownAccess reports access levels the infra
// protocol cannot represent instead of coercing them, and drops the offending
// entry.
func TestStoragesFromManifest_UnknownAccess(t *testing.T) {
	m := &pstorage.Manifest{
		ProtocolVersion: pstorage.ProtocolVersion,
		Resources: []pstorage.Resource{
			{Name: "uploads", Access: pstorage.AccessReadWrite},
			{Name: "legacy", Access: pstorage.Access("append")},
		},
	}
	got, diags := StoragesFromManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatalf("expected a diagnostic for the unmapped access, got none")
	}
	errs := diag.Errors(diags)
	if len(errs) != 1 || errs[0].Code != ErrorCodeInvalidAccess {
		t.Fatalf("diagnostics = %v, want one %s", diags, ErrorCodeInvalidAccess)
	}
	want := []StorageBucket{{Name: "uploads", Access: StorageAccessReadWrite}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StoragesFromManifest = %#v, want %#v (offending entry dropped)", got, want)
	}
}

// TestStoragesFromManifest_CannotLeakBindingCoordinates proves the projection
// carries only {name, access, public, retention}. The resolved physical
// coordinates a workload connects with (backend, bucket, prefix, identity) live
// only in a storage Binding — the Manifest has no field for them — and Project()
// additionally drops the isolation scope and the signed-URL signer. The derived
// infra entries serialize to none of those.
func TestStoragesFromManifest_CannotLeakBindingCoordinates(t *testing.T) {
	m := &pstorage.Manifest{
		ProtocolVersion: pstorage.ProtocolVersion,
		Resources: []pstorage.Resource{
			{Name: "uploads", Access: pstorage.AccessReadWrite, Scope: pstorage.ScopeRuntime, SignedUrls: true, Public: true, Retention: "30d"},
		},
	}
	got, diags := StoragesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{"backend", "bucket", "prefix", "identity", "scope", "signedUrls"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("derived infra storage leaked %q: %s", leaked, encoded)
		}
	}
}
