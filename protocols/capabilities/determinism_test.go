package capabilities

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sampleManifest builds a representative manifest exercising every
// contribution kind, telling the "sql capability" story: a project that
// contributes a config block with a sensitive field, a route, a migration
// bundle bound to a datasource, an infra database requirement, a readiness
// contributor, a lifecycle hook, a package version, and a RequiredCapability
// that ties them together. It is the shared input for the byte-stability and
// canonical-form guards.
func sampleManifest() *Manifest {
	prov := Provenance{
		Project:      "go.putnami.dev/example/iam",
		Package:      "go.putnami.dev/database",
		Version:      "1.4.0",
		SourceKind:   SourceKindFramework,
		EvidencePath: "example/iam/db.go",
	}
	return &Manifest{
		Schema:          "https://putnami.dev/schemas/putnami-capabilities.json",
		ProtocolVersion: ProtocolVersion,
		Project:         "go.putnami.dev/example/iam",
		ConfigDefinitions: []ConfigDefinition{{
			Path: "database.default",
			Fields: []ConfigField{
				{Name: "url", Type: "string"},
				{Name: "password", Type: "string", Sensitive: true},
			},
			Provenance: prov,
		}},
		Schemas: []SchemaContribution{{
			Name:       "listUsers",
			Kind:       SchemaKindRoute,
			Path:       "/api/users",
			Provenance: prov,
		}},
		Discoverers: []Discoverer{{
			Name:       "sqlSources",
			Kind:       DiscovererKindSource,
			Provenance: prov,
		}},
		Migrations: []MigrationBundle{{
			Name:       "iam",
			Datasource: "default",
			Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Provenance: prov,
		}},
		InfraRequirements: []InfraRequirement{{
			Name:       "primary",
			Kind:       InfraKindDatabase,
			Provenance: prov,
		}},
		HealthContributors: []HealthContributor{{
			Name:       "primaryDatabase",
			Probe:      ProbeKindReadiness,
			Provenance: prov,
		}},
		LifecycleHooks: []LifecycleHook{{
			Name:       "connectionPool",
			Phase:      LifecyclePhaseStarter,
			Provenance: prov,
		}},
		PackageVersions: []PackageVersion{{
			Package:    "go.putnami.dev/database",
			Version:    "1.4.0",
			Provenance: prov,
		}},
		RequiredCapabilities: []RequiredCapability{{
			Name: "sql",
			Requires: []CapabilityKind{
				CapabilityKindDatasource,
				CapabilityKindMigration,
				CapabilityKindReadiness,
			},
			Provenance: prov,
		}},
	}
}

// canonical returns the canonical serialization of m: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form
// both the Go and the TypeScript emitters must reproduce (the TS equivalent is
// TypeScript producer's Go-compatible canonical serializer).
func canonical(t *testing.T, m *Manifest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestSerialization_Stable100 verifies the canonical serialization of the same
// manifest is byte-identical across 100 marshals — no map iteration or other
// nondeterminism leaks into the wire form.
func TestSerialization_Stable100(t *testing.T) {
	m := sampleManifest()
	first := canonical(t, m)
	for i := range 100 {
		if got := canonical(t, m); !bytes.Equal(got, first) {
			t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

// TestSerialization_CanonicalByteForm pins the exact canonical bytes: the
// committed fixture fixtures/valid/full.json must equal the Go serialization
// of sampleManifest(). This is the cross-language contract — the TypeScript
// emitter must produce this same file byte-for-byte.
func TestSerialization_CanonicalByteForm(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("fixtures", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := canonical(t, sampleManifest())
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical serialization does not match fixtures/valid/full.json.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRoundTrip_Idempotent verifies parse → marshal → parse → marshal is
// byte-stable for every valid fixture: re-serializing a parsed manifest yields
// the same bytes the second time, so the wire form is a fixed point.
func TestRoundTrip_Idempotent(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m1, diags := ParseManifest(data)
			if m1 == nil {
				t.Fatalf("parse %s: %v", path, diags)
			}
			once := canonical(t, m1)
			m2, _ := ParseManifest(once)
			if m2 == nil {
				t.Fatalf("re-parse %s produced nil manifest", path)
			}
			twice := canonical(t, m2)
			if !bytes.Equal(once, twice) {
				t.Fatalf("%s: round-trip not idempotent\nonce:  %s\ntwice: %s", path, once, twice)
			}
		})
	}
}
