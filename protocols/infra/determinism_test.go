package infra

import (
	"reflect"
	"testing"
)

// TestMerge_Deterministic verifies Merge produces byte-deterministic
// output regardless of contribution input order or Go map iteration.
func TestMerge_Deterministic(t *testing.T) {
	makeContribs := func() []ProjectContribution {
		return []ProjectContribution{
			{
				Project:     "go.putnami.dev/example/iam",
				Contributor: ContributorManual,
				Manifest: PerProjectManifest{
					ProtocolVersion: ProtocolVersion,
					Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"iam"}}},
					Secrets:         []string{"jwks_signing_key"},
				},
			},
			{
				Project:     "go.putnami.dev/example/audit",
				Contributor: FrameworkContributor("go.putnami.dev/database"),
				Manifest: PerProjectManifest{
					ProtocolVersion: ProtocolVersion,
					Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"audit"}}},
					Storage:         []StorageBucket{{Name: "audit-logs", Retention: "90d"}},
					Events:          &Events{Publishes: []string{"audit.event.recorded"}},
				},
			},
			{
				Project:     "go.putnami.dev/example/api",
				Contributor: ContributorManual,
				Manifest: PerProjectManifest{
					ProtocolVersion: ProtocolVersion,
					Events:          &Events{Publishes: []string{"workspace.deploy.completed"}},
				},
			},
		}
	}

	canonical, canonicalDiags := Merge("go.putnami.dev/example/api", makeContribs())
	if len(canonicalDiags) != 0 {
		t.Fatalf("unexpected diagnostics from canonical merge: %v", canonicalDiags)
	}

	for i := 0; i < 100; i++ {
		got, diags := Merge("go.putnami.dev/example/api", makeContribs())
		if len(diags) != 0 {
			t.Fatalf("iteration %d: unexpected diagnostics: %v", i, diags)
		}
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: Merge produced non-deterministic output.\ncanonical: %+v\ngot:       %+v", i, canonical, got)
		}
	}
}

// TestMerge_OrderIndependent verifies Merge produces identical output
// regardless of contribution caller order.
func TestMerge_OrderIndependent(t *testing.T) {
	a := ProjectContribution{
		Project: "a", Contributor: ContributorManual,
		Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"a"}}},
		},
	}
	b := ProjectContribution{
		Project: "b", Contributor: ContributorManual,
		Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"b"}}},
		},
	}
	c := ProjectContribution{
		Project: "c", Contributor: ContributorManual,
		Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"c"}}},
		},
	}

	orderings := [][]ProjectContribution{
		{a, b, c},
		{c, b, a},
		{b, a, c},
		{a, c, b},
	}

	canonical, _ := Merge("w", orderings[0])
	for i, order := range orderings[1:] {
		got, _ := Merge("w", order)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("ordering %d produced different output.\ncanonical: %+v\ngot:       %+v", i+1, canonical, got)
		}
	}
}

// TestValidatePerProjectManifest_Deterministic verifies validation
// diagnostics appear in a stable order.
func TestValidatePerProjectManifest_Deterministic(t *testing.T) {
	m := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		Databases: []Database{
			{Name: "Invalid!", Engine: "mongo"},
			{Name: "another-bad-name!", Engine: EnginePostgres},
		},
		Secrets: []string{"INVALID-secret"},
	}
	canonical := ValidatePerProjectManifest(m)
	if len(canonical) == 0 {
		t.Fatal("expected diagnostics")
	}
	for i := 0; i < 100; i++ {
		got := ValidatePerProjectManifest(m)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidatePerProjectManifest produced non-deterministic diagnostics", i)
		}
	}
}
