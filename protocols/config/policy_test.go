package config

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func testManifest() *SchemaManifest {
	return &SchemaManifest{
		AppName: "task-api",
		Configs: []Block{
			{
				Path: "database",
				Fields: []FieldSchema{
					{Name: "host", Type: "string"},
					{Name: "port", Type: "int"},
					{Name: "password", Type: "string", Sensitive: true},
				},
			},
			{
				Path: "session",
				Fields: []FieldSchema{
					{Name: "cookieSecret", Type: "string", Sensitive: true},
				},
			},
			{
				Path: "server",
				Fields: []FieldSchema{
					{Name: "tls", Type: "object", Fields: []FieldSchema{
						{Name: "certFile", Type: "string"},
						{Name: "keyFile", Type: "string", Sensitive: true},
					}},
					{Name: "listeners", Type: "array", Items: &FieldSchema{
						Type: "object",
						Fields: []FieldSchema{
							{Name: "host", Type: "string"},
							{Name: "port", Type: "int"},
						},
					}},
					{Name: "labels", Type: "map", Keys: "string", Values: &FieldSchema{Type: "string"}},
					{Name: "portsByName", Type: "map", Keys: "string", Values: &FieldSchema{Type: "int"}},
					{Name: "tokens", Type: "map", Keys: "string", Values: &FieldSchema{
						Type: "object",
						Fields: []FieldSchema{
							{Name: "publicID", Type: "string"},
							{Name: "secret", Type: "string", Sensitive: true},
						},
					}},
				},
			},
		},
	}
}

func TestValidateConfigEntryAgainstSchema_RejectsSensitive(t *testing.T) {
	entry := &ConfigEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "database",
		Values: map[string]any{
			"host":     "db.local",
			"password": "secret",
		},
	}

	diags := ValidateConfigEntryAgainstSchema(entry, testManifest())
	if !diag.HasErrors(diags) {
		t.Fatalf("expected error for sensitive field in config entry, got %v", diags)
	}

	var found bool
	for _, d := range diags {
		if d.Code == "sensitive-in-config" && d.Field == "values.password" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sensitive-in-config diagnostic on values.password, got %v", diags)
	}
}

func TestValidateConfigEntryAgainstSchema_AllowsPlainFields(t *testing.T) {
	entry := &ConfigEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "database",
		Values: map[string]any{
			"host": "db.local",
			"port": 5432,
		},
	}

	diags := ValidateConfigEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for plain fields, got %v", diags)
	}
}

func TestValidateConfigEntryAgainstSchema_ValidatesCompositeValues(t *testing.T) {
	entry := &ConfigEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "server",
		Values: map[string]any{
			"tls": map[string]any{
				"certFile": "/etc/tls/cert.pem",
			},
			"listeners": []any{
				map[string]any{"host": "127.0.0.1", "port": float64(8080)},
			},
			"labels":      map[string]any{"tier": "edge"},
			"portsByName": map[string]any{"http": float64(8080)},
		},
	}

	diags := ValidateConfigEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for valid composite fields, got %v", diags)
	}
}

func TestValidateConfigEntryAgainstSchema_RejectsCompositePolicyViolations(t *testing.T) {
	entry := &ConfigEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "server",
		Values: map[string]any{
			"tls": map[string]any{
				"certFile": 99,
				"keyFile":  "secret",
			},
			"listeners": []any{
				map[string]any{"host": "127.0.0.1", "port": "8080"},
			},
			"labels":      map[string]any{"tier": 7},
			"portsByName": map[string]any{"http": "8080"},
			"unknown":     true,
		},
	}

	diags := ValidateConfigEntryAgainstSchema(entry, testManifest())
	wantCodes := map[string]bool{
		"field-not-in-schema":  false,
		"invalid-config-value": false,
		"sensitive-in-config":  false,
	}
	for _, d := range diags {
		if _, ok := wantCodes[d.Code]; ok {
			wantCodes[d.Code] = true
		}
	}
	for code, found := range wantCodes {
		if !found {
			t.Errorf("expected diagnostic code %q in %v", code, diags)
		}
	}
}

func TestValidateConfigEntryAgainstSchema_UnknownBlockNoOp(t *testing.T) {
	// Unknown block paths are handled by ValidatePathInSchema, not here.
	entry := &ConfigEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "cache",
		Values:      map[string]any{"ttl": "5m"},
	}

	diags := ValidateConfigEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for path absent from schema, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_WarnsOnNonSensitive(t *testing.T) {
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "database.host",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if diag.HasErrors(diags) {
		t.Fatalf("over-encrypting a non-sensitive field should warn, not error: %v", diags)
	}
	if len(diags) == 0 {
		t.Fatal("expected a warning diagnostic")
	}
	if diags[0].Code != "non-sensitive-in-secret" {
		t.Errorf("expected non-sensitive-in-secret diagnostic, got %v", diags[0])
	}
	if diags[0].Severity != diag.Warning {
		t.Errorf("expected warning severity, got %s", diags[0].Severity)
	}
}

func TestValidateSecretEntryAgainstSchema_QuietOnSensitive(t *testing.T) {
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "database.password",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for sensitive field in secret store, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_QuietOnNestedSensitive(t *testing.T) {
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "server.tls.keyFile",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for nested sensitive field in secret store, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_QuietOnMapValueSensitive(t *testing.T) {
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "server.tokens.github.secret",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for sensitive field under map value, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_WarnsOnMapValuePlaintext(t *testing.T) {
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "server.tokens.github.publicID",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) == 0 || diags[0].Code != "non-sensitive-in-secret" {
		t.Errorf("expected non-sensitive warning for plaintext field under map value, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_UnknownPathNoOp(t *testing.T) {
	// Unknown paths handled by ValidatePathInSchema.
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "stripe.apiKey",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for path absent from schema, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_BlockPathQuietOnAtLeastOneSensitive(t *testing.T) {
	// "database" block has both plaintext and sensitive fields. A block-level
	// secret entry is plausible (caller chose to encrypt the bundle).
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "database",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, testManifest())
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics when block has any sensitive field, got %v", diags)
	}
}

func TestValidateSecretEntryAgainstSchema_BlockPathWarnsOnAllPlaintext(t *testing.T) {
	// "cache" block has only plaintext fields. Storing the whole block in
	// the secrets store would over-encrypt non-sensitive config.
	manifest := &SchemaManifest{
		AppName: "task-api",
		Configs: []Block{
			{
				Path: "cache",
				Fields: []FieldSchema{
					{Name: "ttl", Type: "duration"},
					{Name: "maxItems", Type: "int"},
				},
			},
		},
	}
	entry := &SecretEntry{
		AppName:     "task-api",
		Environment: "production",
		Path:        "cache",
	}

	diags := ValidateSecretEntryAgainstSchema(entry, manifest)
	if diag.HasErrors(diags) {
		t.Fatalf("over-encrypting a non-sensitive block should warn, not error: %v", diags)
	}
	if len(diags) == 0 {
		t.Fatal("expected a warning diagnostic for all-plaintext block")
	}
	if diags[0].Code != "non-sensitive-in-secret" {
		t.Errorf("expected non-sensitive-in-secret diagnostic, got %v", diags[0])
	}
}

func TestValidatePathInSchema(t *testing.T) {
	m := testManifest()
	tests := []struct {
		path    string
		wantErr bool
	}{
		{"database", false},
		{"database.host", false},
		{"database.password", false},
		{"session.cookieSecret", false},
		{"server.tls.certFile", false},
		{"server.tls.keyFile", false},
		{"server.listeners.host", false},
		{"server.listeners.0.host", false},
		{"server.tokens.github.secret", false},
		{"server.tokens.github.publicID", false},
		{"cache", true},
		{"database.unknown", true},
		{"server.tls.missing", true},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			diags := ValidatePathInSchema(tt.path, m)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("ValidatePathInSchema(%q) errors=%v, want %v; diags=%v",
					tt.path, diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidatePathInSchema_PrefersLongestDottedBlock(t *testing.T) {
	m := &SchemaManifest{
		AppName: "task-api",
		Configs: []Block{
			{Path: "database", Fields: []FieldSchema{{Name: "host", Type: "string"}}},
			{Path: "database.primary", Fields: []FieldSchema{{Name: "password", Type: "string", Sensitive: true}}},
		},
	}

	if diags := ValidatePathInSchema("database.primary.password", m); diag.HasErrors(diags) {
		t.Fatalf("expected database.primary.password to match longest block prefix, got %v", diags)
	}
	if diags := ValidateSecretEntryAgainstSchema(&SecretEntry{Path: "database.primary.password"}, m); len(diags) != 0 {
		t.Fatalf("expected dotted block sensitive field to be accepted in secret store, got %v", diags)
	}
}
