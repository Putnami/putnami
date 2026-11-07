package architecture

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// frozenV1Digests pins every authored document of fixtures/v1, written on
// 2026-11-07 when the module became preview (ADR 0003). A valid version 1
// manifest, baseline or waivers file stays valid: when this map or a file under
// fixtures/v1 needs an edit, the change opens protocol version 2 with a
// migration command instead.
var frozenV1Digests = map[string]string{
	"baseline.json":               "3f8bb16ca2a8916483b3115827bdbaab2941b7a75f505b9c0c3c634d664744f9",
	"manifest.consumer.json":      "3927e5a01365126da0640a37f9d28d5baec28aa2f58c83f15514274498f95972",
	"manifest.observability.json": "2cb69dda0e9faf261c14c33a3a0d62e1c5698a689f19972a18b37dc2ab893afa",
	"manifest.producer.json":      "204e01ff537719898fd862667f00b3b888af1a877fcf17eb960416d4c819ddcf",
	"manifest.runtime.json":       "e34daacb416afd6b021e09a6c5c20da4b0aa6df3d6cc232f7a95e9605d72d773",
	"waivers.json":                "e4d15b5ed7292538619ca241c7c0e93ce6cb78324a5af3b1b61bc0f85049a914",
}

func frozenV1Files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("fixtures", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestFrozenV1AuthoredDocumentsValidate(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "authored-version-one-stays-valid", "every-frozen-v1-authored-document-validates")
	var (
		sources  []ManifestSource
		baseline *Baseline
		waivers  *WaiverFile
	)
	for _, name := range frozenV1Files(t) {
		path := filepath.Join("fixtures", "v1", name)
		data := readFixture(t, path)
		var diagnostics []diag.Diagnostic
		switch {
		case strings.HasPrefix(name, "manifest."):
			var manifest *Manifest
			manifest, diagnostics = ParseAndValidateManifest(data)
			sources = append(sources, ManifestSource{Path: path, Manifest: manifest})
		case name == "baseline.json":
			baseline, diagnostics = ParseAndValidateBaseline(data)
		case name == "waivers.json":
			waivers, diagnostics = ParseAndValidateWaiverFile(data)
		default:
			t.Fatalf("fixtures/v1/%s is not a manifest, the baseline or the waivers file", name)
		}
		if diag.HasErrors(diagnostics) {
			t.Fatalf("frozen version 1 document %s is rejected: %v", name, diagnostics)
		}
	}
	if len(sources) < 2 || baseline == nil || waivers == nil {
		t.Fatalf("fixtures/v1 must hold manifests, a baseline and a waivers file: %d manifest(s), baseline %t, waivers %t",
			len(sources), baseline != nil, waivers != nil)
	}
	if diagnostics := ValidateRepository(sources); diag.HasErrors(diagnostics) {
		t.Fatalf("the frozen version 1 manifests are rejected as one repository: %v", diagnostics)
	}
	if diagnostics := ValidateDebtFiles(baseline, waivers); diag.HasErrors(diagnostics) {
		t.Fatalf("the frozen version 1 baseline and waivers are rejected together: %v", diagnostics)
	}
}

func TestFrozenV1AuthoredDocumentsKeepTheirBytes(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "authored-version-one-stays-valid", "every-frozen-v1-authored-document-keeps-its-digest")
	files := frozenV1Files(t)
	pinned := make([]string, 0, len(frozenV1Digests))
	for name := range frozenV1Digests {
		pinned = append(pinned, name)
	}
	sort.Strings(pinned)
	if !slices.Equal(files, pinned) {
		t.Fatalf("fixtures/v1 holds %v, the pin lists %v: a frozen version 1 document is never added, removed or renamed (ADR 0003)", files, pinned)
	}
	for _, name := range files {
		sum := sha256.Sum256(readFixture(t, filepath.Join("fixtures", "v1", name)))
		if got := hex.EncodeToString(sum[:]); got != frozenV1Digests[name] {
			t.Errorf("fixtures/v1/%s changed (sha256 %s): a valid version 1 document stays valid; open protocol version 2 with a migration command instead (ADR 0003)", name, got)
		}
	}
}
