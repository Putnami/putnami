package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// Shared cross-language fixtures. The TS reader's golden harness
// (typescript/framework/client/test/generator/openapi-golden.test.ts) runs
// against the SAME *.openapi.json inputs, so the two readers cannot drift on
// operation set, naming, optionality, or $ref/named-type structure.
const (
	sharedFixturesRel = "../../../typescript/framework/client/test/generator/fixtures"
	goGoldenDir       = "testdata/clientir"
)

func sharedOpenAPIDir() string { return filepath.Join(sharedFixturesRel, "openapi") }
func sharedTSIRDir() string    { return filepath.Join(sharedFixturesRel, "ir") }

// fixtureNames lists the shared *.openapi.json fixtures, or skips the test when
// the TypeScript tree is absent (e.g. the api module consumed standalone).
func fixtureNames(t *testing.T) []string {
	entries, err := os.ReadDir(sharedOpenAPIDir())
	if err != nil {
		t.Skipf("shared fixtures not available (%v); run inside the monorepo", err)
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".openapi.json")
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no shared openapi fixtures found")
	}
	return names
}

func readFixtureSpec(t *testing.T, name string) SpecIR {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sharedOpenAPIDir(), name+".openapi.json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec(%s): %v", name, err)
	}
	return spec
}

func readFirstPartyFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../protocols/clientcontract/fixtures/openapi/valid/full.openapi.json")
	if err != nil {
		t.Fatalf("read first-party fixture: %v", err)
	}
	return raw
}

func TestReadOpenAPISpec_FirstPartyRetainsNeutralContractAndWireShapes(t *testing.T) {
	spec, err := ReadOpenAPISpec(readFirstPartyFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if spec.IRVersion != clientIRVersion || spec.Contract == nil {
		t.Fatalf("IR contract header = version %d, contract %#v", spec.IRVersion, spec.Contract)
	}
	if spec.NamedTypes != nil || spec.Enums != nil || spec.Unions != nil {
		t.Fatalf("strict IR contains legacy lossy type projections: named=%#v enums=%#v unions=%#v",
			spec.NamedTypes, spec.Enums, spec.Unions)
	}
	if spec.Contract.Protobuf == nil || spec.Contract.Protobuf.Enums[0].Values[2].Number != 7 {
		t.Fatalf("protobuf enum numbers were lost: %#v", spec.Contract.Protobuf)
	}
	sequence := spec.Schemas["WidgetEvent"].Properties["sequence"]
	if sequence.Format != "uint64" || sequence.Maximum == nil || sequence.Maximum.String() != "18446744073709551615" {
		t.Fatalf("lossless uint64 schema = %#v", sequence)
	}

	var getWidget *MethodIR
	methodCount := 0
	for i := range spec.Services {
		for j := range spec.Services[i].Methods {
			methodCount++
			method := &spec.Services[i].Methods[j]
			if method.OperationID == "getWidget" {
				getWidget = method
			}
		}
	}
	if methodCount != 4 || getWidget == nil {
		t.Fatalf("methods = %#v", spec.Services)
	}
	if len(getWidget.Parameters) != 1 || getWidget.Parameters[0].Location != "path" || getWidget.Parameters[0].Schema.Format != "uuid" {
		t.Fatalf("neutral parameters = %#v", getWidget.Parameters)
	}
	if getWidget.Params != nil || getWidget.Query != nil || getWidget.ResponseType != "" || getWidget.Response != nil {
		t.Fatalf("strict method contains legacy lossy projections: %#v", getWidget)
	}
	if len(getWidget.Successes) != 1 || getWidget.Successes[0].Status != 200 || len(getWidget.Successes[0].Content) != 1 {
		t.Fatalf("success variants = %#v", getWidget.Successes)
	}
	if getWidget.Client == nil || len(getWidget.Client.Transports) != 2 || getWidget.Client.Transports[0].ProtobufMethod == "" {
		t.Fatalf("operation client contract = %#v", getWidget.Client)
	}
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			if method.OperationID == "uploadWidgets" {
				if method.Client == nil || method.Client.Messages == nil || method.Client.Messages.Input == nil ||
					method.Client.Messages.Output == nil || len(method.Successes) != 0 {
					t.Fatalf("client stream logical messages/upgrade responses = %#v / %#v", method.Client, method.Successes)
				}
			}
		}
	}
}

func TestReadOpenAPISpec_FirstPartyMatchesSharedNeutralIR(t *testing.T) {
	spec, err := ReadOpenAPISpec(readFirstPartyFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecHash = ""
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal neutral IR: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		t.Fatalf("decode neutral IR for canonicalization: %v", err)
	}
	got, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("canonicalize neutral IR: %v", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile("../../../protocols/clientcontract/fixtures/ir/full.ir.json")
	if err != nil {
		t.Fatalf("read shared neutral IR: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Go neutral IR differs from the shared Go-TypeScript contract\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func TestReadOpenAPISpec_FirstPartyRejectsOperationIdentityCollisions(t *testing.T) {
	fixture := string(readFirstPartyFixture(t))
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{
			name: "duplicate operation id",
			old:  `"operationId": "watchWidgets"`,
			new:  `"operationId": "getWidget"`,
			want: `share operationId "getWidget"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(fixture, tc.old, tc.new, 1)
			if raw == fixture {
				t.Fatalf("fixture replacement %q did not match", tc.old)
			}
			_, err := ReadOpenAPISpec([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadOpenAPISpec error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadOpenAPISpec_FirstPartyFailsOnUnsupportedSchemaSemantic(t *testing.T) {
	raw := strings.Replace(string(readFirstPartyFixture(t)),
		`"type": "string", "format": "uuid"`,
		`"type": "string", "format": "uuid", "x-lossy": true`, 1)
	_, err := ReadOpenAPISpec([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("ReadOpenAPISpec error = %v, want strict unsupported-schema failure", err)
	}
}

func TestReadOpenAPISpec_FirstPartyRejectsUnsupportedOpenAPISemanticsAndDuplicateKeys(t *testing.T) {
	fixture := string(readFirstPartyFixture(t))
	tests := []struct {
		name    string
		old     string
		replace string
		want    string
	}{
		{
			name: "parameter serialization",
			old:  `"required": true,`,
			replace: `"required": true,
            "style": "matrix",
            "explode": true,`,
			want: `unsupported OpenAPI field "explode"`,
		},
		{
			name:    "duplicate operation key",
			old:     `"operationId": "getWidget",`,
			replace: `"operationId": "getWidget", "operationId": "duplicate",`,
			want:    `duplicate JSON key "operationId"`,
		},
		{
			name:    "duplicate parameter key",
			old:     `"name": "id",`,
			replace: `"name": "id", "name": "duplicate",`,
			want:    `duplicate JSON key "name"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(fixture, tc.old, tc.replace, 1)
			if raw == fixture {
				t.Fatalf("fixture replacement %q did not match", tc.old)
			}
			_, err := ReadOpenAPISpec([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadOpenAPISpec error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadOpenAPISpec_FirstPartyFailsOnMissingOperationMarker(t *testing.T) {
	raw := []byte(`{
  "openapi":"3.0.3",
  "x-putnami-client":{"protocolVersion":1,"service":{"id":"health","audience":"urn:health"},"credentials":{}},
  "paths":{"/health":{"get":{"operationId":"health","responses":{"204":{"description":"healthy"}}}}}
}`)
	_, err := ReadOpenAPISpec(raw)
	if err == nil || !strings.Contains(err.Error(), "missing x-putnami-client") {
		t.Fatalf("ReadOpenAPISpec error = %v, want missing operation marker", err)
	}
}

func TestReadOpenAPISpec_FirstPartyFailsOnUnknownProtobufMethod(t *testing.T) {
	raw := strings.Replace(string(readFirstPartyFixture(t)),
		`"protobufMethod": "/fixtures.widgets.v1.WidgetsService/GetWidget"`,
		`"protobufMethod": "/fixtures.widgets.v1.WidgetsService/Missing"`, 1)
	_, err := ReadOpenAPISpec([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("ReadOpenAPISpec error = %v, want descriptor join failure", err)
	}
}

func TestReadOpenAPISpec_FirstPartyFailsOnDanglingSchemaReference(t *testing.T) {
	raw := strings.Replace(string(readFirstPartyFixture(t)),
		`"schema": { "$ref": "#/components/schemas/Widget", "nullable": false }`,
		`"schema": { "$ref": "#/components/schemas/Missing", "nullable": false }`, 1)
	_, err := ReadOpenAPISpec([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), `missing component schema "Missing"`) {
		t.Fatalf("ReadOpenAPISpec error = %v, want dangling component failure", err)
	}
}

func TestReadOpenAPISpec_FirstPartyWebSocketStreamRequiresUpgradeResponse(t *testing.T) {
	raw := strings.Replace(string(readFirstPartyFixture(t)),
		`"responses": { "101": { "description": "Switching Protocols" } },`,
		`"responses": {},`, 1)
	_, err := ReadOpenAPISpec([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "declares no 101 upgrade response") {
		t.Fatalf("ReadOpenAPISpec error = %v, want missing upgrade response failure", err)
	}
}

// The subject is the exact numeric lexeme, so the closed set is declared on an
// integer property rather than on WidgetState: WidgetState is also a protobuf
// enum, and a protobuf enum whose published members are 64-bit numbers is a
// contradiction the Connect parity check refuses on its own (enum wire numbers
// are int32), which would hide the lexeme assertion behind an unrelated refusal.
func TestReadOpenAPISpec_FirstPartyPreservesNumericEnumWithoutLegacyDegrade(t *testing.T) {
	raw := strings.Replace(string(readFirstPartyFixture(t)),
		`"format": "uint64",
            "minimum": 0,`,
		`"format": "uint64",
            "enum": [0, 18446744073709551615],
            "minimum": 0,`, 1)
	spec, err := ReadOpenAPISpec([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	sequence := spec.Schemas["WidgetEvent"].Properties["sequence"]
	if len(sequence.Enum) != 2 || string(sequence.Enum[1]) != "18446744073709551615" {
		t.Fatalf("numeric enum was not preserved exactly: %#v", sequence.Enum)
	}
	if _, lossyLegacyEnum := spec.Enums["WidgetEvent"]; lossyLegacyEnum {
		t.Fatalf("numeric enum was projected into legacy string enum: %#v", spec.Enums["WidgetEvent"])
	}
}

// TestReadOpenAPISpec_Golden pins the Go reader's output for every shared
// fixture. Set PUTNAMI_UPDATE_GOLDEN=1 to regenerate the goldens after an
// intentional change.
func TestReadOpenAPISpec_Golden(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			spec := readFixtureSpec(t, name)
			if spec.SpecHash == "" {
				t.Error("expected a non-empty specHash")
			}
			// specHash is a derived drift token over the raw bytes, not part of
			// the structural IR contract — exclude it from the golden.
			spec.SpecHash = ""

			got, err := json.MarshalIndent(spec, "", "  ")
			if err != nil {
				t.Fatalf("marshal IR: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join(goGoldenDir, name+".ir.json")
			if os.Getenv("PUTNAMI_UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(goGoldenDir, 0o755); err != nil {
					t.Fatalf("mkdir golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s (run with PUTNAMI_UPDATE_GOLDEN=1 to create): %v", goldenPath, err)
			}
			if string(got) != string(want) {
				t.Errorf("IR mismatch for %s\n--- got:\n%s\n--- want:\n%s", name, got, want)
			}
		})
	}
}

func TestReadOpenAPISpec_ResolvesReferencedUnionVariants(t *testing.T) {
	raw := []byte(`{
  "openapi": "3.0.3",
  "paths": {},
  "components": {"schemas": {
    "RenameChange": {
      "type": "object",
      "properties": {"name": {"type": "string"}},
      "required": ["name"]
    },
    "ArchiveChange": {
      "type": "object",
      "properties": {"reason": {"type": "string"}}
    },
    "Change": {
      "oneOf": [
        {"$ref": "#/components/schemas/RenameChange"},
        {"$ref": "#/components/schemas/ArchiveChange"}
      ],
      "discriminator": {
        "propertyName": "kind",
        "mapping": {
          "rename": "#/components/schemas/RenameChange",
          "archive": "#/components/schemas/ArchiveChange"
        }
      }
    }
  }}
}`)
	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	union := spec.Unions["Change"]
	if len(union.Variants) != 2 {
		t.Fatalf("union variants = %#v", union.Variants)
	}
	if got := union.Variants[0]; got.Tag != "rename" || len(got.Fields) != 1 || got.Fields[0].Name != "name" || got.Fields[0].Optional {
		t.Errorf("rename variant = %#v", got)
	}
	if got := union.Variants[1]; got.Tag != "archive" || len(got.Fields) != 1 || got.Fields[0].Name != "reason" || !got.Fields[0].Optional {
		t.Errorf("archive variant = %#v", got)
	}
}

// --- Cross-language structural parity ---

// tsSpecIR mirrors the TS IR (ir.type.ts) for parsing the shared ir.json
// expectations. Only the structural fields are modeled.
type tsSpecIR struct {
	Services   []tsServiceIR          `json:"services"`
	NamedTypes map[string][]tsFieldIR `json:"namedTypes"`
}

type tsServiceIR struct {
	Name      string       `json:"name"`
	ClassName string       `json:"className"`
	Methods   []tsMethodIR `json:"methods"`
}

type tsMethodIR struct {
	Name         string      `json:"name"`
	OperationID  string      `json:"operationId"`
	HTTPMethod   string      `json:"httpMethod"`
	Path         string      `json:"path"`
	Params       []tsFieldIR `json:"params"`
	Query        []tsFieldIR `json:"query"`
	Body         []tsFieldIR `json:"body"`
	BodyType     string      `json:"bodyType"`
	Response     []tsFieldIR `json:"response"`
	ResponseType string      `json:"responseType"`
}

type tsFieldIR struct {
	Name     string `json:"name"`
	TSType   string `json:"tsType"`
	Optional bool   `json:"optional"`
	Array    bool   `json:"array"`
}

// normField is a language-neutral field shape: the primitive token is collapsed
// to a class so a Go `int64`/`float64` and a TS `number` compare equal, while
// model references and the inline-object degrade still must match exactly.
type normField struct {
	Optional bool
	Array    bool
	Class    string // "scalar" | "object" | "model:<Name>"
}

func classifyTS(tsType string) string {
	switch tsType {
	case "string", "number", "boolean":
		return "scalar"
	case "Record<string, unknown>":
		return "object"
	default:
		return "model:" + tsType
	}
}

func classifyGo(goType string) string {
	switch goType {
	case "string", "int", "int32", "int64", "float32", "float64", "bool":
		return "scalar"
	case "map[string]any":
		return "object"
	default:
		return "model:" + goType
	}
}

func normTSFields(fields []tsFieldIR) map[string]normField {
	out := make(map[string]normField, len(fields))
	for _, f := range fields {
		out[f.Name] = normField{Optional: f.Optional, Array: f.Array, Class: classifyTS(f.TSType)}
	}
	return out
}

func normGoFields(fields []FieldIR) map[string]normField {
	out := make(map[string]normField, len(fields))
	for _, f := range fields {
		out[f.Name] = normField{Optional: f.Optional, Array: f.Array, Class: classifyGo(f.GoType)}
	}
	return out
}

// methodKey identifies an operation independent of service ordering.
func methodKey(operationID, httpMethod, path string) string {
	return fmt.Sprintf("%s %s [%s]", httpMethod, path, operationID)
}

// TestReadOpenAPISpec_CrossLanguageParity asserts the Go reader and the TS
// reader produce structurally identical IR over the shared fixtures: the same
// services, the same operation set, and per-field optionality / array-ness /
// type class (scalar vs object vs the same model name). Primitive token
// differences (Go `int64` vs TS `number`) are intentionally normalized away.
func TestReadOpenAPISpec_CrossLanguageParity(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			tsRaw, err := os.ReadFile(filepath.Join(sharedTSIRDir(), name+".ir.json"))
			if err != nil {
				t.Skipf("no TS IR expectation for %s: %v", name, err)
			}
			var ts tsSpecIR
			if err := json.Unmarshal(tsRaw, &ts); err != nil {
				t.Fatalf("parse TS IR %s: %v", name, err)
			}
			goSpec := readFixtureSpec(t, name)

			// Services compared as sets keyed by name.
			tsSvc := map[string]tsServiceIR{}
			for _, s := range ts.Services {
				tsSvc[s.Name] = s
			}
			goSvc := map[string]ServiceIR{}
			for _, s := range goSpec.Services {
				goSvc[s.Name] = s
			}
			if len(tsSvc) != len(goSvc) {
				t.Fatalf("service count differs: TS %d, Go %d", len(tsSvc), len(goSvc))
			}

			for svcName, tsS := range tsSvc {
				goS, ok := goSvc[svcName]
				if !ok {
					t.Fatalf("Go IR missing service %q", svcName)
				}
				if tsS.ClassName != goS.ClassName {
					t.Errorf("service %q className: TS %q, Go %q", svcName, tsS.ClassName, goS.ClassName)
				}

				tsMethods := map[string]tsMethodIR{}
				for _, m := range tsS.Methods {
					tsMethods[methodKey(m.OperationID, m.HTTPMethod, m.Path)] = m
				}
				goMethods := map[string]MethodIR{}
				for _, m := range goS.Methods {
					goMethods[methodKey(m.OperationID, m.HTTPMethod, m.Path)] = m
				}
				if len(tsMethods) != len(goMethods) {
					t.Fatalf("service %q method count: TS %d, Go %d", svcName, len(tsMethods), len(goMethods))
				}
				for key, tsM := range tsMethods {
					goM, ok := goMethods[key]
					if !ok {
						t.Fatalf("Go IR missing method %q in service %q", key, svcName)
					}
					assertFieldsEqual(t, key+" params", normTSFields(tsM.Params), normGoFields(goM.Params))
					assertFieldsEqual(t, key+" query", normTSFields(tsM.Query), normGoFields(goM.Query))
					assertFieldsEqual(t, key+" body", normTSFields(tsM.Body), normGoFields(goM.Body))
					assertFieldsEqual(t, key+" response", normTSFields(tsM.Response), normGoFields(goM.Response))
					if tsM.BodyType != goM.BodyType {
						t.Errorf("method %q bodyType: TS %q, Go %q", key, tsM.BodyType, goM.BodyType)
					}
					if tsM.ResponseType != goM.ResponseType {
						t.Errorf("method %q responseType: TS %q, Go %q", key, tsM.ResponseType, goM.ResponseType)
					}
				}
			}

			// Named types compared as a map of model → field map.
			if len(ts.NamedTypes) != len(goSpec.NamedTypes) {
				t.Fatalf("namedTypes count: TS %d, Go %d", len(ts.NamedTypes), len(goSpec.NamedTypes))
			}
			for model, tsFields := range ts.NamedTypes {
				goFields, ok := goSpec.NamedTypes[model]
				if !ok {
					t.Fatalf("Go IR missing named type %q", model)
				}
				assertFieldsEqual(t, "namedType "+model, normTSFields(tsFields), normGoFields(goFields))
			}
		})
	}
}

func TestReadOpenAPISpec_PathItemParametersAndMetadata(t *testing.T) {
	raw := []byte(`{
  "openapi": "3.0.3",
  "info": { "title": "Path params", "version": "1.0.0" },
  "paths": {
    "/users/{id}": {
      "summary": "path item metadata is valid OpenAPI",
      "parameters": [
        { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }
      ],
      "get": {
        "operationId": "getUser",
        "responses": { "204": { "description": "No Content" } }
      }
    }
  }
}`)

	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec: %v", err)
	}
	if len(spec.Services) != 1 || len(spec.Services[0].Methods) != 1 {
		t.Fatalf("services = %+v", spec.Services)
	}
	params := spec.Services[0].Methods[0].Params
	if len(params) != 1 || params[0].Name != "id" || params[0].GoType != "string" || params[0].Optional {
		t.Fatalf("params = %+v, want required string id from path item", params)
	}
}

func assertFieldsEqual(t *testing.T, label string, want, got map[string]normField) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s field count: TS %d, Go %d (TS=%v Go=%v)", label, len(want), len(got), want, got)
		return
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: Go missing field %q", label, name)
			continue
		}
		if w != g {
			t.Errorf("%s field %q: TS %+v != Go %+v", label, name, w, g)
		}
	}
}

// clientContractFixtureDir is the shared Go/TypeScript first-party corpus.
// The same expectations.json drives the TypeScript reader, so a document one
// reader accepts and the other rejects fails on one side or the other.
const clientContractFixtureDir = "../../../protocols/clientcontract/fixtures/openapi"

type clientContractExpectations struct {
	Valid   []string          `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

func readClientContractExpectations(t *testing.T) clientContractExpectations {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(clientContractFixtureDir, "expectations.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var expectations clientContractExpectations
	if err := json.Unmarshal(raw, &expectations); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	if len(expectations.Valid) == 0 || len(expectations.Invalid) == 0 {
		t.Fatalf("expectations corpus is empty: %#v", expectations)
	}
	return expectations
}

// TestReadOpenAPISpec_MatchesTheSharedContractExpectations runs the Go reader
// over the whole shared corpus: every valid document is read, and every invalid
// one fails carrying the diagnostic code the corpus names. Conformance is
// stated by the corpus, not by the reader's own tests.
func TestReadOpenAPISpec_MatchesTheSharedContractExpectations(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "shared-contract-source", "the-go-reader-matches-the-shared-first-party-corpus-expectations")

	expectations := readClientContractExpectations(t)

	for _, name := range expectations.Valid {
		t.Run("valid/"+name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(clientContractFixtureDir, "valid", name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			spec, err := ReadOpenAPISpec(raw)
			if err != nil {
				t.Fatalf("ReadOpenAPISpec rejected a corpus-valid document: %v", err)
			}
			if spec.Contract == nil {
				t.Fatalf("corpus-valid document produced no first-party contract")
			}
		})
	}

	names := make([]string, 0, len(expectations.Invalid))
	for name := range expectations.Invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		code := expectations.Invalid[name]
		t.Run("invalid/"+name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(clientContractFixtureDir, "invalid", name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			_, err = ReadOpenAPISpec(raw)
			if err == nil {
				t.Fatalf("ReadOpenAPISpec accepted a corpus-invalid document")
			}
			if !strings.Contains(err.Error(), code) {
				t.Fatalf("error = %v, want the corpus diagnostic code %q", err, code)
			}
		})
	}
}

// The declared integer width survives the read: uint64 stays uint64 with its
// exact bounds, and no bound is rounded through a float (D0.2).
func TestReadOpenAPISpec_KeepsDeclaredIntegerWidthAndExactBounds(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "shared-contract-source", "a-declared-integer-width-and-its-exact-bounds-survive-the-read")

	spec, err := ReadOpenAPISpec(readFirstPartyFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	sequence := spec.Schemas["WidgetEvent"].Properties["sequence"]
	if sequence.Type != "integer" || sequence.Format != "uint64" {
		t.Fatalf("declared width = %q/%q, want integer/uint64", sequence.Type, sequence.Format)
	}
	if sequence.Minimum == nil || sequence.Minimum.String() != "0" {
		t.Fatalf("minimum = %#v, want exact 0", sequence.Minimum)
	}
	if sequence.Maximum == nil || sequence.Maximum.String() != "18446744073709551615" {
		t.Fatalf("maximum = %#v, want the exact uint64 ceiling", sequence.Maximum)
	}
	source, err := generateStrictClient(spec, ClientGenOptions{PackageName: "widgets", ClientName: "WidgetsClient"})
	if err == nil {
		if !strings.Contains(source, "Sequence uint64") {
			t.Fatalf("emitted Go narrowed the declared uint64:\n%s", source)
		}
		return
	}
	// The corpus declares stream transports the Go runtime cannot honor yet, so
	// full emission may refuse. The width assertion above is the contract; check
	// the projection directly when emission stops earlier.
	generator := &strictClientGen{spec: spec, opts: ClientGenOptions{PackageName: "widgets"}, clientName: "WidgetsClient",
		imports: map[string]bool{}, defs: map[string]string{}, declaring: map[string]bool{}}
	goType, typeErr := generator.schemaType(sequence, "Sequence")
	if typeErr != nil || goType != "uint64" {
		t.Fatalf("schemaType(uint64) = %q, %v", goType, typeErr)
	}
}

// --- Connect contract: the descriptor, the schema and the IR are one view ---

// The corpus is the reference declaration set: real proto messages with exact
// field numbers, a oneof, a map, an enum with a proto3 zero value, a uint64 and
// a bytes field. Reading it proves the three views agree on all of them.
func TestReadOpenAPISpec_FirstPartyConnectContractMatchesTheCorpusDescriptor(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "the-corpus-descriptor-schema-and-ir-carry-one-declaration")
	spec, err := ReadOpenAPISpec(readFirstPartyFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := spec.Contract.Protobuf
	if descriptor == nil {
		t.Fatal("the corpus document declares a protobuf descriptor")
	}

	widget := protobufMessageNamed(t, descriptor, "Widget")
	byName := map[string]clientcontract.ProtobufField{}
	for _, field := range widget.Fields {
		byName[field.JSONName] = field
	}
	// A map is absent-as-empty on both wires and carries its exact key/value.
	labels := byName["labels"]
	if labels.Number != 4 || labels.TypeKind != "map" || labels.Map == nil || labels.Map.ValueType != "string" {
		t.Errorf("labels lost its map shape or field number: %+v", labels)
	}
	if labels.Optional {
		t.Errorf("a proto3 map never carries explicit presence: %+v", labels)
	}
	// bytes on the wire is base64 text in the schema; both are the same value.
	if payload := byName["payload"]; payload.Type != "bytes" || payload.Number != 2 {
		t.Errorf("payload lost its bytes shape or field number: %+v", payload)
	}
	if got := spec.Schemas["Widget"].Properties["payload"]; got.Type != "string" || got.Format != "byte" {
		t.Errorf("published payload schema = %+v, want string/byte", got)
	}
	// The oneof arms stay in their declared group.
	if len(widget.OneOfs) != 1 || widget.OneOfs[0] != "owner" {
		t.Fatalf("Widget oneofs = %v, want [owner]", widget.OneOfs)
	}
	for _, name := range []string{"userOwner", "serviceOwner"} {
		if byName[name].OneOf != "owner" {
			t.Errorf("field %q left its oneof group: %+v", name, byName[name])
		}
	}
	// The enum's zero value is the proto3 absence marker, and every other value
	// maps onto a published member.
	if got := descriptor.Enums[0].Values[0]; got.Number != 0 {
		t.Errorf("the first proto3 enum value must be zero: %+v", got)
	}
	// A 64-bit width survives on both wires.
	event := protobufMessageNamed(t, descriptor, "WidgetEvent")
	if event.Fields[0].Type != "uint64" {
		t.Errorf("sequence lost its declared width: %+v", event.Fields[0])
	}
	sequence := spec.Schemas["WidgetEvent"].Properties["sequence"]
	if sequence.Format != "uint64" || sequence.Maximum == nil || sequence.Maximum.String() != "18446744073709551615" {
		t.Errorf("published sequence schema = %+v, want uint64 with its exact bound", sequence)
	}
	// The Connect transport's path is the method identity it names.
	connect := connectTransport(t, spec, "getWidget")
	if connect.Path != connect.ProtobufMethod {
		t.Errorf("connect path %q is not the method identity %q", connect.Path, connect.ProtobufMethod)
	}
}

func protobufMessageNamed(t *testing.T, descriptor *clientcontract.ProtobufDescriptor, name string) clientcontract.ProtobufMessage {
	t.Helper()
	for _, message := range descriptor.Messages {
		if message.Name == name {
			return message
		}
	}
	t.Fatalf("descriptor declares no message %q", name)
	return clientcontract.ProtobufMessage{}
}

func connectTransport(t *testing.T, spec SpecIR, operationID string) clientcontract.Transport {
	t.Helper()
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			if method.OperationID != operationID || method.Client == nil {
				continue
			}
			for _, transport := range method.Client.Transports {
				if transport.Protocol == clientcontract.TransportConnect {
					return transport
				}
			}
		}
	}
	t.Fatalf("operation %q declares no connect transport", operationID)
	return clientcontract.Transport{}
}

// The shared validator refuses an unknown protobuf scalar before the parity
// rules run, so this guard is unreachable through a document. It stays because
// the scalar set lives in two packages: if one grows, the reader must say so
// rather than silently compare a shape it has no mapping for.
func TestCheckProtobufFieldShape_RefusesAScalarWithNoPublishedJSONShape(t *testing.T) {
	err := checkProtobufFieldShape("field", clientcontract.ProtobufField{TypeKind: "scalar", Type: "group"},
		clientcontract.Schema{Type: "string"}, nil)
	if err == nil || !strings.Contains(err.Error(), "has no published JSON shape") {
		t.Fatalf("error = %v, want an unmapped-scalar refusal", err)
	}
}

// Each refusal is exercised on a single mutation of the corpus, so the rule is
// proved against a document that is otherwise a valid first-party contract.
func TestReadOpenAPISpec_FirstPartyRefusesAConnectContractThatDisagreesWithItself(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "the-reader-refuses-a-descriptor-that-disagrees-with-the-schema")
	for name, testCase := range map[string]struct{ from, to, want string }{
		"a field with no declared presence": {
			`"typeKind": "map",
              "type": "map",
              "map": {
                "keyType": "string",
                "valueKind": "scalar",
                "valueType": "string"
              }`,
			`"typeKind": "scalar",
              "type": "string"`,
			"lets the property be absent or null",
		},
		"an integer narrowed by the transport": {
			`"jsonName": "sequence",
              "number": 1,
              "typeKind": "scalar",
              "type": "uint64"`,
			`"jsonName": "sequence",
              "number": 1,
              "typeKind": "scalar",
              "type": "uint32"`,
			"a width is declared, never narrowed by a transport",
		},
		"an unpublished protobuf scalar": {
			`"jsonName": "sequence",
              "number": 1,
              "typeKind": "scalar",
              "type": "uint64"`,
			`"jsonName": "sequence",
              "number": 1,
              "typeKind": "scalar",
              "type": "group"`,
			`protobuf scalar type "group" is unsupported`,
		},
		"a descriptor field the schema does not declare": {
			`"name": "payload",
              "jsonName": "payload",`,
			`"name": "payload",
              "jsonName": "payloadBytes",`,
			"is not a property of the published schema",
		},
		"an enum value the schema does not publish": {
			`{ "name": "WIDGET_STATE_ARCHIVED", "number": 7 }`,
			`{ "name": "WIDGET_STATE_RETIRED", "number": 7 }`,
			"which the published schema does not list as a member",
		},
		"a connect path that is not the method identity": {
			`"protocol": "connect",
              "path": "/fixtures.widgets.v1.WidgetsService/GetWidget",`,
			`"protocol": "connect",
              "path": "/widgets/{id}",`,
			"a Connect path is the method identity",
		},
		"an integer with no declared width": {
			`"type": "integer",
            "format": "uint64",
            "minimum": 0,`,
			`"type": "integer",
            "minimum": 0,`,
			"declare int32, int64, uint32 or uint64",
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw := string(readFirstPartyFixture(t))
			mutated := strings.Replace(raw, testCase.from, testCase.to, 1)
			if mutated == raw {
				t.Fatalf("fixture no longer contains the mutation anchor:\n%s", testCase.from)
			}
			_, err := ReadOpenAPISpec([]byte(mutated))
			if err == nil {
				t.Fatal("ReadOpenAPISpec accepted a contract that disagrees with itself")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error = %v, want it to name %q", err, testCase.want)
			}
		})
	}
}

// The parity rules are exercised directly for the shapes the shared corpus does
// not contain, so every refusal is proved rather than assumed reachable.
func TestProtobufParityRules_RefuseEachDisagreement(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "every-descriptor-parity-rule-names-its-disagreement")
	boolValue := func(value bool) *bool { return &value }
	object := clientcontract.Schema{Type: "object"}
	components := map[string]clientcontract.Schema{
		"Loop":  {Ref: "#/components/schemas/Loop"},
		"State": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active"`)}},
	}

	t.Run("presence declared where the schema requires a value", func(t *testing.T) {
		err := checkProtobufFieldPresence("field", clientcontract.ProtobufField{Optional: true},
			clientcontract.Schema{Type: "string", Nullable: boolValue(false)}, true)
		if err == nil || !strings.Contains(err.Error(), "requires the property and forbids null") {
			t.Fatalf("error = %v, want a superfluous-presence refusal", err)
		}
	})
	t.Run("shape mismatches", func(t *testing.T) {
		for name, testCase := range map[string]struct {
			field  clientcontract.ProtobufField
			schema clientcontract.Schema
			want   string
		}{
			"map against a plain object": {
				clientcontract.ProtobufField{TypeKind: "map", Type: "map"}, object,
				"not an object with a typed additionalProperties value",
			},
			"message against a scalar": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "Widget"}, clientcontract.Schema{Type: "string"},
				`references protobuf message "Widget", but the published schema is not an object`,
			},
			"enum against a plain string": {
				clientcontract.ProtobufField{TypeKind: "enum", Type: "State"}, clientcontract.Schema{Type: "string"},
				"declares no string enum members",
			},
			"repeated against a non-array": {
				clientcontract.ProtobufField{TypeKind: "scalar", Type: "string", Repeated: true}, clientcontract.Schema{Type: "string"},
				"is not an array",
			},
			"scalar against the wrong json type": {
				clientcontract.ProtobufField{TypeKind: "scalar", Type: "bool"}, clientcontract.Schema{Type: "string"},
				`declares JSON type "string" instead of "boolean"`,
			},
			"a well-known type against the wrong json shape": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "google.protobuf.Timestamp"},
				clientcontract.Schema{Type: "string", Format: "byte"},
				`declares format "byte" instead of "date-time"`,
			},
		} {
			t.Run(name, func(t *testing.T) {
				err := checkProtobufFieldShape("field", testCase.field, testCase.schema, components)
				if err == nil || !strings.Contains(err.Error(), testCase.want) {
					t.Fatalf("error = %v, want it to name %q", err, testCase.want)
				}
			})
		}
	})
	t.Run("shapes that agree", func(t *testing.T) {
		for name, testCase := range map[string]struct {
			field  clientcontract.ProtobufField
			schema clientcontract.Schema
		}{
			"a repeated scalar against an array of it": {
				clientcontract.ProtobufField{TypeKind: "scalar", Type: "string", Repeated: true},
				clientcontract.Schema{Type: "array", Items: &clientcontract.Schema{Type: "string"}},
			},
			"a message against a declared union": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "Owner"},
				clientcontract.Schema{OneOf: []clientcontract.Schema{object}},
			},
			"a repeated message against an array of objects": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "Owner", Repeated: true},
				clientcontract.Schema{Type: "array", Items: &object},
			},
			// A well-known type is the one place the two wires legitimately
			// carry different representations of one value.
			"a timestamp against its RFC 3339 string": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "google.protobuf.Timestamp"},
				clientcontract.Schema{Type: "string", Format: "date-time"},
			},
			"a duration against its nanosecond integer": {
				clientcontract.ProtobufField{TypeKind: "message", Type: "google.protobuf.Duration"},
				clientcontract.Schema{Type: "integer", Format: "int64"},
			},
			"an enum against its published members": {
				clientcontract.ProtobufField{TypeKind: "enum", Type: "State"},
				clientcontract.Schema{Ref: "#/components/schemas/State"},
			},
		} {
			t.Run(name, func(t *testing.T) {
				if err := checkProtobufFieldShape("field", testCase.field, testCase.schema, components); err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
			})
		}
	})
	t.Run("a self-referential reference terminates", func(t *testing.T) {
		if got := resolveClientSchema(components["Loop"], components); got.Ref != "#/components/schemas/Loop" {
			t.Fatalf("resolved = %+v, want the unresolved back-edge", got)
		}
	})
	t.Run("a non-string enum member is not a string enum", func(t *testing.T) {
		if _, ok := stringEnumMembers(clientcontract.Schema{Type: "string", Enum: []json.RawMessage{json.RawMessage(`7`)}}); ok {
			t.Fatal("a numeric member must not read as a string enum")
		}
	})
}

// Enum parity is proved on the three disagreements a document can carry.
func TestValidateProtobufEnumParity_RefusesEachDisagreement(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "an-enum-and-its-published-members-are-one-set")
	descriptorWith := func(values ...clientcontract.ProtobufEnumValue) *clientcontract.ProtobufDescriptor {
		return &clientcontract.ProtobufDescriptor{Enums: []clientcontract.ProtobufEnum{{Name: "State", Values: values}}}
	}
	unspecified := clientcontract.ProtobufEnumValue{Name: "STATE_UNSPECIFIED", Number: 0}
	published := SpecIR{Schemas: map[string]clientcontract.Schema{
		"State": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"active"`), json.RawMessage(`"archived"`)}},
	}}

	for name, testCase := range map[string]struct {
		spec       SpecIR
		descriptor *clientcontract.ProtobufDescriptor
		want       string
	}{
		"published as something other than a string enum": {
			SpecIR{Schemas: map[string]clientcontract.Schema{"State": {Type: "object"}}},
			descriptorWith(unspecified),
			"declares no string enum members",
		},
		"a zero value that is a declared member": {
			published,
			descriptorWith(clientcontract.ProtobufEnumValue{Name: "STATE_ACTIVE", Number: 0}),
			"the proto3 zero value is the absence marker",
		},
		"a member the descriptor has no value for": {
			published,
			descriptorWith(unspecified, clientcontract.ProtobufEnumValue{Name: "STATE_ACTIVE", Number: 1}),
			`no value for published member "archived"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateProtobufEnumParity(testCase.spec, testCase.descriptor)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to name %q", err, testCase.want)
			}
		})
	}

	t.Run("an enum with no published schema is left alone", func(t *testing.T) {
		if err := validateProtobufEnumParity(SpecIR{}, descriptorWith(unspecified)); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	})
	t.Run("a message with no published schema is left alone", func(t *testing.T) {
		descriptor := &clientcontract.ProtobufDescriptor{Messages: []clientcontract.ProtobufMessage{
			{Name: "GetWidgetRequest", Fields: []clientcontract.ProtobufField{{Name: "id", JSONName: "id", TypeKind: "scalar", Type: "string"}}},
		}}
		if err := validateProtobufMessageParity(SpecIR{}, descriptor); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	})
}

// A path segment that carries characters no identifier can must still name a
// legal client class ("/.well-known/putnami/events" never yields
// ".wellKnownClient"). The TypeScript reader's openapi-reader.test.ts holds the
// same table, so the two readers cannot drift on service names.
func TestInferServiceNameIsALegalIdentifier(t *testing.T) {
	for path, want := range map[string]string{
		"/users/{id}":                 "UsersService",
		"/user-profiles":              "UserProfilesService",
		"/audit_log":                  "AuditLogService",
		"/.well-known/putnami/events": "WellKnownService",
		"/v1.2/items":                 "V12Service",
		"/2fa/codes":                  "_2faService",
		"/.../x":                      "ApiService",
		"/{id}":                       "ApiService",
	} {
		if got := inferServiceName(path); got != want {
			t.Errorf("inferServiceName(%q) = %q, want %q", path, got, want)
		}
	}
}
