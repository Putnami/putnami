package config

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseSchemaManifest_Valid(t *testing.T) {
	data := []byte(`{
		"appName": "test",
		"version": "1.0.0",
		"schemaHash": "sha256:abcdef0123456789",
		"configs": [{"path": "server", "fields": [{"name": "port", "type": "int"}]}]
	}`)

	m, diags := ParseSchemaManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m.AppName != "test" {
		t.Errorf("appName = %q, want %q", m.AppName, "test")
	}
}

func TestParseSchemaManifest_UnknownField(t *testing.T) {
	data := []byte(`{"appName": "test", "unknown": true, "configs": []}`)
	_, diags := ParseSchemaManifest(data)
	if !diag.HasErrors(diags) {
		t.Error("expected error for unknown field")
	}
}

func TestParseSchemaManifest_RejectsRegisteredAt(t *testing.T) {
	data := []byte(`{
		"appName": "test",
		"version": "1.0.0",
		"schemaHash": "sha256:abcdef0123456789",
		"registeredAt": "2026-03-15T10:30:00Z",
		"configs": [{"path": "server", "fields": [{"name": "port", "type": "int"}]}]
	}`)

	_, diags := ParseSchemaManifest(data)
	if !diag.HasErrors(diags) {
		t.Error("expected error for registeredAt on publish-time schema manifest")
	}
}

func TestValidateSchemaManifest_MissingAppName(t *testing.T) {
	m := &SchemaManifest{
		Configs: []Block{{Path: "server", Fields: []FieldSchema{{Name: "port", Type: "int"}}}},
	}
	diags := ValidateSchemaManifest(m)
	if !diag.HasErrors(diags) {
		t.Error("expected error for missing appName")
	}
}

// TestValidateSchemaManifest_ProjectNameAppName guards the register-schema path
// against regressing on real project names: appName is the project name, which
// for scoped npm packages and Go modules contains '@' and '/'.
func TestValidateSchemaManifest_ProjectNameAppName(t *testing.T) {
	for _, appName := range []string{"@putnami/application", "go.putnami.dev/examples/task-api"} {
		m := &SchemaManifest{
			AppName: appName,
			Configs: []Block{{Path: "server", Fields: []FieldSchema{{Name: "port", Type: "int"}}}},
		}
		if diags := ValidateSchemaManifest(m); diag.HasErrors(diags) {
			t.Errorf("appName %q should validate, got %v", appName, diags)
		}
	}
}

func TestValidateSchemaManifest_EmptyConfigs(t *testing.T) {
	m := &SchemaManifest{AppName: "test", Configs: []Block{}}
	diags := ValidateSchemaManifest(m)
	if !diag.HasErrors(diags) {
		t.Error("expected error for empty configs")
	}
}

func TestValidateSchemaManifest_BadHashPrefix(t *testing.T) {
	m := &SchemaManifest{
		AppName:    "test",
		SchemaHash: "fnv:abc123",
		Configs:    []Block{{Path: "s", Fields: []FieldSchema{{Name: "a", Type: "string"}}}},
	}
	diags := ValidateSchemaManifest(m)
	if !diag.HasErrors(diags) {
		t.Error("expected error for bad hash prefix")
	}
}

func TestValidateSchemaManifest_InvalidFieldType(t *testing.T) {
	m := &SchemaManifest{
		AppName: "test",
		Configs: []Block{{Path: "s", Fields: []FieldSchema{{Name: "a", Type: "number"}}}},
	}
	diags := ValidateSchemaManifest(m)
	if !diag.HasErrors(diags) {
		t.Error("expected error for invalid field type 'number'")
	}
}

func TestValidateSchemaManifest_DuplicatePath(t *testing.T) {
	m := &SchemaManifest{
		AppName: "test",
		Configs: []Block{
			{Path: "server", Fields: []FieldSchema{{Name: "a", Type: "string"}}},
			{Path: "server", Fields: []FieldSchema{{Name: "b", Type: "int"}}},
		},
	}
	diags := ValidateSchemaManifest(m)
	found := false
	for _, d := range diags {
		if d.Code == "duplicate-path" {
			found = true
		}
	}
	if !found {
		t.Error("expected duplicate-path diagnostic")
	}
}

func TestValidateSchemaManifest_InvalidPath(t *testing.T) {
	m := &SchemaManifest{
		AppName: "test",
		Configs: []Block{
			{Path: "server/api", Fields: []FieldSchema{{Name: "port", Type: "int"}}},
		},
	}
	diags := ValidateSchemaManifest(m)
	if !diag.HasErrors(diags) {
		t.Error("expected error for invalid config block path")
	}
}

func TestNormalizeSchemaManifest(t *testing.T) {
	m := &SchemaManifest{AppName: "test"}
	NormalizeSchemaManifest(m)
	if m.Configs == nil {
		t.Error("configs should not be nil after normalize")
	}
}

func TestParseAndValidateSchemaManifest(t *testing.T) {
	data := []byte(`{
		"appName": "test",
		"version": "1.0.0",
		"schemaHash": "sha256:abcdef0123456789",
		"configs": [{"path": "server", "fields": [{"name": "port", "type": "int"}]}]
	}`)

	m, diags := ParseAndValidateSchemaManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
}

func TestParseAndValidateRegisteredSchemaManifest(t *testing.T) {
	data := []byte(`{
		"appName": "test",
		"version": "1.0.0",
		"schemaHash": "sha256:abcdef0123456789",
		"registeredAt": "2026-03-15T10:30:00Z",
		"configs": [{"path": "server", "fields": [{"name": "port", "type": "int"}]}]
	}`)

	m, diags := ParseAndValidateRegisteredSchemaManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("registered manifest should not be nil")
	}
	if m.RegisteredAt == nil {
		t.Fatal("registeredAt should not be nil")
	}
}

func TestValidateResolveRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     *ResolveRequest
		wantErr bool
	}{
		{"valid", &ResolveRequest{AppName: "app", Environment: "prod"}, false},
		{"nil", nil, true},
		{"missing appName", &ResolveRequest{Environment: "prod"}, true},
		{"missing environment", &ResolveRequest{AppName: "app"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateResolveRequest(tt.req)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}
