package collaboration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The fixture corpus: fixtures/valid and fixtures/invalid hold one document
// per file, named "<kind>.<...>.json". The kind decides the parser:
//
//	bindings.<case>                        ParseBindings
//	envelope.<case>                        ParseEnvelope
//	response.<case>                        ParseResponse
//	request.<contract>.<operation>.<case>  ParseRequest(contract, 1, operation)
//	result.<contract>.<operation>.<case>   ParseResult(contract, 1, operation)
//
// fixtures/expectations.json lists every valid file and pins the exact
// distinct codes each invalid file produces.

type expectations struct {
	Valid   []string            `json:"valid"`
	Invalid map[string][]string `json:"invalid"`
}

func readExpectations(t *testing.T) expectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "expectations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp expectations
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	return exp
}

// parseFixture routes one fixture to its parser by file name.
func parseFixture(t *testing.T, name string, data []byte) []diag.Diagnostic {
	t.Helper()
	parts := strings.Split(strings.TrimSuffix(name, ".json"), ".")
	switch parts[0] {
	case "bindings":
		_, diags := ParseBindings(data)
		return diags
	case "envelope":
		_, diags := ParseEnvelope(data)
		return diags
	case "response":
		_, diags := ParseResponse(data)
		return diags
	case "request":
		if len(parts) < 4 {
			t.Fatalf("fixture %s must be request.<contract>.<operation>.<case>.json", name)
		}
		_, diags := ParseRequest(parts[1], 1, parts[2], data)
		return diags
	case "result":
		if len(parts) < 4 {
			t.Fatalf("fixture %s must be result.<contract>.<operation>.<case>.json", name)
		}
		_, diags := ParseResult(parts[1], 1, parts[2], data)
		return diags
	}
	t.Fatalf("fixture %s has no known kind", name)
	return nil
}

func distinctCodes(diags []diag.Diagnostic) []string {
	seen := map[string]bool{}
	for _, d := range diag.Errors(diags) {
		seen[d.Code] = true
	}
	codes := make([]string, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func listFixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("fixtures", dir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func TestConformance_ValidFixturesParseClean(t *testing.T) {
	exp := readExpectations(t)
	files := listFixtures(t, "valid")
	if !slices.Equal(files, exp.Valid) {
		t.Fatalf("fixtures/valid holds %v, expectations list %v", files, exp.Valid)
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
			if err != nil {
				t.Fatal(err)
			}
			if diags := parseFixture(t, name, data); diag.HasErrors(diags) {
				t.Fatalf("valid fixture rejected: %v", diags)
			}
		})
	}
}

func TestConformance_InvalidFixturesProduceTheirCodes(t *testing.T) {
	exp := readExpectations(t)
	files := listFixtures(t, "invalid")
	listed := make([]string, 0, len(exp.Invalid))
	for name := range exp.Invalid {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	if !slices.Equal(files, listed) {
		t.Fatalf("fixtures/invalid holds %v, expectations list %v", files, listed)
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			got := distinctCodes(parseFixture(t, name, data))
			want := append([]string(nil), exp.Invalid[name]...)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Fatalf("codes = %v, want %v", got, want)
			}
			for _, code := range got {
				if !ValidErrorCodes[code] {
					t.Errorf("code %q is outside the closed taxonomy", code)
				}
			}
		})
	}
}

// TestConformance_ValidDocumentsRoundTrip re-encodes every valid request and
// result and parses the encoding again: what the orchestrator forwards after
// decoding is exactly what the contract accepts.
func TestConformance_ValidDocumentsRoundTrip(t *testing.T) {
	for _, name := range listFixtures(t, "valid") {
		parts := strings.Split(strings.TrimSuffix(name, ".json"), ".")
		if parts[0] != "request" && parts[0] != "result" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
			if err != nil {
				t.Fatal(err)
			}
			parse := ParseRequest
			if parts[0] == "result" {
				parse = ParseResult
			}
			first, diags := parse(parts[1], 1, parts[2], data)
			if diags != nil {
				t.Fatalf("parse: %v", diags)
			}
			encoded, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			second, diags := parse(parts[1], 1, parts[2], encoded)
			if diags != nil {
				t.Fatalf("re-parse of %s: %v", encoded, diags)
			}
			again, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(encoded) {
				t.Fatalf("encoding is not stable:\n%s\n%s", encoded, again)
			}
		})
	}
}

func TestConformance_EveryCodeIsExercised(t *testing.T) {
	exp := readExpectations(t)
	exercised := map[string]bool{}
	for _, codes := range exp.Invalid {
		for _, code := range codes {
			exercised[code] = true
		}
	}
	// Codes whose triggers need a Go value rather than a document are proven
	// by unit tests instead: an oversized document, a malformed provider
	// declaration, a timestamp and a trailing-data document.
	provenElsewhere := map[string]bool{
		ErrorCodeParseError:         true,
		ErrorCodeTooLarge:           true,
		ErrorCodeInvalidTimestamp:   true,
		ErrorCodeInvalidDeclaration: true,
	}
	for code := range ValidErrorCodes {
		if !exercised[code] && !provenElsewhere[code] {
			t.Errorf("code %s is exercised by no invalid fixture", code)
		}
	}
}
