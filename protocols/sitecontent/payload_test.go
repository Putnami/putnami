package sitecontent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

type payloadFixture struct {
	Manifest  Manifest              `json:"manifest"`
	Entries   []payloadFixtureEntry `json:"entries"`
	WantCodes []string              `json:"wantCodes"`
}

type payloadFixtureEntry struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	Data     string `json:"data,omitempty"`
	Linkname string `json:"linkname,omitempty"`
}

func TestVerifyPayload_Fixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/payload/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no payload fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var f payloadFixture
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatal(err)
			}

			payload := buildPayload(t, f.Entries)
			diags := VerifyPayload(&f.Manifest, payload)
			if len(f.WantCodes) == 0 {
				if diag.HasErrors(diags) {
					t.Fatalf("unexpected diagnostics: %v", diags)
				}
				return
			}
			for _, code := range f.WantCodes {
				if !hasCode(diags, code) {
					t.Fatalf("missing diagnostic code %q in %v", code, diags)
				}
			}
		})
	}
}

func TestVerifyPayload_InvalidGzip(t *testing.T) {
	m := validPayloadManifest()
	diags := VerifyPayload(&m, []byte("not gzip"))
	if !hasCode(diags, ErrorCodeParseError) {
		t.Fatalf("want parse error, got %v", diags)
	}
}

func buildPayload(t *testing.T, entries []payloadFixtureEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		switch entry.Type {
		case "file":
			data := []byte(entry.Data)
			if err := tw.WriteHeader(&tar.Header{
				Name:     entry.Path,
				Mode:     0o644,
				Size:     int64(len(data)),
				Typeflag: tar.TypeReg,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := tw.WriteHeader(&tar.Header{
				Name:     entry.Path,
				Mode:     0o777,
				Typeflag: tar.TypeSymlink,
				Linkname: entry.Linkname,
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown fixture entry type %q", entry.Type)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func validPayloadManifest() Manifest {
	return Manifest{
		FormatVersion: FormatVersion,
		Name:          "cloud-docs",
		Mounts:        []Mount{{URLPrefix: "/docs/cloud"}},
		Source:        Source{Repo: "acme-platform", Commit: "abc123"},
		Files: []File{{
			Path:   "docs/cloud/index.html",
			Digest: "1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5",
		}},
	}
}
