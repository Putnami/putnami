package sitecontent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

type archiveFixture struct {
	// Manifest, when present, is marshaled and injected as the reserved
	// bundle.json root entry ahead of Entries. Hostile carrier shapes (missing
	// manifest, bundle.json as a symlink) set OmitManifest and model the
	// carrier inside Entries instead.
	Manifest             *Manifest             `json:"manifest,omitempty"`
	OmitManifest         bool                  `json:"omitManifest,omitempty"`
	Entries              []payloadFixtureEntry `json:"entries"`
	WantCodes            []string              `json:"wantCodes"`
	WantNewerFormatMajor bool                  `json:"wantNewerFormatMajor,omitempty"`
}

func TestVerifyArchive_Fixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/archive/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no archive fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var f archiveFixture
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatal(err)
			}
			if (f.Manifest == nil) == !f.OmitManifest {
				t.Fatal("fixture must set exactly one of manifest / omitManifest")
			}

			entries := f.Entries
			if f.Manifest != nil {
				manifestJSON, err := json.Marshal(f.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				entries = append([]payloadFixtureEntry{
					{Type: "file", Path: ManifestEntryName, Data: string(manifestJSON)},
				}, entries...)
			}
			blob := buildPayload(t, entries)

			result := VerifyArchive(blob)
			if result.NewerFormatMajor != f.WantNewerFormatMajor {
				t.Fatalf("NewerFormatMajor = %v, want %v (diags %v)",
					result.NewerFormatMajor, f.WantNewerFormatMajor, result.Diagnostics)
			}
			if len(f.WantCodes) == 0 {
				if result.Bundle == nil {
					t.Fatalf("want verified bundle, got diagnostics %v", result.Diagnostics)
				}
				if result.Bundle.Manifest.Name != f.Manifest.Name {
					t.Fatalf("manifest name %q, want %q", result.Bundle.Manifest.Name, f.Manifest.Name)
				}
				if _, ok := result.Bundle.Files[ManifestEntryName]; ok {
					t.Fatal("verified files must not contain the reserved manifest entry")
				}
				if len(result.Bundle.Files) != len(f.Entries) {
					t.Fatalf("got %d payload files, want %d", len(result.Bundle.Files), len(f.Entries))
				}
				return
			}
			if result.Bundle != nil {
				t.Fatalf("want rejection with codes %v, got a verified bundle", f.WantCodes)
			}
			for _, code := range f.WantCodes {
				if !hasCode(result.Diagnostics, code) {
					t.Fatalf("missing diagnostic code %q in %v", code, result.Diagnostics)
				}
			}
		})
	}
}

func TestVerifyArchive_GatesNewerMajorBeforeStrictManifestDecode(t *testing.T) {
	blob := buildPayload(t, []payloadFixtureEntry{{
		Type: "file",
		Path: ManifestEntryName,
		Data: `{
  "formatVersion": "2.0",
  "replacementManifest": {"layout": "not-a-v1-shape"}
}`,
	}})

	result := VerifyArchive(blob)
	if !result.NewerFormatMajor {
		t.Fatalf("newer major with an unknown shape must trigger fallback: %v", result.Diagnostics)
	}
	if result.Bundle != nil {
		t.Fatal("newer major must not produce a verified v1 bundle")
	}
	if !hasCode(result.Diagnostics, ErrorCodeInvalidFormatVersion) || hasCode(result.Diagnostics, ErrorCodeUnknownField) {
		t.Fatalf("newer major must gate before strict field decoding: %v", result.Diagnostics)
	}
}

func TestVerifyArchive_InvalidVersionEnvelopeFailsClosed(t *testing.T) {
	blob := buildPayload(t, []payloadFixtureEntry{{
		Type: "file",
		Path: ManifestEntryName,
		Data: `{"formatVersion":2}`,
	}})

	result := VerifyArchive(blob)
	if result.NewerFormatMajor || result.Bundle != nil || !hasCode(result.Diagnostics, ErrorCodeParseError) {
		t.Fatalf("invalid version envelope must fail, not trigger fallback: %#v", result)
	}
}

func TestBuildArchive_RoundTripDeterministic(t *testing.T) {
	files := map[string][]byte{
		"docs/cloud/index.html":      []byte("hello cloud\n"),
		"docs/cloud/guides/start.md": []byte("# Start\n"),
	}
	m := Manifest{
		FormatVersion: FormatVersion,
		Name:          "cloud-docs",
		Mounts:        []Mount{{URLPrefix: "/docs/cloud"}},
		Source:        Source{Repo: "acme-platform", Commit: "abc123"},
	}
	for path, data := range files {
		sum := sha256.Sum256(data)
		m.Files = append(m.Files, File{Path: path, Digest: hex.EncodeToString(sum[:])})
	}

	first, diags := BuildArchive(&m, files)
	if diag.HasErrors(diags) || first == nil {
		t.Fatalf("BuildArchive failed: %v", diags)
	}
	second, diags := BuildArchive(&m, files)
	if diag.HasErrors(diags) || !bytes.Equal(first, second) {
		t.Fatal("BuildArchive is not deterministic for identical input")
	}

	result := VerifyArchive(first)
	if result.Bundle == nil {
		t.Fatalf("built archive does not verify: %v", result.Diagnostics)
	}
	for path, data := range files {
		if !bytes.Equal(result.Bundle.Files[path], data) {
			t.Fatalf("payload file %q did not round-trip", path)
		}
	}
}

func TestBuildArchive_RejectsNonConformant(t *testing.T) {
	data := []byte("orphan\n")
	sum := sha256.Sum256(data)
	m := Manifest{
		FormatVersion: FormatVersion,
		Name:          "cloud-docs",
		Mounts:        []Mount{{URLPrefix: "/docs/cloud"}},
		Source:        Source{Repo: "acme-platform", Commit: "abc123"},
		Files:         []File{{Path: "elsewhere/orphan.md", Digest: hex.EncodeToString(sum[:])}},
	}
	blob, diags := BuildArchive(&m, map[string][]byte{"elsewhere/orphan.md": data})
	if blob != nil || !hasCode(diags, ErrorCodePathOutsideMount) {
		t.Fatalf("want self-verify rejection with %s, got blob=%v diags=%v",
			ErrorCodePathOutsideMount, blob != nil, diags)
	}
}

func TestBuildArchive_RejectsReservedName(t *testing.T) {
	m := validPayloadManifest()
	blob, diags := BuildArchive(&m, map[string][]byte{ManifestEntryName: []byte("{}")})
	if blob != nil || !hasCode(diags, ErrorCodeInvalidPayloadEntry) {
		t.Fatalf("want reserved-name rejection, got blob=%v diags=%v", blob != nil, diags)
	}
}
