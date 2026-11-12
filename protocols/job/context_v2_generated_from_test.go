package job

import (
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// contractDigest is a well-formed contract digest.
const contractDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestValidate_V2ProjectGeneratedFrom pins the form of a present
// `project.generatedFrom`: a service, a known language, Go options only on a
// Go client, and a clean workspace-relative contract path when one resolved.
func TestValidate_V2ProjectGeneratedFrom(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "every-reject-branch-has-a-fixture", "every-version-2-reject-branch-has-a-fixture")
	cases := []struct {
		name    string
		from    *GeneratedFrom
		present bool
		code    string
		field   string
	}{
		{name: "absent is accepted"},
		{name: "a resolved go client is accepted", from: &GeneratedFrom{
			Service: "items", Language: GeneratedLanguageGo, PackageName: "itemsclient", ClientName: "ItemsClient",
			OmitOperations: []string{"deleteItem"}, ContractPath: "services/items/schema/openapi.json",
			ContractSHA256: contractDigest,
		}},
		{name: "an unresolved ts client is accepted", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageTypeScript}},
		{name: "present null is rejected", present: true, code: ErrorCodeInvalidValue, field: "project.generatedFrom"},
		{name: "a blank service is rejected", from: &GeneratedFrom{Service: " ", Language: GeneratedLanguageGo},
			code: ErrorCodeMissingField, field: "project.generatedFrom.service"},
		{name: "an unknown language is rejected", from: &GeneratedFrom{Service: "items", Language: "java"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.language"},
		{name: "a Go package name on ts is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageTypeScript, PackageName: "p"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.packageName"},
		{name: "a Go client name on ts is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageTypeScript, ClientName: "C"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.clientName"},
		{name: "omitted operations on ts are rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageTypeScript, OmitOperations: []string{}},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.omitOperations"},
		{name: "a blank omitted operation is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, OmitOperations: []string{"a", ""}},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.omitOperations[1]"},
		{name: "an absolute contract path is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractPath: "/ws/s/schema/openapi.json"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractPath"},
		{name: "an escaping contract path is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractPath: "../s/schema/openapi.json"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractPath"},
		{name: "an unclean contract path is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractPath: "s/./schema/openapi.json"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractPath"},
		{name: "a contract path without its digest is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractPath: "s/schema/openapi.json"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractSha256"},
		{name: "a digest without a contract path is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractSHA256: contractDigest},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractSha256"},
		{name: "a digest that is not lowercase hex sha256 is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo,
			ContractPath: "s/schema/openapi.json", ContractSHA256: "ABC"},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractSha256"},
		{name: "a backslash contract path is rejected", from: &GeneratedFrom{Service: "items", Language: GeneratedLanguageGo, ContractPath: `s\schema\openapi.json`},
			code: ErrorCodeInvalidValue, field: "project.generatedFrom.contractPath"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.Project.GeneratedFrom = tc.from
			ctx.presence.projectGeneratedFrom = tc.present
			diags := Validate(ctx)
			if tc.field == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("valid generation binding rejected: %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, tc.code, tc.field)
		})
	}
}

// TestV2ProjectGeneratedFromFixtures pins each fixture to the one diagnostic it
// exists for, so a fixture cannot stay red for an unrelated reason, and reads
// each valid fixture back through the lenient entry point a task uses.
func TestV2ProjectGeneratedFromFixtures(t *testing.T) {
	spectest.Proves(t, "job/execution-context", "strict-producers-lenient-consumers", "every-invalid-fixture-fails-the-strict-parser")
	invalid := []struct {
		fixture string
		code    string
		field   string
	}{
		{"project-generated-from-null.json", ErrorCodeInvalidValue, "project.generatedFrom"},
		{"project-generated-from-missing-service.json", ErrorCodeMissingField, "project.generatedFrom.service"},
		{"project-generated-from-unknown-language.json", ErrorCodeInvalidValue, "project.generatedFrom.language"},
		{"project-generated-from-go-option-on-ts.json", ErrorCodeInvalidValue, "project.generatedFrom.packageName"},
		{"project-generated-from-absolute-contract-path.json", ErrorCodeInvalidValue, "project.generatedFrom.contractPath"},
		{"project-generated-from-invalid-contract-digest.json", ErrorCodeInvalidValue, "project.generatedFrom.contractSha256"},
		{"v1-with-project-generated-from.json", ErrorCodeUnexpectedField, "project.generatedFrom"},
	}
	for _, tc := range invalid {
		t.Run(tc.fixture, func(t *testing.T) {
			_, got := ParseAndValidate(readFixture(t, "fixtures", "v2", "invalid", tc.fixture))
			if len(got) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one", got)
			}
			if got[0].Code != tc.code || got[0].Field != tc.field {
				t.Errorf("diagnostic = (%s, %s), want (%s, %s)", got[0].Code, got[0].Field, tc.code, tc.field)
			}
		})
	}

	resolved, err := Parse(readFixture(t, "fixtures", "v2", "valid", "project-generated-from.json"))
	if err != nil {
		t.Fatal(err)
	}
	from := resolved.Project.GeneratedFrom
	if from == nil {
		t.Fatal("project.generatedFrom = nil, want the binding the fixture carries")
	}
	if from.Service != "items" || from.Language != GeneratedLanguageGo || from.PackageName != "itemsclient" ||
		from.ClientName != "ItemsClient" || !slices.Equal(from.OmitOperations, []string{"deleteItem"}) ||
		from.ContractPath != "services/items/schema/openapi.json" || from.ContractSHA256 != contractDigest {
		t.Errorf("project.generatedFrom = %+v, want the fixture's binding", *from)
	}

	unresolved, err := Parse(readFixture(t, "fixtures", "v2", "valid", "project-generated-from-unresolved.json"))
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.Project.GeneratedFrom == nil || unresolved.Project.GeneratedFrom.ContractPath != "" {
		t.Errorf("project.generatedFrom = %+v, want a binding with no contract path", unresolved.Project.GeneratedFrom)
	}
}
