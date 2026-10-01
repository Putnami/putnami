package migration

import (
	"io/fs"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

// LoadBundle reads, validates, and materializes a bundle directory rooted at
// fsys (the directory containing bundle.json). It is the read-side counterpart
// of WriteBundle: it parses and strictly validates the manifest, then reads
// every referenced payload file and verifies its content hash, returning the
// normalized bundle alongside a map of payload path to bytes.
//
// A generic runner uses this to load a published bundle without the application
// graph that produced it: the manifest yields the operations and ordering, and
// the payload map yields the bytes to execute.
func LoadBundle(fsys fs.FS) (*Bundle, map[string][]byte, []diag.Diagnostic) {
	data, err := fs.ReadFile(fsys, BundleFileName)
	if err != nil {
		return nil, nil, []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidBundle, "", "could not read %s: %v", BundleFileName, err),
		}
	}

	bundle, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		return nil, nil, diags
	}

	payloads := make(map[string][]byte)
	for _, rel := range PayloadPaths(bundle) {
		content, err := fs.ReadFile(fsys, rel)
		if err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, "",
				"payload file %q could not be read: %v", rel, err))
			continue
		}
		payloads[rel] = content
	}
	if diag.HasErrors(diags) {
		return nil, nil, diags
	}

	for i := range bundle.Operations {
		op := bundle.Operations[i]
		field := "operations[" + strconv.Itoa(i) + "]"
		if op.Up.Path != "" {
			diags = append(diags, verifyPayloadBytes(payloads[op.Up.Path], op.Up, field+".up")...)
		}
		if op.Down != nil && op.Down.Path != "" {
			diags = append(diags, verifyPayloadBytes(payloads[op.Down.Path], *op.Down, field+".down")...)
		}
	}
	if diag.HasErrors(diags) {
		return nil, nil, diags
	}

	return bundle, payloads, diags
}
