package analytics

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestConformance_InvalidFixturesCoverEveryCode holds the "corpus is the spec"
// rule: another language's sanitizer validates itself against these files and
// never sees a reject branch the corpus does not exercise. A code that no
// fixture produces is therefore only aspirationally shared — the TypeScript side
// can omit it and stay green.
//
// parse_error is the one exception. It fires on a body that is not decodable
// JSON, which a corpus of .json files cannot hold; validate_test.go pins it
// directly instead.
func TestConformance_InvalidFixturesCoverEveryCode(t *testing.T) {
	produced := make(map[string]bool, len(ValidErrorCodes))
	for _, path := range fixturePaths(t, "invalid") {
		for _, code := range codesOf(t, path) {
			produced[code] = true
		}
	}

	want := make(map[string]bool, len(ValidErrorCodes))
	for code := range ValidErrorCodes {
		if code != ErrorCodeParseError {
			want[code] = true
		}
	}

	for code := range want {
		if !produced[code] {
			t.Errorf("no invalid fixture produces %q; add fixtures/batch/invalid/%s.json",
				code, strings.TrimPrefix(code, "analytics."))
		}
	}
	for code := range produced {
		if !want[code] {
			t.Errorf("the invalid corpus produces %q, which is outside the closed vocabulary", code)
		}
	}
}

// TestConformance_InvalidFixtureNamesMatchCodes pins the naming convention the
// TypeScript conformance test relies on: the file stem is the code the fixture
// exists to trigger. Without it a fixture could drift into triggering a
// different rule and still look green, and the TS side — which asserts the drop
// reason equals the stem — would fail instead with no local explanation.
//
// A fixture may report more than its own code: several mutations are a single
// edit to a valid page view, and putting an action property on a page view is
// both misplaced and, by design, still checked for shape.
func TestConformance_InvalidFixtureNamesMatchCodes(t *testing.T) {
	for _, path := range fixturePaths(t, "invalid") {
		stem := strings.TrimSuffix(filepath.Base(path), ".json")
		want := "analytics." + stem
		t.Run(stem, func(t *testing.T) {
			if !ValidErrorCodes[want] {
				t.Fatalf("fixture %s names %q, which is not in ValidErrorCodes", path, want)
			}
			codes := codesOf(t, path)
			for _, code := range codes {
				if code == want {
					return
				}
			}
			t.Errorf("fixture %s produced %v, want at least %q", path, codes, want)
		})
	}
}

// codesOf returns the sorted, deduplicated diagnostic codes a fixture produces.
func codesOf(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, diags := ParseAndValidateBatch(data)
	if len(diags) == 0 {
		t.Fatalf("invalid fixture %s produced no diagnostics", path)
	}
	seen := make(map[string]bool, len(diags))
	codes := make([]string, 0, len(diags))
	for _, d := range diags {
		if !seen[d.Code] {
			seen[d.Code] = true
			codes = append(codes, d.Code)
		}
	}
	sort.Strings(codes)
	return codes
}
