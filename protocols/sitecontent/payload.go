package sitecontent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
)

const tarTypeRegularAlt byte = 0

// MaxVerifiedPayloadFileBytes caps the decompressed bytes read for one payload
// entry. The contract is designed for documentation/site content; this keeps
// the reference verifier from spending unbounded CPU or memory on compressed
// input.
const MaxVerifiedPayloadFileBytes int64 = 1 << 30 // 1 GiB

// archiveEntry is one decoded tar entry. Regular-file contents are
// materialized up to MaxVerifiedPayloadFileBytes; larger files carry tooBig
// instead of data so the rule loop can report them without holding the bytes.
type archiveEntry struct {
	name     string
	typeflag byte
	data     []byte
	tooBig   bool
}

// readArchiveEntries gunzips and decodes a tar stream into entries. gzOK is
// false only when the blob is not a gzip stream at all (the historical
// VerifyPayload early-return case); every other failure is reported as a
// diagnostic alongside the entries decoded so far.
func readArchiveEntries(blob []byte) (entries []archiveEntry, diags []diag.Diagnostic, gzOK bool) {
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "invalid tar.gz payload: %v", err)}, false
	}

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeParseError, "", "invalid tar payload: %v", err))
			break
		}

		entry := archiveEntry{name: h.Name, typeflag: h.Typeflag}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tarTypeRegularAlt {
			var buf bytes.Buffer
			limited := &io.LimitedReader{R: tr, N: MaxVerifiedPayloadFileBytes + 1}
			n, err := io.Copy(&buf, limited)
			if err != nil {
				diags = append(diags, diag.Errorf(ErrorCodeParseError, h.Name,
					"failed to read payload file %q: %v", h.Name, err))
				continue
			}
			if n > MaxVerifiedPayloadFileBytes {
				entry.tooBig = true
			} else {
				entry.data = buf.Bytes()
			}
		}
		entries = append(entries, entry)
	}
	if err := gz.Close(); err != nil {
		diags = append(diags, diag.Errorf(ErrorCodeParseError, "", "invalid tar.gz payload close: %v", err))
	}
	return entries, diags, true
}

// verifyEntries runs the payload rules over decoded entries: regular files
// only, no symlinks or hardlinks, safe relative paths, every file under a
// declared mount, listed exactly once, present, and digest-matched. When
// reserved is non-empty, entries with that exact name are the manifest carrier
// and are exempt from every rule.
func verifyEntries(m *Manifest, entries []archiveEntry, reserved string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	listed := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		listed[f.Path] = f.Digest
	}
	seen := make(map[string]bool, len(m.Files))

	for _, entry := range entries {
		if reserved != "" && entry.name == reserved {
			continue
		}
		path := entry.name
		if entry.typeflag == tar.TypeSymlink || entry.typeflag == tar.TypeLink {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSymlink, path,
				"payload entry %q is a link; symlinks and hardlinks are not allowed", path))
			continue
		}
		if entry.typeflag != tar.TypeReg && entry.typeflag != tarTypeRegularAlt {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayloadEntry, path,
				"payload entry %q has unsupported tar type %d; only regular files are allowed", path, entry.typeflag))
			continue
		}
		if !ValidRelPath(path) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, path,
				"payload path %q must be a forward-slash relative path with no traversal", path))
			continue
		}
		if _, ok := MountForPath(m.Mounts, path); !ok {
			diags = append(diags, diag.Errorf(ErrorCodePathOutsideMount, path,
				"payload path %q (served at %q) falls under no declared mount", path, URLPath(path)))
			continue
		}
		expected, ok := listed[path]
		if !ok {
			diags = append(diags, diag.Errorf(ErrorCodeUnlistedFile, path,
				"payload file %q is not listed in manifest files", path))
			continue
		}
		if seen[path] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateFile, path,
				"payload file %q appears more than once", path))
			continue
		}
		seen[path] = true
		if entry.tooBig {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPayloadEntry, path,
				"payload file %q exceeds the verifier limit of %d bytes", path, MaxVerifiedPayloadFileBytes))
			continue
		}
		sum := sha256.Sum256(entry.data)
		if got := hex.EncodeToString(sum[:]); got != expected {
			diags = append(diags, diag.Errorf(ErrorCodeDigestMismatch, path,
				"payload file %q digest %s does not match manifest digest %s", path, got, expected))
		}
	}

	for _, f := range m.Files {
		if !seen[f.Path] {
			diags = append(diags, diag.Errorf(ErrorCodeMissingFile, f.Path,
				"manifest file %q is missing from payload", f.Path))
		}
	}
	return diags
}

// VerifyPayload verifies a manifest-less tar.gz payload against a validated
// manifest held separately. The canonical publishable artifact embeds the
// manifest in the archive instead — use VerifyArchive for blobs fetched from a
// registry, and BuildArchive to produce them.
func VerifyPayload(m *Manifest, payload []byte) []diag.Diagnostic {
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		return diags
	}

	entries, rdiags, gzOK := readArchiveEntries(payload)
	diags = append(diags, rdiags...)
	if !gzOK {
		return diags
	}
	return append(diags, verifyEntries(m, entries, "")...)
}

func duplicateManifestPath(path string, first, second int) diag.Diagnostic {
	return diag.Errorf(ErrorCodeDuplicateFile, fmt.Sprintf("files[%d].path", second),
		"path %q duplicates files[%d].path; every payload file must be listed once", path, first)
}
