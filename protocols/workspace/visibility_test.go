package workspace

import (
	"encoding/json"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestResolveVisibilityClosesOnAnythingButPublic(t *testing.T) {
	cases := []struct {
		in   Visibility
		want Visibility
	}{
		{"", VisibilityScope},
		{VisibilityScope, VisibilityScope},
		{VisibilityPublic, VisibilityPublic},
		{"internal", VisibilityScope},
		{"PUBLIC", VisibilityScope},
	}
	for _, tc := range cases {
		if got := ResolveVisibility(tc.in); got != tc.want {
			t.Errorf("ResolveVisibility(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProjectConfigCarriesVisibility(t *testing.T) {
	cfg, diags := ParseAndValidateProjectConfig([]byte(`{"name":"x","visibility":"public"}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid visibility rejected: %v", diags)
	}
	if cfg.Visibility != VisibilityPublic {
		t.Fatalf("visibility = %q, want %q", cfg.Visibility, VisibilityPublic)
	}

	_, diags = ParseAndValidateProjectConfig([]byte(`{"name":"x","visibility":"internal"}`))
	if !diag.HasErrors(diags) {
		t.Fatal("unknown visibility accepted; a typo must not resolve to a boundary nobody declared")
	}
}

func TestVisibilityRoundTripsOmittingTheDefault(t *testing.T) {
	encoded, err := json.Marshal(ProjectConfig{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); got != `{"name":"x"}` {
		t.Fatalf("encoded = %s, want the default visibility omitted", got)
	}
}

func TestStrongerDependencySourcePrefersAnImportAndIsOrderIndependent(t *testing.T) {
	cases := []struct {
		a, b DependencySource
		want DependencySource
	}{
		{"", DependencySourceDeclared, DependencySourceDeclared},
		{DependencySourceDeclared, "", DependencySourceDeclared},
		{DependencySourceDeclared, DependencySourceGoModule, DependencySourceGoModule},
		{DependencySourceGoModule, DependencySourceDeclared, DependencySourceGoModule},
		{DependencySourcePackageJSON, DependencySourceDeclared, DependencySourcePackageJSON},
		{DependencySourceGoModule, DependencySourcePackageJSON, DependencySourceGoModule},
		{DependencySourcePackageJSON, DependencySourceGoModule, DependencySourceGoModule},
	}
	for _, tc := range cases {
		if got := StrongerDependencySource(tc.a, tc.b); got != tc.want {
			t.Errorf("StrongerDependencySource(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDependencySourceClassification(t *testing.T) {
	if !DependencySourceGoModule.IsImport() || !DependencySourcePackageJSON.IsImport() {
		t.Error("a go.mod require and a package.json dependency are imports")
	}
	if DependencySourceDeclared.IsImport() || DependencySourceContract.IsImport() {
		t.Error("a declaration and a contract edge are not imports")
	}
	if DependencySourceContract.ReportableByProvider() {
		t.Error("the contract source is core's derivation; no provider may claim it")
	}
}

func TestNormalizeProbeProjectDropsProvenanceForUnreportedEdges(t *testing.T) {
	project := ProbeProject{
		Path:         "./go/framework/http",
		Dependencies: []string{"./go/framework/app"},
		DependencySources: map[string]DependencySource{
			"go/framework/app/":       DependencySourceGoModule,
			"protocols/http-routes":   DependencySourceGoModule,
			"go/framework/app/../app": DependencySourceDeclared,
		},
	}
	NormalizeProbeProject(&project)

	want := map[string]DependencySource{"go/framework/app": DependencySourceGoModule}
	if !reflect.DeepEqual(project.DependencySources, want) {
		t.Fatalf("dependencySources = %v, want %v", project.DependencySources, want)
	}
}

func TestProbeDigestMovesWithEdgeProvenance(t *testing.T) {
	declared := ProbeProject{
		Path:              "a",
		Dependencies:      []string{"b"},
		DependencySources: map[string]DependencySource{"b": DependencySourceDeclared},
	}
	imported := ProbeProject{
		Path:              "a",
		Dependencies:      []string{"b"},
		DependencySources: map[string]DependencySource{"b": DependencySourceGoModule},
	}
	if ProbeProjectDigest(declared) == ProbeProjectDigest(imported) {
		t.Fatal("two answers that disagree on an edge's provenance must not share a digest")
	}
}

func TestMergeProbeResultsUnionsEdgeProvenanceOrderIndependently(t *testing.T) {
	goResult := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "@putnami/go",
		Projects: []ProbeProject{{
			Path:              "services/api",
			Dependencies:      []string{"libs/core"},
			DependencySources: map[string]DependencySource{"libs/core": DependencySourceGoModule},
		}},
	}
	tsResult := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "@putnami/typescript",
		Projects: []ProbeProject{{
			Path:              "services/api",
			Dependencies:      []string{"libs/core", "libs/ui"},
			DependencySources: map[string]DependencySource{"libs/ui": DependencySourcePackageJSON},
		}},
	}

	forward, _ := MergeProbeResults([]ProbeResult{goResult, tsResult}, nil)
	backward, _ := MergeProbeResults([]ProbeResult{tsResult, goResult}, nil)
	if !reflect.DeepEqual(forward, backward) {
		t.Fatal("the merged view depends on provider order")
	}

	want := map[string]DependencySource{
		"libs/core": DependencySourceGoModule,
		"libs/ui":   DependencySourcePackageJSON,
	}
	if got := forward["services/api"].DependencySources; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged dependencySources = %v, want %v", got, want)
	}
}

func TestMergeProbeResultsLeavesDeclaredEdgesWithoutProvenance(t *testing.T) {
	result := ProbeResult{
		Version:   ProbeProtocolVersion,
		Extension: "@putnami/go",
		Projects:  []ProbeProject{{Path: "services/api"}},
	}
	merged, _ := MergeProbeResults([]ProbeResult{result}, map[string]ExplicitProject{
		"services/api": {Dependencies: []string{"libs/core"}},
	})
	view := merged["services/api"]
	if len(view.Dependencies) != 1 || view.Dependencies[0] != "libs/core" {
		t.Fatalf("dependencies = %v, want the authored edge", view.Dependencies)
	}
	if len(view.DependencySources) != 0 {
		t.Fatalf("dependencySources = %v, want none: an authored edge is declared, not imported", view.DependencySources)
	}
}

// The distribution level is a separate field from the import boundary, and it
// fails closed: an empty block and an unknown level are both errors.
func TestProjectConfigCarriesDistributionVisibility(t *testing.T) {
	cfg, diags := ParseAndValidateProjectConfig([]byte(`{"name":"x","visibility":"scope","distribution":{"visibility":"public"}}`))
	if diag.HasErrors(diags) {
		t.Fatalf("valid distribution level rejected: %v", diags)
	}
	if cfg.Distribution == nil || cfg.Distribution.Visibility != "public" || cfg.Visibility != VisibilityScope {
		t.Fatalf("config = %+v, want distribution public beside import boundary scope", cfg)
	}
	for _, input := range []string{
		`{"name":"x","distribution":{}}`,
		`{"name":"x","distribution":{"visibility":"scope"}}`,
		`{"name":"x","distribution":{"visibility":"PUBLIC"}}`,
	} {
		if _, diags := ParseAndValidateProjectConfig([]byte(input)); !diag.HasErrors(diags) {
			t.Errorf("%s accepted; a distribution level must be internal, private, or public", input)
		}
	}
	if _, diags := ParseAndValidateScopeConfig([]byte(`{"tags":["x"],"distribution":{"visibility":"world"}}`)); !diag.HasErrors(diags) {
		t.Error("a scope with an unknown distribution level was accepted")
	}
}
