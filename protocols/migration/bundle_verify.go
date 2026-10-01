package migration

import (
	"io/fs"
	"sort"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

// VerifyBundlePayloads checks that every payload referenced by a bundle exists
// in fsys and that its on-disk content hashes to the pinned reference hash.
//
// Structural validation (ValidateBundle) proves the manifest is well-formed;
// this proves the manifest matches the materialized payload files. Publish
// tooling runs both so a tampered or incomplete bundle fails loudly before it
// is ever uploaded. fsys is rooted at the bundle directory (the directory that
// contains bundle.json), and payload paths are resolved relative to it.
func VerifyBundlePayloads(fsys fs.FS, b *Bundle) []diag.Diagnostic {
	if b == nil {
		return []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidBundle, "", "migration bundle is nil"),
		}
	}

	normalized := NormalizeBundle(*b)
	var diags []diag.Diagnostic
	for i := range normalized.Operations {
		op := normalized.Operations[i]
		field := "operations[" + strconv.Itoa(i) + "]"
		diags = append(diags, verifyPayloadFile(fsys, op.Up, field+".up")...)
		if op.Down != nil {
			diags = append(diags, verifyPayloadFile(fsys, *op.Down, field+".down")...)
		}
	}
	return diags
}

// verifyPayloadFile reads one payload file and compares its hash to the ref.
func verifyPayloadFile(fsys fs.FS, ref PayloadRef, field string) []diag.Diagnostic {
	if ref.Path == "" {
		return nil // structural validation already reported the missing path
	}
	data, err := fs.ReadFile(fsys, ref.Path)
	if err != nil {
		return []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidPayload, field+".path",
				"payload file %q could not be read: %v", ref.Path, err),
		}
	}
	return verifyPayloadBytes(data, ref, field)
}

// verifyPayloadBytes compares a payload's bytes against the pinned ref hash.
func verifyPayloadBytes(data []byte, ref PayloadRef, field string) []diag.Diagnostic {
	got := ComputePayloadHash(data)
	if got != ref.Hash {
		return []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidPayload, field+".hash",
				"payload %q hashes to %q but manifest pins %q", ref.Path, got, ref.Hash),
		}
	}
	return nil
}

// PayloadPaths returns the sorted, de-duplicated set of payload paths a bundle
// references. Publish tooling uses it to enumerate the files that must be
// uploaded alongside the manifest.
func PayloadPaths(b *Bundle) []string {
	if b == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, op := range b.Operations {
		if op.Up.Path != "" {
			seen[op.Up.Path] = true
		}
		if op.Down != nil && op.Down.Path != "" {
			seen[op.Down.Path] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
