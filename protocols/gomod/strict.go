package gomod

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict validation failures. Conformance suites and clients key
// off these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeInvalidBlobUpload = "gomod.invalid_blob_upload"
	ErrorCodeInvalidPublish    = "gomod.invalid_publish"
	ErrorCodeInvalidDigest     = "gomod.invalid_digest"
	ErrorCodeInvalidGoMod      = "gomod.invalid_go_mod"
	ErrorCodeInvalidRelease    = "gomod.invalid_release"
)

// ValidErrorCodes enumerates the canonical gomod error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidBlobUpload: true,
	ErrorCodeInvalidPublish:    true,
	ErrorCodeInvalidDigest:     true,
	ErrorCodeInvalidGoMod:      true,
	ErrorCodeInvalidRelease:    true,
}

// A digest is "sha256:" + 64 lowercase hex chars.
const (
	digestAlgorithm = "sha256"
	digestSep       = ":"
	digestHexLen    = 64
)

// validDigest reports whether s is a well-formed sha256 content address.
func validDigest(s string) bool {
	rest, ok := strings.CutPrefix(s, digestAlgorithm+digestSep)
	if !ok || len(rest) != digestHexLen {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ParseBlobUploadResponse strict-parses the POST {BlobUploadPath} response body
// (unknown fields rejected).
func ParseBlobUploadResponse(data []byte) (*BlobUploadResponse, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var resp BlobUploadResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBlobUpload, "",
			"invalid blob upload response JSON: %v", err)}
	}
	return &resp, nil
}

// ValidateBlobUploadResponse enforces that the registry returned a usable digest.
func ValidateBlobUploadResponse(resp BlobUploadResponse) []diag.Diagnostic {
	if !validDigest(resp.Digest) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest %q must be sha256: followed by 64 lowercase hex chars", resp.Digest)}
	}
	return nil
}

// ParseAndValidateBlobUploadResponse strict-parses then validates.
func ParseAndValidateBlobUploadResponse(data []byte) (*BlobUploadResponse, []diag.Diagnostic) {
	resp, diags := ParseBlobUploadResponse(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return resp, append(diags, ValidateBlobUploadResponse(*resp)...)
}

// ParsePublishVersionRequest strict-parses the PUT {VersionPath} request body.
func ParsePublishVersionRequest(data []byte) (*PublishVersionRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req PublishVersionRequest
	if err := dec.Decode(&req); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPublish, "",
			"invalid publish version request JSON: %v", err)}
	}
	return &req, nil
}

// ValidatePublishVersionRequest enforces the gomod-write/v1 publish body rules.
// DistTag is optional and unconstrained here (the server owns channel naming).
func ValidatePublishVersionRequest(req PublishVersionRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(req.GoMod) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidGoMod, "go_mod",
			"go_mod content is required"))
	}
	if !validDigest(req.ZipDigest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "zip_digest",
			"zip_digest %q must be sha256: followed by 64 lowercase hex chars", req.ZipDigest))
	}
	return diags
}

// ParseAndValidatePublishVersionRequest strict-parses then validates.
func ParseAndValidatePublishVersionRequest(data []byte) (*PublishVersionRequest, []diag.Diagnostic) {
	req, diags := ParsePublishVersionRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return req, append(diags, ValidatePublishVersionRequest(*req)...)
}

// ParseReleaseVersionResponse strict-parses the POST {ReleasePath} response.
// Unknown fields and trailing JSON values are rejected: release is an
// authorization boundary, so a client must not accept a response shape it does
// not understand.
func ParseReleaseVersionResponse(data []byte) (*ReleaseVersionResponse, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var resp ReleaseVersionResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRelease, "",
			"invalid release version response JSON: %v", err)}
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRelease, "",
			"invalid release version response JSON: %v", err)}
	}
	return &resp, nil
}

// ValidateReleaseVersionResponse validates the self-description returned by a
// successful release. Matching it to the request remains the client's job.
func ValidateReleaseVersionResponse(resp ReleaseVersionResponse) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if resp.Module == "" || strings.TrimSpace(resp.Module) != resp.Module {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRelease, "module",
			"module must be non-empty and contain no surrounding whitespace"))
	}
	if resp.Version == "" || strings.TrimSpace(resp.Version) != resp.Version {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRelease, "version",
			"version must be non-empty and contain no surrounding whitespace"))
	}
	if resp.Visibility != ReleaseVisibilityPublic {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRelease, "visibility",
			"visibility %q must be %q", resp.Visibility, ReleaseVisibilityPublic))
	}
	return diags
}

// ValidateReleaseVersionResponseFor validates that a successful release
// acknowledgement names the exact immutable coordinate requested by the
// client. Keeping these comparisons in the protocol prevents consumers from
// accepting subtly different exchange semantics.
func ValidateReleaseVersionResponseFor(resp ReleaseVersionResponse, module, version string) []diag.Diagnostic {
	diags := ValidateReleaseVersionResponse(resp)
	if resp.Module != module {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRelease, "module",
			"module %q does not match requested module %q", resp.Module, module))
	}
	if resp.Version != version {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRelease, "version",
			"version %q does not match requested version %q", resp.Version, version))
	}
	return diags
}

// ParseAndValidateReleaseVersionResponse strict-parses then validates.
func ParseAndValidateReleaseVersionResponse(data []byte) (*ReleaseVersionResponse, []diag.Diagnostic) {
	resp, diags := ParseReleaseVersionResponse(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return resp, append(diags, ValidateReleaseVersionResponse(*resp)...)
}

// ParseAndValidateReleaseVersionResponseFor strict-parses and validates a
// release response against the requested immutable coordinate.
func ParseAndValidateReleaseVersionResponseFor(data []byte, module, version string) (*ReleaseVersionResponse, []diag.Diagnostic) {
	resp, diags := ParseReleaseVersionResponse(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return resp, append(diags, ValidateReleaseVersionResponseFor(*resp, module, version)...)
}
