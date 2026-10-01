package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The v2 result contract's cross-language corpus.
//
// conformance/manifest.json is ONE file read by both runtimes: this test and
// @putnami/cli-protocol's result-v2-conformance.test.ts. Each case pins the
// EXACT violation set (codes and paths, sorted) its document produces, so a
// validator that drifts in either language — a different code, a different
// path, one violation too many or too few — fails that language's test against
// a corpus the other language still passes. That is the whole point: the corpus
// turns Go/TypeScript shape drift into a test failure instead of a wire that
// quietly means two things.
//
// A contract change lands as ONE commit touching three places: this corpus, the
// Go validator, and the TypeScript validator. Never loosen a case to make one
// runtime pass.

const (
	conformanceManifestPath = "conformance/manifest.json"
	conformancePackPath     = "conformance/pack.json"
)

type conformanceCase struct {
	ID       string       `json:"id"`
	Document DocumentKind `json:"document"`
	Expect   string       `json:"expect"`
	Summary  string       `json:"summary"`
	// Value is the case document, embedded in the corpus so both runtimes read
	// the same bytes.
	Value json.RawMessage `json:"value"`
	// Raw carries documents that are not valid JSON values (a truncated line,
	// trailing data) and therefore cannot be embedded as Value.
	Raw string `json:"raw"`
	// Violations is the exact expected result, sorted by path then code.
	Violations []Violation `json:"violations"`
}

type conformanceManifest struct {
	Protocol        string                  `json:"protocol"`
	Suite           string                  `json:"suite"`
	ProtocolVersion int                     `json:"protocolVersion"`
	Documents       []string                `json:"documents"`
	Cases           []conformanceCase       `json:"cases"`
	StreamCases     []conformanceStreamCase `json:"streamCases"`
}

type conformanceStreamReplacement struct {
	Token string `json:"token"`
	Text  string `json:"text"`
	Count int    `json:"count"`
}

type conformanceStreamCase struct {
	ID           string                         `json:"id"`
	Expect       string                         `json:"expect"`
	Summary      string                         `json:"summary"`
	Live         string                         `json:"live"`
	Artifact     string                         `json:"artifact"`
	Replacements []conformanceStreamReplacement `json:"replacements"`
	Violations   []Violation                    `json:"violations"`
}

func (c conformanceStreamCase) streams(t *testing.T) ([]byte, []byte) {
	t.Helper()
	live, artifact := c.Live, c.Artifact
	for _, replacement := range c.Replacements {
		if replacement.Token == "" || replacement.Count < 0 {
			t.Fatalf("stream case %s has invalid replacement", c.ID)
		}
		value := strings.Repeat(replacement.Text, replacement.Count)
		live = strings.ReplaceAll(live, replacement.Token, value)
		artifact = strings.ReplaceAll(artifact, replacement.Token, value)
	}
	return []byte(live), []byte(artifact)
}

func loadConformanceManifest(t *testing.T) conformanceManifest {
	t.Helper()
	data, err := os.ReadFile(conformanceManifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", conformanceManifestPath, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var manifest conformanceManifest
	if err := dec.Decode(&manifest); err != nil {
		t.Fatalf("parse %s (strict): %v", conformanceManifestPath, err)
	}
	if len(manifest.Cases) == 0 {
		t.Fatalf("%s declares no cases", conformanceManifestPath)
	}
	return manifest
}

// document returns the bytes to validate for a case: the embedded value's own
// bytes, or the raw text for the not-valid-JSON cases.
func (c conformanceCase) document(t *testing.T) []byte {
	t.Helper()
	switch {
	case len(c.Value) > 0 && c.Raw != "":
		t.Fatalf("case %s declares both value and raw", c.ID)
	case len(c.Value) > 0:
		return c.Value
	case c.Raw != "":
		return []byte(c.Raw)
	default:
		t.Fatalf("case %s declares neither value nor raw", c.ID)
	}
	return nil
}

// TestConformanceCorpus runs every case and requires the exact violation set.
func TestConformanceCorpus(t *testing.T) {
	manifest := loadConformanceManifest(t)
	for _, testCase := range manifest.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			if !ValidDocumentKinds[testCase.Document] {
				t.Fatalf("case %s names unknown document kind %q", testCase.ID, testCase.Document)
			}
			want := expectedViolations(t, testCase)
			got := ValidateDocument(testCase.Document, testCase.document(t))
			if len(got) == 0 {
				got = nil
			}
			if reflect.DeepEqual(got, want) {
				return
			}
			t.Errorf("case %s (%s) violations mismatch\n  got:  %v\n  want: %v\n\n"+
				"A contract change lands as one commit touching THREE places: %s, the Go "+
				"validator (result_v2_rules.go), and the TypeScript validator "+
				"(typescript/framework/cli-protocol/src/result-v2.ts). This failure means "+
				"the change forgot one of them — update the fixture AND the other runtime.",
				testCase.ID, testCase.Summary, got, want, conformanceManifestPath)
		})
	}
	for _, testCase := range manifest.StreamCases {
		t.Run(testCase.ID, func(t *testing.T) {
			live, artifact := testCase.streams(t)
			got := ValidateSessionStream(live, artifact)
			if len(got) == 0 {
				got = nil
			}
			want := expectedStreamViolations(t, testCase)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("stream case %s (%s) violations mismatch: got %v, want %v", testCase.ID, testCase.Summary, got, want)
			}
		})
	}
}

func expectedStreamViolations(t *testing.T, testCase conformanceStreamCase) []Violation {
	t.Helper()
	switch testCase.Expect {
	case "accept":
		if len(testCase.Violations) > 0 {
			t.Fatalf("stream case %s expects accept but declares violations", testCase.ID)
		}
		return nil
	case "reject":
		if len(testCase.Violations) == 0 {
			t.Fatalf("stream case %s expects reject but declares no violations", testCase.ID)
		}
		for i, violation := range testCase.Violations {
			if !ValidViolationCodes[violation.Code] {
				t.Fatalf("stream case %s expects unknown violation %q", testCase.ID, violation.Code)
			}
			if i > 0 {
				previous := testCase.Violations[i-1]
				if previous.Path > violation.Path || (previous.Path == violation.Path && previous.Code > violation.Code) {
					t.Fatalf("stream case %s declares violations out of order", testCase.ID)
				}
			}
		}
		return testCase.Violations
	default:
		t.Fatalf("stream case %s has unknown expect %q", testCase.ID, testCase.Expect)
		return nil
	}
}

// expectedViolations validates the case's own declaration and returns the
// violations it expects: none for "accept", the declared set for "reject".
func expectedViolations(t *testing.T, testCase conformanceCase) []Violation {
	t.Helper()
	switch testCase.Expect {
	case "accept":
		if len(testCase.Violations) > 0 {
			t.Fatalf("case %s expects accept but declares violations", testCase.ID)
		}
		return nil
	case "reject":
		if len(testCase.Violations) == 0 {
			t.Fatalf("case %s expects reject but declares no violations", testCase.ID)
		}
		assertSortedViolations(t, testCase)
		return testCase.Violations
	default:
		t.Fatalf("case %s has unknown expect %q", testCase.ID, testCase.Expect)
		return nil
	}
}

// assertSortedViolations keeps the corpus in the contract's deterministic order
// (path, then code) so the expectation is comparable as-is in both runtimes.
func assertSortedViolations(t *testing.T, testCase conformanceCase) {
	t.Helper()
	for i, violation := range testCase.Violations {
		if !ValidViolationCodes[violation.Code] {
			t.Fatalf("case %s expects violation code %q outside ValidViolationCodes", testCase.ID, violation.Code)
		}
		if i == 0 {
			continue
		}
		previous := testCase.Violations[i-1]
		if previous.Path > violation.Path ||
			(previous.Path == violation.Path && previous.Code > violation.Code) {
			t.Fatalf("case %s declares violations out of order at index %d (sort by path, then code)", testCase.ID, i)
		}
	}
}

// TestConformanceCorpus_Complete ratchets coverage: every document kind and
// every violation code must be exercised, so a code added without a case — or a
// case deleted — is a test failure rather than an untested contract clause.
func TestConformanceCorpus_Complete(t *testing.T) {
	manifest := loadConformanceManifest(t)

	documents := map[DocumentKind]bool{}
	codes := map[string]bool{}
	accepted, rejected := 0, 0
	for _, testCase := range manifest.Cases {
		documents[testCase.Document] = true
		for _, violation := range testCase.Violations {
			codes[violation.Code] = true
		}
		if testCase.Expect == "accept" {
			accepted++
		} else {
			rejected++
		}
	}
	for _, testCase := range manifest.StreamCases {
		for _, violation := range testCase.Violations {
			codes[violation.Code] = true
		}
		if testCase.Expect == "accept" {
			accepted++
		} else {
			rejected++
		}
	}

	for kind := range ValidDocumentKinds {
		if !documents[kind] {
			t.Errorf("no corpus case covers document kind %q", kind)
		}
	}
	for code := range ValidViolationCodes {
		if !codes[code] {
			t.Errorf("no corpus case covers violation code %q", code)
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Errorf("corpus needs both accepted and rejected cases (accept=%d reject=%d)", accepted, rejected)
	}
}

// TestConformanceCorpus_UniqueIDs keeps case ids addressable across runtimes.
func TestConformanceCorpus_UniqueIDs(t *testing.T) {
	manifest := loadConformanceManifest(t)
	seen := map[string]bool{}
	for _, testCase := range manifest.Cases {
		if seen[testCase.ID] {
			t.Errorf("duplicate corpus case id %q", testCase.ID)
		}
		seen[testCase.ID] = true
		if testCase.Summary == "" {
			t.Errorf("corpus case %q has no summary", testCase.ID)
		}
	}
	for _, testCase := range manifest.StreamCases {
		if seen[testCase.ID] {
			t.Errorf("duplicate corpus case id %q", testCase.ID)
		}
		seen[testCase.ID] = true
		if testCase.Summary == "" {
			t.Errorf("corpus stream case %q has no summary", testCase.ID)
		}
	}
}

// TestConformanceCorpus_Header pins the corpus header against the Go constants,
// including the document vocabulary the TypeScript runner also switches on.
func TestConformanceCorpus_Header(t *testing.T) {
	manifest := loadConformanceManifest(t)
	if manifest.Protocol != "putnami.cli.result.v2" {
		t.Errorf("corpus protocol = %q, want %q", manifest.Protocol, "putnami.cli.result.v2")
	}
	if manifest.ProtocolVersion != ResultProtocolVersion {
		t.Errorf("corpus protocolVersion = %d, want %d", manifest.ProtocolVersion, ResultProtocolVersion)
	}
	declared := map[DocumentKind]bool{}
	for _, name := range manifest.Documents {
		declared[DocumentKind(name)] = true
	}
	if !reflect.DeepEqual(declared, ValidDocumentKinds) {
		t.Errorf("corpus documents = %v, want the ValidDocumentKinds vocabulary %v", manifest.Documents, ValidDocumentKinds)
	}
}

// TestConformancePack pins the committed pack-manifest convention: a
// stable id, the languages both runners implement, and a corpus pointer that
// resolves.
func TestConformancePack(t *testing.T) {
	data, err := os.ReadFile(conformancePackPath)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var pack struct {
		ID              string   `json:"id"`
		Corpus          string   `json:"corpus"`
		CapabilityKinds []string `json:"capabilityKinds"`
		Languages       []string `json:"languages"`
	}
	if err := dec.Decode(&pack); err != nil {
		t.Fatalf("parse pack.json (strict): %v", err)
	}
	if pack.ID != "putnami.cli.result.conformance" {
		t.Errorf("pack id = %q, want %q", pack.ID, "putnami.cli.result.conformance")
	}
	if want := []string{"go", "typescript"}; !reflect.DeepEqual(pack.Languages, want) {
		t.Errorf("pack languages = %v, want %v", pack.Languages, want)
	}
	if len(pack.CapabilityKinds) != 0 {
		t.Errorf("pack capabilityKinds = %v, want empty: this pack certifies a machine contract, not a capability kind", pack.CapabilityKinds)
	}
	if pack.Corpus != "manifest.json" {
		t.Fatalf("pack corpus = %q, want %q", pack.Corpus, "manifest.json")
	}
	if _, err := os.ReadFile(filepath.Join("conformance", pack.Corpus)); err != nil {
		t.Fatalf("pack corpus %q does not resolve: %v", pack.Corpus, err)
	}
}
