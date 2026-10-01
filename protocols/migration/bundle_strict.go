package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseBundle decodes JSON data into a Bundle using strict mode. Unknown fields
// are rejected so malformed or future-version bundles fail loudly instead of
// silently dropping data.
func ParseBundle(data []byte) (*Bundle, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidBundle, "", "failed to parse migration bundle: %v", err),
		}
	}
	return &b, nil
}

// ValidateBundle checks structural invariants on a migration bundle. It
// validates the protocol identifier, identity fields, every operation and its
// payload references, rejects duplicate operations, and — when a digest is
// present — verifies it matches the recomputed content digest.
func ValidateBundle(b *Bundle) []diag.Diagnostic {
	if b == nil {
		return []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidBundle, "", "migration bundle is nil"),
		}
	}

	normalized := NormalizeBundle(*b)
	var diags []diag.Diagnostic

	if b.Protocol != "" && b.Protocol != BundleProtocol {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, "protocol",
			"unknown bundle protocol %q; expected %q", b.Protocol, BundleProtocol))
	}

	if b.AppName == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, "appName", "bundle appName is required"))
	}

	if len(normalized.Operations) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, "operations",
			"bundle must declare at least one operation"))
	}

	seen := make(map[string]int)
	for i := range normalized.Operations {
		op := normalized.Operations[i]
		field := fmt.Sprintf("operations[%d]", i)
		diags = append(diags, validateOperation(op, field)...)

		key := operationKey(op)
		if first, ok := seen[key]; ok {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateOperation, field+".name",
				"duplicate operation %q; first seen at normalized index %d", key, first))
			continue
		}
		seen[key] = i
	}

	// Digest is optional, but when present it must content-address this bundle.
	// This is the loud failure that protects idempotent publish-by-digest.
	if b.Digest != "" {
		want := ComputeBundleDigest(*b)
		if b.Digest != want {
			diags = append(diags, diag.Errorf(ErrorCodeDigestMismatch, "digest",
				"bundle digest mismatch: manifest declares %q but content hashes to %q", b.Digest, want))
		}
	}

	return diags
}

// validateOperation checks one normalized operation and its payload references.
func validateOperation(op BundleOperation, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic

	if !ValidOperationKinds[op.Kind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, field+".kind",
			"unknown operation kind %q", op.Kind))
	}
	if op.Target == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, field+".target", "operation target is required"))
	}
	if op.Name == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, field+".name", "operation name is required"))
	}
	if !ValidSafetyClasses[op.Safety] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, field+".safety",
			"unknown safety classification %q", op.Safety))
	}

	diags = append(diags, validatePayloadRef(op.Up, field+".up", true)...)
	if op.Down != nil {
		diags = append(diags, validatePayloadRef(*op.Down, field+".down", false)...)
	}

	// Reversibility must be backed by a rollback payload.
	if op.Capabilities.Reversible && op.Down == nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBundle, field+".down",
			"reversible operations must provide a down payload"))
	}

	return diags
}

// validatePayloadRef checks a payload reference. The up payload is required; a
// nil-checked down payload reuses the same rules. Paths must be clean, relative,
// forward-slash paths under the bundle root and hashes must be canonical SHA-256.
func validatePayloadRef(ref PayloadRef, field string, required bool) []diag.Diagnostic {
	var diags []diag.Diagnostic

	if ref.Path == "" {
		if required {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".path", "payload path is required"))
		}
	} else if !isCleanRelPath(ref.Path) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".path",
			"payload path %q must be a clean relative path under the bundle root", ref.Path))
	}

	if ref.Hash == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".hash", "payload hash is required"))
	} else if !sha256HexPattern.MatchString(ref.Hash) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".hash",
			"payload hash must be a lowercase 64-character SHA-256 hex string"))
	}

	return diags
}

// isCleanRelPath reports whether p is a forward-slash relative path that does
// not escape the bundle root. Bundles are portable archives, so absolute paths,
// backslashes, and parent traversal are rejected.
func isCleanRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	cleaned := path.Clean(p)
	if cleaned != p {
		return false
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return true
}

// operationKey returns the duplicate-detection identity of an operation:
// kind, target, namespace, and name. Two operations sharing this key collide
// in the bundle regardless of payload contents.
func operationKey(op BundleOperation) string {
	return string(op.Kind) + "|" + op.Target + "|" + op.Namespace + "|" + op.Name
}

// ParseAndValidateBundle combines strict parsing, validation, and
// normalization. The returned bundle is normalized (canonical defaults applied,
// operations sorted) so callers receive a deterministic value.
func ParseAndValidateBundle(data []byte) (*Bundle, []diag.Diagnostic) {
	b, diags := ParseBundle(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	diags = append(diags, ValidateBundle(b)...)

	normalized := NormalizeBundle(*b)
	return &normalized, diags
}

// SortOperations sorts operations into canonical bundle order in place. It is
// exported for emitters that build operations incrementally and want the same
// deterministic ordering the protocol uses for hashing.
func SortOperations(ops []BundleOperation) {
	sort.SliceStable(ops, func(i, j int) bool {
		return lessOperation(ops[i], ops[j])
	})
}
