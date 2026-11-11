package ci

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

const validRootVerdictPolicy = `{
  "$schema": "https://putnami.dev/schemas/putnami-verdict-policy.json",
  "declarations": {
    "adr": "never",
    "ci": "always",
    "spec": { "modeled": "when-weakening", "default-on": "always" }
  },
  "gaps": {
    "no-requirement": "inform",
    "unknown-impact": "block",
    "unknown-declarations": "accept",
    "unknown-requirements": "accept",
    "unproven-requirement": { "modeled": "inform", "coded": "accept" },
    "lost-proof": { "modeled": "accept", "live-verified": "block" }
  }
}`

func TestParseVerdictPolicyAdmitsAValidFile(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "a-verdict-policy-is-validated", "a-valid-verdict-policy-is-admitted")
	policy, err := ParseVerdictPolicy(VerdictPolicyFile, []byte(validRootVerdictPolicy))
	if err != nil {
		t.Fatalf("ParseVerdictPolicy: %v", err)
	}
	if got := policy.Gaps[PolicyLostProof].ByMaturity["live-verified"]; got != PolicyBlock {
		t.Fatalf("lost-proof from live-verified = %q, want block", got)
	}
	if got := policy.Declarations[DeclarationADR].Value; got != PolicyNever {
		t.Fatalf("adr = %q, want never", got)
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseVerdictPolicy(VerdictPolicyFile, encoded)
	if err != nil {
		t.Fatalf("the encoded policy does not parse again: %v", err)
	}
	if !slices.Equal(sortedMapKeys(again.Gaps), sortedMapKeys(policy.Gaps)) {
		t.Fatalf("round trip lost gaps: %v", again.Gaps)
	}
	for _, filePath := range []string{"libs/core/" + VerdictPolicyFile, "a/" + VerdictPolicyFile} {
		if _, err := ParseVerdictPolicy(filePath, []byte(`{"gaps": {"no-requirement": "block"}}`)); err != nil {
			t.Errorf("%s: %v", filePath, err)
		}
	}
	if _, err := ParseVerdictPolicy(VerdictPolicyFile, []byte(`{}`)); err != nil {
		t.Errorf("an empty policy: %v", err)
	}
}

func TestEachVerdictPolicyDefectIsRefused(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "a-verdict-policy-is-validated", "each-verdict-policy-defect-is-refused")
	project := "libs/core/" + VerdictPolicyFile
	for name, test := range map[string]struct {
		path, body, code, field string
	}{
		"another file name":                   {"libs/core/policy.json", `{}`, "ci.verdict_policy_path", "(document)"},
		"an absolute path":                    {"/" + VerdictPolicyFile, `{}`, "ci.verdict_policy_path", "(document)"},
		"a dot segment":                       {"a/../" + VerdictPolicyFile, `{}`, "ci.verdict_policy_path", "(document)"},
		"a backslash":                         {`a\` + VerdictPolicyFile, `{}`, "ci.verdict_policy_path", "(document)"},
		"another schema":                      {project, `{"$schema": "https://putnami.dev/schemas/putnami-ci.json"}`, "ci.verdict_policy_schema", "$schema"},
		"an unknown declaration kind":         {project, `{"declarations": {"readme": "never"}}`, "ci.verdict_policy_kind", "declarations.readme"},
		"the policy kind":                     {project, `{"declarations": {"policy": "never"}}`, "ci.verdict_policy_kind", "declarations.policy"},
		"an unknown gap kind":                 {project, `{"gaps": {"flaky": "inform"}}`, "ci.verdict_policy_kind", "gaps.flaky"},
		"the unapproved declaration":          {project, `{"gaps": {"unapproved-declaration": "inform"}}`, "ci.verdict_policy_kind", "gaps.unapproved-declaration"},
		"unknown-impact in a project":         {project, `{"gaps": {"unknown-impact": "inform"}}`, "ci.verdict_policy_root_only", "gaps.unknown-impact"},
		"unknown-declarations deeper":         {project, `{"gaps": {"unknown-declarations": "inform"}}`, "ci.verdict_policy_root_only", "gaps.unknown-declarations"},
		"a gap value":                         {project, `{"gaps": {"no-requirement": "never"}}`, "ci.verdict_policy_value", "gaps.no-requirement"},
		"a declaration value":                 {project, `{"declarations": {"adr": "block"}}`, "ci.verdict_policy_value", "declarations.adr"},
		"a maturity map without a feature":    {project, `{"gaps": {"no-requirement": {"modeled": "inform"}}}`, "ci.verdict_policy_maturity", "gaps.no-requirement"},
		"a declaration map without a feature": {project, `{"declarations": {"adr": {"modeled": "never"}}}`, "ci.verdict_policy_maturity", "declarations.adr"},
		"a map without modeled":               {project, `{"gaps": {"lost-proof": {"ga": "block"}}}`, "ci.verdict_policy_maturity", "gaps.lost-proof"},
		"an unknown maturity":                 {project, `{"gaps": {"lost-proof": {"modeled": "accept", "beta": "block"}}}`, "ci.verdict_policy_maturity", "gaps.lost-proof.beta"},
		"a value inside a map":                {project, `{"declarations": {"spec": {"modeled": "inform"}}}`, "ci.verdict_policy_value", "declarations.spec.modeled"},
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := ParseVerdictPolicyWithDiagnostics(test.path, []byte(test.body))
			if !hasDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("diagnostics %v, want %s at %s", diagnostics, test.code, test.field)
			}
			if _, err := ParseVerdictPolicy(test.path, []byte(test.body)); err == nil {
				t.Fatal("ParseVerdictPolicy admitted the defect")
			}
		})
	}
}

func TestParseVerdictPolicyIsStrictAndBounded(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "verdict-policies-parse-strictly-against-one-schema", "parse-verdict-policy-is-strict-and-bounded")
	large := `{"gaps": {"no-requirement": "inform"}}` + strings.Repeat(" ", MaxVerdictPolicyBytes)
	for name, test := range map[string]struct{ body, code string }{
		"too large":         {large, "ci.verdict_policy_too_large"},
		"a duplicate key":   {`{"gaps": {"no-requirement": "inform", "no-requirement": "block"}}`, "ci.duplicate_key"},
		"an unknown member": {`{"projects": {}}`, "ci.invalid_json"},
		// The token walk that finds duplicate keys also refuses a second value.
		"a second value":  {`{} {}`, "ci.duplicate_key"},
		"not an object":   {`[]`, "ci.invalid_json"},
		"a number value":  {`{"gaps": {"no-requirement": 1}}`, "ci.invalid_json"},
		"a nested object": {`{"gaps": {"lost-proof": {"modeled": {"x": "y"}}}}`, "ci.invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := ParseVerdictPolicyWithDiagnostics(VerdictPolicyFile, []byte(test.body))
			if !hasDiagnostic(diagnostics, test.code, "(document)") {
				t.Fatalf("diagnostics %v, want %s", diagnostics, test.code)
			}
		})
	}
	exact := []byte(`{}` + strings.Repeat(" ", MaxVerdictPolicyBytes-2))
	if _, err := ParseVerdictPolicy(VerdictPolicyFile, exact); err != nil {
		t.Fatalf("a file of exactly MaxVerdictPolicyBytes: %v", err)
	}
}

func TestVerdictPolicySchemaIsEmbeddedAndClosed(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "verdict-policies-parse-strictly-against-one-schema", "the-verdict-policy-schema-is-closed")
	schema := verdictPolicySchema(t)
	if schema["$id"] != VerdictPolicySchemaURL {
		t.Fatalf("schema $id = %v, want %s", schema["$id"], VerdictPolicySchemaURL)
	}
	closed := 0
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch value := node.(type) {
		case map[string]any:
			if value["type"] == "object" {
				closed++
				if value["additionalProperties"] != false {
					t.Errorf("%s is an object schema that is not closed", path)
				}
			}
			for key, child := range value {
				walk(path+"/"+key, child)
			}
		case []any:
			for i, child := range value {
				walk(fmt.Sprintf("%s/%d", path, i), child)
			}
		}
	}
	walk("#", schema)
	// The root, declarations, gaps, and the two maps by maturity.
	if closed != 5 {
		t.Fatalf("schema has %d object schemas, want 5", closed)
	}
	copied := VerdictPolicySchema()
	copied[0] = 'x'
	if VerdictPolicySchema()[0] != '{' {
		t.Fatal("VerdictPolicySchema shares its bytes with the caller")
	}
}

// TestVerdictPolicySchemaTracksTheGoVocabulary pins the schema to the Go
// validator: the same keys, the same values, and a map by maturity exactly
// on the keys that concern a feature.
func TestVerdictPolicySchemaTracksTheGoVocabulary(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "verdict-policies-parse-strictly-against-one-schema", "the-verdict-policy-schema-tracks-the-go-vocabulary")
	schema := verdictPolicySchema(t)
	declarationKinds := slices.DeleteFunc(slices.Clone(verdictDeclarationKinds), func(kind string) bool { return kind == DeclarationPolicy })
	slices.Sort(declarationKinds)
	gapKeys := slices.Sorted(slices.Values(verdictPolicyGapKeys))
	for _, test := range []struct {
		path      string
		keys      []string
		byFeature []string
		single    string
		mapped    string
	}{
		{"properties/declarations", declarationKinds, verdictPolicyFeatureDeclarationKeys, "#/$defs/approval", "#/$defs/approvalByMaturity"},
		{"properties/gaps", gapKeys, verdictPolicyFeatureGapKeys, "#/$defs/cost", "#/$defs/costByMaturity"},
	} {
		properties := schemaNode(t, schema, test.path)["properties"].(map[string]any)
		if got := sortedMapKeys(properties); !slices.Equal(got, test.keys) {
			t.Errorf("%s keys %v, Go %v", test.path, got, test.keys)
		}
		for key, node := range properties {
			want := test.single
			if slices.Contains(test.byFeature, key) {
				want = test.mapped
			}
			if ref := node.(map[string]any)["$ref"]; ref != want {
				t.Errorf("%s/%s refers to %v, want %s", test.path, key, ref, want)
			}
		}
	}
	for path, values := range map[string][]string{
		"$defs/approval": verdictPolicyDeclarationValues,
		"$defs/cost":     verdictPolicyGapValues,
	} {
		var got []string
		for _, value := range schemaValue(t, schema, path+"/enum").([]any) {
			got = append(got, value.(string))
		}
		if !slices.Equal(sortedStrings(got), sortedStrings(values)) {
			t.Errorf("%s enum %v, Go %v", path, got, values)
		}
	}
	for _, path := range []string{"$defs/approvalByMaturity/oneOf/1", "$defs/costByMaturity/oneOf/1"} {
		node := schemaNode(t, schema, path)
		if got := sortedMapKeys(node["properties"].(map[string]any)); !slices.Equal(got, sortedStrings(verdictPolicyMaturities)) {
			t.Errorf("%s maturities %v, Go %v", path, got, verdictPolicyMaturities)
		}
		if required := node["required"].([]any); len(required) != 1 || required[0] != verdictPolicyMaturities[0] {
			t.Errorf("%s requires %v, want [%s]", path, required, verdictPolicyMaturities[0])
		}
	}
}

func TestThePolicyMaturitiesAreTheFeaturesLadder(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-feature-maturity-picks-the-value", "the-policy-maturities-are-the-features-ladder")
	var ladder []string
	for _, stage := range features.OrderedMaturityStages() {
		ladder = append(ladder, string(stage))
	}
	if !slices.Equal(VerdictPolicyMaturities(), ladder) {
		t.Fatalf("policy maturities %v, features ladder %v", VerdictPolicyMaturities(), ladder)
	}
}

func TestThePolicyPathsClimbToTheRoot(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-nearest-verdict-policy-decides", "the-policy-paths-climb-to-the-root")
	for _, test := range []struct {
		got  []string
		want []string
	}{
		{ProjectVerdictPolicyPaths("libs/core"), []string{"libs/core/" + VerdictPolicyFile, "libs/" + VerdictPolicyFile, VerdictPolicyFile}},
		{ProjectVerdictPolicyPaths("/libs/core"), []string{"libs/core/" + VerdictPolicyFile, "libs/" + VerdictPolicyFile, VerdictPolicyFile}},
		{ProjectVerdictPolicyPaths(""), []string{VerdictPolicyFile}},
		{ProjectVerdictPolicyPaths("/"), []string{VerdictPolicyFile}},
		// The directory keeps a grouping folder that the project's ID, /apps/site, leaves out.
		{ProjectVerdictPolicyPaths("apps/(web)/site"), []string{"apps/(web)/site/" + VerdictPolicyFile, "apps/(web)/" + VerdictPolicyFile, "apps/" + VerdictPolicyFile, VerdictPolicyFile}},
		{DeclarationVerdictPolicyPaths("libs/core/specs/api.json"), []string{"libs/core/specs/" + VerdictPolicyFile, "libs/core/" + VerdictPolicyFile, "libs/" + VerdictPolicyFile, VerdictPolicyFile}},
		{DeclarationVerdictPolicyPaths("CODEOWNERS"), []string{VerdictPolicyFile}},
		{DeclarationVerdictPolicyPaths("libs/core/" + VerdictPolicyFile), []string{"libs/core/" + VerdictPolicyFile, "libs/" + VerdictPolicyFile, VerdictPolicyFile}},
	} {
		if !slices.Equal(test.got, test.want) {
			t.Errorf("paths %v, want %v", test.got, test.want)
		}
	}
}

func TestTheNearestFileThatNamesAKeyDecides(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-nearest-verdict-policy-decides", "the-nearest-file-that-names-a-key-decides")
	chain := VerdictPolicyChain{
		mustPolicyAt(t, "libs/core/"+VerdictPolicyFile, `{"gaps": {"no-requirement": "block"}}`),
		mustPolicyAt(t, "libs/"+VerdictPolicyFile, `{"gaps": {"no-requirement": "inform", "unknown-requirements": "inform"}, "declarations": {"adr": "never"}}`),
		mustPolicyAt(t, VerdictPolicyFile, `{"gaps": {"unknown-requirements": "block", "unknown-impact": "block"}, "declarations": {"adr": "always", "ci": "when-weakening"}}`),
	}
	for _, test := range []struct {
		kind, value, by string
		ok              bool
	}{
		{GapNoRequirement, PolicyBlock, "libs/core/" + VerdictPolicyFile, true},
		{GapUnknownRequirements, PolicyInform, "libs/" + VerdictPolicyFile, true},
		{GapUnknownImpact, PolicyBlock, VerdictPolicyFile, true},
		{GapUnprovenRequirement, "", "", false},
	} {
		value, by, ok := chain.Gap(test.kind, "")
		if value != test.value || by != test.by || ok != test.ok {
			t.Errorf("Gap(%s) = %q, %q, %v; want %q, %q, %v", test.kind, value, by, ok, test.value, test.by, test.ok)
		}
	}
	if value, by, _ := chain.Declaration(DeclarationADR, ""); value != PolicyNever || by != "libs/"+VerdictPolicyFile {
		t.Errorf("Declaration(adr) = %q by %q", value, by)
	}
	if value, by, _ := chain.Declaration(DeclarationCI, ""); value != PolicyWhenWeakening || by != VerdictPolicyFile {
		t.Errorf("Declaration(ci) = %q by %q", value, by)
	}
	// A key of the change is read from the root file only, even when a
	// chain built without validation carries it deeper.
	deeper := VerdictPolicyChain{
		{Path: "libs/" + VerdictPolicyFile, Policy: VerdictPolicy{Gaps: map[string]VerdictPolicyValue{GapUnknownImpact: {Value: PolicyInform}}}},
	}
	if _, _, ok := deeper.Gap(GapUnknownImpact, ""); ok {
		t.Error("a key of the change was read from a project file")
	}
}

func TestAPolicyChangeAlwaysWaits(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-nearest-verdict-policy-decides", "a-policy-change-always-waits")
	var empty VerdictPolicyChain
	if value, by, ok := empty.Declaration(DeclarationPolicy, ""); value != PolicyAlways || by != "" || !ok {
		t.Fatalf("Declaration(policy) = %q, %q, %v; want always, decided by no file", value, by, ok)
	}
	if !slices.Contains(verdictDeclarationKinds, DeclarationPolicy) {
		t.Fatal("a verdict cannot list a policy declaration")
	}
}

func TestTheHighestMaturityReachedApplies(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-feature-maturity-picks-the-value", "the-highest-maturity-reached-applies")
	chain := VerdictPolicyChain{mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy)}
	for _, test := range []struct{ maturity, want string }{
		{"modeled", PolicyAccept},
		{"coded", PolicyAccept},
		{"default-on", PolicyAccept},
		{"live-verified", PolicyBlock},
		{"ga", PolicyBlock},
	} {
		if got, _, _ := chain.Gap(PolicyLostProof, test.maturity); got != test.want {
			t.Errorf("lost-proof at %s = %q, want %q", test.maturity, got, test.want)
		}
	}
	for _, test := range []struct{ maturity, want string }{
		{"modeled", PolicyInform},
		{"coded", PolicyAccept},
		{"ga", PolicyAccept},
	} {
		if got, _, _ := chain.Gap(GapUnprovenRequirement, test.maturity); got != test.want {
			t.Errorf("unproven-requirement at %s = %q, want %q", test.maturity, got, test.want)
		}
	}
	if got, _, _ := chain.Declaration(DeclarationSpec, "wired"); got != PolicyWhenWeakening {
		t.Errorf("spec at wired = %q, want when-weakening", got)
	}
	if got, _, _ := chain.Declaration(DeclarationSpec, "default-on"); got != PolicyAlways {
		t.Errorf("spec at default-on = %q, want always", got)
	}
	// A single value ignores the maturity.
	if got, _, _ := chain.Gap(GapNoRequirement, "ga"); got != PolicyInform {
		t.Errorf("no-requirement at ga = %q, want inform", got)
	}
}

func TestAnUnknownMaturityTakesTheStrictestValue(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "the-feature-maturity-picks-the-value", "an-unknown-maturity-takes-the-strictest-value")
	chain := VerdictPolicyChain{mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy)}
	for _, maturity := range []string{"", "beta"} {
		if got, _, _ := chain.Gap(PolicyLostProof, maturity); got != PolicyBlock {
			t.Errorf("lost-proof at %q = %q, want block", maturity, got)
		}
		if got, _, _ := chain.Gap(GapUnprovenRequirement, maturity); got != PolicyAccept {
			t.Errorf("unproven-requirement at %q = %q, want accept", maturity, got)
		}
		if got, _, _ := chain.Declaration(DeclarationSpec, maturity); got != PolicyAlways {
			t.Errorf("spec at %q = %q, want always", maturity, got)
		}
	}
}

func TestARemovedOrCheaperRuleLoosens(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "a-loosened-verdict-policy-is-detected", "a-removed-or-cheaper-rule-loosens")
	for name, edit := range map[string]func(*VerdictPolicy){
		"a removed gap":         func(p *VerdictPolicy) { delete(p.Gaps, PolicyLostProof) },
		"a removed declaration": func(p *VerdictPolicy) { delete(p.Declarations, DeclarationCI) },
		"a removed file":        func(p *VerdictPolicy) { *p = VerdictPolicy{} },
		"a cheaper gap":         func(p *VerdictPolicy) { p.Gaps[GapUnknownImpact] = VerdictPolicyValue{Value: PolicyAccept} },
		"a cheaper declaration": func(p *VerdictPolicy) { p.Declarations[DeclarationCI] = VerdictPolicyValue{Value: PolicyWhenWeakening} },
		"a later maturity": func(p *VerdictPolicy) {
			p.Gaps[PolicyLostProof] = VerdictPolicyValue{ByMaturity: map[string]string{"modeled": PolicyAccept, "ga": PolicyBlock}}
		},
		"a single value cheaper at one maturity": func(p *VerdictPolicy) {
			p.Gaps[PolicyLostProof] = VerdictPolicyValue{Value: PolicyAccept}
		},
		"an added declaration below the strictest": func(p *VerdictPolicy) {
			p.Declarations[DeclarationOwners] = VerdictPolicyValue{Value: PolicyWhenWeakening}
		},
		"an added map below the strictest at one maturity": func(p *VerdictPolicy) {
			p.Declarations[DeclarationBinding] = VerdictPolicyValue{ByMaturity: map[string]string{"modeled": PolicyNever, "coded": PolicyAlways}}
		},
	} {
		base := mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy).Policy
		head := mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy).Policy
		edit(&head)
		if !VerdictPolicyLoosens(base, head) {
			t.Errorf("%s: not detected", name)
		}
	}
	added := mustPolicyAt(t, VerdictPolicyFile, `{"gaps": {"no-requirement": "accept"}}`).Policy
	if !VerdictPolicyLoosens(VerdictPolicy{}, added) {
		t.Error("an added file below the strictest: not detected")
	}
}

func TestAStricterRuleDoesNotLoosen(t *testing.T) {
	spectest.Proves(t, "ci/ci-contract", "a-loosened-verdict-policy-is-detected", "a-stricter-rule-does-not-loosen")
	for name, edit := range map[string]func(*VerdictPolicy){
		"the same file":  func(*VerdictPolicy) {},
		"a stricter gap": func(p *VerdictPolicy) { p.Gaps[GapNoRequirement] = VerdictPolicyValue{Value: PolicyAccept} },
		"a stricter approval": func(p *VerdictPolicy) {
			p.Declarations[DeclarationADR] = VerdictPolicyValue{Value: PolicyWhenWeakening}
		},
		"an earlier maturity": func(p *VerdictPolicy) {
			p.Gaps[PolicyLostProof] = VerdictPolicyValue{ByMaturity: map[string]string{"modeled": PolicyAccept, "coded": PolicyBlock}}
		},
		"a map replaced by its strictest value": func(p *VerdictPolicy) {
			p.Gaps[PolicyLostProof] = VerdictPolicyValue{Value: PolicyBlock}
		},
		"an added strictest declaration": func(p *VerdictPolicy) {
			p.Declarations[DeclarationOwners] = VerdictPolicyValue{Value: PolicyAlways}
		},
	} {
		base := mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy).Policy
		head := mustPolicyAt(t, VerdictPolicyFile, validRootVerdictPolicy).Policy
		edit(&head)
		if VerdictPolicyLoosens(base, head) {
			t.Errorf("%s: reported as loosening", name)
		}
	}
	added := mustPolicyAt(t, VerdictPolicyFile, `{"gaps": {"no-requirement": "block"}}`).Policy
	if VerdictPolicyLoosens(VerdictPolicy{}, added) {
		t.Error("an added file at the strictest: reported as loosening")
	}
}

func mustPolicyAt(t *testing.T, filePath, body string) VerdictPolicyFileAt {
	t.Helper()
	policy, err := ParseVerdictPolicy(filePath, []byte(body))
	if err != nil {
		t.Fatalf("%s: %v", filePath, err)
	}
	return VerdictPolicyFileAt{Path: filePath, Policy: policy}
}

func verdictPolicySchema(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(VerdictPolicySchema(), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func hasDiagnostic(diagnostics []diag.Diagnostic, code, field string) bool {
	for _, finding := range diagnostics {
		if finding.Code == code && finding.Field == field {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	return slices.Sorted(slices.Values(values))
}
