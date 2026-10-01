package infra

import (
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
)

// LoadPerProjectManifest reads a per-project manifest from disk, then
// strict-parses and validates it. The returned diagnostics are prefixed
// with path for caller context.
func LoadPerProjectManifest(path string) (*PerProjectManifest, []diag.Diagnostic) {
	data, err := readFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"read per-project infra manifest %s: %v", path, err)}
	}
	m, diags := ParseAndValidatePerProjectManifest(data)
	prefixWithPath(diags, path)
	return m, diags
}

// LoadAggregatedManifest reads an aggregated manifest from disk, then
// strict-parses and validates it.
func LoadAggregatedManifest(path string) (*AggregatedManifest, []diag.Diagnostic) {
	data, err := readFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"read aggregated infra manifest %s: %v", path, err)}
	}
	m, diags := ParseAndValidateAggregatedManifest(data)
	prefixWithPath(diags, path)
	return m, diags
}

// prefixWithPath rewrites diagnostic messages to lead with the file
// path so consumers can trace failures back to disk locations.
func prefixWithPath(diags []diag.Diagnostic, path string) {
	for i := range diags {
		diags[i].Message = fmt.Sprintf("%s: %s", path, diags[i].Message)
	}
}
