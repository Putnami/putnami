package features

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func reviewOnlyMark() *bool {
	mark := true
	return &mark
}

func scaleToZero() Decision {
	return Decision{
		ID:        "D-001",
		Statement: "serverless workloads scale to zero",
		Settled:   "2026-09-03",
		SettledBy: "fdumay",
		Check: &DecisionCheck{
			Kind:    DecisionCheckKindJSONValue,
			Files:   []string{"**/infra/requirements.json"},
			Pointer: "/scaling/minInstances",
			Rule:    DecisionRuleEquals,
			Value:   json.RawMessage(`0`),
		},
	}
}

func registryOf(decisions ...Decision) *DecisionRegistry {
	return &DecisionRegistry{ProtocolVersion: DecisionsProtocolVersion, Decisions: decisions}
}

// TestDecisionRegistryRoundTripsCanonically pins the committed file's byte
// contract: unsorted authored content re-emits sorted with the published
// schema URL, and the emitted bytes pass their own strict reader.
func TestDecisionRegistryRoundTripsCanonically(t *testing.T) {
	authored := registryOf(
		Decision{ID: "D-002", Statement: "every pull request names what it deletes", Settled: "2026-09-03", SettledBy: "fdumay", ReviewOnly: reviewOnlyMark()},
		scaleToZero(),
	)
	authored.Decisions[1].Check.Files = []string{"z/**/deploy.json", "**/infra/requirements.json"}

	encoded, err := MarshalDecisionRegistry(authored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), DecisionsSchemaURL) {
		t.Fatalf("canonical bytes do not stamp the schema URL:\n%s", encoded)
	}
	parsed, findings := ParseAndValidateDecisionRegistry(encoded)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("canonical bytes failed their own reader: %v", findings)
	}
	if parsed.Decisions[0].ID != "D-001" || parsed.Decisions[1].ID != "D-002" {
		t.Fatalf("decisions are not sorted by id: %+v", parsed.Decisions)
	}
	if got := parsed.Decisions[0].Check.Files; got[0] != "**/infra/requirements.json" || got[1] != "z/**/deploy.json" {
		t.Fatalf("check globs are not sorted: %v", got)
	}
	// Canonicalization copies rather than reorders the caller's slices.
	if authored.Decisions[1].Check.Files[0] != "z/**/deploy.json" {
		t.Fatal("CanonicalDecisionRegistry sorted a caller-owned glob slice in place")
	}
	again, err := MarshalDecisionRegistry(parsed)
	if err != nil || string(again) != string(encoded) {
		t.Fatalf("canonical form is not stable:\n%s\n%s", encoded, again)
	}
}

// TestDecisionRegistryIsStrictOnTheWire keeps the registry fail-closed on the
// three shapes a reader must never guess at: a wrong or missing protocol
// version, an unknown member, and an explicit null.
func TestDecisionRegistryIsStrictOnTheWire(t *testing.T) {
	valid := `{"protocolVersion":1,"decisions":[{"id":"D-1","statement":"s","settled":"2026-01-02","settledBy":"a","reviewOnly":true}]}`
	if registry, findings := ParseAndValidateDecisionRegistry([]byte(valid)); registry == nil || diag.HasErrors(findings) {
		t.Fatalf("valid registry rejected: %v", findings)
	}
	for name, input := range map[string]string{
		"no version":      `{"decisions":[]}`,
		"float version":   `{"protocolVersion":1.0,"decisions":[]}`,
		"future version":  `{"protocolVersion":2,"decisions":[]}`,
		"unknown member":  `{"protocolVersion":1,"decisions":[],"policy":"strict"}`,
		"explicit null":   `{"protocolVersion":1,"decisions":null}`,
		"trailing value":  `{"protocolVersion":1,"decisions":[]} {}`,
		"not an object":   `[]`,
		"unknown in item": `{"protocolVersion":1,"decisions":[{"id":"D-1","statement":"s","settled":"2026-01-02","settledBy":"a","reviewOnly":true,"why":"x"}]}`,
	} {
		if registry, findings := ParseAndValidateDecisionRegistry([]byte(input)); registry != nil && !diag.HasErrors(findings) {
			t.Errorf("%s was accepted: %s", name, input)
		}
	}
}

// TestDecisionRegistryValidationFailsClosed is D7: every shape that would let
// a settled decision be silently unenforceable is a registry error, not a
// skipped entry.
func TestDecisionRegistryValidationFailsClosed(t *testing.T) {
	neither := scaleToZero()
	neither.Check = nil
	both := scaleToZero()
	both.ReviewOnly = reviewOnlyMark()
	falseMark := scaleToZero()
	falseMark.Check = nil
	mark := false
	falseMark.ReviewOnly = &mark
	noGlobs := scaleToZero()
	noGlobs.Check.Files = nil
	duplicateGlob := scaleToZero()
	duplicateGlob.Check.Files = []string{"a.json", "a.json"}
	escapingGlob := scaleToZero()
	escapingGlob.Check.Files = []string{"../outside/*.json"}
	absoluteGlob := scaleToZero()
	absoluteGlob.Check.Files = []string{"/etc/*.json"}
	windowsGlob := scaleToZero()
	windowsGlob.Check.Files = []string{`infra\requirements.json`}
	badKind := scaleToZero()
	badKind.Check.Kind = "yaml-value"
	badRule := scaleToZero()
	badRule.Check.Rule = "greaterThan"
	badMissing := scaleToZero()
	badMissing.Check.WhenMissing = "ignored"
	badPointer := scaleToZero()
	badPointer.Check.Pointer = "scaling/minInstances"
	badEscape := scaleToZero()
	badEscape.Check.Pointer = "/a~2b"
	objectValue := scaleToZero()
	objectValue.Check.Value = json.RawMessage(`{"minInstances":0}`)
	noValue := scaleToZero()
	noValue.Check.Value = nil
	badDate := scaleToZero()
	badDate.Settled = "2026-9-3"
	impossibleDate := scaleToZero()
	impossibleDate.Settled = "2026-02-31"
	badID := scaleToZero()
	badID.ID = "D 001"
	multilineStatement := scaleToZero()
	multilineStatement.Statement = "serverless workloads\nscale to zero"
	noAuthor := scaleToZero()
	noAuthor.SettledBy = ""
	badADR := scaleToZero()
	badADR.ADR = "tooling/cli/doc/decisions/0001-x.md"

	second := scaleToZero()
	for name, registry := range map[string]*DecisionRegistry{
		"neither check nor reviewOnly": registryOf(neither),
		"both check and reviewOnly":    registryOf(both),
		"reviewOnly false":             registryOf(falseMark),
		"duplicate id":                 registryOf(scaleToZero(), second),
		"no globs":                     registryOf(noGlobs),
		"duplicate glob":               registryOf(duplicateGlob),
		"escaping glob":                registryOf(escapingGlob),
		"absolute glob":                registryOf(absoluteGlob),
		"windows glob":                 registryOf(windowsGlob),
		"unknown kind":                 registryOf(badKind),
		"unknown rule":                 registryOf(badRule),
		"unknown whenMissing":          registryOf(badMissing),
		"relative pointer":             registryOf(badPointer),
		"bad pointer escape":           registryOf(badEscape),
		"object value":                 registryOf(objectValue),
		"missing value":                registryOf(noValue),
		"loose date":                   registryOf(badDate),
		"impossible date":              registryOf(impossibleDate),
		"id with a space":              registryOf(badID),
		"multi-line statement":         registryOf(multilineStatement),
		"no author":                    registryOf(noAuthor),
		"adr outside doc/adr":          registryOf(badADR),
	} {
		if findings := ValidateDecisionRegistry(registry); !diag.HasErrors(findings) {
			t.Errorf("%s produced no error: %v", name, findings)
		}
	}

	if findings := ValidateDecisionRegistry(registryOf(scaleToZero())); diag.HasErrors(findings) {
		t.Fatalf("a well-formed registry was refused: %v", findings)
	}
}

// TestDecisionRegistryErrorsCarryTheReservedCode keeps a registry failure in
// the shared automation vocabulary rather than as free text.
func TestDecisionRegistryErrorsCarryTheReservedCode(t *testing.T) {
	broken := scaleToZero()
	broken.Check.Rule = "greaterThan"
	findings := ValidateDecisionRegistry(registryOf(broken))
	if len(findings) == 0 {
		t.Fatal("an unknown rule produced no finding")
	}
	for _, finding := range findings {
		if !ValidDiagnosticCodes[finding.Code] {
			t.Errorf("finding code %q is not in the reserved vocabulary", finding.Code)
		}
		if finding.Code != ErrorCodeInvalidDecisionRegistry {
			t.Errorf("finding code = %q, want %q", finding.Code, ErrorCodeInvalidDecisionRegistry)
		}
	}
}

// TestResolveJSONPointerFollowsRFC6901 pins the pointer semantics, escapes
// included, because a wrong resolution is a check that reads a member nobody
// declared and passes.
func TestResolveJSONPointerFollowsRFC6901(t *testing.T) {
	document, err := decodeDecisionDocument([]byte(`{"scaling":{"minInstances":1},"a/b":"slash","m~n":"tilde","list":[10,20],"":"empty"}`))
	if err != nil {
		t.Fatal(err)
	}
	for pointer, want := range map[string]string{
		"/scaling/minInstances": "1",
		"/a~1b":                 `"slash"`,
		"/m~0n":                 `"tilde"`,
		"/list/0":               "10",
		"/list/1":               "20",
		"/":                     `"empty"`,
	} {
		resolved, found := ResolveJSONPointer(document, pointer)
		if !found {
			t.Errorf("pointer %q resolved to nothing", pointer)
			continue
		}
		if got := renderDecisionValue(resolved); got != want {
			t.Errorf("pointer %q = %s, want %s", pointer, got, want)
		}
	}
	for _, pointer := range []string{
		"/scaling/maxInstances", // absent member
		"/list/2",               // out of range
		"/list/-",               // the element after the last never exists
		"/list/01",              // RFC 6901 forbids a leading zero
		"/scaling/minInstances/deeper",
		"/a~1b/deeper", // a scalar has no children
		"scaling",      // not a pointer
		"/a~2b",        // invalid escape
	} {
		if _, found := ResolveJSONPointer(document, pointer); found {
			t.Errorf("pointer %q resolved to something", pointer)
		}
	}
	if resolved, found := ResolveJSONPointer(document, ""); !found || resolved == nil {
		t.Fatal("the empty pointer must name the whole document")
	}
}

// TestEvaluateDecisionCheckJudgesOneFile is the heart of the gate: what counts
// as a violation, and what deliberately does not.
func TestEvaluateDecisionCheckJudgesOneFile(t *testing.T) {
	check := scaleToZero().Check

	violating := EvaluateDecisionCheck(check, []byte(`{"scaling":{"minInstances":1}}`))
	if !violating.Violated {
		t.Fatal("minInstances=1 did not violate a scale-to-zero decision")
	}
	if violating.Detail != "scaling.minInstances = 1" {
		t.Fatalf("detail = %q, want the dotted member and its value", violating.Detail)
	}
	if satisfied := EvaluateDecisionCheck(check, []byte(`{"scaling":{"minInstances":0}}`)); satisfied.Violated {
		t.Fatalf("minInstances=0 was reported as a violation: %q", satisfied.Detail)
	}
	// A number compares by VALUE, so a reformat that writes 0.0 or -0 does not
	// fail a build.
	for _, spelled := range []string{`{"scaling":{"minInstances":0.0}}`, `{"scaling":{"minInstances":-0}}`, `{"scaling":{"minInstances":0e0}}`} {
		if outcome := EvaluateDecisionCheck(check, []byte(spelled)); outcome.Violated {
			t.Errorf("%s was reported as a violation: %q", spelled, outcome.Detail)
		}
	}
	// A member of the wrong shape never equals a declared scalar.
	if outcome := EvaluateDecisionCheck(check, []byte(`{"scaling":{"minInstances":{"floor":0}}}`)); !outcome.Violated || outcome.Detail != "scaling.minInstances = an object" {
		t.Fatalf("an object member = %+v, want a violation naming the shape", outcome)
	}
	// An absent member says nothing by default, and everything when the
	// decision says the member must be declared.
	if outcome := EvaluateDecisionCheck(check, []byte(`{"databases":[]}`)); outcome.Violated {
		t.Fatalf("an undeclared member violated a whenMissing=satisfied check: %q", outcome.Detail)
	}
	required := scaleToZero().Check
	required.WhenMissing = DecisionMissingViolated
	if outcome := EvaluateDecisionCheck(required, []byte(`{"databases":[]}`)); !outcome.Violated || outcome.Detail != "scaling.minInstances is not declared" {
		t.Fatalf("whenMissing=violated outcome = %+v", outcome)
	}
	// Fail-closed: a matched file that is not JSON is a violation naming the
	// parse error. Skipping it would let a decision be defeated by breaking
	// the document the check reads.
	broken := EvaluateDecisionCheck(check, []byte(`{"scaling":`))
	if !broken.Violated || !strings.HasPrefix(broken.Detail, "the file is not valid JSON: ") {
		t.Fatalf("a malformed document = %+v, want a violation naming the parse error", broken)
	}

	notEquals := scaleToZero().Check
	notEquals.Rule = DecisionRuleNotEquals
	if outcome := EvaluateDecisionCheck(notEquals, []byte(`{"scaling":{"minInstances":0}}`)); !outcome.Violated {
		t.Fatal("notEquals did not fire on the forbidden value")
	}
	if outcome := EvaluateDecisionCheck(notEquals, []byte(`{"scaling":{"minInstances":1}}`)); outcome.Violated {
		t.Fatalf("notEquals fired on an allowed value: %q", outcome.Detail)
	}

	stringCheck := &DecisionCheck{Kind: DecisionCheckKindJSONValue, Files: []string{"a.json"}, Pointer: "/tier", Rule: DecisionRuleEquals, Value: json.RawMessage(`"serverless"`)}
	if outcome := EvaluateDecisionCheck(stringCheck, []byte(`{"tier":"managed"}`)); !outcome.Violated || outcome.Detail != `tier = "managed"` {
		t.Fatalf("string outcome = %+v", outcome)
	}
	boolCheck := &DecisionCheck{Kind: DecisionCheckKindJSONValue, Files: []string{"a.json"}, Pointer: "/public", Rule: DecisionRuleEquals, Value: json.RawMessage(`false`)}
	if outcome := EvaluateDecisionCheck(boolCheck, []byte(`{"public":true}`)); !outcome.Violated || outcome.Detail != "public = true" {
		t.Fatalf("bool outcome = %+v", outcome)
	}
}

// TestMatchDecisionFileGlobIsSegmentWise pins the `**` semantics a repository
// authors its checks against.
func TestMatchDecisionFileGlobIsSegmentWise(t *testing.T) {
	for _, row := range []struct {
		path, glob string
		want       bool
	}{
		{"sites/putnami.dev/infra/requirements.json", "**/infra/requirements.json", true},
		{"infra/requirements.json", "**/infra/requirements.json", true},
		{"infra/other.json", "**/infra/requirements.json", false},
		{"a/b/c/infra/requirements.json", "**/infra/requirements.json", true},
		{"sites/putnami.dev/infra/requirements.json", "*/infra/requirements.json", false},
		{"sites/infra/requirements.json", "*/infra/requirements.json", true},
		{"deploy.json", "deploy.json", true},
		{"nested/deploy.json", "deploy.json", false},
		{"a/b.json", "a/*.json", true},
		{"a/b/c.json", "a/*.json", false},
		{"a/b/c.json", "a/**/c.json", true},
		{"a/c.json", "a/**/c.json", true},
	} {
		if got := MatchDecisionFileGlob(row.path, row.glob); got != row.want {
			t.Errorf("MatchDecisionFileGlob(%q, %q) = %v, want %v", row.path, row.glob, got, row.want)
		}
	}
}

// TestDecisionViolationMessageNamesTheDecision is contract point 3: a reader
// gets the id, the statement and the settled date, never a generic lint line.
func TestDecisionViolationMessageNamesTheDecision(t *testing.T) {
	message := scaleToZero().ViolationMessage("decisions.json", "sites/putnami.dev/infra/requirements.json", "scaling.minInstances = 1")
	want := "decision D-001 \"serverless workloads scale to zero\" is violated by sites/putnami.dev/infra/requirements.json: scaling.minInstances = 1\n" +
		"Settled 2026-09-03 by fdumay. To change it, change the decision in decisions.json, not the code."
	if message != want {
		t.Fatalf("violation message =\n%s\nwant\n%s", message, want)
	}
}

// TestDecisionRegistryScope pins where a registry lives and which files it
// judges: its own directory, never a sibling that shares a name prefix.
func TestDecisionRegistryScope(t *testing.T) {
	for scope, want := range map[string]string{
		"":             "decisions.json",
		".":            "decisions.json",
		"tooling/cli":  "tooling/cli/decisions.json",
		"tooling/cli/": "tooling/cli/decisions.json",
	} {
		if got := DecisionRegistryPath(scope); got != want {
			t.Errorf("DecisionRegistryPath(%q) = %q, want %q", scope, got, want)
		}
	}
	for _, row := range []struct {
		scope, path, want string
		inside            bool
	}{
		{"", "tooling/cli/putnami.json", "tooling/cli/putnami.json", true},
		{"tooling/cli", "tooling/cli/infra/requirements.json", "infra/requirements.json", true},
		{"tooling/cli", "tooling/cli-model/putnami.json", "", false},
		{"tooling/cli", "tooling/putnami.json", "", false},
	} {
		got, inside := DecisionScopeRelative(row.scope, row.path)
		if got != row.want || inside != row.inside {
			t.Errorf("DecisionScopeRelative(%q, %q) = %q, %v; want %q, %v", row.scope, row.path, got, inside, row.want, row.inside)
		}
	}
}

// TestDecisionsSchemaPublishesTheWireType keeps the committed schema and the Go
// wire type from drifting: the file is what an author's editor validates
// against, and a member only one of them knows is a member nobody checks.
func TestDecisionsSchemaPublishesTheWireType(t *testing.T) {
	path := filepath.Join("schemas", "putnami-decisions.json")
	assertSchemaProperties(t, path, DecisionsSchemaURL, jsonFieldNames(t, DecisionRegistry{}))
	assertDefProperties(t, path, "decision", jsonFieldNames(t, Decision{}))
	assertDefProperties(t, path, "jsonValueCheck", jsonFieldNames(t, DecisionCheck{}))
}
