package storage

import (
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// findCode reports whether diags contains a diagnostic with the given code.
func findCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestEnumValid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
		ok    bool
	}{
		{"access read", AccessRead.Valid(), true},
		{"access readwrite", AccessReadWrite.Valid(), true},
		{"access bogus", Access("append").Valid(), false},
		{"scope runtime", ScopeRuntime.Valid(), true},
		{"scope bogus", Scope("tenant").Valid(), false},
		{"identity static", IdentityStatic.Valid(), true},
		{"identity workload", IdentityWorkload.Valid(), true},
		{"identity bogus", Identity("oauth").Valid(), false},
	} {
		if tc.valid != tc.ok {
			t.Errorf("%s: Valid() = %v, want %v", tc.name, tc.valid, tc.ok)
		}
	}
}

func TestParseManifest_UnknownField(t *testing.T) {
	_, diags := ParseManifest([]byte(`{"protocolVersion": 1, "nope": true}`))
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseManifest_InvalidJSON(t *testing.T) {
	_, diags := ParseManifest([]byte("{not json"))
	if !findCode(diags, ErrorCodeParseError) {
		t.Errorf("want %s, got %v", ErrorCodeParseError, diags)
	}
}

func TestParseAndValidateManifest_RoundTrip(t *testing.T) {
	data := []byte(`{
		"protocolVersion": 1,
		"resources": [
			{ "name": "uploads", "access": "readwrite", "scope": "runtime", "signedUrls": true, "public": true, "retention": "30d" }
		]
	}`)
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil || len(m.Resources) != 1 {
		t.Fatalf("expected one resource, got %+v", m)
	}
	r := m.Resources[0]
	if r.Access != AccessReadWrite || r.Scope != ScopeRuntime || !r.SignedUrls || !r.Public || r.Retention != "30d" {
		t.Errorf("round-trip mismatch: %+v", r)
	}
}

func TestValidateManifest_InvalidName(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Resources: []Resource{{Name: "Bad!Name"}}}
	if diags := ValidateManifest(m); !findCode(diags, ErrorCodeInvalidName) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidName, diags)
	}
}

func TestValidateManifest_InvalidAccess(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Resources: []Resource{{Name: "uploads", Access: "append"}}}
	if diags := ValidateManifest(m); !findCode(diags, ErrorCodeInvalidAccess) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidAccess, diags)
	}
}

func TestValidateManifest_InvalidScope(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Resources: []Resource{{Name: "uploads", Scope: "tenant"}}}
	if diags := ValidateManifest(m); !findCode(diags, ErrorCodeInvalidScope) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidScope, diags)
	}
}

func TestValidateManifest_InvalidProtocolVersion(t *testing.T) {
	m := &Manifest{ProtocolVersion: 99, Resources: []Resource{{Name: "uploads"}}}
	if diags := ValidateManifest(m); !findCode(diags, ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidProtocolVersion, diags)
	}
}

func TestParseAndValidateBinding_RoundTrip(t *testing.T) {
	data := []byte(`{
		"protocolVersion": 1,
		"name": "uploads",
		"backend": "gcs",
		"bucket": "acme-uploads-prod",
		"prefix": "preview-42/",
		"identity": "workload",
		"signedUrls": true
	}`)
	b, diags := ParseAndValidateBinding(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if b.Backend != "gcs" || b.Bucket != "acme-uploads-prod" || b.Prefix != "preview-42/" ||
		b.Identity != IdentityWorkload || !b.SignedUrls {
		t.Errorf("round-trip mismatch: %+v", b)
	}
}

func TestValidateBinding_MissingBackendAndBucket(t *testing.T) {
	b := &Binding{ProtocolVersion: 1, Name: "uploads"}
	diags := ValidateBinding(b)
	if !findCode(diags, ErrorCodeMissingBackend) {
		t.Errorf("want %s, got %v", ErrorCodeMissingBackend, diags)
	}
	if !findCode(diags, ErrorCodeMissingBucket) {
		t.Errorf("want %s, got %v", ErrorCodeMissingBucket, diags)
	}
}

func TestValidateBinding_InvalidIdentity(t *testing.T) {
	b := &Binding{ProtocolVersion: 1, Name: "uploads", Backend: "gcs", Bucket: "b", Identity: "oauth"}
	if diags := ValidateBinding(b); !findCode(diags, ErrorCodeInvalidIdentity) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidIdentity, diags)
	}
}

func TestProject_FlattensSortedByName(t *testing.T) {
	m := &Manifest{
		ProtocolVersion: ProtocolVersion,
		Resources: []Resource{
			{Name: "uploads", Access: AccessReadWrite, Scope: ScopeRuntime, SignedUrls: true, Retention: "30d"},
			{Name: "avatars", Access: AccessRead, Public: true},
			{Name: "audit-logs", Access: AccessWrite, Retention: "90d"},
		},
	}
	got := m.Project()
	want := []ProjectedResource{
		{Name: "audit-logs", Access: AccessWrite, Retention: "90d"},
		{Name: "avatars", Access: AccessRead, Public: true},
		{Name: "uploads", Access: AccessReadWrite, Retention: "30d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Project() = %#v, want %#v", got, want)
	}
}

func TestProject_NilAndEmpty(t *testing.T) {
	if got := (*Manifest)(nil).Project(); got != nil {
		t.Errorf("nil manifest: Project() = %#v, want nil", got)
	}
	if got := (&Manifest{ProtocolVersion: ProtocolVersion}).Project(); got != nil {
		t.Errorf("empty manifest: Project() = %#v, want nil", got)
	}
}

// TestProject_DropsResolutionAndSignerFields proves Scope and SignedUrls never
// reach the infra projection: scope is a logical→physical resolution concern and
// the signed-URL signer grant is a workload-level concern, so neither belongs in
// the thin {name, access, public, retention} requirement a deployer provisions
// from. ProjectedResource has no field for either, so the type itself enforces
// the guarantee.
func TestProject_DropsResolutionAndSignerFields(t *testing.T) {
	m := &Manifest{
		ProtocolVersion: ProtocolVersion,
		Resources:       []Resource{{Name: "uploads", Access: AccessReadWrite, Scope: ScopeRuntime, SignedUrls: true}},
	}
	got := m.Project()
	if len(got) != 1 {
		t.Fatalf("Project() returned %d entries, want 1", len(got))
	}
	if want := (ProjectedResource{Name: "uploads", Access: AccessReadWrite}); got[0] != want {
		t.Errorf("Project()[0] = %#v, want %#v", got[0], want)
	}
}
