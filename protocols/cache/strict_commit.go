package cache

import diag "go.putnami.dev/protocol/diagnostic"

// --- commit request ---

// ParseCommitRequestStrict decodes JSON into a CommitRequest, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseCommitRequestStrict(data []byte) (*CommitRequest, []diag.Diagnostic) {
	return parseStrict[CommitRequest](data, "commit request")
}

// ValidateCommitRequest checks structural invariants on a parsed commit
// request: a supported version, a well-formed key, a result carrying a status,
// and a present, well-formed manifest.
func ValidateCommitRequest(r *CommitRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}

	var diags []diag.Diagnostic

	if r.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", r.ProtocolVersion, ProtocolVersion))
	}
	if !ValidKey(r.Key) {
		diags = append(diags, diag.Errorf(CodeInvalidKey, "key", "malformed cache key %q", r.Key))
	}
	diags = append(diags, validateActionResult("result", r.Result)...)
	if r.Manifest == nil {
		diags = append(diags, diag.Errorf(CodeRequiredField, "manifest", "a manifest is required"))
	} else {
		diags = append(diags, validateManifestFiles("manifest", r.Manifest)...)
	}

	return diags
}

// NormalizeCommitRequest applies canonical defaults and ordering in place.
func NormalizeCommitRequest(r *CommitRequest) *CommitRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	NormalizeManifestFiles(r.Manifest)
	return r
}

// ParseAndValidateCommitRequest combines strict parsing, validation, and
// normalization. The request is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateCommitRequest(data []byte) (*CommitRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseCommitRequestStrict, ValidateCommitRequest, NormalizeCommitRequest)
}

// --- commit response ---

// ParseCommitResponseStrict decodes JSON into a CommitResponse, rejecting
// unknown fields and returning structured diagnostics on a decode error.
func ParseCommitResponseStrict(data []byte) (*CommitResponse, []diag.Diagnostic) {
	return parseStrict[CommitResponse](data, "commit response")
}

// ValidateCommitResponse checks structural invariants on a parsed commit
// response: a supported version and a well-formed key.
func ValidateCommitResponse(resp *CommitResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	if !ValidKey(resp.Key) {
		diags = append(diags, diag.Errorf(CodeInvalidKey, "key", "malformed cache key %q", resp.Key))
	}

	return diags
}

// NormalizeCommitResponse applies canonical defaults in place.
func NormalizeCommitResponse(resp *CommitResponse) *CommitResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	return resp
}

// ParseAndValidateCommitResponse combines strict parsing, validation, and
// normalization. The response is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateCommitResponse(data []byte) (*CommitResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseCommitResponseStrict, ValidateCommitResponse, NormalizeCommitResponse)
}
