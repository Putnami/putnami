package cache

import (
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// --- run marker lookup request ---

// ParseRunMarkerRequestStrict decodes JSON into a RunMarkerRequest, rejecting
// unknown fields and returning structured diagnostics on a decode error.
func ParseRunMarkerRequestStrict(data []byte) (*RunMarkerRequest, []diag.Diagnostic) {
	return parseStrict[RunMarkerRequest](data, "run marker request")
}

// ValidateRunMarkerRequest checks structural invariants on a parsed run marker
// lookup request.
func ValidateRunMarkerRequest(r *RunMarkerRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}
	return validateRunMarkerKey("run marker request", r.ProtocolVersion, r.Workspace, r.Branch, r.Commands, r.Selection)
}

// NormalizeRunMarkerRequest applies canonical defaults and trims command
// values in place.
func NormalizeRunMarkerRequest(r *RunMarkerRequest) *RunMarkerRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	r.Workspace = strings.TrimSpace(r.Workspace)
	r.Branch = strings.TrimSpace(r.Branch)
	r.ParamsHash = strings.TrimSpace(r.ParamsHash)
	r.Commands = NormalizeRunMarkerCommands(r.Commands)
	if r.Selection == "" {
		r.Selection = RunMarkerSelectionAll
	}
	return r
}

// ParseAndValidateRunMarkerRequest combines strict parsing, validation, and
// normalization. The request is nil only when parsing or a hard validation error
// fails.
func ParseAndValidateRunMarkerRequest(data []byte) (*RunMarkerRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseRunMarkerRequestStrict, ValidateRunMarkerRequest, NormalizeRunMarkerRequest)
}

// --- run marker lookup response ---

// ParseRunMarkerResponseStrict decodes JSON into a RunMarkerResponse, rejecting
// unknown fields and returning structured diagnostics on a decode error.
func ParseRunMarkerResponseStrict(data []byte) (*RunMarkerResponse, []diag.Diagnostic) {
	return parseStrict[RunMarkerResponse](data, "run marker response")
}

// ValidateRunMarkerResponse checks structural invariants on a parsed run marker
// lookup response.
func ValidateRunMarkerResponse(resp *RunMarkerResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}
	diags := make([]diag.Diagnostic, 0, 2)
	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	diags = append(diags, validateRunMarker(resp.Marker)...)
	return diags
}

// NormalizeRunMarkerResponse applies canonical defaults in place.
func NormalizeRunMarkerResponse(resp *RunMarkerResponse) *RunMarkerResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	normalizeRunMarker(resp.Marker)
	return resp
}

// ParseAndValidateRunMarkerResponse combines strict parsing, validation, and
// normalization. The response is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateRunMarkerResponse(data []byte) (*RunMarkerResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseRunMarkerResponseStrict, ValidateRunMarkerResponse, NormalizeRunMarkerResponse)
}

// --- run marker publish request ---

// ParsePublishRunMarkerRequestStrict decodes JSON into a
// PublishRunMarkerRequest, rejecting unknown fields and returning structured
// diagnostics on a decode error.
func ParsePublishRunMarkerRequestStrict(data []byte) (*PublishRunMarkerRequest, []diag.Diagnostic) {
	return parseStrict[PublishRunMarkerRequest](data, "publish run marker request")
}

// ValidatePublishRunMarkerRequest checks structural invariants on a parsed run
// marker publish request.
func ValidatePublishRunMarkerRequest(r *PublishRunMarkerRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}
	diags := validateRunMarkerKey("publish run marker request", r.ProtocolVersion, r.Workspace, r.Branch, r.Commands, r.Selection)
	if strings.TrimSpace(r.SHA) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "sha", "run marker sha is required"))
	}
	return diags
}

// NormalizePublishRunMarkerRequest applies canonical defaults and trims values
// in place.
func NormalizePublishRunMarkerRequest(r *PublishRunMarkerRequest) *PublishRunMarkerRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	r.Workspace = strings.TrimSpace(r.Workspace)
	r.Branch = strings.TrimSpace(r.Branch)
	r.ParamsHash = strings.TrimSpace(r.ParamsHash)
	r.SHA = strings.TrimSpace(r.SHA)
	r.ObservedSHA = strings.TrimSpace(r.ObservedSHA)
	r.Commands = NormalizeRunMarkerCommands(r.Commands)
	if r.Selection == "" {
		r.Selection = RunMarkerSelectionAll
	}
	return r
}

// ParseAndValidatePublishRunMarkerRequest combines strict parsing, validation,
// and normalization. The request is nil only when parsing or a hard validation
// error fails.
func ParseAndValidatePublishRunMarkerRequest(data []byte) (*PublishRunMarkerRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParsePublishRunMarkerRequestStrict, ValidatePublishRunMarkerRequest, NormalizePublishRunMarkerRequest)
}

// --- run marker publish response ---

// ParsePublishRunMarkerResponseStrict decodes JSON into a
// PublishRunMarkerResponse, rejecting unknown fields and returning structured
// diagnostics on a decode error.
func ParsePublishRunMarkerResponseStrict(data []byte) (*PublishRunMarkerResponse, []diag.Diagnostic) {
	return parseStrict[PublishRunMarkerResponse](data, "publish run marker response")
}

// ValidatePublishRunMarkerResponse checks structural invariants on a parsed run
// marker publish response.
func ValidatePublishRunMarkerResponse(resp *PublishRunMarkerResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}
	diags := make([]diag.Diagnostic, 0, 2)
	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	diags = append(diags, validateRunMarker(resp.Marker)...)
	return diags
}

// NormalizePublishRunMarkerResponse applies canonical defaults in place.
func NormalizePublishRunMarkerResponse(resp *PublishRunMarkerResponse) *PublishRunMarkerResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	normalizeRunMarker(resp.Marker)
	return resp
}

// ParseAndValidatePublishRunMarkerResponse combines strict parsing, validation,
// and normalization. The response is nil only when parsing or a hard validation
// error fails.
func ParseAndValidatePublishRunMarkerResponse(data []byte) (*PublishRunMarkerResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParsePublishRunMarkerResponseStrict, ValidatePublishRunMarkerResponse, NormalizePublishRunMarkerResponse)
}

// NormalizeRunMarkerCommands trims command names and drops empty entries while
// preserving command order. The order is part of the marker key because
// multi-command invocations run sequentially.
func NormalizeRunMarkerCommands(commands []string) []string {
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		if cmd != "" {
			out = append(out, cmd)
		}
	}
	return out
}

func validateRunMarkerKey(what string, version int, workspace, branch string, commands []string, selection RunMarkerSelection) []diag.Diagnostic {
	diags := make([]diag.Diagnostic, 0, 5)
	if version != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", version, ProtocolVersion))
	}
	if strings.TrimSpace(workspace) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "workspace", "%s workspace is required", what))
	}
	if strings.TrimSpace(branch) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "branch", "%s branch is required", what))
	}
	if len(NormalizeRunMarkerCommands(commands)) == 0 {
		diags = append(diags, diag.Errorf(CodeRequiredField, "commands", "%s command list is required", what))
	}
	if selection == "" || !selection.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidSelection, "selection", "unknown run marker selection %q", selection))
	}
	return diags
}

func validateRunMarker(marker *RunMarker) []diag.Diagnostic {
	if marker == nil {
		return nil
	}
	if strings.TrimSpace(marker.SHA) == "" {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "marker.sha", "run marker sha is required")}
	}
	return nil
}

func normalizeRunMarker(marker *RunMarker) {
	if marker == nil {
		return
	}
	marker.SHA = strings.TrimSpace(marker.SHA)
	marker.UpdatedAt = strings.TrimSpace(marker.UpdatedAt)
}
