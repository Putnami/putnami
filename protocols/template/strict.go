package template

import (
	"bytes"
	"encoding/json"
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseManifestStrict decodes JSON data into a Manifest using strict mode.
// Unknown fields are rejected and structured diagnostics are returned.
func ParseManifestStrict(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse template manifest: %v", err),
		}
	}
	return &m, nil
}

// ValidateManifest checks structural invariants on a parsed template manifest.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-manifest", "", "manifest is nil"),
		}
	}

	var diags []diag.Diagnostic

	if m.Name == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "name is required"))
	}
	if m.Description == "" {
		diags = append(diags, diag.Errorf("required-field", "description", "description is required"))
	}

	return diags
}

// ParseAndValidateManifest combines strict parsing and validation in one call.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifestStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	vDiags := ValidateManifest(m)
	diags = append(diags, vDiags...)

	return m, diags
}

// StrictLoadManifest reads data and applies strict parse and validate.
func StrictLoadManifest(path string, data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		for i := range diags {
			diags[i].Message = fmt.Sprintf("%s: %s", path, diags[i].Message)
		}
	}
	return m, diags
}
