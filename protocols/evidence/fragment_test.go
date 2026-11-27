package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const fragmentFeature = "evidence/evidence-fragment"

func fragmentCodes(diags []diag.Diagnostic) []string {
	codes := make([]string, 0, len(diags))
	for _, d := range diags {
		codes = append(codes, d.Code)
	}
	return codes
}

func readFragmentFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "fragments", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFragments_AValidFileParses(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "fragment-lines-are-strict", "a-valid-file-parses")
	fragments, diags := ParseFragments(readFragmentFixture(t, "valid/findings.jsonl"))
	if diags != nil || len(fragments) != 2 {
		t.Fatalf("fragments = %+v, diags = %v, want the two findings", fragments, diags)
	}
	if fragments[1].Subject.Line != 12 || fragments[1].Severity != FindingError || fragments[1].Source.Tool == "" {
		t.Fatalf("second fragment = %+v", fragments[1])
	}
	// What EncodeFragments writes, ParseFragments reads back unchanged.
	encoded, err := EncodeFragments(fragments)
	if err != nil {
		t.Fatal(err)
	}
	again, diags := ParseFragments(encoded)
	if diags != nil || !slices.Equal(again, fragments) {
		t.Fatalf("round trip = %+v, %v", again, diags)
	}
	if empty, diags := ParseFragments(nil); diags != nil || len(empty) != 0 {
		t.Fatalf("an empty file = %v, %v, want no fragment", empty, diags)
	}
}

func TestFragments_AMalformedLineFailsNamingIt(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "fragment-lines-are-strict", "a-malformed-line-fails-naming-it")
	valid := strings.TrimRight(string(readFragmentFixture(t, "valid/findings.jsonl")), "\n")
	cases := map[string]struct {
		line string
		code string
	}{
		"blank":        {line: "", code: ErrorCodeParseError},
		"not json":     {line: "{", code: ErrorCodeParseError},
		"an array":     {line: "[]", code: ErrorCodeParseError},
		"two values":   {line: `{"kind":"finding"} {}`, code: ErrorCodeParseError},
		"version 2":    {line: `{"protocolVersion":2,"kind":"finding","source":{"format":"x"},"subject":{"path":"a"},"rule":"r","severity":"info","message":"m"}`, code: ErrorCodeUnsupportedProtocolVersion},
		"unknown kind": {line: strings.TrimSpace(string(readFragmentFixture(t, "invalid/unknown-kind.jsonl"))), code: ErrorCodeInvalidFragment},
		"bad severity": {line: strings.TrimSpace(string(readFragmentFixture(t, "invalid/unknown-severity.jsonl"))), code: ErrorCodeInvalidFragment},
		"no rule":      {line: `{"protocolVersion":1,"kind":"finding","source":{"format":"x"},"subject":{"path":"a"},"severity":"info","message":"m"}`, code: ErrorCodeRequired},
		"no subject":   {line: `{"protocolVersion":1,"kind":"finding","source":{"format":"x"},"rule":"r","severity":"info","message":"m"}`, code: ErrorCodeRequired},
		"wrong type":   {line: `{"protocolVersion":1,"kind":"finding","source":{"format":"x"},"subject":{"path":"a","line":"12"},"rule":"r","severity":"info","message":"m"}`, code: ErrorCodeParseError},
		"column alone": {line: `{"protocolVersion":1,"kind":"finding","source":{"format":"x"},"subject":{"path":"a","column":3},"rule":"r","severity":"info","message":"m"}`, code: ErrorCodeInvalidFragment},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseFragments([]byte(valid + "\n" + test.line + "\n"))
			if !slices.Contains(fragmentCodes(diags), test.code) {
				t.Fatalf("diags = %v, want %s", diags, test.code)
			}
			for _, d := range diags {
				if !strings.HasPrefix(d.Field, "fragments[2]") {
					t.Fatalf("diagnostic %v does not name the third line", d)
				}
			}
		})
	}
}

func TestFragments_AnUndeclaredMemberFails(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "fragment-lines-are-strict", "an-undeclared-member-fails")
	_, diags := ParseFragments(readFragmentFixture(t, "invalid/states-a-project.jsonl"))
	if len(diags) != 1 || diags[0].Code != ErrorCodeUnknownField || diags[0].Field != "fragments[0].project" {
		t.Fatalf("diags = %v, want fragments[0].project unknown: a fragment states no project", diags)
	}
	_, diags = ParseFragments([]byte(`{"protocolVersion":1,"kind":"finding","Kind":"finding","source":{"format":"x"},"subject":{"path":"a"},"rule":"r","severity":"info","message":"m"}` + "\n"))
	if len(diags) != 1 || diags[0].Code != ErrorCodeUnknownField {
		t.Fatalf("diags = %v, want a member differing only in case refused", diags)
	}
}

func TestFragments_AFindingCarriesRuleSeverityAndMessage(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "a-finding-names-its-rule-and-location", "a-finding-carries-rule-severity-and-message")
	fragments, diags := ParseFragments([]byte(`{"protocolVersion":1,"kind":"finding","source":{"format":"putnami-sdd"},"subject":{"path":"doc/a.md","symbol":"x","line":4,"column":2},"rule":"docs-links","severity":"error","message":"broken"}` + "\n"))
	if diags != nil || fragments[0].Rule != "docs-links" || fragments[0].Subject.Symbol != "x" || fragments[0].Subject.Column != 2 {
		t.Fatalf("fragments = %+v, diags = %v", fragments, diags)
	}
	for _, missing := range []string{"rule", "severity", "message"} {
		line := map[string]any{"protocolVersion": 1, "kind": "finding", "source": map[string]any{"format": "x"}, "subject": map[string]any{"path": "a"}, "rule": "r", "severity": "info", "message": "m"}
		delete(line, missing)
		data, _ := json.Marshal(line)
		if _, diags := ParseFragments(data); diags == nil {
			t.Fatalf("a finding without %s parsed", missing)
		}
	}
}

func TestFragments_APathLeavingTheTaskFails(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "a-finding-names-its-rule-and-location", "a-path-leaving-the-task-fails")
	_, diags := ParseFragments(readFragmentFixture(t, "invalid/path-leaves.jsonl"))
	if !slices.Contains(fragmentCodes(diags), ErrorCodeInvalidPath) {
		t.Fatalf("diags = %v, want an invalid path", diags)
	}
	for _, path := range []string{"/abs.md", `a\b.md`, "a//b.md", "./a.md", "C:/a.md", "a/../b.md"} {
		line, _ := json.Marshal(map[string]any{"protocolVersion": 1, "kind": "finding", "source": map[string]any{"format": "x"}, "subject": map[string]any{"path": path}, "rule": "r", "severity": "info", "message": "m"})
		if _, diags := ParseFragments(line); !slices.Contains(fragmentCodes(diags), ErrorCodeInvalidPath) {
			t.Errorf("path %q: diags = %v, want an invalid path", path, diags)
		}
	}
}

func TestFragments_AnOversizedFileFails(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "fragments-are-bounded", "an-oversized-file-fails")
	line := `{"protocolVersion":1,"kind":"finding","source":{"format":"x"},"subject":{"path":"a"},"rule":"r","severity":"info","message":"m"}` + "\n"
	many := strings.Repeat(line, MaxFragments+1)
	if _, diags := ParseFragments([]byte(many)); len(diags) != 1 || diags[0].Code != ErrorCodeTooLarge {
		t.Fatalf("diags = %v, want one too_large for the count", diags)
	}
	if fragments, diags := ParseFragments([]byte(strings.Repeat(line, MaxFragments))); diags != nil || len(fragments) != MaxFragments {
		t.Fatalf("a file at the bound: %d fragments, diags %v", len(fragments), diags)
	}
	big := make([]byte, MaxFragmentFileBytes+1)
	if _, diags := ParseFragments(big); len(diags) != 1 || diags[0].Code != ErrorCodeTooLarge {
		t.Fatalf("diags = %v, want one too_large for the size", diags)
	}
}

func TestFragments_AnOversizedStringFails(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "fragments-are-bounded", "an-oversized-string-fails")
	line, _ := json.Marshal(map[string]any{"protocolVersion": 1, "kind": "finding", "source": map[string]any{"format": "x"}, "subject": map[string]any{"path": "a"}, "rule": "r", "severity": "info", "message": strings.Repeat("m", MaxStringBytes+1)})
	if _, diags := ParseFragments(line); len(diags) != 1 || diags[0].Code != ErrorCodeTooLarge || diags[0].Field != "fragments[0].message" {
		t.Fatalf("diags = %v, want fragments[0].message too large", diags)
	}
}

func TestFragments_TheReservedIdentityNamesTheFile(t *testing.T) {
	spectest.Proves(t, fragmentFeature, "the-fragment-artifact-is-reserved", "the-reserved-identity-names-the-file")
	if FragmentArtifactID != "putnami-evidence" || FragmentFilename != "putnami-evidence.jsonl" {
		t.Fatalf("artifact %q names %q", FragmentArtifactID, FragmentFilename)
	}
}

// TestFragments_TheSchemaDeclaresTheReaderShape keeps the published schema and
// the strict reader on one member set.
func TestFragments_TheSchemaDeclaresTheReaderShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("schemas", "evidence-fragment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema schemaNode
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var walk func(node *schemaNode, shape *member, path string)
	walk = func(node *schemaNode, shape *member, path string) {
		if shape == nil || shape.fields == nil {
			return
		}
		var schemaKeys, shapeKeys []string
		for key := range node.Properties {
			schemaKeys = append(schemaKeys, key)
		}
		for key := range shape.fields {
			shapeKeys = append(shapeKeys, key)
		}
		sort.Strings(schemaKeys)
		sort.Strings(shapeKeys)
		if !slices.Equal(schemaKeys, shapeKeys) {
			t.Errorf("%s: schema members %v, reader members %v", path, schemaKeys, shapeKeys)
			return
		}
		for key, child := range shape.fields {
			walk(node.Properties[key], child, joinPath(path, key))
		}
	}
	walk(&schema, fragmentShape, "")
	// The schema's enums, pattern and minimums are the reader's rules.
	if got := schema.Properties["kind"].Enum; !slices.Equal(got, FragmentKinds) {
		t.Errorf("kind: schema enum %v, reader %v", got, FragmentKinds)
	}
	if got := schema.Properties["severity"].Enum; !slices.Equal(got, FindingSeverities) {
		t.Errorf("severity: schema enum %v, reader %v", got, FindingSeverities)
	}
	if got := schema.Properties["source"].Properties["format"].Pattern; got != slugExpression {
		t.Errorf("source.format: schema pattern %q, reader %q", got, slugExpression)
	}
	for _, name := range []string{"line", "column"} {
		if minimum := schema.Properties["subject"].Properties[name].Minimum; minimum == nil || *minimum != 1 {
			t.Errorf("subject.%s: schema minimum %v, the reader counts from 1", name, minimum)
		}
	}
}

// TestFragments_TheReaderRefusesWhatTheSchemaRefuses pins the cases a lenient
// decoder would accept: a null member, a repeated member and a place at 0.
func TestFragments_TheReaderRefusesWhatTheSchemaRefuses(t *testing.T) {
	base := `"protocolVersion":1,"kind":"finding","source":{"format":"x"%s},"subject":{"path":"a"%s},"rule":"r","severity":"info","message":"m"%s`
	cases := map[string]struct{ line, code, field string }{
		"null tool":     {`{` + fmt.Sprintf(base, `,"tool":null`, "", "") + `}`, ErrorCodeInvalidFragment, "fragments[0].source.tool"},
		"null line":     {`{` + fmt.Sprintf(base, "", `,"line":null`, "") + `}`, ErrorCodeInvalidFragment, "fragments[0].subject.line"},
		"repeated rule": {`{` + fmt.Sprintf(base, "", "", `,"rule":"other"`) + `}`, ErrorCodeDuplicate, "fragments[0].rule"},
		"line 0":        {`{` + fmt.Sprintf(base, "", `,"line":0`, "") + `}`, ErrorCodeInvalidFragment, "fragments[0].subject.line"},
		"column 0":      {`{` + fmt.Sprintf(base, "", `,"line":2,"column":0`, "") + `}`, ErrorCodeInvalidFragment, "fragments[0].subject.column"},
	}
	for name, c := range cases {
		_, diags := ParseFragments([]byte(c.line))
		if len(diags) != 1 || diags[0].Code != c.code || diags[0].Field != c.field {
			t.Errorf("%s: diags = %v, want %s at %s", name, diags, c.code, c.field)
		}
	}
	if _, diags := ParseFragments([]byte(`{` + fmt.Sprintf(base, "", `,"line":2,"column":1`, "") + `}`)); diags != nil {
		t.Fatalf("a placed finding: %v", diags)
	}
}

// TestFragments_EncodeWritesOnlyWhatTheReaderReads pins that the writer
// refuses a fragment the reader would refuse, such as an oversized message.
func TestFragments_EncodeWritesOnlyWhatTheReaderReads(t *testing.T) {
	fragment := Fragment{ProtocolVersion: 1, Kind: FragmentKindFinding, Source: FragmentSource{Format: "x"},
		Subject: FragmentSubject{Path: "a"}, Rule: "r", Severity: FindingInfo, Message: strings.Repeat("m", MaxStringBytes+1)}
	if _, err := EncodeFragments([]Fragment{fragment}); err == nil {
		t.Fatal("an oversized message encoded")
	}
	fragment.Message = "m"
	data, err := EncodeFragments([]Fragment{fragment})
	if err != nil {
		t.Fatal(err)
	}
	if parsed, diags := ParseFragments(data); diags != nil || len(parsed) != 1 {
		t.Fatalf("parsed = %v, diags = %v", parsed, diags)
	}
}
