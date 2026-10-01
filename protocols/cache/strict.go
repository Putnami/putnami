package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic codes for cache-protocol parse and validation findings.
const (
	CodeParseError       = "parse-error"
	CodeVersionMismatch  = "version-mismatch"
	CodeRequiredField    = "required-field"
	CodeTooManyKeys      = "too-many-keys"
	CodeInvalidKey       = "invalid-key"
	CodeInvalidDigest    = "invalid-digest"
	CodeInvalidPath      = "invalid-path"
	CodeInvalidMode      = "invalid-mode"
	CodeInvalidMethod    = "invalid-method"
	CodeInvalidSelection = "invalid-selection"
	CodeDuplicateKey     = "duplicate-key"
	CodeUnauthorized     = "unauthorized"
	CodeBlobTooLarge     = "blob-too-large"
)

// parseStrict decodes JSON into a value of type T, rejecting unknown fields
// and returning structured diagnostics on a decode error. what names the
// message in the parse-error diagnostic.
func parseStrict[T any](data []byte, what string) (*T, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var v T
	if err := dec.Decode(&v); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf(CodeParseError, "", "failed to parse %s: %v", what, err),
		}
	}
	return &v, nil
}

// parseAndValidate combines strict parsing, validation, and normalization.
// The value is returned even alongside non-fatal diagnostics (warnings); it is
// nil only when parsing or a hard validation error fails.
func parseAndValidate[T any](
	data []byte,
	parse func([]byte) (*T, []diag.Diagnostic),
	validate func(*T) []diag.Diagnostic,
	normalize func(*T) *T,
) (*T, []diag.Diagnostic) {
	v, diags := parse(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = append(diags, validate(v)...)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	normalize(v)
	return v, diags
}

// ParseRequestStrict decodes JSON into a NegotiateRequest, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseRequestStrict(data []byte) (*NegotiateRequest, []diag.Diagnostic) {
	return parseStrict[NegotiateRequest](data, "negotiate request")
}

// ValidateRequest checks structural invariants on a parsed negotiate request.
func ValidateRequest(r *NegotiateRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}

	var diags []diag.Diagnostic

	if r.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", r.ProtocolVersion, ProtocolVersion))
	}
	if r.Mode != "" && !r.Mode.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidMode, "mode", "unknown materialization mode %q", r.Mode))
	}
	if len(r.Keys) == 0 {
		diags = append(diags, diag.Errorf(CodeRequiredField, "keys", "at least one key is required"))
	}
	if len(r.Keys) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "keys",
			"%d keys exceeds the per-request limit of %d", len(r.Keys), MaxKeysPerRequest))
	}

	seen := make(map[string]struct{}, len(r.Keys))
	for i, k := range r.Keys {
		field := fmt.Sprintf("keys[%d].key", i)
		if !ValidKey(k.Key) {
			diags = append(diags, diag.Errorf(CodeInvalidKey, field, "malformed cache key %q", k.Key))
			continue
		}
		if _, dup := seen[k.Key]; dup {
			diags = append(diags, diag.Warningf(CodeDuplicateKey, field, "duplicate cache key %q", k.Key))
		}
		seen[k.Key] = struct{}{}
	}

	return diags
}

// NormalizeRequest applies canonical defaults and ordering for deterministic
// output. It modifies the request in place and returns it.
func NormalizeRequest(r *NegotiateRequest) *NegotiateRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProtocolVersion
	}
	if r.Mode == "" {
		r.Mode = DefaultMode
	}
	sort.SliceStable(r.Keys, func(i, j int) bool { return r.Keys[i].Key < r.Keys[j].Key })
	return r
}

// ParseAndValidateRequest combines strict parsing, validation, and
// normalization. The request is returned even alongside non-fatal diagnostics
// (warnings); it is nil only when parsing or a hard validation error fails.
func ParseAndValidateRequest(data []byte) (*NegotiateRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseRequestStrict, ValidateRequest, NormalizeRequest)
}

// ParseResponseStrict decodes JSON into a NegotiateResponse, rejecting unknown
// fields and returning structured diagnostics on a decode error.
func ParseResponseStrict(data []byte) (*NegotiateResponse, []diag.Diagnostic) {
	return parseStrict[NegotiateResponse](data, "negotiate response")
}

// ValidateResponse checks structural invariants on a parsed negotiate response.
func ValidateResponse(resp *NegotiateResponse) []diag.Diagnostic {
	if resp == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}

	var diags []diag.Diagnostic

	if resp.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported protocol version %d (want %d)", resp.ProtocolVersion, ProtocolVersion))
	}

	for i := range resp.Results {
		diags = append(diags, validateResult(i, &resp.Results[i])...)
	}

	return diags
}

func validateResult(i int, r *KeyResult) []diag.Diagnostic {
	var diags []diag.Diagnostic
	base := fmt.Sprintf("results[%d]", i)

	if !ValidKey(r.Key) {
		diags = append(diags, diag.Errorf(CodeInvalidKey, base+".key", "malformed cache key %q", r.Key))
	}

	if r.Hit {
		diags = append(diags, validateActionResult(base+".result", r.Result)...)
		diags = append(diags, validateManifestFiles(base+".manifest", r.Manifest)...)
	}

	diags = append(diags, validateTransfers(base+".downloads", TransferGet, r.Downloads)...)
	return diags
}

func validateActionResult(field string, r *ActionResult) []diag.Diagnostic {
	if r == nil || r.Status == "" {
		return []diag.Diagnostic{
			diag.Errorf(CodeRequiredField, field, "a result with a status is required"),
		}
	}

	var diags []diag.Diagnostic
	for i, ev := range r.Events {
		if ev.Type == "" {
			diags = append(diags, diag.Errorf(CodeRequiredField,
				fmt.Sprintf("%s.events[%d].type", field, i),
				"cached result event type is required"))
		}
	}
	return diags
}

// validateManifestFiles checks each file entry's path and digest under the
// given field prefix. A nil manifest is a no-op — callers enforce presence
// separately where a manifest is required.
func validateManifestFiles(field string, m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for j, f := range m.Files {
		ff := fmt.Sprintf("%s.files[%d]", field, j)
		if f.Path == "" {
			diags = append(diags, diag.Errorf(CodeRequiredField, ff+".path", "manifest file path is required"))
		} else if !ValidRelPath(f.Path) {
			// A consumer materializes each manifest file to disk on a cache hit
			// by joining Path to the task output root. Reject absolute paths,
			// "../" traversal, backslashes, and NUL at the protocol boundary so
			// no consumer can be tricked into an out-of-root write by a hostile
			// or MITM'd cache server. Mirrors sitecontent.ValidRelPath.
			diags = append(diags, diag.Errorf(CodeInvalidPath, ff+".path",
				"manifest file path %q must be a safe relative path (no leading '/', '\\', NUL, or '.'/'..' segment)", f.Path))
		}
		if !ValidDigest(f.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, ff+".digest", "malformed digest %q", f.Digest))
		}
	}
	return diags
}

// ValidRelPath reports whether p is a safe payload-relative path: non-empty,
// forward-slash separated, with no leading '/', no backslash, no NUL, and no
// empty, '.' or '..' segment. This rejects absolute paths and "../" traversal
// so a manifest entry cannot escape the consumer's output root. It mirrors
// sitecontent.ValidRelPath, kept local to avoid a cross-protocol dependency.
func ValidRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validateTransfers(field, wantMethod string, transfers []BlobTransfer) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for k, t := range transfers {
		tf := fmt.Sprintf("%s[%d]", field, k)
		if !ValidDigest(t.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, tf+".digest", "malformed digest %q", t.Digest))
		}
		if t.URL == "" {
			diags = append(diags, diag.Errorf(CodeRequiredField, tf+".url", "transfer url is required"))
		}
		if t.Method != wantMethod {
			diags = append(diags, diag.Errorf(CodeInvalidMethod, tf+".method",
				"expected method %q, got %q", wantMethod, t.Method))
		}
	}
	return diags
}

// NormalizeResponse applies canonical defaults and ordering for deterministic
// output. It modifies the response in place and returns it.
func NormalizeResponse(resp *NegotiateResponse) *NegotiateResponse {
	if resp == nil {
		return nil
	}
	if resp.ProtocolVersion == 0 {
		resp.ProtocolVersion = ProtocolVersion
	}
	sort.SliceStable(resp.Results, func(i, j int) bool { return resp.Results[i].Key < resp.Results[j].Key })
	for i := range resp.Results {
		r := &resp.Results[i]
		NormalizeManifestFiles(r.Manifest)
		sortTransfers(r.Downloads)
	}
	return resp
}

// NormalizeManifestFiles sorts a manifest's files by path for deterministic
// output. A nil manifest is a no-op.
func NormalizeManifestFiles(m *Manifest) {
	if m == nil {
		return
	}
	sort.SliceStable(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
}

func sortTransfers(transfers []BlobTransfer) {
	sort.SliceStable(transfers, func(i, j int) bool { return transfers[i].Digest < transfers[j].Digest })
}

// ParseAndValidateResponse combines strict parsing, validation, and
// normalization. The response is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateResponse(data []byte) (*NegotiateResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseResponseStrict, ValidateResponse, NormalizeResponse)
}
