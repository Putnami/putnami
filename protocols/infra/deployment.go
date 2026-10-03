package infra

import (
	"bytes"
	"encoding/json"

	diag "go.putnami.dev/protocol/diagnostic"
)

// A deployment declaration is a workload's aggregated manifest in the one byte
// form a content-addressed store keeps: the bytes are the identity, so two
// producers that resolve the same requirements and runtime write the same
// bytes and derive the same digest.
//
// The canonical form is what encoding/json writes for the typed manifest:
// members in struct field order, no insignificant space, '<', '>' and '&'
// escaped, no trailing newline, and no "$schema" member. "$schema" is an
// editor hint, not content; carrying it would tie every digest to a URL.

// MarshalDeployment returns the canonical deployment declaration of m. It
// does not modify m and drops its Schema. It returns no bytes, and the
// findings, when m does not validate as an aggregated manifest, so every
// byte string it returns is one ParseDeployment accepts.
func MarshalDeployment(m *AggregatedManifest) ([]byte, []diag.Diagnostic) {
	if m == nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	declaration := *m
	declaration.Schema = ""
	diagnostics := ValidateAggregatedManifest(&declaration)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	data, err := json.Marshal(&declaration)
	if err != nil {
		return nil, append(diagnostics, diag.Errorf(ErrorCodeParseError, "", "encode the deployment declaration: %v", err))
	}
	if _, parseDiagnostics := ParseDeployment(data); diag.HasErrors(parseDiagnostics) {
		return nil, append(diagnostics, parseDiagnostics...)
	}
	return data, diagnostics
}

// ParseDeployment strict-parses and validates a deployment declaration. It
// returns a manifest only when data parses as an aggregated manifest with no
// unknown member, validates without an error, carries no "$schema" member,
// and is byte for byte the canonical form MarshalDeployment writes for it. Any
// other spelling of the same manifest, such as indented, reordered or
// newline-terminated bytes, is refused with ErrorCodeNonCanonical.
func ParseDeployment(data []byte) (*AggregatedManifest, []diag.Diagnostic) {
	m, diagnostics := ParseAndValidateAggregatedManifest(data)
	if m == nil || diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	if m.Schema != "" {
		return nil, append(diagnostics, diag.Errorf(ErrorCodeNonCanonical, "$schema",
			"a deployment declaration carries no $schema member"))
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return nil, append(diagnostics, diag.Errorf(ErrorCodeParseError, "", "encode the deployment declaration: %v", err))
	}
	if !bytes.Equal(canonical, data) {
		return nil, append(diagnostics, diag.Errorf(ErrorCodeNonCanonical, "",
			"a deployment declaration must be the compact encoding/json form of its manifest: struct member order, no insignificant space, no trailing newline"))
	}
	return m, diagnostics
}
