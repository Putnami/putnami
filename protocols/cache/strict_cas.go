package cache

import (
	"fmt"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// --- find-missing request ---

// ParseFindMissingRequestStrict decodes JSON into a FindMissingRequest,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseFindMissingRequestStrict(data []byte) (*FindMissingRequest, []diag.Diagnostic) {
	return parseStrict[FindMissingRequest](data, "find-missing request")
}

// ValidateFindMissingRequest checks structural invariants: a supported version,
// a bounded blob count, and a well-formed digest on every blob.
func ValidateFindMissingRequest(r *FindMissingRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}

	var diags []diag.Diagnostic

	if r.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", r.ProtocolVersion, ProtocolVersion))
	}
	if len(r.Blobs) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "blobs",
			"%d blobs exceeds the per-request limit of %d", len(r.Blobs), MaxKeysPerRequest))
	}
	for i, b := range r.Blobs {
		if !ValidDigest(b.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, fmt.Sprintf("blobs[%d].digest", i),
				"malformed digest %q", b.Digest))
		}
	}

	return diags
}

// NormalizeFindMissingRequest applies canonical defaults and ordering in place.
func NormalizeFindMissingRequest(r *FindMissingRequest) *FindMissingRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(r.Blobs, func(i, j int) bool { return r.Blobs[i].Digest < r.Blobs[j].Digest })
	return r
}

// ParseAndValidateFindMissingRequest combines strict parsing, validation, and
// normalization.
func ParseAndValidateFindMissingRequest(data []byte) (*FindMissingRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseFindMissingRequestStrict, ValidateFindMissingRequest, NormalizeFindMissingRequest)
}

// --- find-missing response ---

// ParseFindMissingResponseStrict decodes JSON into a FindMissingResponse,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseFindMissingResponseStrict(data []byte) (*FindMissingResponse, []diag.Diagnostic) {
	return parseStrict[FindMissingResponse](data, "find-missing response")
}

// ValidateFindMissingResponse checks structural invariants: a supported version
// and well-formed PUT transfers (an empty set is valid — full dedup).
func ValidateFindMissingResponse(resp *FindMissingResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	diags = append(diags, validateTransfers("uploads", TransferPut, resp.Uploads)...)

	return diags
}

// NormalizeFindMissingResponse applies canonical defaults and ordering in place.
func NormalizeFindMissingResponse(resp *FindMissingResponse) *FindMissingResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sortTransfers(resp.Uploads)
	return resp
}

// ParseAndValidateFindMissingResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateFindMissingResponse(data []byte) (*FindMissingResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseFindMissingResponseStrict, ValidateFindMissingResponse, NormalizeFindMissingResponse)
}

// --- commit-batch request ---

// ParseCommitBatchRequestStrict decodes JSON into a CommitBatchRequest,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseCommitBatchRequestStrict(data []byte) (*CommitBatchRequest, []diag.Diagnostic) {
	return parseStrict[CommitBatchRequest](data, "commit-batch request")
}

// ValidateCommitBatchRequest checks structural invariants: a supported version,
// a bounded entry count, and — per entry — a well-formed key, a result carrying
// a status, and a present, well-formed manifest. A duplicate key is a warning
// (commit is idempotent), not an error.
func ValidateCommitBatchRequest(r *CommitBatchRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}

	var diags []diag.Diagnostic

	if r.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", r.ProtocolVersion, ProtocolVersion))
	}
	if len(r.Entries) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "entries",
			"%d entries exceeds the per-request limit of %d", len(r.Entries), MaxKeysPerRequest))
	}
	seen := make(map[string]bool, len(r.Entries))
	for i, e := range r.Entries {
		ef := fmt.Sprintf("entries[%d]", i)
		if !ValidKey(e.Key) {
			diags = append(diags, diag.Errorf(CodeInvalidKey, ef+".key", "malformed cache key %q", e.Key))
		} else if seen[e.Key] {
			diags = append(diags, diag.Warningf(CodeDuplicateKey, ef+".key", "duplicate cache key %q", e.Key))
		} else {
			seen[e.Key] = true
		}
		diags = append(diags, validateActionResult(ef+".result", e.Result)...)
		if e.Manifest == nil {
			diags = append(diags, diag.Errorf(CodeRequiredField, ef+".manifest", "a manifest is required"))
		} else {
			diags = append(diags, validateManifestFiles(ef+".manifest", e.Manifest)...)
		}
	}

	return diags
}

// NormalizeCommitBatchRequest applies canonical defaults and ordering in place:
// entries sorted by key, each manifest's files sorted by path.
func NormalizeCommitBatchRequest(r *CommitBatchRequest) *CommitBatchRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(r.Entries, func(i, j int) bool { return r.Entries[i].Key < r.Entries[j].Key })
	for i := range r.Entries {
		NormalizeManifestFiles(r.Entries[i].Manifest)
	}
	return r
}

// ParseAndValidateCommitBatchRequest combines strict parsing, validation, and
// normalization.
func ParseAndValidateCommitBatchRequest(data []byte) (*CommitBatchRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseCommitBatchRequestStrict, ValidateCommitBatchRequest, NormalizeCommitBatchRequest)
}

// --- commit-batch response ---

// ParseCommitBatchResponseStrict decodes JSON into a CommitBatchResponse,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseCommitBatchResponseStrict(data []byte) (*CommitBatchResponse, []diag.Diagnostic) {
	return parseStrict[CommitBatchResponse](data, "commit-batch response")
}

// ValidateCommitBatchResponse checks structural invariants: a supported version
// and a well-formed key on every result.
func ValidateCommitBatchResponse(resp *CommitBatchResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	for i, res := range resp.Results {
		if !ValidKey(res.Key) {
			diags = append(diags, diag.Errorf(CodeInvalidKey, fmt.Sprintf("results[%d].key", i),
				"malformed cache key %q", res.Key))
		}
	}

	return diags
}

// NormalizeCommitBatchResponse applies canonical defaults and ordering in place.
func NormalizeCommitBatchResponse(resp *CommitBatchResponse) *CommitBatchResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(resp.Results, func(i, j int) bool { return resp.Results[i].Key < resp.Results[j].Key })
	return resp
}

// ParseAndValidateCommitBatchResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateCommitBatchResponse(data []byte) (*CommitBatchResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseCommitBatchResponseStrict, ValidateCommitBatchResponse, NormalizeCommitBatchResponse)
}
