package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// frozenV1Schema is the schema version 1 was promised with (ADR 0005).
const frozenV1Schema = "schema.json"

// frozenV1Digests pins every file of fixtures/v1: the frozen schema and the
// frozen version 1 documents. An addition to version 1 changes none of them;
// a change to this map or to a file under fixtures/v1 opens version 2.
var frozenV1Digests = map[string]string{
	"architecture-pull-request.json": "bdf20d7d11815c87281b3173ba71867276cf47a87b15857c9e8dd89287686ffd",
	"decisions-pull-request.json":    "1574588ad004e4377a0dee02fb56161ccd30870b3d9cea35d85f56d54099b0de",
	"declared-pull-request.json":     "30ab989a7f3b385314e90b28a06bd74e948e164f3bcefda8b7e9355b8781a99c",
	"dependency-cycle.json":          "a742168e06931a531ab1060f162ef1014dbfb865eef582cddfb42f8faf196a78",
	"generic-minimal.json":           "cf5544e538fea4bf2b5612039f60402aa50c7410e1098f25311917afa9bab55c",
	"sbom-pull-request.json":         "67709a147f947a450ccc5c13129f969d767f86bcb19261f2b40a6e40928a4198",
	"schema.json":                    "4ee77c7c5ca259d43a19bce21b8fbe70c160315895b0dbc09917ba41d3f1522a",
	"symfony-merge-request.json":     "71f5ecd1c778371357a1837cb5814ca848b8c0a0b8de6541ac366bce994e8e3a",
	"test-cases-pull-request.json":   "af47e14e3d7097b105bc81a1fe3b0ed8bd7f247aa4f1837d83172894e37d8748",
	"turbo-pull-request.json":        "d5489f7f01f86e81aaa58ee081cfdc019120a28d82af24c01f0776b10042bf39",
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

func readFrozenV1(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeSchema(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestFrozenV1_FilesKeepTheirDigests(t *testing.T) {
	spectest.Proves(t, feature, "version-1-changes-only-by-addition", "the-frozen-v1-files-keep-their-digests")
	files := frozenV1Files(t)
	pinned := make([]string, 0, len(frozenV1Digests))
	for name := range frozenV1Digests {
		pinned = append(pinned, name)
	}
	sort.Strings(pinned)
	if !slices.Equal(files, pinned) {
		t.Fatalf("fixtures/v1 holds %v, the pins list %v: a frozen version 1 file is never added, removed or renamed (ADR 0005)", files, pinned)
	}
	if !slices.Contains(files, frozenV1Schema) || len(files) < 2 {
		t.Fatalf("fixtures/v1 must hold %s and at least one frozen document: %v", frozenV1Schema, files)
	}
	for _, name := range files {
		sum := sha256.Sum256(readFrozenV1(t, name))
		if got := hex.EncodeToString(sum[:]); got != frozenV1Digests[name] {
			t.Errorf("fixtures/v1/%s changed (sha256 %s): version 1 changes only by addition; a break opens version 2 instead (ADR 0005)", name, got)
		}
	}
}

// frozenV1ReaderBounds pins each bound of the strict reader as version 1 was
// promised with it. A bound may grow, never shrink: a smaller one refuses a
// document version 1 accepted.
var frozenV1ReaderBounds = map[string][2]int{
	"MaxChangedFiles":            {MaxChangedFiles, 20000},
	"MaxComponents":              {MaxComponents, 5000},
	"MaxDependencyChanges":       {MaxDependencyChanges, 10000},
	"MaxChangedLineCoverageFile": {MaxChangedLineCoverageFile, 20000},
	"MaxDiagnostics":             {MaxDiagnostics, 500},
	"MaxFailingCases":            {MaxFailingCases, 50},
	"MaxTestCases":               {MaxTestCases, 10000},
	"MaxDocumentTestCases":       {MaxDocumentTestCases, 50000},
	"MaxUncoveredLines":          {MaxUncoveredLines, 200},
	"MaxDecisions":               {MaxDecisions, 1000},
	"MaxDeclarations":            {MaxDeclarations, 1024},
	"MaxDeclarationBytes":        {MaxDeclarationBytes, 256 * 1024},
	"MaxSpecVerificationBytes":   {MaxSpecVerificationBytes, 512 * 1024},
	"MaxDecisionScope":           {MaxDecisionScope, 100},
	"MaxGovernedFiles":           {MaxGovernedFiles, 1000},
	"MaxArchitectureRules":       {MaxArchitectureRules, 1000},
	"MaxViolations":              {MaxViolations, 50},
	"MaxLicenses":                {MaxLicenses, 20},
	"MaxVulnerabilities":         {MaxVulnerabilities, 200},
	"MaxEdges":                   {MaxEdges, 100000},
	"MaxStringBytes":             {MaxStringBytes, 4096},
}

func TestFrozenV1_TheReaderBoundsOnlyGrow(t *testing.T) {
	spectest.Proves(t, feature, "version-1-changes-only-by-addition", "the-reader-bounds-only-grow")
	for _, name := range sortedKeys(frozenV1ReaderBounds) {
		if bound := frozenV1ReaderBounds[name]; bound[0] < bound[1] {
			t.Errorf("%s is %d, below the %d version 1 was promised with: version 1 changes only by addition (ADR 0005)", name, bound[0], bound[1])
		}
	}
}

func TestFrozenV1_EveryDocumentValidates(t *testing.T) {
	spectest.Proves(t, feature, "version-1-changes-only-by-addition", "every-frozen-v1-document-validates")
	for _, name := range frozenV1Files(t) {
		if name == frozenV1Schema {
			continue
		}
		if _, diagnostics := ParseAndValidate(readFrozenV1(t, name)); diag.HasErrors(diagnostics) {
			t.Errorf("frozen version 1 document %s is refused: %v; version 1 changes only by addition (ADR 0005)", name, diagnostics)
		}
	}
}

func TestFrozenV1_TheSchemaExtendsTheFrozenSchema(t *testing.T) {
	spectest.Proves(t, feature, "version-1-changes-only-by-addition", "the-schema-extends-the-frozen-v1-schema")
	current, err := os.ReadFile(filepath.Join("schemas", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, found := range schemaBreaks(decodeSchema(t, readFrozenV1(t, frozenV1Schema)), decodeSchema(t, current)) {
		t.Errorf("schemas/evidence.json breaks version 1: %s; version 1 changes only by addition, a break opens version 2 (ADR 0005)", found)
	}
}

func TestFrozenV1_ABreakingSchemaChangeIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "version-1-changes-only-by-addition", "a-breaking-schema-change-is-refused")
	frozen := readFrozenV1(t, frozenV1Schema)
	edit := func(t *testing.T, change func(schema map[string]any)) []string {
		t.Helper()
		current := decodeSchema(t, frozen)
		change(current)
		return schemaBreaks(decodeSchema(t, frozen), current)
	}
	breaking := []struct {
		name   string
		change func(t *testing.T, schema map[string]any)
	}{
		{"a removed member", func(t *testing.T, s map[string]any) {
			delete(frozenNode(t, s, "properties", "subject", "properties"), "branch")
		}},
		{"a removed test case member", func(t *testing.T, s map[string]any) {
			delete(frozenNode(t, s, "$defs", "componentTests", "properties"), "omittedCases")
		}},
		{"a retyped member", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "generatedAt")["type"] = "integer"
		}},
		{"a member that loses its type", func(t *testing.T, s map[string]any) {
			delete(frozenNode(t, s, "properties", "generatedAt"), "type")
		}},
		{"a newly required member", func(t *testing.T, s map[string]any) {
			s["required"] = append(s["required"].([]any), "tests")
		}},
		{"a removed vocabulary value", func(t *testing.T, s map[string]any) {
			node := frozenNode(t, s, "properties", "subject", "properties", "provider")
			node["enum"] = slices.DeleteFunc(slices.Clone(node["enum"].([]any)), func(v any) bool { return v == "generic" })
		}},
		{"a narrower bound", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "subject", "properties", "repository")["maxLength"] = float64(100)
		}},
		{"a higher minimum", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "subject", "properties", "repository")["minLength"] = float64(2)
		}},
		{"a changed pattern", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "subject", "properties", "sha")["pattern"] = "^[0-9a-f]{40}$"
		}},
		{"a new constraint", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "subject", "properties", "branch")["pattern"] = "^[a-z]+$"
		}},
		{"an open object closed", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "specVerification")["additionalProperties"] = false
		}},
		{"a changed reference", func(t *testing.T, s map[string]any) {
			items := frozenNode(t, s, "properties", "changedFiles", "items")
			items["$ref"] = "#/$defs/component"
		}},
		{"a removed definition", func(t *testing.T, s map[string]any) {
			delete(frozenNode(t, s, "$defs"), "testCase")
		}},
		{"a member no longer required", func(t *testing.T, s map[string]any) {
			node := frozenNode(t, s, "$defs", "testCase")
			node["required"] = slices.DeleteFunc(slices.Clone(node["required"].([]any)), func(v any) bool { return v == "outcome" })
		}},
		{"a typed member of an open object", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "specVerification")["properties"] = map[string]any{"groups": map[string]any{"type": "array", "maxItems": float64(1)}}
		}},
		{"another schema dialect", func(t *testing.T, s map[string]any) {
			s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		}},
		{"another protocol version", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "protocolVersion")["const"] = float64(2)
		}},
	}
	for _, test := range breaking {
		t.Run(test.name, func(t *testing.T) {
			if found := edit(t, func(s map[string]any) { test.change(t, s) }); len(found) == 0 {
				t.Errorf("%s passes the compatibility check, want a break", test.name)
			}
		})
	}
	additive := []struct {
		name   string
		change func(t *testing.T, schema map[string]any)
	}{
		{"a new optional member", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties")["releaseNotes"] = map[string]any{"type": "string", "maxLength": float64(4096)}
		}},
		{"a new optional member of a definition", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "$defs", "testCase", "properties")["durationMs"] = map[string]any{"type": "integer", "minimum": float64(0)}
		}},
		{"a new vocabulary value", func(t *testing.T, s map[string]any) {
			node := frozenNode(t, s, "properties", "subject", "properties", "provider")
			node["enum"] = append(node["enum"].([]any), "bitbucket")
		}},
		{"a new definition", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "$defs")["releaseNote"] = map[string]any{"type": "object"}
		}},
		{"a looser bound", func(t *testing.T, s map[string]any) {
			frozenNode(t, s, "properties", "subject", "properties", "repository")["maxLength"] = float64(8192)
		}},
		{"a reworded description", func(t *testing.T, s map[string]any) {
			s["description"] = "Another wording."
			frozenNode(t, s, "properties", "subject")["description"] = "Another wording."
		}},
	}
	for _, test := range additive {
		t.Run(test.name, func(t *testing.T) {
			if found := edit(t, func(s map[string]any) { test.change(t, s) }); len(found) != 0 {
				t.Errorf("%s is refused, want an addition: %v", test.name, found)
			}
		})
	}
}

// frozenNode walks a decoded schema by its keys and fails the test when one
// is missing, so a mutation never edits a node the schema does not hold.
func frozenNode(t *testing.T, schema map[string]any, keys ...string) map[string]any {
	t.Helper()
	node := schema
	for i, key := range keys {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("the schema has no object at %s", strings.Join(keys[:i+1], "/"))
		}
		node = next
	}
	return node
}

// annotationKeywords never constrain a document: rewording them breaks
// nothing.
var annotationKeywords = map[string]bool{
	"$comment": true, "title": true, "description": true, "default": true, "examples": true,
}

// upperBounds may grow and lowerBounds may shrink: either loosens.
var (
	upperBounds = map[string]bool{"maxLength": true, "maxItems": true, "maxProperties": true, "maximum": true}
	lowerBounds = map[string]bool{"minLength": true, "minItems": true, "minProperties": true, "minimum": true}
)

// schemaBreaks lists each place where current refuses, retypes or no longer
// declares what frozen accepted, keyword by keyword at the same location.
// Every keyword it does not know must keep its exact value.
func schemaBreaks(frozen, current map[string]any) []string {
	var breaks []string
	compareFrozenSchema("#", frozen, current, &breaks)
	sort.Strings(breaks)
	return breaks
}

func compareFrozenSchema(at string, frozen, current map[string]any, breaks *[]string) {
	report := func(format string, args ...any) {
		*breaks = append(*breaks, at+": "+fmt.Sprintf(format, args...))
	}
	for _, key := range sortedKeys(frozen) {
		if annotationKeywords[key] {
			continue
		}
		was := frozen[key]
		now, present := current[key]
		switch {
		case key == "properties" || key == "$defs" || key == "definitions":
			wasMembers, _ := was.(map[string]any)
			nowMembers, _ := now.(map[string]any)
			if key == "properties" && frozen["additionalProperties"] != false {
				for _, name := range sortedKeys(nowMembers) {
					if _, known := wasMembers[name]; !known {
						report("member %q constrains an open object", name)
					}
				}
			}
			for _, name := range sortedKeys(wasMembers) {
				nowMember, ok := nowMembers[name].(map[string]any)
				if !ok {
					report("%s %q is removed", key, name)
					continue
				}
				wasMember, _ := wasMembers[name].(map[string]any)
				compareFrozenSchema(at+"/"+key+"/"+name, wasMember, nowMember, breaks)
			}
		case key == "items":
			if !present {
				continue
			}
			wasItems, wasObject := was.(map[string]any)
			nowItems, nowObject := now.(map[string]any)
			if !wasObject || !nowObject {
				if !reflect.DeepEqual(was, now) {
					report("items %v became %v", was, now)
				}
				continue
			}
			compareFrozenSchema(at+"/items", wasItems, nowItems, breaks)
		case key == "additionalProperties":
			if was == false || !present || now == true {
				continue
			}
			wasSchema, wasObject := was.(map[string]any)
			nowSchema, nowObject := now.(map[string]any)
			if wasObject && nowObject {
				compareFrozenSchema(at+"/additionalProperties", wasSchema, nowSchema, breaks)
				continue
			}
			if !reflect.DeepEqual(was, now) {
				report("additionalProperties %v became %v", was, now)
			}
		case key == "required":
			for _, name := range asList(now) {
				if !slices.Contains(asList(was), name) {
					report("member %v is newly required", name)
				}
			}
			for _, name := range asList(was) {
				if !slices.Contains(asList(now), name) {
					report("member %v is no longer required", name)
				}
			}
		case key == "enum":
			if !present {
				continue
			}
			kept := asList(now)
			for _, value := range asList(was) {
				if !slices.Contains(kept, value) {
					report("vocabulary value %v is removed", value)
				}
			}
		case upperBounds[key] || lowerBounds[key]:
			if !present {
				continue
			}
			wasBound, _ := was.(float64)
			nowBound, ok := now.(float64)
			if !ok || (upperBounds[key] && nowBound < wasBound) || (lowerBounds[key] && nowBound > wasBound) {
				report("%s %v became %v", key, was, now)
			}
		default:
			if !present {
				report("%s %v is removed", key, was)
			} else if !reflect.DeepEqual(was, now) {
				report("%s %v became %v", key, was, now)
			}
		}
	}
	for _, key := range sortedKeys(current) {
		if _, known := frozen[key]; known || annotationKeywords[key] {
			continue
		}
		switch key {
		case "$defs", "definitions":
		case "properties":
			if frozen["additionalProperties"] != false {
				report("properties %v constrain an open object", sortedKeys(current[key].(map[string]any)))
			}
		case "additionalProperties":
			if current[key] != true {
				report("additionalProperties %v is a new constraint", current[key])
			}
		default:
			report("%s %v is a new constraint", key, current[key])
		}
	}
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}
