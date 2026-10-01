package config

import (
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid simple", "my-app", false},
		{"valid dots", "com.example.app", false},
		{"valid wildcard", "*", false},
		{"valid underscore", "my_app", false},
		{"empty", "", true},
		{"spaces", "my app", true},
		{"slashes", "my/app", true},
		{"colons", "my:app", true},
		{"too long", strings.Repeat("a", MaxNameLength+1), true},
		{"max length", strings.Repeat("a", MaxNameLength), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateName(tt.value, "field")
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("ValidateName(%q) errors=%v, want %v; diags=%v",
					tt.value, diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateAppName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		// appName is the project name, so the real shapes must validate.
		{"scoped npm package", "@putnami/application", false},
		{"go module path", "go.putnami.dev/events", false},
		{"nested go module path", "go.putnami.dev/examples/task-api", false},
		{"simple", "my-app", false},
		{"dots only", "putnami.dev", false},
		{"underscore", "my_app", false},
		{"wildcard", "*", false},
		{"max length", strings.Repeat("a", MaxNameLength), false},
		// Rejections: empty, oversized, disallowed characters.
		{"empty", "", true},
		{"too long", strings.Repeat("a", MaxNameLength+1), true},
		{"spaces", "my app", true},
		{"colons", "my:app", true},
		// Rejections: path-traversal shapes even though the chars are allowed.
		{"parent traversal", "app/../etc", true},
		{"bare parent", "../secrets", true},
		{"leading slash", "/etc/passwd", true},
		{"trailing slash", "app/", true},
		{"double slash", "a//b", true},
		{"dot segment", "a/./b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateAppName(tt.value, "appName")
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("ValidateAppName(%q) errors=%v, want %v; diags=%v",
					tt.value, diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateOptionalName(t *testing.T) {
	// Empty is valid for optional fields.
	diags := ValidateOptionalName("", "version")
	if diag.HasErrors(diags) {
		t.Errorf("empty optional name should not produce errors: %v", diags)
	}

	// Invalid characters still rejected.
	diags = ValidateOptionalName("bad name!", "version")
	if !diag.HasErrors(diags) {
		t.Error("invalid optional name should produce errors")
	}
}

func TestValidateConfigEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   *ConfigEntry
		wantErr bool
	}{
		{"valid", &ConfigEntry{AppName: "app", Environment: "prod", Path: "server", Values: map[string]any{"port": 8080}}, false},
		{"scoped appName", &ConfigEntry{AppName: "@putnami/application", Environment: "prod", Path: "server", Values: map[string]any{"port": 8080}}, false},
		{"go module appName", &ConfigEntry{AppName: "go.putnami.dev/examples/task-api", Environment: "prod", Path: "server", Values: map[string]any{"port": 8080}}, false},
		{"nil", nil, true},
		{"missing appName", &ConfigEntry{Environment: "prod", Path: "server", Values: map[string]any{}}, true},
		{"missing environment", &ConfigEntry{AppName: "app", Path: "server", Values: map[string]any{}}, true},
		{"missing path", &ConfigEntry{AppName: "app", Environment: "prod", Values: map[string]any{}}, true},
		{"nil values", &ConfigEntry{AppName: "app", Environment: "prod", Path: "server"}, true},
		{"bad appName chars", &ConfigEntry{AppName: "app/../etc", Environment: "prod", Path: "server", Values: map[string]any{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateConfigEntry(tt.entry)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateConfigEntry_MaxKeys(t *testing.T) {
	values := make(map[string]any, MaxValuesKeys+1)
	for i := range MaxValuesKeys + 1 {
		values[strings.Repeat("k", 5)+string(rune('a'+i%26))+strings.Repeat("0", 5)] = i
	}
	// Use distinct keys — build them properly.
	values2 := make(map[string]any, MaxValuesKeys+1)
	for i := range MaxValuesKeys + 1 {
		key := "key" + strings.Repeat("0", 10) + string(rune(i))
		values2[key] = i
	}
	e := &ConfigEntry{AppName: "app", Environment: "prod", Path: "server", Values: values2}
	diags := ValidateConfigEntry(e)
	found := false
	for _, d := range diags {
		if d.Code == "max-keys" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected max-keys diagnostic for %d keys", len(values2))
	}
}

func TestValidateSecretEntry(t *testing.T) {
	validEnvelope := SealedEnvelope{
		KeyURI:     "gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
		WrappedDEK: "dGVzdA==",
		Nonce:      "bm9uY2U=",
		Ciphertext: "Y2lwaGVy",
	}
	tests := []struct {
		name    string
		entry   *SecretEntry
		wantErr bool
	}{
		{"valid", &SecretEntry{AppName: "app", Environment: "prod", Path: "db", Envelope: validEnvelope}, false},
		{"nil", nil, true},
		{"missing appName", &SecretEntry{Environment: "prod", Path: "db", Envelope: validEnvelope}, true},
		{"empty envelope", &SecretEntry{AppName: "app", Environment: "prod", Path: "db", Envelope: SealedEnvelope{}}, true},
		{"invalid base64", &SecretEntry{AppName: "app", Environment: "prod", Path: "db", Envelope: SealedEnvelope{
			KeyURI:     validEnvelope.KeyURI,
			WrappedDEK: "not-base64!",
			Nonce:      validEnvelope.Nonce,
			Ciphertext: validEnvelope.Ciphertext,
		}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateSecretEntry(tt.entry)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateSealedEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		env     *SealedEnvelope
		wantErr bool
	}{
		{"valid", &SealedEnvelope{KeyURI: "k", WrappedDEK: "d3JhcHBlZA==", Nonce: "bm9uY2U=", Ciphertext: "Y2lwaGVydGV4dA=="}, false},
		{"nil", nil, true},
		{"missing keyUri", &SealedEnvelope{WrappedDEK: "d3JhcHBlZA==", Nonce: "bm9uY2U=", Ciphertext: "Y2lwaGVydGV4dA=="}, true},
		{"missing wrappedDek", &SealedEnvelope{KeyURI: "k", Nonce: "bm9uY2U=", Ciphertext: "Y2lwaGVydGV4dA=="}, true},
		{"missing nonce", &SealedEnvelope{KeyURI: "k", WrappedDEK: "d3JhcHBlZA==", Ciphertext: "Y2lwaGVydGV4dA=="}, true},
		{"missing ciphertext", &SealedEnvelope{KeyURI: "k", WrappedDEK: "d3JhcHBlZA==", Nonce: "bm9uY2U="}, true},
		{"invalid wrappedDek", &SealedEnvelope{KeyURI: "k", WrappedDEK: "not-base64!", Nonce: "bm9uY2U=", Ciphertext: "Y2lwaGVydGV4dA=="}, true},
		{"all empty", &SealedEnvelope{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateSealedEnvelope(tt.env)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateSealedEnvelope_InvalidBase64Diagnostic(t *testing.T) {
	diags := ValidateSealedEnvelope(&SealedEnvelope{
		KeyURI:     "kms://key",
		WrappedDEK: "%%%bad%%%",
		Nonce:      "bm9uY2U=",
		Ciphertext: "Y2lwaGVydGV4dA==",
	})

	found := false
	for _, d := range diags {
		if d.Code == "invalid-base64" && d.Field == "envelope.wrappedDek" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected invalid-base64 diagnostic for wrappedDek, got %v", diags)
	}
}

func TestValidateDeleteRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     *DeleteRequest
		wantErr bool
	}{
		{"valid", &DeleteRequest{AppName: "app", Environment: "prod", Path: "server"}, false},
		{"nil", nil, true},
		{"missing path", &DeleteRequest{AppName: "app", Environment: "prod"}, true},
		{"missing environment", &DeleteRequest{AppName: "app", Path: "server"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateDeleteRequest(tt.req)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}

func TestValidateResolveSecretsRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     *ResolveSecretsRequest
		wantErr bool
	}{
		{"valid", &ResolveSecretsRequest{AppName: "app", Environment: "prod"}, false},
		{"valid with version", &ResolveSecretsRequest{AppName: "app", Environment: "prod", Version: "1.0"}, false},
		{"nil", nil, true},
		{"missing appName", &ResolveSecretsRequest{Environment: "prod"}, true},
		{"missing environment", &ResolveSecretsRequest{AppName: "app"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateResolveSecretsRequest(tt.req)
			if diag.HasErrors(diags) != tt.wantErr {
				t.Errorf("got errors=%v, want %v; diags=%v", diag.HasErrors(diags), tt.wantErr, diags)
			}
		})
	}
}
