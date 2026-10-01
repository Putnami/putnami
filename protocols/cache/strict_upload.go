package cache

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// --- upload grant ---

// ParseUploadGrantStrict decodes JSON into an UploadGrant, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseUploadGrantStrict(data []byte) (*UploadGrant, []diag.Diagnostic) {
	return parseStrict[UploadGrant](data, "upload grant")
}

// ValidateUploadGrant checks structural invariants: a supported version, a PUT
// method, and a URL template carrying the digest placeholder so the client can
// form a per-blob URL.
func ValidateUploadGrant(g *UploadGrant) []diag.Diagnostic {
	if g == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "grant is nil")}
	}

	var diags []diag.Diagnostic

	if g.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", g.ProtocolVersion, ProtocolVersion))
	}
	switch {
	case g.URLTemplate == "":
		diags = append(diags, diag.Errorf(CodeRequiredField, "urlTemplate", "a url template is required"))
	case !strings.Contains(g.URLTemplate, DigestPlaceholder):
		diags = append(diags, diag.Errorf(CodeRequiredField, "urlTemplate",
			"url template must contain the %s placeholder", DigestPlaceholder))
	}
	if g.Method != TransferPut {
		diags = append(diags, diag.Errorf(CodeInvalidMethod, "method",
			"expected method %q, got %q", TransferPut, g.Method))
	}

	return diags
}

// NormalizeUploadGrant applies canonical defaults in place.
func NormalizeUploadGrant(g *UploadGrant) *UploadGrant {
	if g == nil {
		return nil
	}
	if g.ProtocolVersion == 0 {
		g.ProtocolVersion = ProtocolVersion
	}
	if g.Method == "" {
		g.Method = TransferPut
	}
	return g
}

// ParseAndValidateUploadGrant combines strict parsing, validation, and
// normalization.
func ParseAndValidateUploadGrant(data []byte) (*UploadGrant, []diag.Diagnostic) {
	g, diags := ParseUploadGrantStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	// Normalize before validating so a defaulted method is accepted.
	NormalizeUploadGrant(g)
	diags = append(diags, ValidateUploadGrant(g)...)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return g, diags
}

// --- upload-batch request ---

// ParseUploadBatchRequestStrict decodes JSON into an UploadBatchRequest,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseUploadBatchRequestStrict(data []byte) (*UploadBatchRequest, []diag.Diagnostic) {
	return parseStrict[UploadBatchRequest](data, "upload-batch request")
}

// ValidateUploadBatchRequest checks structural invariants: a supported version,
// a bounded blob count, and — per blob — a well-formed digest and an inline size
// within MaxInlineBlobBytes (larger blobs belong on the per-blob path). A
// duplicate digest is a warning (idempotent), not an error.
func ValidateUploadBatchRequest(r *UploadBatchRequest) []diag.Diagnostic {
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
	seen := make(map[string]bool, len(r.Blobs))
	for i, b := range r.Blobs {
		bf := fmt.Sprintf("blobs[%d]", i)
		if !ValidDigest(b.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, bf+".digest", "malformed digest %q", b.Digest))
		} else if seen[b.Digest] {
			diags = append(diags, diag.Warningf(CodeDuplicateKey, bf+".digest", "duplicate blob digest %q", b.Digest))
		} else {
			seen[b.Digest] = true
		}
		if len(b.Data) > MaxInlineBlobBytes {
			diags = append(diags, diag.Errorf(CodeBlobTooLarge, bf+".data",
				"inline blob is %d bytes, over the %d limit (use the per-blob path)", len(b.Data), MaxInlineBlobBytes))
		}
	}

	return diags
}

// NormalizeUploadBatchRequest applies canonical defaults and ordering in place.
func NormalizeUploadBatchRequest(r *UploadBatchRequest) *UploadBatchRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(r.Blobs, func(i, j int) bool { return r.Blobs[i].Digest < r.Blobs[j].Digest })
	return r
}

// ParseAndValidateUploadBatchRequest combines strict parsing, validation, and
// normalization.
func ParseAndValidateUploadBatchRequest(data []byte) (*UploadBatchRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseUploadBatchRequestStrict, ValidateUploadBatchRequest, NormalizeUploadBatchRequest)
}

// --- upload-batch response ---

// ParseUploadBatchResponseStrict decodes JSON into an UploadBatchResponse,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseUploadBatchResponseStrict(data []byte) (*UploadBatchResponse, []diag.Diagnostic) {
	return parseStrict[UploadBatchResponse](data, "upload-batch response")
}

// ValidateUploadBatchResponse checks structural invariants: a supported version
// and a well-formed digest on every stored entry.
func ValidateUploadBatchResponse(resp *UploadBatchResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	for i, d := range resp.Stored {
		if !ValidDigest(d) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, fmt.Sprintf("stored[%d]", i),
				"malformed digest %q", d))
		}
	}

	return diags
}

// NormalizeUploadBatchResponse applies canonical defaults and ordering in place.
func NormalizeUploadBatchResponse(resp *UploadBatchResponse) *UploadBatchResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sort.Strings(resp.Stored)
	return resp
}

// ParseAndValidateUploadBatchResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateUploadBatchResponse(data []byte) (*UploadBatchResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseUploadBatchResponseStrict, ValidateUploadBatchResponse, NormalizeUploadBatchResponse)
}

// --- download-batch request ---

// ParseDownloadBatchRequestStrict decodes JSON into a DownloadBatchRequest,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseDownloadBatchRequestStrict(data []byte) (*DownloadBatchRequest, []diag.Diagnostic) {
	return parseStrict[DownloadBatchRequest](data, "download-batch request")
}

// ValidateDownloadBatchRequest checks structural invariants: a supported version,
// a bounded digest count, and a well-formed digest on each entry. A duplicate
// digest is a warning (idempotent), not an error.
func ValidateDownloadBatchRequest(r *DownloadBatchRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}

	var diags []diag.Diagnostic

	if r.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", r.ProtocolVersion, ProtocolVersion))
	}
	if len(r.Digests) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "digests",
			"%d digests exceeds the per-request limit of %d", len(r.Digests), MaxKeysPerRequest))
	}
	seen := make(map[string]bool, len(r.Digests))
	for i, d := range r.Digests {
		df := fmt.Sprintf("digests[%d]", i)
		if !ValidDigest(d) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, df, "malformed digest %q", d))
		} else if seen[d] {
			diags = append(diags, diag.Warningf(CodeDuplicateKey, df, "duplicate digest %q", d))
		} else {
			seen[d] = true
		}
	}

	return diags
}

// NormalizeDownloadBatchRequest applies canonical defaults and ordering in place.
func NormalizeDownloadBatchRequest(r *DownloadBatchRequest) *DownloadBatchRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	sort.Strings(r.Digests)
	return r
}

// ParseAndValidateDownloadBatchRequest combines strict parsing, validation, and
// normalization.
func ParseAndValidateDownloadBatchRequest(data []byte) (*DownloadBatchRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseDownloadBatchRequestStrict, ValidateDownloadBatchRequest, NormalizeDownloadBatchRequest)
}

// --- download-batch response ---

// ParseDownloadBatchResponseStrict decodes JSON into a DownloadBatchResponse,
// rejecting unknown fields and returning structured diagnostics on a decode
// error.
func ParseDownloadBatchResponseStrict(data []byte) (*DownloadBatchResponse, []diag.Diagnostic) {
	return parseStrict[DownloadBatchResponse](data, "download-batch response")
}

// ValidateDownloadBatchResponse checks structural invariants: a supported version
// and — per returned blob — a well-formed digest and a payload within
// MaxInlineBlobBytes (larger blobs belong on the presigned GET path). A duplicate
// digest is a warning.
func ValidateDownloadBatchResponse(resp *DownloadBatchResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}
	seen := make(map[string]bool, len(resp.Blobs))
	for i, b := range resp.Blobs {
		bf := fmt.Sprintf("blobs[%d]", i)
		if !ValidDigest(b.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, bf+".digest", "malformed digest %q", b.Digest))
		} else if seen[b.Digest] {
			diags = append(diags, diag.Warningf(CodeDuplicateKey, bf+".digest", "duplicate blob digest %q", b.Digest))
		} else {
			seen[b.Digest] = true
		}
		if len(b.Data) > MaxInlineBlobBytes {
			diags = append(diags, diag.Errorf(CodeBlobTooLarge, bf+".data",
				"inline blob is %d bytes, over the %d limit (use the presigned GET path)", len(b.Data), MaxInlineBlobBytes))
		}
	}

	return diags
}

// NormalizeDownloadBatchResponse applies canonical defaults and ordering in place.
func NormalizeDownloadBatchResponse(resp *DownloadBatchResponse) *DownloadBatchResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(resp.Blobs, func(i, j int) bool { return resp.Blobs[i].Digest < resp.Blobs[j].Digest })
	return resp
}

// ParseAndValidateDownloadBatchResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateDownloadBatchResponse(data []byte) (*DownloadBatchResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseDownloadBatchResponseStrict, ValidateDownloadBatchResponse, NormalizeDownloadBatchResponse)
}
