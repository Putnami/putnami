package estimate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// exampleResult is the estimate of testdata/plan.json with the default
// table: 6 files, then 9 files and one endpoint, both in the 6 to 15 files
// bucket; the second slice touches a sensitive zone.
func exampleResult(path string) Result {
	bucket := DefaultTable().Files[1]
	return Result{ProtocolVersion: 1, Plan: path, Issue: "42", Size: SizeL,
		Tokens:  Range{35_200_000, 97_000_000},
		Minutes: Range{88, 288},
		Slices: []SliceEstimate{
			{Key: "plan-format", Files: 6, Size: SizeM, Tokens: bucket.Tokens, Minutes: bucket.Minutes},
			{Key: "estimate-command", Files: 11, Size: SizeM, Tokens: bucket.Tokens, Minutes: bucket.Minutes},
		}}
}

func onePlan(files, endpoints, screens int, zones ...string) Plan {
	if zones == nil {
		zones = []string{}
	}
	return Plan{ProtocolVersion: 1, Issue: "7", Slices: []Slice{{Key: "only", Title: "Only", Projects: []string{"a"},
		Files: files, Endpoints: endpoints, Screens: screens, SensitiveZones: zones}}}
}

// TestTheSamePlanGetsTheSameEstimate pins the estimate of the example plan,
// and that the order of its members changes nothing.
func TestTheSamePlanGetsTheSameEstimate(t *testing.T) {
	t.Parallel()
	plan, err := ParsePlan(readExample(t))
	if err != nil {
		t.Fatal(err)
	}
	first := Estimate(plan, DefaultTable(), "plan.json")
	if want := exampleResult("plan.json"); !reflect.DeepEqual(first, want) {
		t.Fatalf("estimate %+v, want %+v", first, want)
	}
	reordered := mutate(t, func(map[string]any) {})
	again, err := ParsePlan(reordered)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(Estimate(again, DefaultTable(), "plan.json"))
	if !bytes.Equal(a, b) {
		t.Fatalf("the same plan gave %s, then %s", a, b)
	}
	for _, tc := range []struct {
		name                      string
		files, endpoints, screens int
		counted                   int
		size                      Size
	}{
		{"no file", 0, 0, 0, 0, SizeS},
		{"the last count of the first bucket", 5, 0, 0, 5, SizeS},
		{"the first count of the second bucket", 6, 0, 0, 6, SizeM},
		{"an endpoint counts two files", 4, 1, 0, 6, SizeM},
		{"a screen counts three files", 2, 0, 1, 5, SizeS},
		{"the third bucket", 40, 0, 0, 40, SizeM},
		{"the open bucket", 41, 0, 0, 41, SizeL},
		{"the largest plan", 10000, 1000, 1000, 15000, SizeL},
	} {
		got := Estimate(onePlan(tc.files, tc.endpoints, tc.screens), DefaultTable(), "p").Slices[0]
		if got.Files != tc.counted || got.Size != tc.size {
			t.Errorf("%s: %d files counted, size %s; want %d, %s", tc.name, got.Files, got.Size, tc.counted, tc.size)
		}
	}
	spectest.Proves(t, estimateFeature, estimateDeterminim, "the-same-plan-gets-the-same-estimate")
}

// TestASensitiveZoneIsNeverS pins that a zone moves an S slice and its plan
// to M, and leaves an L plan L.
func TestASensitiveZoneIsNeverS(t *testing.T) {
	t.Parallel()
	plain := Estimate(onePlan(2, 0, 0), DefaultTable(), "p")
	zoned := Estimate(onePlan(2, 0, 0, "data migration"), DefaultTable(), "p")
	if plain.Size != SizeS || plain.Slices[0].Size != SizeS {
		t.Fatalf("plain plan %s, slice %s; want S", plain.Size, plain.Slices[0].Size)
	}
	if zoned.Size != SizeM || zoned.Slices[0].Size != SizeM {
		t.Fatalf("zoned plan %s, slice %s; want M", zoned.Size, zoned.Slices[0].Size)
	}
	if zoned.Tokens != plain.Tokens || zoned.Minutes != plain.Minutes {
		t.Fatal("a zone changed the ranges; it changes only the size")
	}
	if large := Estimate(onePlan(200, 0, 0, "public API"), DefaultTable(), "p"); large.Size != SizeL {
		t.Fatalf("large zoned plan %s, want L", large.Size)
	}
	spectest.Proves(t, estimateFeature, estimateDeterminim, "a-sensitive-zone-is-never-s")
}

// workspace writes a workspace root with a contributor policy that holds
// estimate, or none when estimate is empty, and the example plan in
// .putnami/42/plan.json.
func workspace(t *testing.T, estimate string) string {
	t.Helper()
	root := t.TempDir()
	policy := `{"version": 1}`
	if estimate != "" {
		policy = `{"version": 1, "estimate": ` + estimate + `}`
	}
	document := `{"options": {"@putnami/contributor": ` + policy + `}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".putnami", "42")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), readExample(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func launchContext(t *testing.T, members map[string]any) string {
	t.Helper()
	members["protocolVersion"] = 2
	data, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCommand(root, contextFile string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Command{Root: root, ContextFile: contextFile, Stdout: &stdout, Stderr: &stderr}.Run(args)
	return code, stdout.String(), stderr.String()
}

// TestTheCommandPrintsTextOrTheJSONResult runs the command on the default
// plan path and on --plan-file, in text and in JSON, and pins its usage errors.
func TestTheCommandPrintsTextOrTheJSONResult(t *testing.T) {
	t.Parallel()
	root := workspace(t, "")
	shown := filepath.Join(".putnami", "42", "plan.json")

	code, stdout, stderr := runCommand(root, "", "--issue", "42")
	want := "Plan " + shown + ": L, 35.2M to 97.0M tokens, 88 to 288 minutes\n" +
		"  1. plan-format: M, 17.6M to 48.5M tokens, 44 to 144 minutes, 6 files counted\n" +
		"  2. estimate-command: M, 17.6M to 48.5M tokens, 44 to 144 minutes, 11 files counted\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}

	code, stdout, stderr = runCommand(root, launchContext(t, map[string]any{"params": map[string]any{"output": "json"}}), "--issue=#42")
	var result Result
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &result) != nil || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q; want one JSON line", code, stdout, stderr)
	}
	if want := exampleResult(shown); !reflect.DeepEqual(result, want) {
		t.Fatalf("result %+v, want %+v", result, want)
	}

	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "mine.json"), readExample(t), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCommand(root, launchContext(t, map[string]any{"invocationDir": elsewhere}), "--plan-file", "mine.json")
	if code != 0 || !strings.HasPrefix(stdout, "Plan mine.json: L,") {
		t.Fatalf("exit %d, stdout %q; want mine.json read from the invocation directory", code, stdout)
	}

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "give --issue <id> to read .putnami/<id>/plan.json, or --plan-file <file>"},
		{[]string{"--issue", "../x"}, 2, "--issue must be an issue id"},
		{[]string{"--issue", "1", "--issue", "2"}, 2, "--issue given twice"},
		{[]string{"--plan-file"}, 2, "--plan-file needs a value"},
		{[]string{"--projects", "a"}, 2, `unexpected argument "--projects"`},
		{[]string{"--issue", "43"}, 1, filepath.Join(".putnami", "43", "plan.json") + ": no plan file"},
		{[]string{"--issue", "41", "--plan-file", filepath.Join(root, shown)}, 1, `the plan names issue "42", not "41"`},
	} {
		code, stdout, stderr := runCommand(root, "", tc.args...)
		if code != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) || !strings.HasPrefix(stderr, "change estimate: ") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit %d and %q", tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
	spectest.Proves(t, estimateFeature, estimateDeterminim, "the-command-prints-text-or-the-json-result")
}

// TestThePolicyReplacesTheMembersItSets pins that each member the policy
// sets replaces the default one whole, and that the others stay.
func TestThePolicyReplacesTheMembersItSets(t *testing.T) {
	t.Parallel()
	table, err := ReadTable(workspace(t, `{"files": [{"upTo": 20, "tokens": {"low": 1, "high": 2}, "minutes": {"low": 3, "high": 4}}, {"tokens": {"low": 5, "high": 6}, "minutes": {"low": 7, "high": 500}}], "sizeMinutes": {"S": 4, "M": 400}}`))
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultTable()
	if len(table.Files) != 2 || *table.Files[0].UpTo != 20 || table.Files[1].UpTo != nil || table.Files[1].Minutes.High != 500 {
		t.Fatalf("files %+v, want the policy's two buckets", table.Files)
	}
	if table.SizeMinutes != (SizeMinutes{S: 4, M: 400}) {
		t.Fatalf("size minutes %+v, want the policy's", table.SizeMinutes)
	}
	if table.EndpointFiles != defaults.EndpointFiles || table.ScreenFiles != defaults.ScreenFiles {
		t.Fatalf("allowances %d and %d, want the defaults", table.EndpointFiles, table.ScreenFiles)
	}
	plan, _ := ParsePlan(readExample(t))
	if result := Estimate(plan, table, "p"); result.Size != SizeM || result.Minutes != (Range{6, 8}) {
		t.Fatalf("estimate %+v, want M over 6 to 8 minutes", result)
	}
	code, stdout, stderr := runCommand(workspace(t, `{"sizeMinutes": {"S": 0, "M": 0}}`), "", "--issue", "42")
	if code != 0 || !strings.Contains(stdout, "plan.json: L,") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the policy thresholds applied", code, stdout, stderr)
	}
	for _, root := range []string{t.TempDir(), workspace(t, "")} {
		if table, err := ReadTable(root); err != nil || !reflect.DeepEqual(table, defaults) {
			t.Fatalf("table %+v, error %v; want the default table", table, err)
		}
	}
	spectest.Proves(t, estimateFeature, estimatePolicy, "the-policy-replaces-the-members-it-sets")
}

// TestAnInvalidPolicyTableIsRefused pins each refusal of a policy table.
func TestAnInvalidPolicyTableIsRefused(t *testing.T) {
	t.Parallel()
	bucket := func(upTo string) string {
		return `{` + upTo + `"tokens": {"low": 1, "high": 2}, "minutes": {"low": 1, "high": 2}}`
	}
	for _, tc := range []struct {
		estimate string
		want     string
	}{
		{`[]`, "contributor policy estimate: must be an object"},
		{`{"buckets": []}`, `contributor policy estimate: unknown member "buckets"`},
		{`{"endpointFiles": 1, "endpointFiles": 2}`, `member "endpointFiles" appears twice`},
		{`{"endpointFiles": -1}`, "contributor policy estimate.endpointFiles: must be an integer from 0 to 1000"},
		{`{"files": []}`, "contributor policy estimate.files: must hold from 1 to 20 items"},
		{`{"files": [` + bucket(`"upTo": 3, `) + `]}`, "files[0].upTo: the last bucket takes every larger count and has no upTo"},
		{`{"files": [` + bucket(``) + `, ` + bucket(``) + `]}`, "files[0].upTo: required on every bucket but the last"},
		{`{"files": [` + bucket(`"upTo": 3, `) + `, ` + bucket(`"upTo": 3, `) + `, ` + bucket(``) + `]}`, "files[1].upTo: must exceed the upTo of files[0]"},
		{`{"files": [{"tokens": {"low": 3, "high": 2}, "minutes": {"low": 1, "high": 2}}]}`, "files[0].tokens: low exceeds high"},
		{`{"files": [{"tokens": {"low": 1, "high": 2}, "minutes": {"low": 1}}]}`, "estimate.files[0].minutes.high: required"},
		{`{"sizeMinutes": {"S": 300, "M": 200}}`, "sizeMinutes: S exceeds M"},
	} {
		_, err := ReadTable(workspace(t, tc.estimate))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one that contains %q", tc.estimate, err, tc.want)
		}
	}
	code, stdout, stderr := runCommand(workspace(t, `{"sizeMinutes": {"S": 300, "M": 200}}`), "", "--issue", "42")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "sizeMinutes: S exceeds M") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming the policy", code, stdout, stderr)
	}
	spectest.Proves(t, estimateFeature, estimatePolicy, "an-invalid-policy-table-is-refused")
}
