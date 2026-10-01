package migration

import (
	"reflect"
	"testing"
)

func TestNormalizeDefinitions_Deterministic(t *testing.T) {
	hashA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hashC := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	defs := []Definition{
		{Datasource: "wealth", Name: "20260101000020-zeta", Hash: hashA, SourceKind: SourceRegistry},
		{Name: "20260101000010-alpha", Hash: hashB, SourceKind: SourceSQLFile},
		{Datasource: "auth", Name: "20260101000030-middle", OrderKey: "20260101000015-middle", Hash: hashC, SourceKind: SourceGenerated},
	}

	canonical := NormalizeDefinitions(defs)
	for i := 0; i < 100; i++ {
		got := NormalizeDefinitions(defs)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: NormalizeDefinitions produced non-deterministic output.\ncanonical: %#v\ngot: %#v", i, canonical, got)
		}
	}

	if canonical[0].Datasource != "auth" {
		t.Fatalf("expected auth datasource first, got %q", canonical[0].Datasource)
	}
	if canonical[1].Datasource != DefaultDatasource {
		t.Fatalf("expected default datasource second, got %q", canonical[1].Datasource)
	}
	if canonical[2].Datasource != "wealth" {
		t.Fatalf("expected wealth datasource last, got %q", canonical[2].Datasource)
	}
}
