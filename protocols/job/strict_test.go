package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseFile(t *testing.T) {
	ctx, err := ParseFile(filepath.Join("fixtures", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Workspace.Name != "putnami" {
		t.Errorf("workspace name = %q, want putnami", ctx.Workspace.Name)
	}
	if ctx.Workspace.RootPath != "/ws" {
		t.Errorf("workspace rootPath = %q, want /ws", ctx.Workspace.RootPath)
	}
}

func TestParseFile_SelectedProjectOutputPath(t *testing.T) {
	ctx, err := ParseFile(filepath.Join("fixtures", "valid", "with-selected-projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.SelectedProjects) != 2 {
		t.Fatalf("selectedProjects = %d, want 2", len(ctx.SelectedProjects))
	}
	// The per-project OutputPath is the batch-execution contract: each selected
	// project carries its own captured output directory so a batched extension
	// process writes each project's outputs to its own cache entry.
	if got := ctx.SelectedProjects[0].OutputPath; got != "/ws/.putnami/out/a/build" {
		t.Errorf("selectedProjects[0].outputPath = %q, want /ws/.putnami/out/a/build", got)
	}
	if got := ctx.SelectedProjects[1].OutputPath; got != "/ws/.putnami/out/libs/b/build" {
		t.Errorf("selectedProjects[1].outputPath = %q, want /ws/.putnami/out/libs/b/build", got)
	}
}

// The declared identity a provider's workspace-sync task needs. Name is the
// resolved answer; SourceName is what the project declared before a scope
// namePattern override. A task that aligns native manifest names must skip the
// members whose SourceName already equals Name — those manifests are where the
// identity came from, and renaming them orphans every sibling reference.
//
// It reads from the v2 corpus because the member is v2-only, and because
// fixtures/valid is the FROZEN v1 corpus other runtimes' SDK conformance suites
// read: adding a member there would make an older strict parser reject a
// document it is required to accept.
func TestParseFile_ProjectReferenceSourceName(t *testing.T) {
	ctx, err := ParseFile(filepath.Join("fixtures", "v2", "valid", "project-references.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.SelectedProjects[1].SourceName; got != "widget" {
		t.Errorf("selectedProjects[1].sourceName = %q, want widget", got)
	}
	// Optional within v2: a producer with nothing to say omits the member, and
	// a task reading the empty value must decline the rename.
	if got := ctx.SelectedProjects[2].SourceName; got != "" {
		t.Errorf("selectedProjects[2].sourceName = %q, want empty when the member is absent", got)
	}
	// ONE shape: the closure carries the member the same way the selection does.
	if got := ctx.Project.DependencyClosure[1].SourceName; got != "widget" {
		t.Errorf("project.dependencyClosure[1].sourceName = %q, want widget", got)
	}
}

// The member round-trips through the strict parser: DisallowUnknownFields means
// a document carrying `sourceName` is only legal because the contract declares
// it, and re-encoding must put it back on the wire for the next reader.
func TestParseStrict_ProjectReferenceSourceNameRoundTrips(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "v2", "valid", "project-references.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, diags := ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("strict parse rejected sourceName: %v", diags)
	}
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round, diags := ParseAndValidate(encoded)
	if diag.HasErrors(diags) {
		t.Fatalf("re-encoded document failed strict parse: %v", diags)
	}
	if got := round.SelectedProjects[1].SourceName; got != "widget" {
		t.Errorf("round-tripped sourceName = %q, want widget", got)
	}
	if got := round.SelectedProjects[2].SourceName; got != "" {
		t.Errorf("round-tripped absent sourceName = %q, want it still absent", got)
	}
	if got := round.Project.DependencyClosure[1].SourceName; got != "widget" {
		t.Errorf("round-tripped closure sourceName = %q, want widget", got)
	}
}

// A v1 document may not carry the member: a v1 consumer would ignore exactly
// what a v2 consumer acts on, and the disagreement is a workspace-wide rename.
func TestValidate_V1RejectsSourceName(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "v2", "invalid", "v1-with-source-name.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, diags := ParseAndValidate(data)
	if !diag.HasErrors(diags) {
		t.Fatal("a v1 document carrying selectedProjects[].sourceName must be rejected")
	}
	var found bool
	for _, d := range diags {
		if d.Code == ErrorCodeUnexpectedField && d.Field == "selectedProjects[1].sourceName" {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics %v do not report selectedProjects[1].sourceName as unexpected at v1", diags)
	}
}

func TestParseFile_Missing(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseFile_BadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParse_BadJSON(t *testing.T) {
	if _, err := Parse([]byte("[]")); err == nil {
		t.Fatal("expected error for non-object JSON")
	}
}

func TestParseStrict_BadJSON(t *testing.T) {
	if _, diags := ParseStrict([]byte("{broken")); !diag.HasErrors(diags) {
		t.Fatal("expected diagnostics for invalid JSON")
	}
}

func TestValidate_Nil(t *testing.T) {
	if diags := Validate(nil); !diag.HasErrors(diags) {
		t.Fatal("expected diagnostics for nil context")
	}
}

func TestParams_BoolSynonyms(t *testing.T) {
	p := Params{
		"on":      []byte(`"on"`),
		"off":     []byte(`"off"`),
		"one":     []byte(`"1"`),
		"zero":    []byte(`"0"`),
		"native":  []byte(`true`),
		"garbage": []byte(`"maybe"`),
	}
	cases := []struct {
		key  string
		def  bool
		want bool
	}{
		{"on", false, true},
		{"off", true, false},
		{"one", false, true},
		{"zero", true, false},
		{"native", false, true},
		{"garbage", true, true}, // unrecognized → default
		{"missing", true, true},
	}
	for _, c := range cases {
		if got := p.Bool(c.key, c.def); got != c.want {
			t.Errorf("Bool(%q, %v) = %v, want %v", c.key, c.def, got, c.want)
		}
	}
}

func TestParams_IntAndString(t *testing.T) {
	p := Params{
		"n":       []byte(`42`),
		"ns":      []byte(`"42"`),
		"s":       []byte(`"hello"`),
		"unquote": []byte(`123abc`),
	}
	if got := p.Int("n", 0); got != 42 {
		t.Errorf("Int(n) = %d, want 42", got)
	}
	if got := p.Int("ns", 0); got != 42 {
		t.Errorf("Int(ns) = %d, want 42 (numeric string)", got)
	}
	if got := p.Int("s", 5); got != 5 {
		t.Errorf("Int(s) = %d, want default 5", got)
	}
	if got := p.String("s"); got != "hello" {
		t.Errorf("String(s) = %q, want hello", got)
	}
	if got := p.String("unquote"); got != "123abc" {
		t.Errorf("String(unquote) = %q, want raw value", got)
	}
	if got := p.Float("missing", 1.5); got != 1.5 {
		t.Errorf("Float(missing) = %v, want default", got)
	}
}

func TestGetBinString_NonString(t *testing.T) {
	p := &Project{Bin: []byte(`{"cli":"dist/cli.js"}`)}
	if got := p.GetBinString(); got != "" {
		t.Errorf("GetBinString on object = %q, want empty", got)
	}
	empty := &Project{}
	if got := empty.GetBinString(); got != "" {
		t.Errorf("GetBinString on nil bin = %q, want empty", got)
	}
}

func TestPublishChannels_Absent(t *testing.T) {
	ctx := &Context{}
	if got := ctx.PublishChannels(); got != nil {
		t.Errorf("PublishChannels = %v, want nil", got)
	}
	bad := &Context{Project: Project{Publish: []byte(`"npm"`)}}
	if got := bad.PublishChannels(); got != nil {
		t.Errorf("PublishChannels on non-array = %v, want nil", got)
	}
}

// The release-set half of a mixed gate+publish selection is the subset of the
// run's own projects whose publication the coordinator owns. A document where
// it is unsorted, duplicated or names a project the run never planned would let
// a consumer subtract one list from the other and describe work that never
// happened, so the corpus rejects all three from the wire.
func TestValidate_SelectionReleaseSetProjectsAreASortedSubset(t *testing.T) {
	for _, fixture := range []string{
		"selection-release-set-unsorted.json",
		"selection-release-set-not-a-subset.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			_, diags := ParseAndValidate(readFixture(t, "fixtures", "v2", "invalid", fixture))
			assertDiagnostic(t, diags, ErrorCodeInvalidValue, "selection.releaseSetProjects")
		})
	}

	valid := &Selection{
		Mode: SelectionModeImpacted, Scoped: true,
		ProjectIDs: []string{"/apps/console", "/libs/widget"},
	}
	for name, releaseSet := range map[string][]string{
		"absent when the session coordinates no release set": nil,
		"the members beside the projects it verified":        {"/apps/console"},
		"every planned project, for a publish-only session":  {"/apps/console", "/libs/widget"},
		"a duplicate is not a sorted list either":            {"/apps/console", "/apps/console"},
	} {
		t.Run(name, func(t *testing.T) {
			selection := *valid
			selection.ReleaseSetProjects = releaseSet
			ctx := validV2Context()
			ctx.Selection = &selection
			ctx.presence.selection = true
			diags := Validate(ctx)
			duplicated := len(releaseSet) == 2 && releaseSet[0] == releaseSet[1]
			if duplicated {
				assertDiagnostic(t, diags, ErrorCodeInvalidValue, "selection.releaseSetProjects")
				return
			}
			if diag.HasErrors(diags) {
				t.Fatalf("expected acceptance, got %v", diags)
			}
		})
	}
}

// The member round-trips: DisallowUnknownFields means a document carrying
// `releaseSetProjects` is only legal because the contract declares it, and
// re-encoding must put it back on the wire for the next reader.
func TestParseStrict_SelectionReleaseSetProjectsRoundTrip(t *testing.T) {
	data := readFixture(t, "fixtures", "v2", "valid", "selection-release-set.json")
	ctx, diags := ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("strict parse rejected releaseSetProjects: %v", diags)
	}
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	round, diags := ParseAndValidate(encoded)
	if diag.HasErrors(diags) {
		t.Fatalf("re-encoded document failed strict parse: %v", diags)
	}
	if got := round.Selection.ReleaseSetProjects; len(got) != 1 || got[0] != "/apps/console" {
		t.Errorf("round-tripped selection.releaseSetProjects = %v, want [/apps/console]", got)
	}
	// The gate's own answer survives beside it: the members were decided by the
	// channel head, the verification set by the baseline reported here.
	if round.Selection.BaselineSource != "upstream" || round.Selection.Baseline != "origin/main" {
		t.Errorf("round-tripped baseline = %q/%q, want origin/main from the git tier",
			round.Selection.Baseline, round.Selection.BaselineSource)
	}
}
