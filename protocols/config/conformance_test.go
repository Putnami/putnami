package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Schema manifest fixtures.

func TestConformance_ValidSchemaFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}

	schemaFiles := make([]string, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "config-entry") ||
			strings.HasPrefix(base, "secret-entry") ||
			strings.HasPrefix(base, "delete-request") ||
			strings.HasPrefix(base, "resolve-secrets") ||
			base == "schema-with-registered-at.json" {
			continue
		}
		schemaFiles = append(schemaFiles, f)
	}
	if len(schemaFiles) == 0 {
		t.Fatal("no valid schema fixtures found")
	}

	for _, path := range schemaFiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, diags := ParseAndValidateSchemaManifest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if m == nil {
				t.Errorf("valid fixture %s returned nil manifest", path)
			}
		})
	}
}

func TestConformance_InvalidSchemaFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}

	schemaFiles := make([]string, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "config-entry") ||
			strings.HasPrefix(base, "secret-entry") ||
			strings.HasPrefix(base, "delete-request") ||
			strings.HasPrefix(base, "resolve-secrets") {
			continue
		}
		schemaFiles = append(schemaFiles, f)
	}
	if len(schemaFiles) == 0 {
		t.Fatal("no invalid schema fixtures found")
	}

	for _, path := range schemaFiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, pDiags := ParseSchemaManifest(data)
			if diag.HasErrors(pDiags) {
				return // parse error is sufficient for invalid fixtures
			}

			vDiags := ValidateSchemaManifest(m)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}

func TestConformance_ValidRegisteredSchemaFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/schema-with-registered-at*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid registered schema fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, diags := ParseAndValidateRegisteredSchemaManifest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if m == nil {
				t.Errorf("valid fixture %s returned nil manifest", path)
			}
			if m != nil && m.RegisteredAt == nil {
				t.Errorf("valid fixture %s should include registeredAt", path)
			}
		})
	}
}

// ConfigEntry fixtures.

func TestConformance_ValidConfigEntryFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/config-entry*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid config-entry fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			e, diags := ParseAndValidateConfigEntry(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if e == nil {
				t.Errorf("valid fixture %s returned nil entry", path)
			}
		})
	}
}

func TestConformance_InvalidConfigEntryFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/config-entry*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid config-entry fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			e, pDiags := ParseConfigEntry(data)
			if diag.HasErrors(pDiags) {
				return
			}

			vDiags := ValidateConfigEntry(e)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}

// SecretEntry fixtures.

func TestConformance_ValidSecretEntryFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/secret-entry*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid secret-entry fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			e, diags := ParseAndValidateSecretEntry(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if e == nil {
				t.Errorf("valid fixture %s returned nil entry", path)
			}
		})
	}
}

func TestConformance_InvalidSecretEntryFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/secret-entry*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid secret-entry fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			e, pDiags := ParseSecretEntry(data)
			if diag.HasErrors(pDiags) {
				return
			}

			vDiags := ValidateSecretEntry(e)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}

// DeleteRequest fixtures.

func TestConformance_ValidDeleteRequestFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/delete-request*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid delete-request fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			r, diags := ParseAndValidateDeleteRequest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if r == nil {
				t.Errorf("valid fixture %s returned nil request", path)
			}
		})
	}
}

func TestConformance_InvalidDeleteRequestFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/delete-request*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid delete-request fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			r, pDiags := ParseDeleteRequest(data)
			if diag.HasErrors(pDiags) {
				return
			}

			vDiags := ValidateDeleteRequest(r)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}

// ResolveSecretsRequest fixtures.

func TestConformance_ValidResolveSecretsRequestFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/resolve-secrets*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid resolve-secrets fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			r, diags := ParseAndValidateResolveSecretsRequest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if r == nil {
				t.Errorf("valid fixture %s returned nil request", path)
			}
		})
	}
}
