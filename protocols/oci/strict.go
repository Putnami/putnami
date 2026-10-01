package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict validation failures. Conformance suites and clients key
// off these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeInvalidCapabilities = "oci.invalid_capabilities"
	ErrorCodeInvalidTagDigest    = "oci.invalid_tag_digest"
	ErrorCodeInvalidDigest       = "oci.invalid_digest"
	ErrorCodeInvalidRepository   = "oci.invalid_repository"
	ErrorCodeInvalidTag          = "oci.invalid_tag"
)

// ValidErrorCodes enumerates the canonical oci error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidCapabilities: true,
	ErrorCodeInvalidTagDigest:    true,
	ErrorCodeInvalidDigest:       true,
	ErrorCodeInvalidRepository:   true,
	ErrorCodeInvalidTag:          true,
}

// A digest is "sha256:" + 64 lowercase hex chars.
const (
	digestAlgorithm = "sha256"
	digestSep       = ":"
	digestHexLen    = 64
)

// tagPattern is the OCI distribution tag grammar:
// [A-Za-z0-9_][A-Za-z0-9._-]{0,127}.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

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

// ParseCapabilitiesResponse strict-parses GET /v2/_putnami/capabilities (unknown
// fields rejected).
func ParseCapabilitiesResponse(data []byte) (*CapabilitiesResponse, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var resp CapabilitiesResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidCapabilities, "",
			"invalid capabilities response JSON: %v", err)}
	}
	return &resp, nil
}

// ParseAndValidateCapabilitiesResponse strict-parses the capabilities response.
// There are no semantic rules beyond strict decoding: an unrecognized capability
// string is simply ignored by SupportsTagDigest, so older clients tolerate newer
// registries. It exists so conformance suites can drive every message through a
// uniform ParseAndValidate entry point.
func ParseAndValidateCapabilitiesResponse(data []byte) (*CapabilitiesResponse, []diag.Diagnostic) {
	return ParseCapabilitiesResponse(data)
}

// ParseTagDigestRequest strict-parses POST /v2/_putnami/tag-digest.
func ParseTagDigestRequest(data []byte) (*TagDigestRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req TagDigestRequest
	if err := dec.Decode(&req); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTagDigest, "",
			"invalid tag-digest request JSON: %v", err)}
	}
	return &req, nil
}

// ValidateTagDigestRequest enforces the tag-digest/v1 body rules.
func ValidateTagDigestRequest(req TagDigestRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if req.Repository == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRepository, "repository",
			"repository is required"))
	}
	if !validDigest(req.Digest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest %q must be sha256: followed by 64 lowercase hex chars", req.Digest))
	}
	if len(req.Tags) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTagDigest, "tags",
			"at least one tag is required"))
	}
	for i, tag := range req.Tags {
		if !tagPattern.MatchString(tag) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTag, fmt.Sprintf("tags[%d]", i),
				"tag %q is not a valid OCI tag", tag))
		}
	}
	return diags
}

// ParseAndValidateTagDigestRequest strict-parses then validates.
func ParseAndValidateTagDigestRequest(data []byte) (*TagDigestRequest, []diag.Diagnostic) {
	req, diags := ParseTagDigestRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return req, append(diags, ValidateTagDigestRequest(*req)...)
}
