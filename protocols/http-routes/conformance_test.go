package httproutes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

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

func TestConformance_ValidFixtures(t *testing.T) {
	expectations := loadExpectations(t)
	assertFixtureCoverage(t, "fixtures/valid", expectations.Valid)
	for _, name := range expectations.Valid {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures/valid", name))
			if err != nil {
				t.Fatal(err)
			}
			manifest, diags := ParseAndValidateManifest(data)
			if diag.HasErrors(diags) {
				t.Fatalf("valid fixture produced diagnostics: %v", diags)
			}
			if manifest == nil {
				t.Fatal("valid fixture returned nil manifest")
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	expectations := loadExpectations(t)
	names := make([]string, 0, len(expectations.Invalid))
	for name := range expectations.Invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	assertFixtureCoverage(t, "fixtures/invalid", names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures/invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateManifest(data)
			if !diag.HasErrors(diags) {
				t.Fatal("invalid fixture produced no error diagnostics")
			}
			wantCode := expectations.Invalid[name]
			for _, finding := range diags {
				if finding.Code == wantCode {
					return
				}
			}
			t.Fatalf("diagnostics %v do not include expected code %q", diags, wantCode)
		})
	}
}

func TestConformance_ErrorTaxonomy(t *testing.T) {
	for code := range ValidErrorCodes {
		if code == "" {
			t.Fatal("empty diagnostic code")
		}
	}
	for _, code := range loadExpectations(t).Invalid {
		if !ValidErrorCodes[code] {
			t.Errorf("fixture expects undeclared diagnostic code %q", code)
		}
	}
}

func assertFixtureCoverage(t *testing.T, dir string, expected []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			actual = append(actual, entry.Name())
		}
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
