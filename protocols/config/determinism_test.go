package config

import (
	"reflect"
	"testing"
)

func TestValidateSchemaManifest_Deterministic(t *testing.T) {
	m := &SchemaManifest{
		AppName: "test",
		Configs: []Block{
			{Path: "zebra", Fields: []FieldSchema{
				{Name: "c", Type: "string"},
				{Name: "a", Type: "badtype"},
				{Name: "b", Type: "int"},
			}},
			{Path: "alpha", Fields: []FieldSchema{
				{Name: "x", Type: ""},
			}},
		},
	}

	first := ValidateSchemaManifest(m)
	for i := 0; i < 100; i++ {
		got := ValidateSchemaManifest(m)
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("diagnostics changed on iteration %d:\nfirst: %v\ngot:   %v", i, first, got)
		}
	}
}

func TestComputeSchemaHash_Deterministic100(t *testing.T) {
	blocks := []Block{
		{Path: "z", Fields: []FieldSchema{{Name: "b", Type: "int"}, {Name: "a", Type: "string"}}},
		{Path: "a", Fields: []FieldSchema{{Name: "x", Type: "bool"}}},
	}

	first := ComputeSchemaHash(blocks)
	for i := 0; i < 100; i++ {
		if got := ComputeSchemaHash(blocks); got != first {
			t.Fatalf("hash changed on iteration %d: %q != %q", i, got, first)
		}
	}
}
