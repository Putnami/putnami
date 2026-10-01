package cache

import diag "go.putnami.dev/protocol/diagnostic"

// --- store request ---

// ParseStoreRequestStrict decodes JSON into a StoreRequest, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseStoreRequestStrict(data []byte) (*StoreRequest, []diag.Diagnostic) {
	return parseStrict[StoreRequest](data, "store request")
}

// ValidateStoreRequest checks structural invariants on a parsed store request:
// a supported version, a well-formed key, and a present, well-formed manifest.
func ValidateStoreRequest(r *StoreRequest) []diag.Diagnostic {
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
	if r.Manifest == nil {
		diags = append(diags, diag.Errorf(CodeRequiredField, "manifest", "a manifest is required"))
	} else {
		diags = append(diags, validateManifestFiles("manifest", r.Manifest)...)
	}

	return diags
}

// NormalizeStoreRequest applies canonical defaults and ordering in place.
func NormalizeStoreRequest(r *StoreRequest) *StoreRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	NormalizeManifestFiles(r.Manifest)
	return r
}

// ParseAndValidateStoreRequest combines strict parsing, validation, and
// normalization. The request is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateStoreRequest(data []byte) (*StoreRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseStoreRequestStrict, ValidateStoreRequest, NormalizeStoreRequest)
}

// --- store response ---

// ParseStoreResponseStrict decodes JSON into a StoreResponse, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseStoreResponseStrict(data []byte) (*StoreResponse, []diag.Diagnostic) {
	return parseStrict[StoreResponse](data, "store response")
}

// ValidateStoreResponse checks structural invariants on a parsed store
// response: a supported version, a well-formed key, and well-formed PUT
// transfers in the upload set (an empty set is valid — full dedup).
func ValidateStoreResponse(resp *StoreResponse) []diag.Diagnostic {
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
	diags = append(diags, validateTransfers("uploads", TransferPut, resp.Uploads)...)

	return diags
}

// NormalizeStoreResponse applies canonical defaults and ordering in place.
func NormalizeStoreResponse(resp *StoreResponse) *StoreResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sortTransfers(resp.Uploads)
	return resp
}

// ParseAndValidateStoreResponse combines strict parsing, validation, and
// normalization. The response is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateStoreResponse(data []byte) (*StoreResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseStoreResponseStrict, ValidateStoreResponse, NormalizeStoreResponse)
}
