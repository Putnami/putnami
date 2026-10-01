package sitecontent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"sort"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ManifestEntryName is the reserved tar entry carrying the bundle.json
// manifest at the archive root. The publishable blob a consumer pins is a
// single tar.gz holding this entry plus every payload file, so ONE digest
// addresses manifest and payload together: a separately fetched manifest would
// escape the consumer's content-address pin. The entry is the manifest
// carrier, not a payload file — it is exempt from the files/mount rules.
const ManifestEntryName = "bundle.json"

// VerifiedBundle is a fully verified archive: its manifest and the payload
// file contents keyed by payload-relative path (the reserved manifest entry
// excluded).
type VerifiedBundle struct {
	Manifest *Manifest
	Files    map[string][]byte
}

// VerifyArchiveResult reports archive verification. Bundle is non-nil only
// when there are no errors.
type VerifyArchiveResult struct {
	Bundle      *VerifiedBundle
	Diagnostics []diag.Diagnostic
	// NewerFormatMajor is true when the manifest parsed but declares a NEWER
	// format major than this package implements. The contract mandates the
	// consumer ignore such a bundle and fall back to its baked content, so
	// callers must treat this as skip-with-warning, never as a failure.
	NewerFormatMajor bool
}

func failArchive(diags []diag.Diagnostic) VerifyArchiveResult {
	return VerifyArchiveResult{Bundle: nil, Diagnostics: diags}
}

// VerifyArchive gunzips and decodes a digest-verified bundle blob, reads the
// minimal format-version envelope, gates on a newer major before decoding its
// unknown shape, then strict-parses and validates a readable bundle.json and
// verifies every payload entry against the manifest. It is the consumer entry
// point for blobs fetched from a registry; the payload rules are exactly
// VerifyPayload's, with ManifestEntryName exempt.
func VerifyArchive(blob []byte) VerifyArchiveResult {
	entries, diags, _ := readArchiveEntries(blob)
	if diag.HasErrors(diags) {
		return failArchive(diags)
	}

	var manifestEntry *archiveEntry
	for i := range entries {
		if entries[i].name == ManifestEntryName {
			manifestEntry = &entries[i]
			break
		}
	}
	if manifestEntry == nil || (manifestEntry.typeflag != tar.TypeReg && manifestEntry.typeflag != tarTypeRegularAlt) {
		return failArchive([]diag.Diagnostic{diag.Errorf(ErrorCodeParseError, ManifestEntryName,
			"bundle archive must carry a regular bundle.json at its root")})
	}

	// Read only the stable version envelope before strict decoding: a newer
	// major may carry fields and shapes this parser cannot judge, and the
	// contract says ignore + fall back. An invalid envelope never triggers the
	// fallback; strict parsing below rejects it instead.
	if version, versionOK := manifestFormatVersion(manifestEntry.data); versionOK {
		major, majorOK := FormatVersionMajor(version)
		if majorOK && major > FormatMajor {
			return VerifyArchiveResult{
				Diagnostics: []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFormatVersion, "formatVersion",
					"formatVersion %q is not readable by this parser (want major %d)", version, FormatMajor)},
				NewerFormatMajor: true,
			}
		}
	}

	m, pdiags := ParseManifest(manifestEntry.data)
	if m == nil {
		return failArchive(pdiags)
	}

	if major, ok := FormatVersionMajor(m.FormatVersion); ok && major > FormatMajor {
		// Defensive parity with the envelope gate. This is unreachable for a
		// valid single-object manifest, but keeps the skip signal coupled to the
		// parsed value if the envelope implementation ever changes.
		return VerifyArchiveResult{
			Diagnostics: []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFormatVersion, "formatVersion",
				"formatVersion %q is not readable by this parser (want major %d)", m.FormatVersion, FormatMajor)},
			NewerFormatMajor: true,
		}
	}

	vdiags := ValidateManifest(m)
	if diag.HasErrors(vdiags) {
		return failArchive(vdiags)
	}
	vdiags = append(vdiags, verifyEntries(m, entries, ManifestEntryName)...)
	if diag.HasErrors(vdiags) {
		return failArchive(vdiags)
	}

	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.name == ManifestEntryName {
			continue
		}
		files[entry.name] = entry.data
	}
	return VerifyArchiveResult{Bundle: &VerifiedBundle{Manifest: m, Files: files}}
}

// BuildArchive assembles the canonical publishable blob from a manifest and
// its payload files: a deterministic tar.gz with bundle.json at the root
// followed by the payload in sorted path order, fixed epoch timestamps, and
// fixed modes, so identical content yields identical bytes on one toolchain.
// The result is self-verified with VerifyArchive before it is returned — a
// producer holding a non-nil blob holds a conformant bundle.
func BuildArchive(m *Manifest, files map[string][]byte) ([]byte, []diag.Diagnostic) {
	if _, ok := files[ManifestEntryName]; ok {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPayloadEntry, ManifestEntryName,
			"payload files must not contain the reserved %q manifest entry", ManifestEntryName)}
	}

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, ManifestEntryName,
			"failed to encode manifest: %v", err)}
	}
	manifestBytes = append(manifestBytes, '\n')

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "failed to open gzip writer: %v", err)}
	}
	tw := tar.NewWriter(gz)

	writeEntry := func(name string, data []byte) []diag.Diagnostic {
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(header); err != nil {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPayloadEntry, name,
				"failed to write archive entry %q: %v", name, err)}
		}
		if _, err := tw.Write(data); err != nil {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPayloadEntry, name,
				"failed to write archive entry %q: %v", name, err)}
		}
		return nil
	}

	if diags := writeEntry(ManifestEntryName, manifestBytes); diags != nil {
		return nil, diags
	}
	for _, path := range paths {
		if diags := writeEntry(path, files[path]); diags != nil {
			return nil, diags
		}
	}
	if err := tw.Close(); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "failed to finalize archive: %v", err)}
	}
	if err := gz.Close(); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "failed to finalize archive: %v", err)}
	}

	blob := buf.Bytes()
	if result := VerifyArchive(blob); result.Bundle == nil {
		return nil, result.Diagnostics
	}
	return blob, nil
}
