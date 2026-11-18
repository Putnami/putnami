package screenshots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "screenshots/manifest-contract"

type fixtureExpectations struct {
	Valid   []string          `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

func loadExpectations(t *testing.T) fixtureExpectations {
	t.Helper()
	data, err := os.ReadFile("fixtures/expectations.json")
	if err != nil {
		t.Fatal(err)
	}
	var expectations fixtureExpectations
	if err := json.Unmarshal(data, &expectations); err != nil {
		t.Fatal(err)
	}
	return expectations
}

func readFixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, finding := range diags {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestConformance_ValidFixtures(t *testing.T) {
	spectest.Proves(t, feature, "the-corpus-is-the-contract", "every-valid-fixture-validates")
	expectations := loadExpectations(t)
	assertFixtureCoverage(t, "fixtures/valid", expectations.Valid)
	for _, name := range expectations.Valid {
		t.Run(name, func(t *testing.T) {
			manifest, diags := Decode(readFixture(t, "valid", name))
			if len(diags) > 0 {
				t.Fatalf("the valid fixture produced diagnostics: %v", diags)
			}
			if manifest == nil {
				t.Fatal("the valid fixture returned no manifest")
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	spectest.Proves(t, feature, "the-corpus-is-the-contract", "every-invalid-fixture-fails-with-its-code")
	expectations := loadExpectations(t)
	names := make([]string, 0, len(expectations.Invalid))
	for name := range expectations.Invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	assertFixtureCoverage(t, "fixtures/invalid", names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			manifest, diags := Decode(readFixture(t, "invalid", name))
			if manifest != nil || !diag.HasErrors(diags) {
				t.Fatal("the invalid fixture decoded")
			}
			// One fixture breaks one rule: any other code is a second rule the
			// fixture breaks without meaning to.
			want := expectations.Invalid[name]
			for _, finding := range diags {
				if finding.Code != want {
					t.Errorf("unexpected diagnostic %v, want only %s", finding, want)
				}
			}
		})
	}
}

// boundCodes are the codes of the four size bounds. Their cases are built
// by TestAManifestPastABoundIsRefused: a fixture past one of them would be a
// file of several hundred kilobytes.
var boundCodes = map[string]bool{ErrorCodeTooLarge: true, ErrorCodeTooManyPages: true, ErrorCodeTooManyCaptures: true,
	ErrorCodeTooManyProofs: true}

func TestConformance_ErrorTaxonomy(t *testing.T) {
	expectations := loadExpectations(t)
	t.Run("every expected code is registered", func(t *testing.T) {
		spectest.Proves(t, feature, "the-corpus-is-the-contract", "every-expected-code-is-registered")
		for name, code := range expectations.Invalid {
			if !ValidErrorCodes[code] {
				t.Errorf("%s expects the undeclared code %q", name, code)
			}
		}
	})
	t.Run("every code has an invalid fixture", func(t *testing.T) {
		spectest.Proves(t, feature, "the-corpus-is-the-contract", "every-code-has-an-invalid-fixture")
		covered := map[string]bool{}
		for _, code := range expectations.Invalid {
			covered[code] = true
		}
		for code := range ValidErrorCodes {
			if !covered[code] && !boundCodes[code] {
				t.Errorf("no invalid fixture expects %s", code)
			}
		}
	})
}

func assertFixtureCoverage(t *testing.T, dir string, expected []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	sort.Strings(actual)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if len(actual) != len(want) {
		t.Fatalf("%s fixture coverage differs: got %v, want %v", dir, actual, want)
	}
	for i := range actual {
		if actual[i] != want[i] {
			t.Fatalf("%s fixture coverage differs: got %v, want %v", dir, actual, want)
		}
	}
}
