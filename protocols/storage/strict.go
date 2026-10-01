package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and the cloud
// implementation key off these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeParseError             = "storage.parse_error"
	ErrorCodeUnknownField           = "storage.unknown_field"
	ErrorCodeInvalidProtocolVersion = "storage.invalid_protocol_version"
	ErrorCodeInvalidName            = "storage.invalid_name"
	ErrorCodeInvalidAccess          = "storage.invalid_access"
	ErrorCodeInvalidScope           = "storage.invalid_scope"
	ErrorCodeInvalidIdentity        = "storage.invalid_identity"
	ErrorCodeMissingBackend         = "storage.missing_backend"
	ErrorCodeMissingBucket          = "storage.missing_bucket"
)

// ValidErrorCodes enumerates the canonical storage-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidName:            true,
	ErrorCodeInvalidAccess:          true,
	ErrorCodeInvalidScope:           true,
	ErrorCodeInvalidIdentity:        true,
	ErrorCodeMissingBackend:         true,
	ErrorCodeMissingBucket:          true,
}

// resourceNamePattern matches a canonical resource identifier. We reuse the
// same shape as the other protocol/* modules (lowercase letters, digits, '-',
// '_', '.', '/'; 1–64 chars) so resource names look uniform across protocols.
var resourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]{0,63}$`)

// ParseManifest decodes a storage Manifest from JSON in strict mode (unknown
// fields rejected). It returns a non-nil manifest only when parsing produced no
// errors.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &m, nil
}

// ParseBinding decodes a storage Binding from JSON in strict mode. It returns a
// non-nil binding only when parsing produced no errors.
func ParseBinding(data []byte) (*Binding, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Binding
	if err := dec.Decode(&b); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &b, nil
}

// decodeError maps a json decode failure to the matching diagnostic code,
// distinguishing an unknown-field rejection from a generic parse error.
func decodeError(err error) diag.Diagnostic {
	msg := err.Error()
	if field, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(field, `"`), "%s", msg)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", msg)
}

// ValidateManifest checks structural invariants on a parsed manifest: a
// supported protocol version and, per resource, a canonical name plus
// in-enum access and scope (each only when non-empty — both are optional).
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	diags := validateProtocolVersion(m.ProtocolVersion)
	for i, r := range m.Resources {
		diags = append(diags, validateResource(fmt.Sprintf("resources[%d]", i), r)...)
	}
	return diags
}

// validateResource validates one resource's name and closed-enum attributes.
// The boolean capabilities (signedUrls, public) carry no invalid state, so only
// the name and the non-empty enums are checked.
func validateResource(field string, r Resource) []diag.Diagnostic {
	diags := validateName(field+".name", r.Name)
	if r.Access != "" && !r.Access.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidAccess, field+".access",
			"access %q is not in the v1 set: read, write, readwrite", r.Access))
	}
	if r.Scope != "" && !r.Scope.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidScope, field+".scope",
			"scope %q is not in the v1 set: project, workspace, environment, runtime", r.Scope))
	}
	return diags
}

// ValidateBinding checks structural invariants on a parsed binding: a supported
// protocol version, a canonical resource name, a non-empty backend kind and
// bucket, and an in-enum identity mode when present.
func ValidateBinding(b *Binding) []diag.Diagnostic {
	if b == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "binding is nil")}
	}
	diags := validateProtocolVersion(b.ProtocolVersion)
	diags = append(diags, validateName("name", b.Name)...)
	if strings.TrimSpace(b.Backend) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingBackend, "backend",
			"binding must name a backend kind"))
	}
	if strings.TrimSpace(b.Bucket) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingBucket, "bucket",
			"binding must name a bucket/container"))
	}
	if b.Identity != "" && !b.Identity.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIdentity, "identity",
			"identity %q is not in the v1 set: static, workload", b.Identity))
	}
	return diags
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// validateName validates a resource name against the canonical pattern. Empty
// names are reported as invalid.
func validateName(field, name string) []diag.Diagnostic {
	if name == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field, "name is required")}
	}
	if !resourceNamePattern.MatchString(name) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field,
			"name %q does not match canonical pattern %q", name, resourceNamePattern.String())}
	}
	return nil
}

// ParseAndValidateManifest runs strict parsing followed by structural
// validation.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifest(m)...)
}

// ParseAndValidateBinding runs strict parsing followed by structural
// validation.
func ParseAndValidateBinding(data []byte) (*Binding, []diag.Diagnostic) {
	b, diags := ParseBinding(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return b, append(diags, ValidateBinding(b)...)
}
