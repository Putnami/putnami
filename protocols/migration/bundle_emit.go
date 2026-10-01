package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// BundlePayload is one materialized payload file destined for a bundle's
// payload tree: a forward-slash relative path under the bundle root and the raw
// bytes whose hash the corresponding PayloadRef pins.
type BundlePayload struct {
	Path  string
	Bytes []byte
}

// BundleContributor is the optional capability a migration runner implements to
// contribute its operations and payloads to a build-time migration bundle.
//
// Implementations load definitions from the authoring source (filesystem, code
// registry) without reaching any live system, so bundle emission stays a pure,
// reproducible build step. The build/describe phase collects contributions from
// every runner that implements this interface and assembles one bundle.
type BundleContributor interface {
	MigrationBundleOperations() ([]BundleOperation, []BundlePayload, error)
}

// WriteBundle materializes a complete bundle directory at root: the canonical
// bundle.json manifest (digest filled in) plus every referenced payload file.
//
// The bundle is normalized, validated, and cross-checked against the supplied
// payloads before anything is written, so a malformed emission fails loudly
// instead of leaving a broken artifact on disk. Only payloads the manifest
// references are written; ordering is canonical for reproducible output.
func WriteBundle(root string, b Bundle, payloads []BundlePayload) error {
	normalized := NormalizeBundle(b)
	normalized.Digest = ComputeBundleDigest(normalized)

	if diags := ValidateBundle(&normalized); diag.HasErrors(diags) {
		return fmt.Errorf("migration bundle is invalid: %s", joinDiagnostics(diags))
	}

	index := make(map[string][]byte, len(payloads))
	for _, p := range payloads {
		index[p.Path] = p.Bytes
	}
	if diags := verifyPayloadIndex(&normalized, index); diag.HasErrors(diags) {
		return fmt.Errorf("migration bundle payloads do not match manifest: %s", joinDiagnostics(diags))
	}

	manifest, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(root, 0o750); err != nil { //nolint:gosec // root is framework-controlled (describe output dir)
		return err
	}
	for _, rel := range PayloadPaths(&normalized) {
		dest := filepath.Join(root, filepath.FromSlash(rel)) //nolint:gosec // rel is a validated clean relative path (no traversal)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dest, index[rel], 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(root, BundleFileName), append(manifest, '\n'), 0o600)
}

// verifyPayloadIndex checks that every payload the manifest references has
// matching bytes in index and that those bytes hash to the pinned reference.
func verifyPayloadIndex(b *Bundle, index map[string][]byte) []diag.Diagnostic {
	var diags []diag.Diagnostic
	check := func(ref PayloadRef, field string) {
		if ref.Path == "" {
			return
		}
		data, ok := index[ref.Path]
		if !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".path",
				"payload %q has no materialized bytes", ref.Path))
			return
		}
		if got := ComputePayloadHash(data); got != ref.Hash {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, field+".hash",
				"payload %q hashes to %q but manifest pins %q", ref.Path, got, ref.Hash))
		}
	}
	for i := range b.Operations {
		op := b.Operations[i]
		field := "operations[" + strconv.Itoa(i) + "]"
		check(op.Up, field+".up")
		if op.Down != nil {
			check(*op.Down, field+".down")
		}
	}
	return diags
}

// joinDiagnostics renders error-severity diagnostics into a single string.
func joinDiagnostics(diags []diag.Diagnostic) string {
	errs := diag.Errors(diags)
	parts := make([]string, 0, len(errs))
	for _, d := range errs {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "; ")
}
