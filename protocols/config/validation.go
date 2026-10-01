package config

import (
	"encoding/base64"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Naming constraints enforced by the config server.
const (
	// MaxNameLength is the maximum length for appName, environment, path, and version fields.
	MaxNameLength = 256

	// MaxValuesKeys is the maximum number of top-level keys in a config values map.
	MaxValuesKeys = 1000
)

// namePattern matches valid identifiers for environment, path, and version.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9.*_-]+$`)

// appNamePattern matches valid appName identifiers. It is intentionally more
// permissive than namePattern because appName is the project name, which for
// Go modules (go.putnami.dev/events) and scoped npm packages
// (@putnami/application) legitimately contains '/' and '@'. Path-traversal
// shapes are rejected separately by ValidateAppName so a project name can
// never escape the config server's per-app storage prefix.
var appNamePattern = regexp.MustCompile(`^[a-zA-Z0-9.*_@/-]+$`)

// ValidateName checks that a string matches the naming constraints:
// non-empty, max 256 chars, pattern [a-zA-Z0-9.*_-]+.
func ValidateName(value, field string) []diag.Diagnostic {
	if value == "" {
		return []diag.Diagnostic{
			diag.Errorf("required-field", field, "%s is required", field),
		}
	}
	var diags []diag.Diagnostic
	if len(value) > MaxNameLength {
		diags = append(diags, diag.Errorf("max-length", field,
			"%s exceeds maximum length of %d characters", field, MaxNameLength))
	}
	if !namePattern.MatchString(value) {
		diags = append(diags, diag.Errorf("invalid-name", field,
			"%s contains invalid characters; must match [a-zA-Z0-9.*_-]+", field))
	}
	return diags
}

// ValidateOptionalName checks naming constraints for an optional field.
// Returns no diagnostics if value is empty.
func ValidateOptionalName(value, field string) []diag.Diagnostic {
	if value == "" {
		return nil
	}
	return ValidateName(value, field)
}

// ValidateAppName checks that an appName is non-empty, within the length
// limit, drawn from the permitted character set, and free of path-traversal
// segments. Unlike ValidateName it permits '/' and '@' so real project names
// (go.putnami.dev/events, @putnami/application) validate, while still rejecting
// the traversal shapes ("..", empty, or "." path segments) that ValidateName's
// stricter pattern blocked wholesale.
func ValidateAppName(value, field string) []diag.Diagnostic {
	if value == "" {
		return []diag.Diagnostic{
			diag.Errorf("required-field", field, "%s is required", field),
		}
	}
	var diags []diag.Diagnostic
	if len(value) > MaxNameLength {
		diags = append(diags, diag.Errorf("max-length", field,
			"%s exceeds maximum length of %d characters", field, MaxNameLength))
	}
	if !appNamePattern.MatchString(value) {
		diags = append(diags, diag.Errorf("invalid-name", field,
			"%s contains invalid characters; must match [a-zA-Z0-9.*_@/-]+", field))
		return diags
	}
	// Reject path-traversal shapes even though the characters are permitted:
	// the config server derives a per-app storage prefix from appName, so an
	// empty, "." or ".." segment must never slip through.
	for seg := range strings.SplitSeq(value, "/") {
		if seg == "" || seg == "." || seg == ".." {
			diags = append(diags, diag.Errorf("invalid-name", field,
				"%s must not contain empty, \".\", or \"..\" path segments", field))
			break
		}
	}
	return diags
}

// ValidateConfigEntry checks a config entry for required fields and naming constraints.
func ValidateConfigEntry(e *ConfigEntry) []diag.Diagnostic {
	if e == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-entry", "", "config entry is nil"),
		}
	}
	diags := make([]diag.Diagnostic, 0, 5)
	diags = append(diags, ValidateAppName(e.AppName, "appName")...)
	diags = append(diags, ValidateName(e.Environment, "environment")...)
	diags = append(diags, ValidateOptionalName(e.Version, "version")...)
	diags = append(diags, ValidateName(e.Path, "path")...)
	if e.Values == nil {
		diags = append(diags, diag.Errorf("required-field", "values", "values is required"))
	} else if len(e.Values) > MaxValuesKeys {
		diags = append(diags, diag.Errorf("max-keys", "values",
			"values exceeds maximum of %d top-level keys", MaxValuesKeys))
	}
	return diags
}

// ValidateSecretEntry checks a secret entry for required fields and naming constraints.
func ValidateSecretEntry(e *SecretEntry) []diag.Diagnostic {
	if e == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-entry", "", "secret entry is nil"),
		}
	}
	diags := make([]diag.Diagnostic, 0, 5)
	diags = append(diags, ValidateAppName(e.AppName, "appName")...)
	diags = append(diags, ValidateName(e.Environment, "environment")...)
	diags = append(diags, ValidateOptionalName(e.Version, "version")...)
	diags = append(diags, ValidateName(e.Path, "path")...)
	diags = append(diags, ValidateSealedEnvelope(&e.Envelope)...)
	return diags
}

// ValidateSealedEnvelope checks that all required fields of a sealed envelope
// are present and that binary payloads are valid base64 strings.
func ValidateSealedEnvelope(e *SealedEnvelope) []diag.Diagnostic {
	if e == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-envelope", "", "sealed envelope is nil"),
		}
	}
	var diags []diag.Diagnostic
	if e.KeyURI == "" {
		diags = append(diags, diag.Errorf("required-field", "envelope.keyUri", "keyUri is required"))
	}
	if e.WrappedDEK == "" {
		diags = append(diags, diag.Errorf("required-field", "envelope.wrappedDek", "wrappedDek is required"))
	} else if !isValidBase64(e.WrappedDEK) {
		diags = append(diags, diag.Errorf("invalid-base64", "envelope.wrappedDek", "wrappedDek must be valid base64"))
	}
	if e.Nonce == "" {
		diags = append(diags, diag.Errorf("required-field", "envelope.nonce", "nonce is required"))
	} else if !isValidBase64(e.Nonce) {
		diags = append(diags, diag.Errorf("invalid-base64", "envelope.nonce", "nonce must be valid base64"))
	}
	if e.Ciphertext == "" {
		diags = append(diags, diag.Errorf("required-field", "envelope.ciphertext", "ciphertext is required"))
	} else if !isValidBase64(e.Ciphertext) {
		diags = append(diags, diag.Errorf("invalid-base64", "envelope.ciphertext", "ciphertext must be valid base64"))
	}
	return diags
}

// ValidateDeleteRequest checks a delete request for required fields and naming constraints.
func ValidateDeleteRequest(r *DeleteRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-request", "", "delete request is nil"),
		}
	}
	diags := make([]diag.Diagnostic, 0, 4)
	diags = append(diags, ValidateAppName(r.AppName, "appName")...)
	diags = append(diags, ValidateName(r.Environment, "environment")...)
	diags = append(diags, ValidateOptionalName(r.Version, "version")...)
	diags = append(diags, ValidateName(r.Path, "path")...)
	return diags
}

// ValidateResolveSecretsRequest checks a secret resolve request for required fields.
func ValidateResolveSecretsRequest(r *ResolveSecretsRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-request", "", "resolve secrets request is nil"),
		}
	}
	diags := make([]diag.Diagnostic, 0, 3)
	diags = append(diags, ValidateAppName(r.AppName, "appName")...)
	diags = append(diags, ValidateName(r.Environment, "environment")...)
	diags = append(diags, ValidateOptionalName(r.Version, "version")...)
	return diags
}

func isValidBase64(value string) bool {
	if _, err := base64.StdEncoding.Strict().DecodeString(value); err == nil {
		return true
	}
	if _, err := base64.RawStdEncoding.Strict().DecodeString(value); err == nil {
		return true
	}
	return false
}
