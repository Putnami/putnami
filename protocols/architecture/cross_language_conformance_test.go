package architecture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The cross-language conformance corpus, Go side.
//
// fixtures/conformance/contract-validation.json states which import contracts
// this protocol refuses and where. Both this runner and the TypeScript mirror in
// `@putnami/application` read the same file, so a rule that exists in one
// language and not the other fails here or there rather than shipping as two
// different answers to "is this contract valid".
//
// The corpus pins (code, field) pairs and deliberately not messages: a message
// is written for a person reading a terminal and each language phrases it
// idiomatically. What must agree is WHICH clause refused.

// conformanceFixtureDir is where both language runners read the corpus from.
const conformanceFixtureDir = "fixtures/conformance"

// conformanceCase is one contract built from a named base by a shallow
// top-level merge. Shallow on purpose — a deep merge would need identical
// semantics in every language, which is one more thing to get subtly different.
type conformanceCase struct {
	Name        string                     `json:"name"`
	Base        string                     `json:"base"`
	Set         map[string]json.RawMessage `json:"set"`
	Remove      []string                   `json:"remove"`
	Diagnostics []conformanceDiagnostic    `json:"diagnostics"`
}

// conformanceDiagnostic is the part of a finding both languages must agree on.
type conformanceDiagnostic struct {
	Code  string `json:"code"`
	Field string `json:"field"`
}

type conformanceCorpus struct {
	ProtocolVersion int                        `json:"protocolVersion"`
	Bases           map[string]json.RawMessage `json:"bases"`
	Cases           []conformanceCase          `json:"cases"`
}

// TestContractValidationConformance runs every corpus case through the
// protocol's own validator.
func TestContractValidationConformance(t *testing.T) {
	corpus := loadConformanceCorpus(t, "contract-validation.json")
	if len(corpus.Cases) == 0 {
		t.Fatal("the conformance corpus is empty; a corpus that asserts nothing is worse than none")
	}
	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			manifest := &Manifest{
				ProtocolVersion: ProtocolVersion,
				Domain:          "consumer",
				Owner:           "consumer",
				Projects:        []string{},
				Exports:         []Export{},
				Imports:         []Import{buildConformanceImport(t, corpus, testCase)},
			}
			got := reduceConformanceDiagnostics(ValidateManifest(manifest))
			want := append([]conformanceDiagnostic(nil), testCase.Diagnostics...)
			sortConformanceDiagnostics(got)
			sortConformanceDiagnostics(want)
			if len(got) != len(want) {
				t.Fatalf("diagnostics = %+v, want %+v", got, want)
			}
			for index := range got {
				if got[index] != want[index] {
					t.Errorf("diagnostics[%d] = %+v, want %+v", index, got[index], want[index])
				}
			}
		})
	}
}

// buildConformanceImport applies the case's shallow merge to its named base.
func buildConformanceImport(t *testing.T, corpus conformanceCorpus, testCase conformanceCase) Import {
	t.Helper()
	raw, found := corpus.Bases[testCase.Base]
	if !found {
		t.Fatalf("case %q names base %q, which the corpus does not define", testCase.Name, testCase.Base)
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("base %q does not parse: %v", testCase.Base, err)
	}
	for member, value := range testCase.Set {
		document[member] = value
	}
	for _, member := range testCase.Remove {
		delete(document, member)
	}
	merged, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("case %q does not re-encode: %v", testCase.Name, err)
	}
	var contract Import
	if err := json.Unmarshal(merged, &contract); err != nil {
		t.Fatalf("case %q does not decode as an import: %v", testCase.Name, err)
	}
	return contract
}

func reduceConformanceDiagnostics(diagnostics []diag.Diagnostic) []conformanceDiagnostic {
	reduced := make([]conformanceDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diag.Errors(diagnostics) {
		reduced = append(reduced, conformanceDiagnostic{Code: diagnostic.Code, Field: diagnostic.Field})
	}
	return reduced
}

func sortConformanceDiagnostics(diagnostics []conformanceDiagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Field != diagnostics[j].Field {
			return diagnostics[i].Field < diagnostics[j].Field
		}
		return diagnostics[i].Code < diagnostics[j].Code
	})
}

func loadConformanceCorpus(t *testing.T, name string) conformanceCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(conformanceFixtureDir, name))
	if err != nil {
		t.Fatalf("read the conformance corpus: %v", err)
	}
	var corpus conformanceCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("the conformance corpus does not parse: %v", err)
	}
	return corpus
}
