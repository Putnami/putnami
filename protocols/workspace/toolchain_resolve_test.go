package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func validToolchainResolveRequest() ToolchainResolveRequest {
	return ToolchainResolveRequest{
		Version:    ToolchainResolveProtocolVersion,
		Extension:  "@putnami/go",
		Identities: []string{"go", "golangci-lint"},
		Locked: map[string]LockToolchainEntry{
			"go": {Version: "1.26.1", Integrities: map[string]string{"darwin/arm64": strings.Repeat("5", 64)}, Source: "https://go.dev/dl/"},
		},
		Platform: "darwin/arm64",
	}
}

func validToolchainResolveResult() ToolchainResolveResult {
	pin := validV5GoEntry()
	return ToolchainResolveResult{
		Version:   ToolchainResolveProtocolVersion,
		Extension: "@putnami/go",
		Toolchains: map[string]ToolchainResolution{
			"go":            {Pin: &pin},
			"golangci-lint": {Problem: &ToolchainProblem{Message: "golangci-lint publishes no darwin/arm64 archive", Remedy: "declare 2.4.0"}},
		},
	}
}

// assertDiagnostic fails unless diags carries code on field; an empty code
// asserts no diagnostic at all.
func assertDiagnostic(t *testing.T, diags []diag.Diagnostic, code, field string) {
	t.Helper()
	if code == "" {
		if len(diags) != 0 {
			t.Fatalf("valid document rejected: %v", diags)
		}
		return
	}
	for _, d := range diags {
		if d.Code == code && d.Field == field {
			return
		}
	}
	t.Fatalf("want %s on %s, got %v", code, field, diags)
}

func TestValidateToolchainResolveRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-toolchain-answer-pins-only-what-was-asked", "a-toolchain-request-and-result-are-validated")
	tooMany := make([]string, maxToolchainResolveIdentities+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("tool%d", i)
	}
	cases := []struct {
		name  string
		edit  func(*ToolchainResolveRequest)
		code  string
		field string
	}{
		{"valid", func(*ToolchainResolveRequest) {}, "", ""},
		{"unsorted identities", func(r *ToolchainResolveRequest) { r.Identities = []string{"golangci-lint", "go"} }, "", ""},
		{"offline without a lock", func(r *ToolchainResolveRequest) { r.Locked, r.Offline = nil, true }, "", ""},
		{"64 identities", func(r *ToolchainResolveRequest) { r.Identities, r.Locked = tooMany[:64], nil }, "", ""},
		{"version", func(r *ToolchainResolveRequest) { r.Version = 2 }, "invalid-toolchain-resolve-version", "version"},
		{"extension", func(r *ToolchainResolveRequest) { r.Extension = " " }, "required-field", "extension"},
		{"no identities", func(r *ToolchainResolveRequest) { r.Identities, r.Locked = nil, nil }, "required-field", "identities"},
		{"65 identities", func(r *ToolchainResolveRequest) { r.Identities, r.Locked = tooMany, nil }, "limit-exceeded", "identities"},
		{"leading digit", func(r *ToolchainResolveRequest) { r.Identities = []string{"go", "1lint"} }, "invalid-value", "identities[1]"},
		{"space", func(r *ToolchainResolveRequest) { r.Identities = []string{"go lang"} }, "invalid-value", "identities[0]"},
		{"129 bytes", func(r *ToolchainResolveRequest) { r.Identities = []string{"go", strings.Repeat("a", 129)} }, "invalid-value", "identities[1]"},
		{"duplicate", func(r *ToolchainResolveRequest) { r.Identities = []string{"go", "go"} }, "duplicate-value", "identities[1]"},
		{"locked not asked", func(r *ToolchainResolveRequest) {
			r.Locked["bun"] = LockToolchainEntry{Version: "1.3.14"}
		}, "unknown-reference", "locked.bun"},
		{"no platform", func(r *ToolchainResolveRequest) { r.Platform = "" }, "required-field", "platform"},
		{"dashed platform", func(r *ToolchainResolveRequest) { r.Platform = "darwin-arm64" }, "invalid-value", "platform"},
		{"uppercase platform", func(r *ToolchainResolveRequest) { r.Platform = "Darwin/arm64" }, "invalid-value", "platform"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := validToolchainResolveRequest()
			tc.edit(&request)
			assertDiagnostic(t, ValidateToolchainResolveRequest(&request), tc.code, tc.field)
		})
	}
	if diags := ValidateToolchainResolveRequest(nil); !hasDiagnosticCode(diags, "nil-request") {
		t.Fatalf("nil request diagnostics = %v", diags)
	}
}

func TestValidateToolchainResolveResult(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-toolchain-answer-pins-only-what-was-asked", "a-toolchain-request-and-result-are-validated")
	cases := []struct {
		name  string
		edit  func(*ToolchainResolveResult)
		code  string
		field string
	}{
		{"valid", func(*ToolchainResolveResult) {}, "", ""},
		{"no toolchains", func(r *ToolchainResolveResult) { r.Toolchains = nil }, "", ""},
		{"version", func(r *ToolchainResolveResult) { r.Version = 0 }, "invalid-toolchain-resolve-version", "version"},
		{"extension", func(r *ToolchainResolveResult) { r.Extension = "" }, "required-field", "extension"},
		{"invalid identity", func(r *ToolchainResolveResult) {
			r.Toolchains["-go"] = r.Toolchains["go"]
		}, "invalid-value", "toolchains.-go"},
		{"neither pin nor problem", func(r *ToolchainResolveResult) {
			r.Toolchains["go"] = ToolchainResolution{}
		}, "required-field", "toolchains.go"},
		{"pin and problem", func(r *ToolchainResolveResult) {
			resolution := r.Toolchains["go"]
			resolution.Problem = &ToolchainProblem{Message: "both"}
			r.Toolchains["go"] = resolution
		}, "conflicting-field", "toolchains.go"},
		{"pin follows the lock version 5 rules", func(r *ToolchainResolveResult) {
			r.Toolchains["go"].Pin.Source = "https://go.dev/dl/"
		}, "field-version", "toolchains.go.pin.source"},
		{"pin without archives", func(r *ToolchainResolveResult) {
			r.Toolchains["go"].Pin.Archives = nil
		}, "required-field", "toolchains.go.pin.archives"},
		{"blank problem", func(r *ToolchainResolveResult) {
			r.Toolchains["golangci-lint"].Problem.Message = " \n"
		}, "required-field", "toolchains.golangci-lint.problem.message"},
		{"1024-byte problem", func(r *ToolchainResolveResult) {
			r.Toolchains["golangci-lint"].Problem.Message = strings.Repeat("m", 1024)
			r.Toolchains["golangci-lint"].Problem.Remedy = strings.Repeat("r", 1024)
		}, "", ""},
		{"oversized problem", func(r *ToolchainResolveResult) {
			r.Toolchains["golangci-lint"].Problem.Message = strings.Repeat("m", 1025)
		}, "limit-exceeded", "toolchains.golangci-lint.problem.message"},
		{"oversized remedy", func(r *ToolchainResolveResult) {
			r.Toolchains["golangci-lint"].Problem.Remedy = strings.Repeat("r", 1025)
		}, "limit-exceeded", "toolchains.golangci-lint.problem.remedy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := validToolchainResolveResult()
			tc.edit(&result)
			assertDiagnostic(t, ValidateToolchainResolveResult(&result), tc.code, tc.field)
		})
	}
	if diags := ValidateToolchainResolveResult(nil); !hasDiagnosticCode(diags, "nil-result") {
		t.Fatalf("nil result diagnostics = %v", diags)
	}
}

func TestValidateToolchainResolveAnswer(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-toolchain-answer-pins-only-what-was-asked", "a-toolchain-answer-matches-its-request")
	cases := []struct {
		name  string
		edit  func(*ToolchainResolveRequest, *ToolchainResolveResult)
		code  string
		field string
	}{
		{"valid", func(*ToolchainResolveRequest, *ToolchainResolveResult) {}, "", ""},
		{"asked identity omitted", func(_ *ToolchainResolveRequest, r *ToolchainResolveResult) {
			delete(r.Toolchains, "golangci-lint")
		}, "", ""},
		{"problem for the host", func(_ *ToolchainResolveRequest, r *ToolchainResolveResult) {
			r.Toolchains["go"] = ToolchainResolution{Problem: &ToolchainProblem{Message: "no darwin/arm64 archive"}}
		}, "", ""},
		{"other extension", func(_ *ToolchainResolveRequest, r *ToolchainResolveResult) {
			r.Extension = "@putnami/typescript"
		}, "invalid-value", "extension"},
		{"unasked identity", func(_ *ToolchainResolveRequest, r *ToolchainResolveResult) {
			r.Toolchains["bun"] = ToolchainResolution{Problem: &ToolchainProblem{Message: "not mine"}}
		}, "unknown-reference", "toolchains.bun"},
		{"pin without the host integrity", func(q *ToolchainResolveRequest, _ *ToolchainResolveResult) {
			q.Platform = "windows/arm64"
		}, "required-field", "toolchains.go.pin.integrities.windows/arm64"},
		{"pin without the host archive", func(q *ToolchainResolveRequest, _ *ToolchainResolveResult) {
			q.Platform = "windows/arm64"
		}, "required-field", "toolchains.go.pin.archives.windows/arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, result := validToolchainResolveRequest(), validToolchainResolveResult()
			tc.edit(&request, &result)
			assertDiagnostic(t, ValidateToolchainResolveAnswer(&request, &result), tc.code, tc.field)
		})
	}
	request, result := validToolchainResolveRequest(), validToolchainResolveResult()
	if diags := ValidateToolchainResolveAnswer(nil, &result); !hasDiagnosticCode(diags, "nil-request") {
		t.Fatalf("nil request diagnostics = %v", diags)
	}
	if diags := ValidateToolchainResolveAnswer(&request, nil); !hasDiagnosticCode(diags, "nil-result") {
		t.Fatalf("nil result diagnostics = %v", diags)
	}
}

func TestToolchainResolveDiagnosticsAreDeterministic(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-toolchain-answer-pins-only-what-was-asked", "a-toolchain-request-and-result-are-validated")
	request := validToolchainResolveRequest()
	request.Locked = map[string]LockToolchainEntry{"z": {}, "b": {}, "m": {}}
	result := validToolchainResolveResult()
	for _, identity := range []string{"zig", "bun", "node"} {
		result.Toolchains[identity] = ToolchainResolution{}
	}
	for name, validate := range map[string]func() []diag.Diagnostic{
		"request": func() []diag.Diagnostic { return ValidateToolchainResolveRequest(&request) },
		"result":  func() []diag.Diagnostic { return ValidateToolchainResolveResult(&result) },
		"answer":  func() []diag.Diagnostic { return ValidateToolchainResolveAnswer(&request, &result) },
	} {
		canonical := validate()
		if len(canonical) < 3 {
			t.Fatalf("%s: want several diagnostics, got %v", name, canonical)
		}
		for i := 0; i < 100; i++ {
			if got := validate(); !reflect.DeepEqual(got, canonical) {
				t.Fatalf("%s: iteration %d produced non-deterministic diagnostics:\nwant %v\ngot %v", name, i, canonical, got)
			}
		}
	}
}

func TestParseToolchainResolveRejectsTrailingDocument(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-toolchain-answer-pins-only-what-was-asked", "a-toolchain-request-and-result-are-validated")
	if _, diags := ParseToolchainResolveRequest([]byte(`{"version":1,"extension":"x","identities":["go"],"platform":"linux/amd64"} {}`)); !hasDiagnosticCode(diags, "parse-error") {
		t.Fatalf("trailing request document was accepted: %v", diags)
	}
	if _, diags := ParseToolchainResolveResult([]byte(`{"version":1,"extension":"x"}` + "\ninfo: done\n")); !hasDiagnosticCode(diags, "parse-error") {
		t.Fatalf("trailing result data was accepted: %v", diags)
	}
}

func TestConformance_ToolchainResolveRequestFixtures(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-fixture-corpus-holds-every-shape", "every-fixture-gets-its-verdict")
	runShapeConformance(t, "toolchain-resolve-request", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateToolchainResolveRequest(b)
		return d
	})
}

func TestConformance_ToolchainResolveResultFixtures(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-fixture-corpus-holds-every-shape", "every-fixture-gets-its-verdict")
	runShapeConformance(t, "toolchain-resolve-result", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateToolchainResolveResult(b)
		return d
	})
}

// Each valid result fixture is an answer to the full request fixture, so the
// corpus also exercises the relation between the two documents.
func TestConformance_ToolchainResolveResultFixturesAnswerTheFullRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-fixture-corpus-holds-every-shape", "each-valid-result-fixture-answers-its-request")
	data, err := os.ReadFile("fixtures/toolchain-resolve-request/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	request, diags := ParseAndValidateToolchainResolveRequest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("full request fixture rejected: %v", diags)
	}
	files, err := filepath.Glob("fixtures/toolchain-resolve-result/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result, diags := ParseAndValidateToolchainResolveResult(data)
			if diag.HasErrors(diags) {
				t.Fatalf("result fixture rejected: %v", diags)
			}
			if diags := ValidateToolchainResolveAnswer(request, result); len(diags) != 0 {
				t.Fatalf("%s does not answer the full request: %v", path, diags)
			}
		})
	}
}

func TestDrift_ToolchainResolveResult(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "every-schema-matches-its-go-type")
	assertFieldParity(t, "ToolchainResolveResult",
		jsonSchemaProperties(t, "schemas/toolchain-resolve.json"),
		goTypeJSONFields(t, ToolchainResolveResult{}))
}

func TestDrift_ToolchainResolveRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "every-schema-matches-its-go-type")
	assertFieldParity(t, "ToolchainResolveRequest",
		jsonSchemaDefinitionProperties(t, "schemas/toolchain-resolve.json", "toolchainResolveRequest"),
		goTypeJSONFields(t, ToolchainResolveRequest{}))
}

func TestDrift_ToolchainResolution(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "every-schema-matches-its-go-type")
	assertFieldParity(t, "ToolchainResolution",
		jsonSchemaDefinitionProperties(t, "schemas/toolchain-resolve.json", "toolchainResolution"),
		goTypeJSONFields(t, ToolchainResolution{}))
}

func TestDrift_ToolchainProblem(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "every-schema-matches-its-go-type")
	assertFieldParity(t, "ToolchainProblem",
		jsonSchemaDefinitionProperties(t, "schemas/toolchain-resolve.json", "toolchainProblem"),
		goTypeJSONFields(t, ToolchainProblem{}))
}

// A locked entry is any committed toolchain entry, so its schema carries every
// member of the Go type, the version 3 and 4 source included.
func TestDrift_ToolchainResolveLockedEntry(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "every-schema-matches-its-go-type")
	assertFieldParity(t, "LockToolchainEntry (locked)",
		jsonSchemaDefinitionProperties(t, "schemas/toolchain-resolve.json", "lockedToolchainEntry"),
		goTypeJSONFields(t, LockToolchainEntry{}))
}

// schemas/toolchain-resolve.json states the identity and platform grammars
// the validator applies, and takes the pin's shape from schemas/lock.json so
// an answer's pin and a lock entry cannot diverge.
func TestToolchainResolveSchemaAgreesWithTheValidator(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-schemas-and-the-go-types-agree", "schema-rules-agree-with-the-validator")
	type rule struct {
		Pattern   string `json:"pattern"`
		MaxLength int    `json:"maxLength"`
		MaxItems  int    `json:"maxItems"`
		Ref       string `json:"$ref"`
		AllOf     []rule `json:"allOf"`
	}
	var schema struct {
		Properties struct {
			Toolchains struct {
				PropertyNames rule `json:"propertyNames"`
			} `json:"toolchains"`
		} `json:"properties"`
		Definitions struct {
			Identity rule `json:"identity"`
			Platform rule `json:"platform"`
			Request  struct {
				Properties struct {
					Identities struct {
						MaxItems int  `json:"maxItems"`
						Items    rule `json:"items"`
					} `json:"identities"`
					Locked struct {
						PropertyNames rule `json:"propertyNames"`
					} `json:"locked"`
					Platform rule `json:"platform"`
				} `json:"properties"`
			} `json:"toolchainResolveRequest"`
			Resolution struct {
				Properties struct {
					Pin rule `json:"pin"`
				} `json:"properties"`
			} `json:"toolchainResolution"`
			Problem struct {
				Properties map[string]rule `json:"properties"`
			} `json:"toolchainProblem"`
		} `json:"definitions"`
	}
	data, err := os.ReadFile("schemas/toolchain-resolve.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	defs := schema.Definitions
	if defs.Identity.Pattern != lockToolchainIdentityPattern || defs.Identity.MaxLength != lockToolchainIdentityMaxBytes {
		t.Errorf("schema identity = %q (max %d), the validator applies %q (max %d)",
			defs.Identity.Pattern, defs.Identity.MaxLength, lockToolchainIdentityPattern, lockToolchainIdentityMaxBytes)
	}
	if defs.Platform.Pattern != lockPlatformPattern {
		t.Errorf("schema platform = %q, the validator applies %q", defs.Platform.Pattern, lockPlatformPattern)
	}
	for where, ref := range map[string]string{
		"result toolchains keys": schema.Properties.Toolchains.PropertyNames.Ref,
		"request identities":     defs.Request.Properties.Identities.Items.Ref,
		"request locked keys":    defs.Request.Properties.Locked.PropertyNames.Ref,
	} {
		if ref != "#/definitions/identity" {
			t.Errorf("%s reference %q, want the identity definition", where, ref)
		}
	}
	if len(defs.Request.Properties.Platform.AllOf) != 1 || defs.Request.Properties.Platform.AllOf[0].Ref != "#/definitions/platform" {
		t.Errorf("request platform does not reference the platform definition: %+v", defs.Request.Properties.Platform)
	}
	if defs.Request.Properties.Identities.MaxItems != maxToolchainResolveIdentities {
		t.Errorf("schema caps identities at %d, the validator at %d", defs.Request.Properties.Identities.MaxItems, maxToolchainResolveIdentities)
	}
	if pin := defs.Resolution.Properties.Pin; len(pin.AllOf) != 1 || pin.AllOf[0].Ref != "https://putnami.dev/schemas/putnami-lock.json#/definitions/toolchainEntry" {
		t.Errorf("pin does not reference the lock's toolchain entry: %+v", pin)
	}
	for _, name := range []string{"message", "remedy"} {
		if got := defs.Problem.Properties[name].MaxLength; got != maxToolchainProblemBytes {
			t.Errorf("schema caps problem %s at %d, the validator at %d", name, got, maxToolchainProblemBytes)
		}
	}
}
