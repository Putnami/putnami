package configcli

import (
	"strings"
	"testing"
)

func serverSchemaDoc() map[string]any {
	return map[string]any{
		"appName":    "my-app",
		"version":    "1.0.0",
		"schemaHash": "sha256:test",
		"configs": []any{
			map[string]any{
				"path": "server",
				"fields": []any{
					map[string]any{"name": "host", "type": "string"},
					map[string]any{"name": "port", "type": "int"},
				},
			},
		},
	}
}

func TestValidateBlockWrites_ValidValuesPass(t *testing.T) {
	diags, err := validateBlockWrites(serverSchemaDoc(), "my-app", "prod", []blockWrite{
		{Path: "server", Values: map[string]any{"host": "localhost", "port": 8080}},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("valid values produced diagnostics: %#v", diags)
	}
	if err := configValidationError(diags); err != nil {
		t.Fatalf("configValidationError on no diags = %v, want nil", err)
	}
}

func TestValidateBlockWrites_InvalidTypeFails(t *testing.T) {
	// port is declared int; a non-numeric string is exactly the value
	// class the runtime could not load — it must be rejected client-side.
	diags, err := validateBlockWrites(serverSchemaDoc(), "my-app", "prod", []blockWrite{
		{Path: "server", Values: map[string]any{"host": "localhost", "port": "not-a-number"}},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("invalid int value produced no diagnostics")
	}
	if diags[0].Path != "server" {
		t.Errorf("diag path = %q, want server", diags[0].Path)
	}
	gateErr := configValidationError(diags)
	if gateErr == nil {
		t.Fatal("configValidationError returned nil for a failing block")
	}
	if !strings.Contains(gateErr.Error(), "server") {
		t.Errorf("gate error missing block context: %v", gateErr)
	}
}

func TestValidateBlockWrites_PathNotInSchemaFails(t *testing.T) {
	diags, err := validateBlockWrites(serverSchemaDoc(), "my-app", "prod", []blockWrite{
		{Path: "phantom", Values: map[string]any{"x": 1}},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("a path absent from the schema produced no diagnostics")
	}
}
