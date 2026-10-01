package workspace

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestProbe_ProtocolVersionPinned(t *testing.T) {
	if ProbeProtocolVersion != 1 {
		t.Fatalf("ProbeProtocolVersion = %d, want 1 — bumping requires a migration story on both "+
			"core and every out-of-tree provider", ProbeProtocolVersion)
	}
}

// Original v1 consumers decode with DisallowUnknownFields. That makes adding an
// "optional" result member a breaking provider-newer-than-core change: the
// consumer rejects the whole probe before it can schedule any task. Freeze the
// result-side JSON members while the version remains 1; new facts must reuse an
// existing member, ship behind a protocol-version migration, or be NEGOTIATED:
// requested by a request member core sends only to a provider whose manifest
// declares it, and stripped by ServeProbe from every answer that was not asked
// (dependencySources is the one such member; see
// TestServeProbe_AnswersDependencySourcesOnlyWhenAsked).
func TestProbeV1ResultWireShapeStaysStrictConsumerCompatible(t *testing.T) {
	assertJSONFields := func(name string, value any, want []string) {
		t.Helper()
		typ := reflect.TypeOf(value)
		got := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if field != "" && field != "-" {
				got = append(got, field)
			}
		}
		gotSet := make(map[string]bool, len(got))
		for _, field := range got {
			gotSet[field] = true
		}
		compatible := len(got) == len(want)
		for _, field := range want {
			compatible = compatible && gotSet[field]
		}
		if !compatible {
			t.Fatalf("%s JSON fields = %v, want strict v1 shape %v", name, got, want)
		}
	}

	assertJSONFields("ProbeResult", ProbeResult{}, []string{
		"version", "extension", "projects", "watchedFiles", "diagnostics",
	})
	assertJSONFields("ProbeProject", ProbeProject{}, []string{
		"path", "sourceName", "sourceFile", "version", "type", "tags",
		"dependencies", "dependencySources", "publish", "runsWith", "extensions",
		"watchedFiles", "metadata",
	})
}

func TestNormalizeProbePath(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		wantK bool
	}{
		{"", ProbeRootPath, true},
		{".", ProbeRootPath, true},
		{"  ", ProbeRootPath, true},
		{"./go/framework/http", "go/framework/http", true},
		{"go/framework/../framework/http", "go/framework/http", true},
		{"go/framework/http/", "go/framework/http", true},
		{"/go/framework/http", "", false},
		{"..", "", false},
		{"../outside", "", false},
		{"go\\framework\\http", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeProbePath(tc.in)
		if ok != tc.wantK || got != tc.want {
			t.Errorf("NormalizeProbePath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantK)
		}
	}
}

// The digest must be a function of the FACTS, not of how a provider happened
// to serialize them. Two results that differ only in authoring order, path
// spelling, list order, duplicate entries and metadata key order must digest
// identically — otherwise a provider's internal map iteration would oscillate
// every cache key that observes it.
func TestProbeResultDigest_IndependentOfAuthoringOrder(t *testing.T) {
	a := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "@putnami/go",
		Projects: []ProbeProject{
			{
				Path:         "go/framework/http",
				Dependencies: []string{"./go/framework/app", "protocols/http-routes", "go/framework/app"},
				Tags:         []string{"go", "go"},
				WatchedFiles: []string{"go.work.sum", "go.work", "go.work"},
				Metadata:     json.RawMessage(`{"b":2,"a":1}`),
			},
			{Path: "go/framework/app"},
		},
		WatchedFiles: []string{"go.work.sum", "go.work"},
	}
	b := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "@putnami/go",
		Projects: []ProbeProject{
			{Path: "./go/framework/app/"},
			{
				Path:         "go/framework/http/",
				Dependencies: []string{"protocols/http-routes", "go/framework/app"},
				Tags:         []string{"go"},
				WatchedFiles: []string{"go.work", "./go.work.sum"},
				Metadata:     json.RawMessage("{\n  \"a\": 1,\n  \"b\": 2\n}"),
			},
		},
		WatchedFiles: []string{"go.work", "./go.work.sum"},
	}

	if ProbeResultDigest(a) != ProbeResultDigest(b) {
		t.Fatalf("digest depends on authoring order:\n a=%s\n b=%s", ProbeResultDigest(a), ProbeResultDigest(b))
	}
	// Digesting must not mutate the caller's value.
	if a.Projects[0].Path != "go/framework/http" || len(a.Projects[0].Dependencies) != 3 {
		t.Errorf("ProbeResultDigest mutated its argument: %+v", a.Projects[0])
	}
}

func TestProbeResultDigest_ExcludesDiagnostics(t *testing.T) {
	base := ProbeResult{Version: ProbeProtocolVersion, Extension: "@putnami/go",
		Projects: []ProbeProject{{Path: "go/framework/http"}}}
	noisy := base
	noisy.Diagnostics = []diag.Diagnostic{diag.Warningf("slow-probe", "", "took a while")}

	if ProbeResultDigest(base) != ProbeResultDigest(noisy) {
		t.Fatal("advisory diagnostics moved the digest — a reworded warning must not cold every cache key")
	}
}

func TestProbeResultDigest_MovesWithEveryContractMember(t *testing.T) {
	base := ProbeResult{Version: ProbeProtocolVersion, Extension: "@putnami/go",
		Projects: []ProbeProject{{Path: "go/framework/http"}}}
	baseDigest := ProbeResultDigest(base)

	mutations := map[string]func(*ProbeResult){
		"extension":    func(r *ProbeResult) { r.Extension = "@putnami/typescript" },
		"sourceName":   func(r *ProbeResult) { r.Projects[0].SourceName = "go.putnami.dev/http" },
		"sourceFile":   func(r *ProbeResult) { r.Projects[0].SourceFile = "go/framework/http/go.mod" },
		"version":      func(r *ProbeResult) { r.Projects[0].Version = "0.2.0" },
		"type":         func(r *ProbeResult) { r.Projects[0].Type = "library" },
		"tags":         func(r *ProbeResult) { r.Projects[0].Tags = []string{"go"} },
		"dependencies": func(r *ProbeResult) { r.Projects[0].Dependencies = []string{"go/framework/app"} },
		"publish":      func(r *ProbeResult) { r.Projects[0].Publish = []string{"go"} },
		"runsWith":     func(r *ProbeResult) { r.Projects[0].RunsWith = []string{"postgres"} },
		"extensions":   func(r *ProbeResult) { r.Projects[0].Extensions = []string{"/go/extension"} },
		"watchedFiles": func(r *ProbeResult) { r.Projects[0].WatchedFiles = []string{"go/framework/http/go.sum"} },
		"metadata":     func(r *ProbeResult) { r.Projects[0].Metadata = json.RawMessage(`{"toolchain":"1.25.7"}`) },
		"resultWatched": func(r *ProbeResult) {
			r.WatchedFiles = []string{"go.work"}
		},
		"newProject": func(r *ProbeResult) {
			r.Projects = append(r.Projects, ProbeProject{Path: "go/framework/app"})
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := ProbeResult{Version: base.Version, Extension: base.Extension,
				Projects: []ProbeProject{{Path: "go/framework/http"}}}
			mutate(&mutated)
			if ProbeResultDigest(mutated) == baseDigest {
				t.Errorf("moving %s did not move the digest — the member is contract and must key the cache", name)
			}
		})
	}
}

func TestProbeWorkspaceDigest_IndependentOfProviderOrder(t *testing.T) {
	goResult := ProbeResult{Version: ProbeProtocolVersion, Extension: "@putnami/go",
		Projects: []ProbeProject{{Path: "go/framework/http"}}}
	tsResult := ProbeResult{Version: ProbeProtocolVersion, Extension: "@putnami/typescript",
		Projects: []ProbeProject{{Path: "typescript/framework/web"}}}

	forward := ProbeWorkspaceDigest([]ProbeResult{goResult, tsResult})
	reverse := ProbeWorkspaceDigest([]ProbeResult{tsResult, goResult})
	if forward != reverse {
		t.Fatalf("aggregate digest depends on provider order: %s vs %s", forward, reverse)
	}
	if forward == ProbeWorkspaceDigest([]ProbeResult{goResult}) {
		t.Fatal("dropping a provider did not move the aggregate digest")
	}
	if ProbeWorkspaceDigest(nil) == "" {
		t.Fatal("the empty aggregate must still be a stable digest, not an empty string")
	}
}

func TestCanonicalMetadata(t *testing.T) {
	canonical, ok := CanonicalMetadata(json.RawMessage(`{"b":2,"a":{"d":4,"c":3}}`))
	if !ok {
		t.Fatal("object metadata rejected")
	}
	if string(canonical) != `{"a":{"c":3,"d":4},"b":2}` {
		t.Errorf("canonical metadata = %s", canonical)
	}
	if _, ok := CanonicalMetadata(json.RawMessage(`[1,2]`)); ok {
		t.Error("array metadata accepted; it must be a JSON object")
	}
	if _, ok := CanonicalMetadata(json.RawMessage(`"text"`)); ok {
		t.Error("string metadata accepted; it must be a JSON object")
	}
	if got, ok := CanonicalMetadata(nil); !ok || got != nil {
		t.Errorf("absent metadata = (%v, %v), want (nil, true)", got, ok)
	}
	if got, ok := CanonicalMetadata(json.RawMessage(`{}`)); !ok || got != nil {
		t.Errorf("empty-object metadata = (%v, %v), want (nil, true)", got, ok)
	}
}

func TestNormalizeProbeResult_Idempotent(t *testing.T) {
	r := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "  @putnami/go  ",
		Projects: []ProbeProject{
			{Path: "./b/", Tags: []string{" z ", "a", "a"}, Dependencies: []string{"./x", "x"}},
			{Path: "a"},
		},
		WatchedFiles: []string{"./go.work", "go.work"},
	}
	NormalizeProbeResult(&r)
	once, _ := json.Marshal(r)
	NormalizeProbeResult(&r)
	twice, _ := json.Marshal(r)
	if string(once) != string(twice) {
		t.Fatalf("normalization is not idempotent:\n once=%s\ntwice=%s", once, twice)
	}
	if r.Extension != "@putnami/go" {
		t.Errorf("extension = %q", r.Extension)
	}
	if r.Projects[0].Path != "a" || r.Projects[1].Path != "b" {
		t.Errorf("projects not sorted by path: %q, %q", r.Projects[0].Path, r.Projects[1].Path)
	}
	if len(r.Projects[1].Tags) != 2 || r.Projects[1].Tags[0] != "a" || r.Projects[1].Tags[1] != "z" {
		t.Errorf("tags = %v, want [a z] (trimmed, deduped, sorted)", r.Projects[1].Tags)
	}
	if len(r.WatchedFiles) != 1 {
		t.Errorf("watchedFiles = %v, want one deduped entry", r.WatchedFiles)
	}
}

// --- validation -------------------------------------------------------------

func TestValidateProbeResult_Codes(t *testing.T) {
	cases := []struct {
		name   string
		result ProbeResult
		want   string
	}{
		{"version", ProbeResult{Version: 2, Extension: "x"}, "invalid-probe-version"},
		{"extension", ProbeResult{Version: 1}, "required-field"},
		{"missing path", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{SourceName: "y"}}}, "required-field"},
		{"absolute path", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{Path: "/abs"}}}, "invalid-path"},
		{"duplicate", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{Path: "a"}, {Path: "./a"}}}, "duplicate-project"},
		{"bad dependency", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{Path: "a", Dependencies: []string{"../b"}}}}, "invalid-path"},
		{"bad source file", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{Path: "a", SourceFile: "/etc/passwd"}}}, "invalid-path"},
		{"bad metadata", ProbeResult{Version: 1, Extension: "x",
			Projects: []ProbeProject{{Path: "a", Metadata: json.RawMessage(`[]`)}}}, "invalid-metadata"},
		{"bad watched file", ProbeResult{Version: 1, Extension: "x",
			WatchedFiles: []string{"a\\b"}}, "invalid-path"},
		{"empty watched file", ProbeResult{Version: 1, Extension: "x",
			WatchedFiles: []string{" "}}, "required-field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateProbeResult(&tc.result)
			if !hasCode(diags, tc.want) {
				t.Errorf("want code %q, got %v", tc.want, diags)
			}
		})
	}

	if diags := ValidateProbeResult(nil); !hasCode(diags, "nil-result") {
		t.Errorf("nil result: %v", diags)
	}
}

func TestValidateProbeRequest_Codes(t *testing.T) {
	cases := []struct {
		name    string
		request ProbeRequest
		want    string
	}{
		{"version", ProbeRequest{Version: 9, Extension: "x"}, "invalid-probe-version"},
		{"extension", ProbeRequest{Version: 1}, "required-field"},
		{"reason", ProbeRequest{Version: 1, Extension: "x", Reason: "guess"}, "invalid-probe-reason"},
		{"path", ProbeRequest{Version: 1, Extension: "x", Paths: []string{"/abs"}}, "invalid-path"},
		{"file", ProbeRequest{Version: 1, Extension: "x", Files: []string{"../out"}}, "invalid-path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateProbeRequest(&tc.request)
			if !hasCode(diags, tc.want) {
				t.Errorf("want code %q, got %v", tc.want, diags)
			}
		})
	}

	ok := ProbeRequest{Version: 1, Extension: "x", Reason: ProbeReasonPlan, Paths: []string{"a/b"}}
	if diags := ValidateProbeRequest(&ok); diag.HasErrors(diags) {
		t.Errorf("valid request rejected: %v", diags)
	}
	if diags := ValidateProbeRequest(nil); !hasCode(diags, "nil-request") {
		t.Errorf("nil request: %v", diags)
	}
}

func TestParseAndValidateProbe_RoundTrip(t *testing.T) {
	result, diags := ParseAndValidateProbeResult([]byte(`{"version":1,"extension":"@putnami/go",
		"projects":[{"path":"a","metadata":{"k":"v"}}]}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid payload rejected: %v", diags)
	}
	if result.Projects[0].Path != "a" {
		t.Errorf("path = %q", result.Projects[0].Path)
	}

	if _, diags := ParseAndValidateProbeResult([]byte(`{"version":1,"nope":true}`)); !diag.HasErrors(diags) {
		t.Error("unknown field accepted; strict decoding must bite")
	}
	if _, diags := ParseAndValidateProbeRequest([]byte(`{"version":1,"nope":true}`)); !diag.HasErrors(diags) {
		t.Error("unknown request field accepted; strict decoding must bite")
	}

	req, diags := ParseAndValidateProbeRequest([]byte(`{"version":1,"extension":"@putnami/go","reason":"plan"}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid request rejected: %v", diags)
	}
	if req.Reason != ProbeReasonPlan {
		t.Errorf("reason = %q", req.Reason)
	}
}

// --- merge rules ------------------------------------------------------------

func TestMergeProbeResults_UnionsListsAndBucketsMetadata(t *testing.T) {
	goResult := ProbeResult{Version: 1, Extension: "@putnami/go", Projects: []ProbeProject{{
		Path:         "svc",
		SourceName:   "go.putnami.dev/svc",
		Dependencies: []string{"go/framework/http"},
		Tags:         []string{"go"},
		Metadata:     json.RawMessage(`{"module":"go.putnami.dev/svc"}`),
	}}}
	tsResult := ProbeResult{Version: 1, Extension: "@putnami/typescript", Projects: []ProbeProject{{
		Path:         "svc",
		Dependencies: []string{"typescript/framework/web"},
		Tags:         []string{"ts"},
		Metadata:     json.RawMessage(`{"bundler":"bun"}`),
	}}}
	explicit := map[string]ExplicitProject{"svc": {Dependencies: []string{"protocols/cli"}, Publish: []string{"oci"}}}

	merged, diags := MergeProbeResults([]ProbeResult{tsResult, goResult}, explicit)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected conflicts: %v", diags)
	}
	view := merged["svc"]
	wantDeps := []string{"go/framework/http", "protocols/cli", "typescript/framework/web"}
	if len(view.Dependencies) != len(wantDeps) {
		t.Fatalf("dependencies = %v, want union %v", view.Dependencies, wantDeps)
	}
	for i, want := range wantDeps {
		if view.Dependencies[i] != want {
			t.Errorf("dependencies[%d] = %q, want %q", i, view.Dependencies[i], want)
		}
	}
	if len(view.Tags) != 2 || view.Tags[0] != "go" || view.Tags[1] != "ts" {
		t.Errorf("tags = %v, want union [go ts]", view.Tags)
	}
	if len(view.Publish) != 1 || view.Publish[0] != "oci" {
		t.Errorf("publish = %v, want the explicitly declared channel", view.Publish)
	}
	if string(view.Metadata["@putnami/go"]) != `{"module":"go.putnami.dev/svc"}` {
		t.Errorf("go metadata = %s", view.Metadata["@putnami/go"])
	}
	if string(view.Metadata["@putnami/typescript"]) != `{"bundler":"bun"}` {
		t.Errorf("ts metadata = %s", view.Metadata["@putnami/typescript"])
	}
	if view.SourceName != "go.putnami.dev/svc" {
		t.Errorf("sourceName = %q, want the single provider claim", view.SourceName)
	}
}

func TestMergeProbeResults_ConflictingScalarsAreHardErrors(t *testing.T) {
	a := ProbeResult{Version: 1, Extension: "@putnami/go",
		Projects: []ProbeProject{{Path: "svc", Type: "library"}}}
	b := ProbeResult{Version: 1, Extension: "@putnami/typescript",
		Projects: []ProbeProject{{Path: "svc", Type: "application"}}}

	merged, diags := MergeProbeResults([]ProbeResult{a, b}, nil)
	if !hasCode(diags, "probe-conflict") {
		t.Fatalf("conflicting non-empty scalars must be a hard error, got %v", diags)
	}
	if merged["svc"].Type != "" {
		t.Errorf("type = %q, want empty — the merge must not silently pick a winner", merged["svc"].Type)
	}

	// Provider order must not change the verdict or the message.
	_, reversed := MergeProbeResults([]ProbeResult{b, a}, nil)
	if len(reversed) != len(diags) || reversed[0].Message != diags[0].Message {
		t.Errorf("conflict diagnostic depends on provider order:\n %v\n %v", diags, reversed)
	}

	// Explicit config resolves it: the author already decided.
	resolvedMerged, resolvedDiags := MergeProbeResults([]ProbeResult{a, b},
		map[string]ExplicitProject{"svc": {Type: "application"}})
	if diag.HasErrors(resolvedDiags) {
		t.Fatalf("explicit config must resolve the conflict, got %v", resolvedDiags)
	}
	if resolvedMerged["svc"].Type != "application" {
		t.Errorf("type = %q, want the explicitly declared value", resolvedMerged["svc"].Type)
	}
}

// sourceFile is PROVENANCE, not identity. A directory that carries two
// providers' manifests — the shape that becomes reachable the moment a second
// language provider ships, e.g. a Go service with a package.json for its
// front-end tooling — must not fail the whole workspace load, because nothing
// authors a sourceFile and the conflict diagnostic's advertised escape hatch
// ("set it explicitly in putnami.json") does not exist for this field.
func TestMergeProbeResults_ProvenanceFollowsTheResolvedName(t *testing.T) {
	goResult := ProbeResult{Version: 1, Extension: "@putnami/go", Projects: []ProbeProject{
		{Path: "svc", SourceName: "example.com/svc", SourceFile: "svc/go.mod"},
	}}
	tsResult := ProbeResult{Version: 1, Extension: "@putnami/typescript", Projects: []ProbeProject{
		{Path: "svc", SourceName: "@acme/svc-tools", SourceFile: "svc/package.json"},
	}}

	// Explicit config decides the NAME; the two provenance claims must not add
	// a second, unresolvable conflict on top of it.
	merged, diags := MergeProbeResults([]ProbeResult{goResult, tsResult},
		map[string]ExplicitProject{"svc": {Name: "@acme/svc"}})
	if diag.HasErrors(diags) {
		t.Fatalf("two source files for one directory must not be a hard error: %v", diags)
	}
	if merged["svc"].SourceFile != "svc/go.mod" {
		t.Errorf("sourceFile = %q, want a deterministic claim", merged["svc"].SourceFile)
	}

	// With no explicit name the sourceName conflict is still reported (that IS
	// identity), and provenance stays deterministic rather than empty.
	_, nameDiags := MergeProbeResults([]ProbeResult{goResult, tsResult}, nil)
	if !hasCode(nameDiags, "probe-conflict") {
		t.Fatalf("conflicting source NAMES must still be a hard error, got %v", nameDiags)
	}
	for _, d := range nameDiags {
		if strings.Contains(d.Field, "sourceFile") {
			t.Errorf("provenance was reported as a conflict: %+v", d)
		}
	}

	// When one provider's name wins outright, provenance follows IT rather than
	// the alphabetically smaller file.
	unnamed := ProbeResult{Version: 1, Extension: "@putnami/scaffold", Projects: []ProbeProject{
		{Path: "svc", SourceFile: "svc/Dockerfile"},
	}}
	followed, followDiags := MergeProbeResults([]ProbeResult{goResult, unnamed}, nil)
	if diag.HasErrors(followDiags) {
		t.Fatalf("unexpected diagnostics: %v", followDiags)
	}
	if followed["svc"].SourceFile != "svc/go.mod" {
		t.Errorf("sourceFile = %q, want the file the winning sourceName was read from",
			followed["svc"].SourceFile)
	}

	// Provider order must not move the answer: it reaches the snapshot digest.
	reversed, _ := MergeProbeResults([]ProbeResult{unnamed, goResult}, nil)
	if reversed["svc"].SourceFile != followed["svc"].SourceFile {
		t.Errorf("provenance depends on provider order: %q vs %q",
			reversed["svc"].SourceFile, followed["svc"].SourceFile)
	}
}

func TestMergeProbeResults_AgreeingProvidersAreNotAConflict(t *testing.T) {
	a := ProbeResult{Version: 1, Extension: "@putnami/go",
		Projects: []ProbeProject{{Path: "svc", Type: "library"}}}
	b := ProbeResult{Version: 1, Extension: "@putnami/scaffold",
		Projects: []ProbeProject{{Path: "svc", Type: "library"}}}

	merged, diags := MergeProbeResults([]ProbeResult{a, b}, nil)
	if diag.HasErrors(diags) {
		t.Fatalf("agreeing providers must not conflict: %v", diags)
	}
	if merged["svc"].Type != "library" {
		t.Errorf("type = %q", merged["svc"].Type)
	}
}

func TestMergeProbeResults_ExplicitOnlyProjectSurvives(t *testing.T) {
	merged, diags := MergeProbeResults(nil, map[string]ExplicitProject{
		"svc": {Name: "svc", Type: "application", Tags: []string{"go"}},
	})
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	view, ok := merged["svc"]
	if !ok {
		t.Fatal("a project only core knows about disappeared from the merged view")
	}
	if view.SourceName != "svc" || view.Type != "application" || len(view.Tags) != 1 {
		t.Errorf("merged view = %+v", view)
	}
}

func TestMergeProbeResults_DoesNotMutateInput(t *testing.T) {
	input := []ProbeResult{{Version: 1, Extension: "@putnami/go", Projects: []ProbeProject{
		{Path: "./b/", Tags: []string{"z", "a"}},
		{Path: "a"},
	}}}
	MergeProbeResults(input, nil)
	if input[0].Projects[0].Path != "./b/" || input[0].Projects[0].Tags[0] != "z" {
		t.Errorf("MergeProbeResults mutated its argument: %+v", input[0].Projects[0])
	}
}

func TestExplicitFromProjectConfig(t *testing.T) {
	if got := ExplicitFromProjectConfig(nil); got.Name != "" || got.Tags != nil {
		t.Errorf("nil config = %+v, want zero value", got)
	}
	cfg := &ProjectConfig{Name: "n", Type: "library", Tags: []string{"go"},
		Dependencies: []string{"a"}, Publish: []string{"go"}, RunsWith: []string{"pg"},
		Extensions: []string{"/go/extension"}}
	got := ExplicitFromProjectConfig(cfg)
	if got.Name != "n" || got.Type != "library" ||
		len(got.Tags) != 1 || len(got.Dependencies) != 1 || len(got.Publish) != 1 ||
		len(got.RunsWith) != 1 || len(got.Extensions) != 1 {
		t.Errorf("projection dropped a member: %+v", got)
	}
}

func TestResolveProjectName_Precedence(t *testing.T) {
	cases := []struct {
		explicit, probe, pattern, basename, want string
	}{
		{"explicit", "probe", "pattern", "base", "explicit"},
		{"", "probe", "pattern", "base", "probe"},
		{"", "", "pattern", "base", "pattern"},
		{"", "", "", "base", "base"},
		{"", "  ", "", "base", "base"},
		{"", "", "", "", ""},
	}
	for _, tc := range cases {
		if got := ResolveProjectName(tc.explicit, tc.probe, tc.pattern, tc.basename); got != tc.want {
			t.Errorf("ResolveProjectName(%q,%q,%q,%q) = %q, want %q",
				tc.explicit, tc.probe, tc.pattern, tc.basename, got, tc.want)
		}
	}
}

func TestFillScopeTags_FillsButNeverOverwrites(t *testing.T) {
	if got := FillScopeTags([]string{"go"}, []string{"protocol"}); len(got) != 1 || got[0] != "go" {
		t.Errorf("FillScopeTags = %v, want the project's own tags untouched", got)
	}
	if got := FillScopeTags(nil, []string{"protocol", "go"}); len(got) != 2 || got[0] != "go" {
		t.Errorf("FillScopeTags = %v, want the scope tags normalized", got)
	}
	if got := FillScopeTags(nil, nil); got != nil {
		t.Errorf("FillScopeTags = %v, want nil", got)
	}
}

// --- typed failures ---------------------------------------------------------

func TestProbeFailure_TypedAndCausal(t *testing.T) {
	failure := NewProbeFailure(ProbeFailureTimeout, "@putnami/go", "no answer after %ds", 30)
	if failure.Error() != "timeout: @putnami/go: no answer after 30s" {
		t.Errorf("Error() = %q", failure.Error())
	}
	if d := failure.Diagnostic(); d.Code != string(ProbeFailureTimeout) || d.Severity != diag.Error {
		t.Errorf("Diagnostic() = %+v", d)
	}

	anonymous := NewProbeFailure(ProbeFailureUnavailable, "", "extension not installed")
	if anonymous.Error() != "provider-unavailable: extension not installed" {
		t.Errorf("Error() = %q", anonymous.Error())
	}

	// An unclassified kind must not travel: it is coerced, never carried.
	coerced := NewProbeFailure("made-up", "x", "boom")
	if coerced.Kind != ProbeFailureTransport {
		t.Errorf("kind = %q, want coercion to %q", coerced.Kind, ProbeFailureTransport)
	}
	for _, kind := range ValidProbeFailureKinds {
		if !kind.Valid() {
			t.Errorf("%q is in ValidProbeFailureKinds but Valid() is false", kind)
		}
	}
	if ProbeFailureKind("nope").Valid() {
		t.Error("unknown kind reported valid")
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
