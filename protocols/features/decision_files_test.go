package features

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// mapDecisionFiles is a workspace held in memory: workspace-relative path to
// content.
type mapDecisionFiles map[string]string

func (m mapDecisionFiles) ReadFile(name string) ([]byte, error) {
	content, found := m[name]
	if !found {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(content), nil
}

func (m mapDecisionFiles) ReadDir(dir string) ([]string, error) {
	var names []string
	found := false
	for name := range m {
		if strings.HasPrefix(name, dir+"/") {
			found = true
			if rest := name[len(dir)+1:]; !strings.Contains(rest, "/") {
				names = append(names, rest)
			}
		}
	}
	if !found {
		return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrNotExist}
	}
	sort.Strings(names)
	return names, nil
}

func decisionFile(t *testing.T, decision Decision) string {
	t.Helper()
	data, err := MarshalDecisionFile(decision)
	if err != nil {
		t.Fatalf("marshal %s: %v", decision.ID, err)
	}
	return string(data)
}

func reviewOnlyDecision(id string) Decision {
	return Decision{ID: id, Statement: "every pull request names what it deletes", Settled: "2026-09-03", SettledBy: "fdumay", ReviewOnly: reviewOnlyMark()}
}

// TestDecisionFileRoundTripsCanonically pins the byte contract of one decision
// file: the schema URL and protocol version stamped, the check's globs sorted,
// and the emitted bytes accepted by their own strict reader under their name.
func TestDecisionFileRoundTripsCanonically(t *testing.T) {
	authored := scaleToZero()
	authored.Check.Files = []string{"z/**/deploy.json", "**/infra/requirements.json"}
	encoded, err := MarshalDecisionFile(authored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), DecisionSchemaURL) || !strings.Contains(string(encoded), `"protocolVersion": 1`) {
		t.Fatalf("canonical bytes do not stamp the schema URL and version:\n%s", encoded)
	}
	parsed, findings := ParseAndValidateDecisionFile("D-001.json", encoded)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("canonical bytes failed their own reader: %v", findings)
	}
	if got := parsed.Check.Files; got[0] != "**/infra/requirements.json" || got[1] != "z/**/deploy.json" {
		t.Fatalf("check globs are not sorted: %v", got)
	}
	if authored.Check.Files[0] != "z/**/deploy.json" {
		t.Fatal("MarshalDecisionFile sorted a caller-owned glob slice in place")
	}
	again, err := MarshalDecisionFile(*parsed)
	if err != nil || string(again) != string(encoded) {
		t.Fatalf("canonical form is not stable:\n%s\n%s", encoded, again)
	}
}

// TestDecisionFileIsStrictOnTheWire keeps one decision file as fail-closed as
// the registry: a wrong version, an unknown member, an explicit null, a
// trailing value, an invalid decision and a name that is not its id are all
// refused.
func TestDecisionFileIsStrictOnTheWire(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-decision", "a-decision-file-is-strict-and-named-by-its-id")
	valid := `{"protocolVersion":1,"id":"cache-trust","statement":"s","settled":"2026-01-02","settledBy":"a","reviewOnly":true}`
	if decision, findings := ParseAndValidateDecisionFile("cache-trust.json", []byte(valid)); decision == nil || diag.HasErrors(findings) {
		t.Fatalf("valid file refused: %v", findings)
	}
	for name, row := range map[string]struct{ file, data string }{
		"missing version": {"cache-trust.json", `{"id":"cache-trust","statement":"s","settled":"2026-01-02","settledBy":"a","reviewOnly":true}`},
		"wrong version":   {"cache-trust.json", strings.Replace(valid, `"protocolVersion":1`, `"protocolVersion":2`, 1)},
		"unknown member":  {"cache-trust.json", strings.Replace(valid, `"reviewOnly":true`, `"reviewOnly":true,"owner":"x"`, 1)},
		"explicit null":   {"cache-trust.json", strings.Replace(valid, `"reviewOnly":true`, `"reviewOnly":true,"adr":null`, 1)},
		"trailing value":  {"cache-trust.json", valid + ` {}`},
		"no check":        {"cache-trust.json", strings.Replace(valid, `,"reviewOnly":true`, ``, 1)},
		"registry shape":  {"cache-trust.json", `{"protocolVersion":1,"decisions":[]}`},
		"name mismatch":   {"cache-trust-v2.json", valid},
		"name case":       {"Cache-Trust.json", valid},
	} {
		if decision, findings := ParseAndValidateDecisionFile(row.file, []byte(row.data)); decision != nil || !diag.HasErrors(findings) {
			t.Errorf("%s: accepted %s %s", name, row.file, row.data)
		}
	}
	_, findings := ParseAndValidateDecisionFile("other.json", []byte(valid))
	if len(findings) != 1 || findings[0].Field != "id" || !strings.Contains(findings[0].Message, "other.json") {
		t.Fatalf("name mismatch findings = %+v, want one on id naming the file", findings)
	}
}

// TestReadScopeDecisionsReadsTheDirectoryForm: every decisions/<id>.json of a
// scope is one decision, sorted by id, each sourced on its own file; an entry
// that is not a .json file is not a decision.
func TestReadScopeDecisionsReadsTheDirectoryForm(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-decision", "a-scope-reads-one-file-per-decision")
	files := mapDecisionFiles{
		"tooling/cli/decisions/D-002.json":       decisionFile(t, reviewOnlyDecision("D-002")),
		"tooling/cli/decisions/cache-trust.json": decisionFile(t, reviewOnlyDecision("cache-trust")),
		"tooling/cli/decisions/D-001.json":       decisionFile(t, scaleToZero()),
		"tooling/cli/decisions/README.md":        "not a decision",
	}
	scope, findings := ReadScopeDecisions(files, "tooling/cli/")
	if diag.HasErrors(findings) || !scope.Present || scope.Location != "tooling/cli/decisions" || scope.Scope != "tooling/cli" {
		t.Fatalf("scope = %+v, findings = %v", scope, findings)
	}
	var got []string
	for _, decision := range scope.Decisions {
		got = append(got, decision.Decision.ID+"@"+decision.Source)
	}
	want := []string{
		"D-001@tooling/cli/decisions/D-001.json",
		"D-002@tooling/cli/decisions/D-002.json",
		"cache-trust@tooling/cli/decisions/cache-trust.json",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("decisions = %v, want %v", got, want)
	}
}

// TestReadScopeDecisionsReadsTheSingleFileForm: a scope that still holds one
// decisions.json reads as before, each decision sourced on that file.
func TestReadScopeDecisionsReadsTheSingleFileForm(t *testing.T) {
	encoded, err := MarshalDecisionRegistry(registryOf(reviewOnlyDecision("D-002"), scaleToZero()))
	if err != nil {
		t.Fatal(err)
	}
	scope, findings := ReadScopeDecisions(mapDecisionFiles{"decisions.json": string(encoded)}, "")
	if diag.HasErrors(findings) || !scope.Present || scope.Location != "decisions.json" || len(scope.Decisions) != 2 {
		t.Fatalf("scope = %+v, findings = %v", scope, findings)
	}
	if scope.Decisions[0].Decision.ID != "D-001" || scope.Decisions[0].Source != "decisions.json" {
		t.Fatalf("first decision = %+v", scope.Decisions[0])
	}
}

// TestReadScopeDecisionsRefusesBothForms: a scope holds one form or the other,
// and the refusal says how to leave the single file.
func TestReadScopeDecisionsRefusesBothForms(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-decision", "a-scope-holds-one-registry-form")
	encoded, err := MarshalDecisionRegistry(registryOf(scaleToZero()))
	if err != nil {
		t.Fatal(err)
	}
	files := mapDecisionFiles{
		"api/decisions.json":         string(encoded),
		"api/decisions/D-002.json":   decisionFile(t, reviewOnlyDecision("D-002")),
		"other/decisions/D-003.json": decisionFile(t, reviewOnlyDecision("D-003")),
	}
	scope, findings := ReadScopeDecisions(files, "api")
	if !scope.Present || scope.Decisions != nil || !diag.HasErrors(findings) {
		t.Fatalf("both forms accepted: scope = %+v, findings = %v", scope, findings)
	}
	if findings[0].Field != "api/decisions.json" || !strings.Contains(findings[0].Message, "move each decision of api/decisions.json to api/decisions/<id>.json") {
		t.Fatalf("finding = %+v, want the way out", findings[0])
	}
}

// TestReadScopeDecisionsAbsenceIsAdoption: no file and no decisions/
// directory, or a decisions/ directory with no .json file, is no registry.
func TestReadScopeDecisionsAbsenceIsAdoption(t *testing.T) {
	for name, files := range map[string]mapDecisionFiles{
		"nothing":            {},
		"directory, no json": {"api/decisions/notes.md": "x"},
	} {
		scope, findings := ReadScopeDecisions(files, "api")
		if scope.Present || len(findings) != 0 || scope.Location != "" {
			t.Errorf("%s: scope = %+v, findings = %v", name, scope, findings)
		}
	}
}

// TestReadScopeDecisionsFailsClosed: one broken decision file leaves the scope
// with no decision, and the finding is anchored on the broken file.
func TestReadScopeDecisionsFailsClosed(t *testing.T) {
	files := mapDecisionFiles{
		"decisions/D-001.json": decisionFile(t, scaleToZero()),
		"decisions/D-002.json": `{"protocolVersion":1,"id":"D-002","statement":"s","settled":"2026-13-01","settledBy":"a","reviewOnly":true}`,
	}
	scope, findings := ReadScopeDecisions(files, "")
	if scope.Decisions != nil || !scope.Present {
		t.Fatalf("a broken file left decisions: %+v", scope)
	}
	if len(findings) != 1 || findings[0].Field != "decisions/D-002.json#settled" {
		t.Fatalf("findings = %+v, want one anchored on decisions/D-002.json#settled", findings)
	}
}

// TestReadScopeDecisionsBoundsTheDirectory keeps a scope's decisions finite:
// the guidance and the project context render them whole.
func TestReadScopeDecisionsBoundsTheDirectory(t *testing.T) {
	files := mapDecisionFiles{}
	for i := 0; i <= decisionsMaxEntries; i++ {
		id := fmt.Sprintf("d-%04d", i)
		files[path.Join(DecisionsDirname, id+".json")] = decisionFile(t, reviewOnlyDecision(id))
	}
	scope, findings := ReadScopeDecisions(files, "")
	if scope.Decisions != nil || !diag.HasErrors(findings) {
		t.Fatalf("an unbounded directory was read: %d decisions", len(scope.Decisions))
	}
}

// TestDecisionFilePaths pins where the directory form lives.
func TestDecisionFilePaths(t *testing.T) {
	for scope, want := range map[string]string{"": "decisions", ".": "decisions", "tooling/cli/": "tooling/cli/decisions"} {
		if got := DecisionsDirectoryPath(scope); got != want {
			t.Errorf("DecisionsDirectoryPath(%q) = %q, want %q", scope, got, want)
		}
	}
	if got := DecisionFilePath("tooling/cli", "cache-trust"); got != "tooling/cli/decisions/cache-trust.json" {
		t.Errorf("DecisionFilePath = %q", got)
	}
}

// TestDecisionSchemaPublishesTheWireType keeps the committed schema of one
// decision file and its Go wire type from drifting.
func TestDecisionSchemaPublishesTheWireType(t *testing.T) {
	schema := filepath.Join("schemas", "putnami-decision.json")
	assertSchemaProperties(t, schema, DecisionSchemaURL, jsonFieldNames(t, DecisionDocument{}))
	assertDefProperties(t, schema, "jsonValueCheck", jsonFieldNames(t, DecisionCheck{}))
}
