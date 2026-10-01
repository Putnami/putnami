package clientcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// closedIntegerFormats is the set ADR 0004 fixes. An integer schema outside it,
// or without a format at all, is a declaration this contract cannot emit a
// lossless client for.
var closedIntegerFormats = map[string]bool{"int32": true, "int64": true, "uint32": true, "uint64": true}

// TestCorpusUsesTheFrameworkErrorVocabulary guards ADR 0003 from the corpus
// side. The `errors.` prefix the fixtures once carried is written by no runtime
// in either language: a provider that declared it would promise its consumers a
// code its own server never sends.
func TestCorpusUsesTheFrameworkErrorVocabulary(t *testing.T) {
	forEachCorpusFile(t, func(t *testing.T, path string, raw []byte) {
		for _, prefixed := range []string{`"errors.`, `"Errors.`} {
			if strings.Contains(string(raw), prefixed) {
				t.Errorf("%s declares an error code prefixed with %q; ADR 0003 fixes the vocabulary on go/framework/errors", path, prefixed)
			}
		}
	})
	document := parseCorpusDocument(t, "fixtures/openapi/valid/full.openapi.json")
	found := map[string]bool{}
	for _, item := range document.paths {
		for _, declared := range item.errors {
			found[declared] = true
		}
	}
	for _, want := range []string{"not_found", "unavailable"} {
		if !found[want] {
			t.Errorf("the corpus no longer declares the %q error code; the vocabulary guard would pass vacuously", want)
		}
	}
}

// TestCorpusDeclaresIntegerWidthExactly guards ADR 0004 from the corpus side:
// every integer schema names its width and carries exact bounds, so no reader
// has to guess and no emitter has to default.
func TestCorpusDeclaresIntegerWidthExactly(t *testing.T) {
	integers := 0
	forEachCorpusFile(t, func(t *testing.T, path string, raw []byte) {
		var document any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("%s is not valid JSON: %v", path, err)
		}
		walkJSON(document, func(node map[string]any) {
			if node["type"] != "integer" {
				return
			}
			integers++
			format, _ := node["format"].(string)
			if !closedIntegerFormats[format] {
				t.Errorf("%s declares an integer with format %q; ADR 0004 requires int32, int64, uint32 or uint64", path, format)
			}
			if _, ok := node["minimum"]; !ok {
				t.Errorf("%s declares an integer without an exact minimum", path)
			}
			if _, ok := node["maximum"]; !ok {
				t.Errorf("%s declares an integer without an exact maximum", path)
			}
		})
	})
	if integers == 0 {
		t.Fatal("the corpus contains no integer schema, so this guard would pass vacuously")
	}
}

// TestCorpusDeclaresAHandshakeBudget pins the field ADR 0005 adds: the corpus
// exercises it, so a reader that drops it fails here rather than silently
// folding the handshake back into the attempt budget.
func TestCorpusDeclaresAHandshakeBudget(t *testing.T) {
	data, err := os.ReadFile("fixtures/openapi/valid/full.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	document, diags := ParseAndValidateDocument(extractExtension(t, data))
	if len(diags) > 0 {
		t.Fatalf("the valid corpus document produced diagnostics: %v", diags)
	}
	stream := document.Defaults.Resilience.Stream
	if stream == nil || stream.HandshakeTimeoutMs == nil {
		t.Fatal("the corpus does not declare resilience.stream.handshakeTimeoutMs")
	}
	if *stream.HandshakeTimeoutMs <= 0 {
		t.Fatalf("handshakeTimeoutMs = %d, want a positive budget", *stream.HandshakeTimeoutMs)
	}
}

func extractExtension(t *testing.T, raw []byte) []byte {
	t.Helper()
	var document struct {
		Extension json.RawMessage `json:"x-putnami-client"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document.Extension
}

// corpusDocument is the narrow view the error-vocabulary guard needs; the full
// projection is already proved by the conformance corpus.
type corpusDocument struct {
	paths []struct{ errors []string }
}

func parseCorpusDocument(t *testing.T, path string) corpusDocument {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document corpusDocument
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	walkJSON(decoded, func(node map[string]any) {
		declared, ok := node["errors"].([]any)
		if !ok {
			return
		}
		var codes []string
		for _, item := range declared {
			if entry, ok := item.(map[string]any); ok {
				if code, ok := entry["code"].(string); ok {
					codes = append(codes, code)
				}
			}
		}
		document.paths = append(document.paths, struct{ errors []string }{errors: codes})
	})
	return document
}

func forEachCorpusFile(t *testing.T, check func(*testing.T, string, []byte)) {
	t.Helper()
	roots := []string{"fixtures/ir", "fixtures/openapi/valid", "fixtures/openapi/invalid"}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(root, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			check(t, path, raw)
		}
	}
}

func walkJSON(node any, visit func(map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range value {
			walkJSON(child, visit)
		}
	}
}
