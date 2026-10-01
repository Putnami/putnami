package cache

import (
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// --- capabilities response ---

// ParseCapabilitiesResponseStrict decodes JSON into a CapabilitiesResponse,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseCapabilitiesResponseStrict(data []byte) (*CapabilitiesResponse, []diag.Diagnostic) {
	return parseStrict[CapabilitiesResponse](data, "capabilities response")
}

// ValidateCapabilitiesResponse checks the only hard invariant: a supported
// protocol version. Unknown capability strings are ignored, not errors, so the
// set can grow without breaking older clients.
func ValidateCapabilitiesResponse(resp *CapabilitiesResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}
	var diags []diag.Diagnostic
	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	return diags
}

// NormalizeCapabilitiesResponse applies canonical defaults and ordering in place.
func NormalizeCapabilitiesResponse(resp *CapabilitiesResponse) *CapabilitiesResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sort.Strings(resp.Capabilities)
	return resp
}

// ParseAndValidateCapabilitiesResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateCapabilitiesResponse(data []byte) (*CapabilitiesResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseCapabilitiesResponseStrict, ValidateCapabilitiesResponse, NormalizeCapabilitiesResponse)
}
