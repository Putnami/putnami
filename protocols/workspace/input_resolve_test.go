package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func validInputResolveRequest() InputResolveRequest {
	return InputResolveRequest{
		Version:   InputResolveProtocolVersion,
		Extension: "@putnami/go",
		Inputs: []InputResolveQuery{
			{Project: "go/framework/web", Selector: "go-embed:build", Patterns: []string{"**/*.go", "!**/*_test.go"}},
			{Project: "go/framework/web", Selector: "go-embed:test", Patterns: []string{"**/*.go", "git:../shared/**"}},
			{Project: ".", Selector: "go-embed:build"},
		},
	}
}

func validInputResolveResult() InputResolveResult {
	return InputResolveResult{
		Version:   InputResolveProtocolVersion,
		Extension: "@putnami/go",
		Inputs: []InputResolveAnswer{
			{
				Project:  "go/framework/web",
				Selector: "go-embed:build",
				Files:    []string{"assets/app.css", "assets/app.js", "templates/index.html"},
				Basis: InputBasis{
					Content: [][]string{{"**/*.go", "go.mod", "!vendor/**"}},
					Listing: [][]string{{"assets/**"}, {"templates/*.html"}},
				},
			},
			{
				Project:  "go/framework/web",
				Selector: "go-embed:test",
				Error:    "web_test.go:12: pattern testdata/*.golden: no matching files found",
			},
			{
				Project:  ".",
				Selector: "go-embed:build",
				Basis:    InputBasis{Content: [][]string{{"**/*.go", "go.mod"}}},
			},
		},
	}
}

func TestIsInputSelectorName(t *testing.T) {
	cases := map[string]bool{
		"go-embed:build":                true,
		"go-embed:test":                 true,
		"a:0":                           true,
		"x1-y:name-2":                   true,
		"git:**":                        false,
		"git:build":                     false,
		"gitx:build":                    true,
		"Go-embed:build":                false,
		"go_embed:build":                false,
		"1go:build":                     false,
		"-go:build":                     false,
		"go-embed:-build":               false,
		"go-embed:Build":                false,
		"go-embed:":                     false,
		":build":                        false,
		"go-embed":                      false,
		"go-embed:build:extra":          false,
		"go-embed:**":                   false,
		"go-embed:build" + " ":          false,
		"":                              false,
		"s:" + strings.Repeat("n", 126): true,
		"s:" + strings.Repeat("n", 127): false,
	}
	for name, want := range cases {
		if got := IsInputSelectorName(name); got != want {
			t.Errorf("IsInputSelectorName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestValidateInputResolveRequest(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*InputResolveRequest)
		code  string
		field string
	}{
		{"valid", func(*InputResolveRequest) {}, "", ""},
		{"one selector, two pattern sets", func(r *InputResolveRequest) {
			r.Inputs = append(r.Inputs, InputResolveQuery{Project: ".", Selector: "go-embed:build", Patterns: []string{"cmd/**"}})
		}, "", ""},
		{"a pattern outside the project", func(r *InputResolveRequest) { r.Inputs[0].Patterns = []string{"../shared/**/*.go"} }, "", ""},
		{"version", func(r *InputResolveRequest) { r.Version = 2 }, "invalid-input-resolve-version", "version"},
		{"extension", func(r *InputResolveRequest) { r.Extension = " " }, "required-field", "extension"},
		{"no inputs", func(r *InputResolveRequest) { r.Inputs = nil }, "required-field", "inputs"},
		{"no project", func(r *InputResolveRequest) { r.Inputs[1].Project = "" }, "required-field", "inputs[1].project"},
		{"absolute project", func(r *InputResolveRequest) { r.Inputs[1].Project = "/w/go" }, "invalid-path", "inputs[1].project"},
		{"escaping project", func(r *InputResolveRequest) { r.Inputs[1].Project = "../other" }, "invalid-path", "inputs[1].project"},
		{"unclean project", func(r *InputResolveRequest) { r.Inputs[1].Project = "go/./framework" }, "invalid-path", "inputs[1].project"},
		{"backslash project", func(r *InputResolveRequest) { r.Inputs[1].Project = `go\framework` }, "invalid-path", "inputs[1].project"},
		{"trailing slash project", func(r *InputResolveRequest) { r.Inputs[1].Project = "go/" }, "invalid-path", "inputs[1].project"},
		{"no selector", func(r *InputResolveRequest) { r.Inputs[2].Selector = "" }, "required-field", "inputs[2].selector"},
		{"git selector", func(r *InputResolveRequest) { r.Inputs[2].Selector = "git:build" }, "invalid-value", "inputs[2].selector"},
		{"uppercase selector", func(r *InputResolveRequest) { r.Inputs[2].Selector = "Go-embed:build" }, "invalid-value", "inputs[2].selector"},
		{"blank pattern", func(r *InputResolveRequest) { r.Inputs[0].Patterns[1] = "!" }, "invalid-value", "inputs[0].patterns[1]"},
		{"backslash pattern", func(r *InputResolveRequest) { r.Inputs[0].Patterns[0] = `src\*.go` }, "invalid-value", "inputs[0].patterns[0]"},
		{"absolute pattern", func(r *InputResolveRequest) { r.Inputs[0].Patterns[0] = "git:/etc/**" }, "invalid-value", "inputs[0].patterns[0]"},
		{"bad glob pattern", func(r *InputResolveRequest) { r.Inputs[0].Patterns[0] = "src/[a-/*.go" }, "invalid-value", "inputs[0].patterns[0]"},
		{"selector as a pattern", func(r *InputResolveRequest) { r.Inputs[0].Patterns[0] = "go-embed:test" }, "invalid-value", "inputs[0].patterns[0]"},
		{"duplicate input", func(r *InputResolveRequest) {
			r.Inputs = append(r.Inputs, InputResolveQuery{
				Project: "go/framework/web", Selector: "go-embed:build", Patterns: []string{"!**/*_test.go", "**/*.go", "**/*.go"},
			})
		}, "duplicate-value", "inputs[3]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := validInputResolveRequest()
			tc.edit(&request)
			assertDiagnostic(t, ValidateInputResolveRequest(&request), tc.code, tc.field)
		})
	}
	if diags := ValidateInputResolveRequest(nil); !hasDiagnosticCode(diags, "nil-request") {
		t.Fatalf("nil request diagnostics = %v", diags)
	}
}

func TestValidateInputResolveResult(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*InputResolveResult)
		code  string
		field string
	}{
		{"valid", func(*InputResolveResult) {}, "", ""},
		{"no files and no basis", func(r *InputResolveResult) { r.Inputs[2].Basis = InputBasis{} }, "", ""},
		{"a file a content set selects", func(r *InputResolveResult) {
			r.Inputs[2].Files = []string{"internal/link.go"}
		}, "", ""},
		{"an exclusion keeps another set's path", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing = [][]string{{"assets/**", "!assets/app.js"}, {"assets/app.js"}, {"templates/*.html"}}
		}, "", ""},
		{"4096-byte error", func(r *InputResolveResult) { r.Inputs[1].Error = strings.Repeat("e", 4096) }, "", ""},
		{"version", func(r *InputResolveResult) { r.Version = 0 }, "invalid-input-resolve-version", "version"},
		{"extension", func(r *InputResolveResult) { r.Extension = "" }, "required-field", "extension"},
		{"no inputs", func(r *InputResolveResult) { r.Inputs = nil }, "required-field", "inputs"},
		{"no project", func(r *InputResolveResult) { r.Inputs[0].Project = "" }, "required-field", "inputs[0].project"},
		{"escaping project", func(r *InputResolveResult) { r.Inputs[0].Project = "go/../.." }, "invalid-path", "inputs[0].project"},
		{"invalid selector", func(r *InputResolveResult) { r.Inputs[0].Selector = "git:**" }, "invalid-value", "inputs[0].selector"},
		{"blank error", func(r *InputResolveResult) { r.Inputs[1].Error = " \n" }, "required-field", "inputs[1].error"},
		{"oversized error", func(r *InputResolveResult) { r.Inputs[1].Error = strings.Repeat("e", 4097) }, "limit-exceeded", "inputs[1].error"},
		{"error with files", func(r *InputResolveResult) {
			r.Inputs[1].Files = []string{"testdata/a.golden"}
		}, "conflicting-field", "inputs[1]"},
		{"error with a basis", func(r *InputResolveResult) {
			r.Inputs[1].Basis = InputBasis{Listing: [][]string{{"testdata/*.golden"}}}
		}, "conflicting-field", "inputs[1]"},
		{"absolute file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "/etc/passwd" }, "invalid-path", "inputs[0].files[0]"},
		{"escaping file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "../secret" }, "invalid-path", "inputs[0].files[0]"},
		{"dot segment file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "assets/./app.css" }, "invalid-path", "inputs[0].files[0]"},
		{"dot-dot segment file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "assets/../assets/app.css" }, "invalid-path", "inputs[0].files[0]"},
		{"empty file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "" }, "invalid-path", "inputs[0].files[0]"},
		{"root file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "." }, "invalid-path", "inputs[0].files[0]"},
		{"backslash file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = `assets\app.css` }, "invalid-path", "inputs[0].files[0]"},
		{"double slash file", func(r *InputResolveResult) { r.Inputs[0].Files[0] = "assets//app.css" }, "invalid-path", "inputs[0].files[0]"},
		{"unsorted files", func(r *InputResolveResult) {
			r.Inputs[0].Files = []string{"assets/app.js", "assets/app.css"}
		}, "invalid-order", "inputs[0].files[1]"},
		{"duplicate file", func(r *InputResolveResult) {
			r.Inputs[0].Files = []string{"assets/app.css", "assets/app.css"}
		}, "duplicate-value", "inputs[0].files[1]"},
		{"unselected file", func(r *InputResolveResult) {
			r.Inputs[0].Files = append(r.Inputs[0].Files, "web.yaml")
		}, "unselected-file", "inputs[0].files[3]"},
		{"an exclusion removes a path from its set", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing = [][]string{{"assets/**", "!assets/app.js"}, {"templates/*.html"}}
		}, "unselected-file", "inputs[0].files[1]"},
		{"empty set", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Content = append(r.Inputs[0].Basis.Content, []string{})
		}, "required-field", "inputs[0].basis.content[1]"},
		{"exclusions alone", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing = append(r.Inputs[0].Basis.Listing, []string{"!vendor/**"})
		}, "required-field", "inputs[0].basis.listing[2]"},
		{"git pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Content[0][0] = "git:**/*.go"
		}, "invalid-value", "inputs[0].basis.content[0][0]"},
		{"escaping pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Content[0][1] = "../go.mod"
		}, "invalid-value", "inputs[0].basis.content[0][1]"},
		{"escaping exclusion", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Content[0][2] = "!../vendor/**"
		}, "invalid-value", "inputs[0].basis.content[0][2]"},
		{"unclean pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing[0][0] = "./assets/**"
		}, "invalid-value", "inputs[0].basis.listing[0][0]"},
		{"absolute pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing[1][0] = "/templates/*.html"
		}, "invalid-value", "inputs[0].basis.listing[1][0]"},
		{"bad glob pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing[1][0] = "templates/[*.html"
		}, "invalid-value", "inputs[0].basis.listing[1][0]"},
		{"backslash pattern", func(r *InputResolveResult) {
			r.Inputs[0].Basis.Listing[1][0] = `templates\*.html`
		}, "invalid-value", "inputs[0].basis.listing[1][0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := validInputResolveResult()
			tc.edit(&result)
			assertDiagnostic(t, ValidateInputResolveResult(&result), tc.code, tc.field)
		})
	}
	if diags := ValidateInputResolveResult(nil); !hasDiagnosticCode(diags, "nil-result") {
		t.Fatalf("nil result diagnostics = %v", diags)
	}
}

func TestValidateInputResolveAnswer(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*InputResolveRequest, *InputResolveResult)
		code  string
		field string
	}{
		{"valid", func(*InputResolveRequest, *InputResolveResult) {}, "", ""},
		{"other extension", func(_ *InputResolveRequest, r *InputResolveResult) {
			r.Extension = "@putnami/typescript"
		}, "invalid-value", "extension"},
		{"an input left unanswered", func(_ *InputResolveRequest, r *InputResolveResult) {
			r.Inputs = r.Inputs[:2]
		}, "invalid-value", "inputs"},
		{"an unasked input", func(_ *InputResolveRequest, r *InputResolveResult) {
			r.Inputs = append(r.Inputs, r.Inputs[2])
		}, "invalid-value", "inputs"},
		{"answers out of order", func(_ *InputResolveRequest, r *InputResolveResult) {
			r.Inputs[0], r.Inputs[2] = r.Inputs[2], r.Inputs[0]
		}, "invalid-value", "inputs[0].project"},
		{"another selector at a position", func(_ *InputResolveRequest, r *InputResolveResult) {
			r.Inputs[0], r.Inputs[1] = r.Inputs[1], r.Inputs[0]
		}, "invalid-value", "inputs[1].selector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, result := validInputResolveRequest(), validInputResolveResult()
			tc.edit(&request, &result)
			assertDiagnostic(t, ValidateInputResolveAnswer(&request, &result), tc.code, tc.field)
		})
	}
	request, result := validInputResolveRequest(), validInputResolveResult()
	if diags := ValidateInputResolveAnswer(nil, &result); !hasDiagnosticCode(diags, "nil-request") {
		t.Fatalf("nil request diagnostics = %v", diags)
	}
	if diags := ValidateInputResolveAnswer(&request, nil); !hasDiagnosticCode(diags, "nil-result") {
		t.Fatalf("nil result diagnostics = %v", diags)
	}
}

func TestInputResolveDiagnosticsAreDeterministic(t *testing.T) {
	request := validInputResolveRequest()
	request.Inputs = append(request.Inputs, request.Inputs[0], InputResolveQuery{Project: "/x", Selector: "git:x"})
	result := validInputResolveResult()
	result.Inputs[0].Files = []string{"z", "a", "a", "/b"}
	result.Inputs[0].Basis.Listing = append(result.Inputs[0].Basis.Listing, []string{"!x/**", "git:y"})
	result.Inputs[0], result.Inputs[2] = result.Inputs[2], result.Inputs[0]
	for name, validate := range map[string]func() []diag.Diagnostic{
		"request": func() []diag.Diagnostic { return ValidateInputResolveRequest(&request) },
		"result":  func() []diag.Diagnostic { return ValidateInputResolveResult(&result) },
		"answer":  func() []diag.Diagnostic { return ValidateInputResolveAnswer(&request, &result) },
	} {
		canonical := validate()
		if len(canonical) < 2 {
			t.Fatalf("%s: want several diagnostics, got %v", name, canonical)
		}
		for i := 0; i < 100; i++ {
			if got := validate(); !reflect.DeepEqual(got, canonical) {
				t.Fatalf("%s: iteration %d produced non-deterministic diagnostics:\nwant %v\ngot %v", name, i, canonical, got)
			}
		}
	}
}

func TestParseInputResolveRejectsTrailingDocument(t *testing.T) {
	if _, diags := ParseInputResolveRequest([]byte(`{"version":1,"extension":"x","inputs":[{"project":".","selector":"go-embed:build"}]} {}`)); !hasDiagnosticCode(diags, "parse-error") {
		t.Fatalf("trailing request document was accepted: %v", diags)
	}
	if _, diags := ParseInputResolveResult([]byte(`{"version":1,"extension":"x","inputs":[]}` + "\ninfo: done\n")); !hasDiagnosticCode(diags, "parse-error") {
		t.Fatalf("trailing result data was accepted: %v", diags)
	}
}

// An error answer omits its basis on the wire, and an answer without an
// error omits the error member.
func TestInputResolveAnswerWireForm(t *testing.T) {
	data, err := json.Marshal(validInputResolveResult())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"extension":"@putnami/go","inputs":[` +
		`{"project":"go/framework/web","selector":"go-embed:build","files":["assets/app.css","assets/app.js","templates/index.html"],` +
		`"basis":{"content":[["**/*.go","go.mod","!vendor/**"]],"listing":[["assets/**"],["templates/*.html"]]}},` +
		`{"project":"go/framework/web","selector":"go-embed:test","error":"web_test.go:12: pattern testdata/*.golden: no matching files found"},` +
		`{"project":".","selector":"go-embed:build","basis":{"content":[["**/*.go","go.mod"]]}}]}`
	if string(data) != want {
		t.Fatalf("wire form:\n got %s\nwant %s", data, want)
	}
}

func TestConformance_InputResolveRequestFixtures(t *testing.T) {
	runShapeConformance(t, "input-resolve-request", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateInputResolveRequest(b)
		return d
	})
}

func TestConformance_InputResolveResultFixtures(t *testing.T) {
	runShapeConformance(t, "input-resolve-result", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateInputResolveResult(b)
		return d
	})
}

// Each valid result fixture answers the valid request fixture of the same
// name, so the corpus also exercises the relation between the two documents.
func TestConformance_InputResolveResultFixturesAnswerTheirRequest(t *testing.T) {
	files, err := filepath.Glob("fixtures/input-resolve-result/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid input-resolve result fixture")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures/input-resolve-request/valid", filepath.Base(path)))
			if err != nil {
				t.Fatalf("no request fixture of the same name: %v", err)
			}
			request, diags := ParseAndValidateInputResolveRequest(data)
			if diag.HasErrors(diags) {
				t.Fatalf("request fixture rejected: %v", diags)
			}
			if data, err = os.ReadFile(path); err != nil {
				t.Fatal(err)
			}
			result, diags := ParseAndValidateInputResolveResult(data)
			if diag.HasErrors(diags) {
				t.Fatalf("result fixture rejected: %v", diags)
			}
			if diags := ValidateInputResolveAnswer(request, result); len(diags) != 0 {
				t.Fatalf("%s does not answer its request: %v", path, diags)
			}
		})
	}
}

func TestDrift_InputResolveResult(t *testing.T) {
	assertFieldParity(t, "InputResolveResult",
		jsonSchemaProperties(t, "schemas/input-resolve.json"),
		goTypeJSONFields(t, InputResolveResult{}))
}

func TestDrift_InputResolveRequest(t *testing.T) {
	assertFieldParity(t, "InputResolveRequest",
		jsonSchemaDefinitionProperties(t, "schemas/input-resolve.json", "inputResolveRequest"),
		goTypeJSONFields(t, InputResolveRequest{}))
}

func TestDrift_InputResolveQuery(t *testing.T) {
	assertFieldParity(t, "InputResolveQuery",
		jsonSchemaDefinitionProperties(t, "schemas/input-resolve.json", "inputResolveQuery"),
		goTypeJSONFields(t, InputResolveQuery{}))
}

func TestDrift_InputResolveAnswer(t *testing.T) {
	assertFieldParity(t, "InputResolveAnswer",
		jsonSchemaDefinitionProperties(t, "schemas/input-resolve.json", "inputResolveAnswer"),
		goTypeJSONFields(t, InputResolveAnswer{}))
}

func TestDrift_InputBasis(t *testing.T) {
	assertFieldParity(t, "InputBasis",
		jsonSchemaDefinitionProperties(t, "schemas/input-resolve.json", "inputBasis"),
		goTypeJSONFields(t, InputBasis{}))
}

// schemas/input-resolve.json states the selector, path and pattern grammars
// and the bounds the validator applies.
func TestInputResolveSchemaAgreesWithTheValidator(t *testing.T) {
	type rule struct {
		Pattern   string `json:"pattern"`
		MaxLength int    `json:"maxLength"`
		Not       *rule  `json:"not"`
		Ref       string `json:"$ref"`
		AnyOf     []rule `json:"anyOf"`
	}
	var schema struct {
		Definitions struct {
			Selector rule `json:"selector"`
			Path     rule `json:"path"`
			Project  rule `json:"project"`
			Pattern  rule `json:"pattern"`
			Answer   struct {
				Properties struct {
					Error rule `json:"error"`
				} `json:"properties"`
			} `json:"inputResolveAnswer"`
		} `json:"definitions"`
	}
	data, err := os.ReadFile("schemas/input-resolve.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	defs := schema.Definitions
	if defs.Selector.Pattern != inputSelectorNamePattern || defs.Selector.MaxLength != inputSelectorNameMaxBytes {
		t.Errorf("schema selector = %q (max %d), the validator applies %q (max %d)",
			defs.Selector.Pattern, defs.Selector.MaxLength, inputSelectorNamePattern, inputSelectorNameMaxBytes)
	}
	if defs.Selector.Not == nil || defs.Selector.Not.Pattern != "^"+inputSelectorReservedScheme+":" {
		t.Errorf("schema selector does not refuse the reserved %q scheme: %+v", inputSelectorReservedScheme, defs.Selector.Not)
	}
	if defs.Path.Pattern != inputPathPattern {
		t.Errorf("schema path = %q, the validator applies %q", defs.Path.Pattern, inputPathPattern)
	}
	wantPattern := "^!?" + strings.TrimPrefix(inputPathPattern, "^")
	if defs.Pattern.Pattern != wantPattern {
		t.Errorf("schema pattern = %q, the validator applies %q", defs.Pattern.Pattern, wantPattern)
	}
	if defs.Pattern.Not == nil || defs.Pattern.Not.Pattern != "^!?git:" {
		t.Errorf("schema pattern does not refuse the git: prefix: %+v", defs.Pattern.Not)
	}
	if len(defs.Project.AnyOf) != 2 || defs.Project.AnyOf[0].Pattern != `^\.$` || defs.Project.AnyOf[1].Ref != "#/definitions/path" {
		t.Errorf("schema project is not %q or a path: %+v", ProbeRootPath, defs.Project)
	}
	if got := defs.Answer.Properties.Error.MaxLength; got != maxInputResolveErrorBytes {
		t.Errorf("schema caps an error at %d bytes, the validator at %d", got, maxInputResolveErrorBytes)
	}
}

// Each invalid fixture breaks one rule, so the coverage test's code names the
// reason it fails.
func TestInputResolveInvalidFixturesFailForOneReason(t *testing.T) {
	for shape, parse := range map[string]func([]byte) []diag.Diagnostic{
		"input-resolve-request": func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateInputResolveRequest(b); return d },
		"input-resolve-result":  func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateInputResolveResult(b); return d },
	} {
		files, err := filepath.Glob(filepath.Join("fixtures", shape, "invalid", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if errs := diag.Errors(parse(data)); len(errs) != 1 {
				t.Errorf("%s fails for %d reasons, want one: %v", path, len(errs), errs)
			}
		}
	}
}
