package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "evidence/change-evidence-document"

// expectations is fixtures/expectations.json: every valid fixture must validate
// cleanly; every invalid fixture must produce EXACTLY the listed distinct codes.
type expectations struct {
	Valid   []string            `json:"valid"`
	Invalid map[string][]string `json:"invalid"`
}

func readExpectations(t *testing.T) expectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "expectations.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var index expectations
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	return index
}

func distinctCodes(diags []diag.Diagnostic) []string {
	seen := map[string]bool{}
	var codes []string
	for _, d := range diag.Errors(diags) {
		if !seen[d.Code] {
			seen[d.Code] = true
			codes = append(codes, d.Code)
		}
	}
	sort.Strings(codes)
	return codes
}

func readValid(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture %s: %v", name, diags)
	}
	return document
}

// codesOf validates a mutated copy of a valid fixture.
func codesOf(t *testing.T, name string, mutate func(*Document)) []string {
	t.Helper()
	document := readValid(t, name)
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var clone Document
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	mutate(&clone)
	return distinctCodes(ValidateDocument(&clone))
}

func expectCodes(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: codes = %v, want %v", label, got, want)
	}
}

func TestConformance_ProtocolVersion(t *testing.T) {
	spectest.Proves(t, feature, "version-1-and-closed-vocabularies", "the-protocol-version-is-1")
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1: bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_VocabulariesAreClosed pins every closed vocabulary. Adding a
// value is a protocol change every reader must learn.
func TestConformance_VocabulariesAreClosed(t *testing.T) {
	spectest.Proves(t, feature, "version-1-and-closed-vocabularies", "the-vocabularies-are-closed")
	pins := map[string][2][]string{
		"providers":   {Providers, {"github", "gitlab", "generic"}},
		"ci systems":  {CISystems, {"github-actions", "gitlab-ci", "generic"}},
		"sources":     {ComponentSources, {"orchestrator", "workspace", "manifest", "fallback", "declared"}},
		"impacts":     {Impacts, {"changed", "dependent", "unaffected"}},
		"statuses":    {FileStatuses, {"added", "modified", "deleted", "renamed"}},
		"dep changes": {DependencyChanges, {"added", "removed", "upgraded", "downgraded", "changed"}},
	}
	for name, pin := range pins {
		if !slices.Equal(pin[0], pin[1]) {
			t.Errorf("%s = %v, want %v", name, pin[0], pin[1])
		}
	}
	// Ecosystems stay open: a new stack needs no protocol version.
	expectCodes(t, "open ecosystem", codesOf(t, "generic-minimal.json", func(d *Document) {
		d.Components[0].Ecosystem = "maven"
	}))
}

func TestConformance_EveryCodeStartsWithEvidence(t *testing.T) {
	spectest.Proves(t, feature, "version-1-and-closed-vocabularies", "every-code-starts-with-evidence")
	for code := range ValidErrorCodes {
		if !strings.HasPrefix(code, "evidence.") {
			t.Errorf("code %q does not start with evidence.", code)
		}
	}
}

// schemaNode is the subset of JSON Schema the drift checks read.
type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Defs       map[string]*schemaNode `json:"$defs"`
	Ref        string                 `json:"$ref"`
	Enum       []string               `json:"enum"`
	Pattern    string                 `json:"pattern"`
}

func readSchema(t *testing.T) *schemaNode {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("schemas", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	return &root
}

func resolve(root, node *schemaNode) *schemaNode {
	if node != nil && strings.HasPrefix(node.Ref, "#/$defs/") {
		return root.Defs[strings.TrimPrefix(node.Ref, "#/$defs/")]
	}
	return node
}

func jsonMembers(typ reflect.Type) map[string]reflect.Type {
	members := map[string]reflect.Type{}
	for index := range typ.NumField() {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		members[name] = field.Type
	}
	return members
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// objectType unwraps pointers and slices to the struct a member carries, or nil
// for a scalar or a slice of scalars.
func objectType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct {
		return typ
	}
	return nil
}

func compareSchema(t *testing.T, root, node *schemaNode, typ reflect.Type, path string) {
	t.Helper()
	node = resolve(root, node)
	if node == nil {
		t.Errorf("%s: no schema node", path)
		return
	}
	members := jsonMembers(typ)
	if got, want := sortedKeys(node.Properties), sortedKeys(members); !slices.Equal(got, want) {
		t.Errorf("%s: schema members %v, Go members %v", path, got, want)
	}
	for name, memberType := range members {
		inner := objectType(memberType)
		if inner == nil {
			continue
		}
		child := resolve(root, node.Properties[name])
		if child == nil {
			continue
		}
		if memberType.Kind() == reflect.Slice {
			child = child.Items
		}
		compareSchema(t, root, child, inner, path+"."+name)
	}
}

func compareShape(t *testing.T, shape *member, typ reflect.Type, path string) {
	t.Helper()
	if shape == nil {
		t.Errorf("%s: no strict shape", path)
		return
	}
	members := jsonMembers(typ)
	if got, want := sortedKeys(shape.fields), sortedKeys(members); !slices.Equal(got, want) {
		t.Errorf("%s: strict members %v, Go members %v", path, got, want)
	}
	for name, memberType := range members {
		inner := objectType(memberType)
		if inner == nil {
			continue
		}
		child := shape.fields[name]
		if child != nil && memberType.Kind() == reflect.Slice {
			child = child.items
		}
		compareShape(t, child, inner, path+"."+name)
	}
}

func TestConformance_SchemaDeclaresTheGoMembers(t *testing.T) {
	spectest.Proves(t, feature, "one-member-set", "the-schema-declares-the-go-members")
	root := readSchema(t)
	compareSchema(t, root, root, reflect.TypeFor[Document](), "document")
}

func TestConformance_StrictWalkDeclaresTheGoMembers(t *testing.T) {
	spectest.Proves(t, feature, "one-member-set", "the-strict-walk-declares-the-go-members")
	compareShape(t, documentShape, reflect.TypeFor[Document](), "document")
}

func TestConformance_SchemaEnumsAreTheGoVocabularies(t *testing.T) {
	spectest.Proves(t, feature, "one-member-set", "the-schema-enums-are-the-go-vocabularies")
	root := readSchema(t)
	enums := map[string][2][]string{
		"subject.provider":   {root.Properties["subject"].Properties["provider"].Enum, Providers},
		"ci.system":          {root.Properties["ci"].Properties["system"].Enum, CISystems},
		"component.source":   {root.Defs["component"].Properties["source"].Enum, ComponentSources},
		"component.impact":   {root.Defs["component"].Properties["impact"].Enum, Impacts},
		"changedFile.status": {root.Defs["changedFile"].Properties["status"].Enum, FileStatuses},
		"dependency.change":  {root.Defs["dependencyChange"].Properties["change"].Enum, DependencyChanges},
	}
	for name, pair := range enums {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s: schema enum %v, Go vocabulary %v", name, pair[0], pair[1])
		}
	}
	// The schema's patterns are the expressions the Go validator applies.
	patterns := map[string][2]string{
		"subject.sha":          {root.Properties["subject"].Properties["sha"].Pattern, shaExpression},
		"subject.baseSha":      {root.Properties["subject"].Properties["baseSha"].Pattern, shaExpression},
		"subject.projectId":    {root.Properties["subject"].Properties["projectId"].Pattern, projectIDExpression},
		"subject.host":         {root.Properties["subject"].Properties["host"].Pattern, hostExpression},
		"generatedAt":          {root.Properties["generatedAt"].Pattern, timestampExpression},
		"component.ecosystem":  {root.Defs["component"].Properties["ecosystem"].Pattern, slugExpression},
		"dependency.ecosystem": {root.Defs["dependencyChange"].Properties["ecosystem"].Pattern, slugExpression},
		"diagnostic.code":      {root.Defs["diagnostic"].Properties["code"].Pattern, codeExpression},
	}
	for name, pair := range patterns {
		if pair[0] != pair[1] {
			t.Errorf("%s: schema pattern %q, Go expression %q", name, pair[0], pair[1])
		}
	}
}

func TestConformance_CorpusIsWhatExpectationsLists(t *testing.T) {
	spectest.Proves(t, feature, "one-fixture-corpus", "the-corpus-is-what-expectations-lists")
	index := readExpectations(t)
	for dir, listed := range map[string][]string{"valid": index.Valid, "invalid": sortedKeys(index.Invalid)} {
		entries, err := os.ReadDir(filepath.Join("fixtures", dir))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk []string
		for _, entry := range entries {
			onDisk = append(onDisk, entry.Name())
		}
		sort.Strings(onDisk)
		want := slices.Clone(listed)
		sort.Strings(want)
		if !slices.Equal(onDisk, want) {
			t.Errorf("fixtures/%s holds %v, expectations list %v", dir, onDisk, want)
		}
	}
}

func TestConformance_EveryFixtureGetsItsListedCodes(t *testing.T) {
	spectest.Proves(t, feature, "one-fixture-corpus", "every-fixture-gets-its-listed-codes")
	index := readExpectations(t)
	for _, name := range index.Valid {
		readValid(t, name)
	}
	for name, want := range index.Invalid {
		data, err := os.ReadFile(filepath.Join("fixtures", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseAndValidate(data)
		for _, code := range want {
			if !ValidErrorCodes[code] {
				t.Errorf("%s lists %q, outside the closed set", name, code)
			}
		}
		expectCodes(t, name, distinctCodes(diags), want...)
	}
}

func TestConformance_NonDocumentsAreParseErrors(t *testing.T) {
	spectest.Proves(t, feature, "parsing-is-strict", "non-documents-are-parse-errors")
	minimal, err := os.ReadFile(filepath.Join("fixtures", "valid", "generic-minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"not JSON":      "{",
		"two documents": string(minimal) + "{}",
		"an array":      "[]",
		"a string":      `"evidence"`,
		"wrong type":    strings.Replace(string(minimal), `"protocolVersion": 1`, `"protocolVersion": "1"`, 1),
	}
	for label, input := range cases {
		_, diags := ParseAndValidate([]byte(input))
		expectCodes(t, label, distinctCodes(diags), ErrorCodeParseError)
	}
	// A required member absent or null is evidence.required, not a silent zero.
	for _, member := range requiredMembers {
		var raw map[string]any
		if err := json.Unmarshal(minimal, &raw); err != nil {
			t.Fatal(err)
		}
		for _, value := range []any{nil, "absent"} {
			if value == nil {
				raw[member] = nil
			} else {
				delete(raw, member)
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidate(data)
			expectCodes(t, member+" absent or null", distinctCodes(diags), ErrorCodeRequired)
		}
	}
}

func TestConformance_UndeclaredMemberIsAnUnknownField(t *testing.T) {
	spectest.Proves(t, feature, "parsing-is-strict", "an-undeclared-member-is-an-unknown-field")
	data, err := os.ReadFile(filepath.Join("fixtures", "invalid", "unknown-field.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, diags := ParseAndValidate(data)
	if len(diags) != 1 || diags[0].Code != ErrorCodeUnknownField || diags[0].Field != "components[0].owner" {
		t.Fatalf("diags = %v, want one unknown_field at components[0].owner", diags)
	}
}

func TestConformance_EachSubjectDefectGetsItsCode(t *testing.T) {
	spectest.Proves(t, feature, "the-subject-names-the-change", "each-subject-defect-gets-its-code")
	cases := map[string]struct {
		fixture string
		mutate  func(*Document)
		want    string
	}{
		"unknown provider":    {"generic-minimal.json", func(d *Document) { d.Subject.Provider = "bitbucket" }, ErrorCodeInvalidSubject},
		"github without host": {"turbo-pull-request.json", func(d *Document) { d.Subject.Host = "" }, ErrorCodeInvalidSubject},
		"uppercase host":      {"turbo-pull-request.json", func(d *Document) { d.Subject.Host = "GitHub.com" }, ErrorCodeInvalidSubject},
		"non-numeric project": {"symfony-merge-request.json", func(d *Document) { d.Subject.ProjectID = "shop/backoffice" }, ErrorCodeInvalidSubject},
		"short sha":           {"generic-minimal.json", func(d *Document) { d.Subject.SHA = "4f6c2a9" }, ErrorCodeInvalidSubject},
		"bad base sha":        {"turbo-pull-request.json", func(d *Document) { d.Subject.BaseSHA = "main" }, ErrorCodeInvalidSubject},
		"negative request":    {"turbo-pull-request.json", func(d *Document) { d.Subject.ChangeRequest.Number = -1 }, ErrorCodeInvalidSubject},
		"no repository":       {"generic-minimal.json", func(d *Document) { d.Subject.Repository = " " }, ErrorCodeRequired},
		"diff without base":   {"turbo-pull-request.json", func(d *Document) { d.Subject.BaseSHA = "" }, ErrorCodeInvalidSubject},
		"coverage, no base": {"generic-minimal.json", func(d *Document) {
			d.ChangedLineCoverage = &ChangedLineCoverage{Files: []ChangedLineCoverageFile{}}
		}, ErrorCodeInvalidSubject},
	}
	for _, host := range []string{"github.com\n", "-", "...", "gitlab.example.com.", "a_b.example", "ｇｉｔｈｕｂ.com", "git lab.com", "user@gitlab.com"} {
		expectCodes(t, "host "+host, codesOf(t, "symfony-merge-request.json", func(d *Document) {
			d.Subject.Host = host
		}), ErrorCodeInvalidSubject)
	}
	expectCodes(t, "self-managed host with a port", codesOf(t, "symfony-merge-request.json", func(d *Document) {
		d.Subject.Host = "gitlab.internal.example:8443"
	}))
	for label, c := range cases {
		expectCodes(t, label, codesOf(t, c.fixture, c.mutate), c.want)
	}
	// A sha-256 repository is accepted, and a generic subject needs no host or id.
	expectCodes(t, "sha-256", codesOf(t, "generic-minimal.json", func(d *Document) {
		d.Subject.SHA = strings.Repeat("ab", 32)
	}))
}

func TestConformance_EachCIDefectGetsItsCode(t *testing.T) {
	spectest.Proves(t, feature, "the-ci-run-is-identified", "each-ci-defect-gets-its-code")
	cases := map[string]func(*Document){
		"http run url":       func(d *Document) { d.CI.URL = "http://github.com/run/1" },
		"credentials in url": func(d *Document) { d.CI.URL = "https://token:x@github.com/run/1" },
		"unknown ci":         func(d *Document) { d.CI.System = "circleci" },
		"attempt zero":       func(d *Document) { zero := 0; d.CI.Attempt = &zero },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeInvalidCI)
	}
}

func TestConformance_EachPathDefectIsAnInvalidPath(t *testing.T) {
	spectest.Proves(t, feature, "paths-stay-in-the-repository", "each-path-defect-is-an-invalid-path")
	for _, bad := range []string{"/etc/passwd", `src\main.go`, "src/../../x", "src//x", "./src/x", "src/", "..", "c:/x", "src/a\x00b", "src/\nx"} {
		expectCodes(t, "changed file "+bad, codesOf(t, "turbo-pull-request.json", func(d *Document) {
			d.ChangedFiles = append(d.ChangedFiles, ChangedFile{Path: bad, Status: FileModified})
		}), ErrorCodeInvalidPath)
	}
	expectCodes(t, "renamed onto itself", codesOf(t, "symfony-merge-request.json", func(d *Document) {
		d.ChangedFiles[0].PreviousPath = d.ChangedFiles[0].Path
	}), ErrorCodeInvalidPath)
	expectCodes(t, "changed-line coverage of an unchanged file", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.ChangedLineCoverage.Files[0].Path = "packages/ui/src/index.ts"
	}), ErrorCodeInvalidPath)
	expectCodes(t, "changed-line coverage of a deleted file", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.ChangedFiles[0].Status = FileDeleted
	}), ErrorCodeInvalidPath)
	expectCodes(t, "empty lockfile", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.DependencyChanges[0].Lockfile = ""
	}), ErrorCodeInvalidPath)
	expectCodes(t, "previous path on a modified file", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.ChangedFiles[2].PreviousPath = "yarn.lock"
	}), ErrorCodeInvalidPath)
	expectCodes(t, "unknown status", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.ChangedFiles[2].Status = "copied"
	}), ErrorCodeInvalidPath)
	// The root component id is the single dot, and only there.
	readValid(t, "symfony-merge-request.json")
}

func TestConformance_DuplicatesAreRefused(t *testing.T) {
	spectest.Proves(t, feature, "references-resolve", "duplicates-are-refused")
	expectCodes(t, "changed file twice", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.ChangedFiles = append(d.ChangedFiles, d.ChangedFiles[2])
	}), ErrorCodeDuplicate)
	expectCodes(t, "tests twice", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Tests = append(d.Tests, ComponentTests{Component: "apps/web"})
	}), ErrorCodeDuplicate)
	expectCodes(t, "coverage twice", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Coverage = append(d.Coverage, ComponentCoverage{Component: "apps/web"})
	}), ErrorCodeDuplicate)
	expectCodes(t, "edge twice", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Components[0].DependsOn = []string{"packages/ui", "packages/ui"}
	}), ErrorCodeDuplicate)
}

func TestConformance_EveryReferenceNamesAComponent(t *testing.T) {
	spectest.Proves(t, feature, "references-resolve", "every-reference-names-a-component")
	cases := map[string]func(*Document){
		"changed file": func(d *Document) { d.ChangedFiles[2].Component = "apps/admin" },
		"edge":         func(d *Document) { d.Components[1].DependsOn = []string{"apps/admin"} },
		"tests":        func(d *Document) { d.Tests[1].Component = "apps/admin" },
		"coverage":     func(d *Document) { d.Coverage[1].Component = "apps/admin" },
		"dependency":   func(d *Document) { d.DependencyChanges[1].Component = "apps/admin" },
		"empty edge":   func(d *Document) { d.Components[1].DependsOn = []string{""} },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeUnknownComponent)
	}
}

func TestConformance_EachComponentDefectIsAnInvalidComponent(t *testing.T) {
	spectest.Proves(t, feature, "components-are-well-formed", "each-component-defect-is-an-invalid-component")
	cases := map[string]func(*Document){
		"file outside its component": func(d *Document) { d.ChangedFiles[2].Component = "apps/docs" },
		"a self edge":                func(d *Document) { d.Components[1].DependsOn = []string{"apps/docs"} },
		"ecosystem not a slug":       func(d *Document) { d.Components[1].Ecosystem = "Node.js" },
		"unknown source":             func(d *Document) { d.Components[1].Source = "guessed" },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeInvalidComponent)
	}
}

func TestConformance_ImpactFollowsTheFilesAndTheEdges(t *testing.T) {
	spectest.Proves(t, feature, "impact-is-consistent", "the-impact-follows-the-files-and-the-edges")
	readValid(t, "dependency-cycle.json")
	cases := map[string]func(*Document){
		"changed component marked dependent":  func(d *Document) { d.Components[2].Impact = ImpactDependent },
		"dependent marked unaffected":         func(d *Document) { d.Components[0].Impact = ImpactUnaffected },
		"unaffected marked changed":           func(d *Document) { d.Components[1].Impact = ImpactChanged },
		"a file moved out of the changed one": func(d *Document) { d.ChangedFiles[0].Component, d.ChangedFiles[1].Component = "", "" },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeInvalidImpact)
	}
	// A dependent several edges away is still dependent.
	expectCodes(t, "transitive dependent", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Components[1].DependsOn = []string{"apps/web"}
		d.Components[1].Impact = ImpactDependent
	}))
}

func TestConformance_EachCountDefectIsAnInvalidCount(t *testing.T) {
	spectest.Proves(t, feature, "counts-add-up", "each-count-defect-is-an-invalid-count")
	cases := map[string]func(*Document){
		"negative passed":            func(d *Document) { d.Tests[1].Passed = -1 },
		"negative duration":          func(d *Document) { d.Tests[1].DurationMs = -5 },
		"covered above total":        func(d *Document) { d.Coverage[1].CoveredLines = 4000 },
		"negative total":             func(d *Document) { d.Coverage[1].TotalLines, d.Coverage[1].CoveredLines = -1, -1 },
		"totals not the sums":        func(d *Document) { d.ChangedLineCoverage.CoveredLines = 13 },
		"uncovered not increasing":   func(d *Document) { d.ChangedLineCoverage.Files[0].UncoveredLines = []int{22, 22, 31, 40} },
		"uncovered line zero":        func(d *Document) { d.ChangedLineCoverage.Files[0].UncoveredLines = []int{0, 22, 31, 40} },
		"more uncovered than missed": func(d *Document) { d.ChangedLineCoverage.Files[0].UncoveredLines = []int{1, 2, 3, 4, 5} },
		"cases above failures":       func(d *Document) { d.Tests[0].Failed = 0 },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeInvalidCount)
	}
}

func TestConformance_EachChangeKindHasItsVersions(t *testing.T) {
	spectest.Proves(t, feature, "dependency-changes-are-well-formed", "each-change-kind-has-its-versions")
	cases := map[string]func(*Document){
		"added with from":  func(d *Document) { d.DependencyChanges[1].From = "1.0.0" },
		"added without to": func(d *Document) { d.DependencyChanges[1].To = "" },
		"removed with to": func(d *Document) {
			d.DependencyChanges[1] = DependencyChange{Ecosystem: "npm", Lockfile: "pnpm-lock.yaml", Name: "x", Change: DependencyRemoved, From: "1", To: "2"}
		},
		"upgraded without to":  func(d *Document) { d.DependencyChanges[0].To = "" },
		"upgraded to the same": func(d *Document) { d.DependencyChanges[0].To = d.DependencyChanges[0].From },
		"unknown kind":         func(d *Document) { d.DependencyChanges[0].Change = "bumped" },
		"no name":              func(d *Document) { d.DependencyChanges[0].Name = "" },
		"ecosystem not a slug": func(d *Document) { d.DependencyChanges[0].Ecosystem = "NPM" },
		"unchanged lockfile":   func(d *Document) { d.DependencyChanges[0].Lockfile = "apps/web/package-lock.json" },
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeInvalidDependencyChange)
	}
}

func TestConformance_EachBoundIsEnforced(t *testing.T) {
	spectest.Proves(t, feature, "the-document-is-bounded", "each-bound-is-enforced")
	cases := map[string]func(*Document){
		"changed files": func(d *Document) { d.ChangedFiles = make([]ChangedFile, MaxChangedFiles+1) },
		"components":    func(d *Document) { d.Components = make([]Component, MaxComponents+1) },
		"dependencies":  func(d *Document) { d.DependencyChanges = make([]DependencyChange, MaxDependencyChanges+1) },
		"diagnostics":   func(d *Document) { d.Diagnostics = make([]diag.Diagnostic, MaxDiagnostics+1) },
		"failing cases": func(d *Document) {
			d.Tests[0].Failed = 100
			d.Tests[0].FailingCases = make([]FailingCase, MaxFailingCases+1)
		},
		"changed-line files": func(d *Document) {
			d.ChangedLineCoverage.Files = make([]ChangedLineCoverageFile, MaxChangedLineCoverageFile+1)
		},
		"uncovered lines": func(d *Document) {
			d.ChangedLineCoverage.Files[0].UncoveredLines = make([]int, MaxUncoveredLines+1)
		},
	}
	for label, mutate := range cases {
		expectCodes(t, label, codesOf(t, "turbo-pull-request.json", mutate), ErrorCodeTooLarge)
	}
	expectCodes(t, "edges", codesOf(t, "generic-minimal.json", func(d *Document) {
		d.Components = make([]Component, 21)
		for index := range d.Components {
			d.Components[index] = Component{ID: fmt.Sprintf("c%d", index), Ecosystem: "go", Source: SourceWorkspace, Impact: ImpactUnaffected}
			d.Components[index].DependsOn = make([]string, MaxEdges/20)
		}
	}), ErrorCodeTooLarge)
	data, err := os.ReadFile(filepath.Join("fixtures", "invalid", "oversized-message.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, diags := ParseAndValidate(data); len(diags) != 1 || diags[0].Field != "tests[0].failingCases[0].message" {
		t.Errorf("oversized string: diags = %v, want one too_large at tests[0].failingCases[0].message", diags)
	}
	// The impact walk is linear: a dense graph at the component bound validates
	// in well under a second, where enumerating paths would never finish.
	document := denseGraph(MaxComponents, 20)
	started := time.Now()
	if diags := ValidateDocument(document); diag.HasErrors(diags) {
		t.Fatalf("dense graph: %v", diag.ErrorText(diags))
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("dense graph took %s", elapsed)
	}
}

// denseGraph builds size components where each depends on the next fanout
// ones, and only the last is changed, so every other component is dependent.
func denseGraph(size, fanout int) *Document {
	document := &Document{
		ProtocolVersion: ProtocolVersion,
		Subject:         Subject{Provider: ProviderGeneric, Repository: "dense", SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)},
		CI:              CI{System: CISystemGeneric},
		Producer:        Producer{Name: "test", Version: "0"},
		GeneratedAt:     "2026-11-03T08:00:00Z",
	}
	for index := range size {
		component := Component{ID: fmt.Sprintf("c%d", index), Ecosystem: "go", Source: SourceWorkspace, Impact: ImpactDependent}
		for next := index + 1; next <= index+fanout && next < size; next++ {
			component.DependsOn = append(component.DependsOn, fmt.Sprintf("c%d", next))
		}
		document.Components = append(document.Components, component)
	}
	last := fmt.Sprintf("c%d", size-1)
	document.Components[size-1].Impact = ImpactChanged
	document.ChangedFiles = []ChangedFile{{Path: last + "/main.go", Status: FileModified, Component: last}}
	return document
}

func TestConformance_TimestampsAndDiagnosticsAreChecked(t *testing.T) {
	spectest.Proves(t, feature, "timestamps-and-diagnostics-are-well-formed", "timestamps-and-diagnostics-are-checked")
	for _, bad := range []string{"", "2026-11-03", "2026-11-03 08:00:00Z", "2026-11-03T08:00:00"} {
		expectCodes(t, "timestamp "+bad, codesOf(t, "generic-minimal.json", func(d *Document) {
			d.GeneratedAt = bad
		}), ErrorCodeInvalidTimestamp)
	}
	expectCodes(t, "unknown severity", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Diagnostics[0].Severity = "fatal"
	}), ErrorCodeInvalidDiagnostic)
	expectCodes(t, "undotted code", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Diagnostics[0].Code = "report_missing"
	}), ErrorCodeInvalidDiagnostic)
	expectCodes(t, "diagnostic without a message", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Diagnostics[0].Message = ""
	}), ErrorCodeInvalidDiagnostic)
	expectCodes(t, "impossible timestamp", codesOf(t, "generic-minimal.json", func(d *Document) {
		d.GeneratedAt = "2026-99-99T99:99:99+99:99"
	}), ErrorCodeInvalidTimestamp)
	// A reader diagnostic of error severity never makes the document invalid.
	expectCodes(t, "error diagnostic", codesOf(t, "turbo-pull-request.json", func(d *Document) {
		d.Diagnostics[0].Severity = diag.Error
	}))
}
