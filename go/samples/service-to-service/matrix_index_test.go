package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The machine index (test-scenarios.json) is a claim about this repository, and
// a claim nothing checks rots. This test is what makes it evidence: every
// scenario names a test that exists, every cell of the matrix appears for every
// consumer and every family, and every combination that is not proven says why
// — a protocol rule for `n/a`, a reason for `missing`.

type matrixIndex struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Matrix          string `json:"matrix"`
	Cells           []struct {
		ID string `json:"id"`
	} `json:"cells"`
	Families []struct {
		ID string `json:"id"`
	} `json:"families"`
	Consumers []struct {
		Target string `json:"target"`
	} `json:"consumers"`
	Scenarios []struct {
		ID       string `json:"id"`
		Target   string `json:"target"`
		Cell     string `json:"cell"`
		Family   string `json:"family"`
		Test     string `json:"test"`
		Result   string `json:"result"`
		Auth     string `json:"auth"`
		Policy   string `json:"policy"`
		Encoding string `json:"encoding"`
	} `json:"scenarios"`
	Coverage []struct {
		Target    string   `json:"target"`
		Cell      string   `json:"cell"`
		Family    string   `json:"family"`
		Status    string   `json:"status"`
		Scenarios []string `json:"scenarios"`
		Rule      string   `json:"rule"`
		Note      string   `json:"note"`
	} `json:"coverage"`
}

func readMatrixIndex(t *testing.T) matrixIndex {
	t.Helper()
	raw, err := os.ReadFile("test-scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var index matrixIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

// declaredTests lists the test symbols a file declares: `func TestX` for Go,
// `it('…')` for a bun test file.
func declaredTests(t *testing.T, path string) map[string]bool {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the index names %s, which does not exist: %v", path, err)
	}
	declared := map[string]bool{}
	if strings.HasSuffix(path, ".go") {
		for _, match := range regexp.MustCompile(`(?m)^func (Test\w+)`).FindAllStringSubmatch(string(content), -1) {
			declared[match[1]] = true
		}
		return declared
	}
	// A bun test names itself with either quote style, through `it` or through
	// `specTest` when it also publishes a declared check. The index quotes the
	// name exactly as the file does.
	for _, match := range regexp.MustCompile(`(?m)^\s*(?:it|specTest)\(\s*(?:'([^']+)'|"([^"]+)")`).FindAllStringSubmatch(string(content), -1) {
		declared[match[1]+match[2]] = true
	}
	return declared
}

func TestMatrixIndexNamesTestsThatExist(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-matrix-index-is-checkable", "the-index-names-tests-that-exist")
	index := readMatrixIndex(t)
	if index.ProtocolVersion != 2 || index.Matrix != "client-matrix" {
		t.Fatalf("index header = %d/%q", index.ProtocolVersion, index.Matrix)
	}

	byFile := map[string]map[string]bool{}
	for _, scenario := range index.Scenarios {
		file, symbol, ok := strings.Cut(scenario.Test, ":")
		if !ok {
			t.Errorf("scenario %s: test %q is not <file>:<test>", scenario.ID, scenario.Test)
			continue
		}
		if _, seen := byFile[file]; !seen {
			byFile[file] = declaredTests(t, filepath.FromSlash(file))
		}
		if !byFile[file][symbol] {
			t.Errorf("scenario %s names %s, which %s does not declare", scenario.ID, symbol, file)
		}
		if scenario.Result != "pass" {
			t.Errorf("scenario %s has result %q; a scenario row exists only when its test passes", scenario.ID, scenario.Result)
		}
		if scenario.Auth == "" || scenario.Policy == "" || scenario.Encoding == "" {
			t.Errorf("scenario %s does not state its auth, policy and encoding", scenario.ID)
		}
	}
}

func TestMatrixIndexCoversEveryCellAndFamily(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-matrix-index-is-checkable", "the-index-states-every-cell-and-family-for-every-consumer")
	index := readMatrixIndex(t)
	scenarios := map[string]bool{}
	for _, scenario := range index.Scenarios {
		if scenarios[scenario.ID] {
			t.Errorf("scenario id %s appears twice", scenario.ID)
		}
		scenarios[scenario.ID] = true
	}

	seen := map[string]bool{}
	for _, entry := range index.Coverage {
		key := fmt.Sprintf("%s/%s/%s", entry.Target, entry.Cell, entry.Family)
		if seen[key] {
			t.Errorf("coverage entry %s appears twice", key)
		}
		seen[key] = true
		switch entry.Status {
		case "pass":
			if len(entry.Scenarios) == 0 {
				t.Errorf("%s is marked pass with no scenario", key)
			}
			for _, id := range entry.Scenarios {
				if !scenarios[id] {
					t.Errorf("%s names unknown scenario %s", key, id)
				}
			}
		case "n/a":
			// A combination is inapplicable because a protocol says so, never
			// because nothing implements it.
			if entry.Rule == "" {
				t.Errorf("%s is marked n/a without the rule that makes it so", key)
			}
		case "missing":
			if entry.Note == "" {
				t.Errorf("%s is marked missing without a reason", key)
			}
		default:
			t.Errorf("%s has unknown status %q", key, entry.Status)
		}
	}

	for _, consumer := range index.Consumers {
		for _, cell := range index.Cells {
			for _, family := range index.Families {
				key := fmt.Sprintf("%s/%s/%s", consumer.Target, cell.ID, family.ID)
				if !seen[key] {
					t.Errorf("the index states nothing about %s", key)
				}
			}
		}
	}
}
