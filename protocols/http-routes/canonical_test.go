package httproutes

import (
	"bytes"
	"os"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const goldenDigest = "sha256:b9b1df83b351439e30d8aff5c6d2ec7de61de8a9bab1444738c4c27984c1f77e"

func TestCanonicalJSON_MatchesCrossLanguageGolden(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse valid fixture: %v", diags)
	}

	got, diags := CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	want, err := os.ReadFile("fixtures/equivalence/http-routes.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes differ from cross-language golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if manifest.Digest != goldenDigest {
		t.Fatalf("digest = %q, want %q", manifest.Digest, goldenDigest)
	}
}

func TestCanonicalize_OrderAndMethodCaseIndependent(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, parseDiags := ParseManifest(data)
	if diag.HasErrors(parseDiags) {
		t.Fatalf("parse: %v", parseDiags)
	}

	permuted := cloneRoutes(manifest.Routes)
	for left, right := 0, len(permuted)-1; left < right; left, right = left+1, right-1 {
		permuted[left], permuted[right] = permuted[right], permuted[left]
	}
	for i := range permuted {
		for j := range permuted[i].Methods {
			permuted[i].Methods[j] = bytesToLowerASCII(permuted[i].Methods[j])
		}
		for left, right := 0, len(permuted[i].Methods)-1; left < right; left, right = left+1, right-1 {
			permuted[i].Methods[left], permuted[i].Methods[right] = permuted[i].Methods[right], permuted[i].Methods[left]
		}
	}

	canonical, diags := Canonicalize(permuted)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize permutation: %v", diags)
	}
	if canonical.Digest != goldenDigest {
		t.Fatalf("permuted digest = %q, want %q", canonical.Digest, goldenDigest)
	}
	got, diags := CanonicalJSON(canonical)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	want, err := os.ReadFile("fixtures/equivalence/http-routes.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("permuted inventory did not produce golden bytes\n%s", got)
	}

	for i := 0; i < 100; i++ {
		again, againDiags := Canonicalize(permuted)
		if diag.HasErrors(againDiags) || again.Digest != canonical.Digest {
			t.Fatalf("iteration %d was not deterministic: %v, %q", i, againDiags, again.Digest)
		}
	}
}

func TestValidateRoutes_OverlapPolicy(t *testing.T) {
	base := Provenance{Project: "example/app", SourceKind: SourceManual}
	tests := []struct {
		name     string
		routes   []Route
		wantCode string
	}{
		{
			name: "different visibility exact under prefix",
			routes: []Route{
				{Match: MatchPrefix, Path: "/assets/", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchExact, Path: "/assets/private.json", Methods: []string{"GET"}, PublicEdge: false, Provenance: base},
			},
			wantCode: ErrorCodeVisibilityOverlap,
		},
		{
			name: "same template language ignores parameter names",
			routes: []Route{
				{Match: MatchTemplate, Path: "/users/{id}", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchTemplate, Path: "/users/{name}", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
			},
			wantCode: ErrorCodeDuplicateRoute,
		},
		{
			name: "method-disjoint visibility is safe",
			routes: []Route{
				{Match: MatchPrefix, Path: "/api/", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchExact, Path: "/api/admin", Methods: []string{"POST"}, PublicEdge: false, Provenance: base},
			},
		},
		{
			name: "same visibility overlap is safe",
			routes: []Route{
				{Match: MatchTemplate, Path: "/users/{id}", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchExact, Path: "/users/me", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
			},
		},
		{
			name: "trailing slash remains distinct",
			routes: []Route{
				{Match: MatchExact, Path: "/docs", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchExact, Path: "/docs/", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
			},
		},
		{
			name: "template parameter does not match empty trailing segment",
			routes: []Route{
				{Match: MatchTemplate, Path: "/users/{id}", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
				{Match: MatchExact, Path: "/users/", Methods: []string{"GET"}, PublicEdge: false, Provenance: base},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateRoutes(tt.routes)
			if tt.wantCode == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("unexpected diagnostics: %v", diags)
				}
				return
			}
			for _, finding := range diags {
				if finding.Code == tt.wantCode {
					return
				}
			}
			t.Fatalf("diagnostics %v do not include %q", diags, tt.wantCode)
		})
	}
}

func TestValidateRoutes_CatchAllOverlapPolicy(t *testing.T) {
	base := Provenance{Project: "example/app", SourceKind: SourceManual}
	pub := func(match MatchKind, p string) Route {
		return Route{Match: match, Path: p, Methods: []string{"GET"}, PublicEdge: true, Provenance: base}
	}
	priv := func(match MatchKind, p string) Route {
		return Route{Match: match, Path: p, Methods: []string{"GET"}, PublicEdge: false, Provenance: base}
	}
	tests := []struct {
		name    string
		a, b    Route
		overlap bool
	}{
		{"root catch-all covers private exact", pub(MatchTemplate, "/{m...}"), priv(MatchExact, "/internal/health"), true},
		{"root catch-all covers private prefix", pub(MatchTemplate, "/{m...}"), priv(MatchPrefix, "/internal/"), true},
		{"suffix catch-all disjoint from shorter private", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchExact, "/internal/health"), false},
		{"suffix catch-all disjoint by fixed suffix", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchExact, "/x/@latest/list"), false},
		{"suffix catch-all matches consistent private exact", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchExact, "/x/@v/list"), true},
		{"suffix catch-all matches private template suffix", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchTemplate, "/x/@v/{file}"), true},
		{"two catch-alls disjoint by suffix", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchTemplate, "/{n...}/@latest"), false},
		{"two catch-alls overlap on shared suffix", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchTemplate, "/{n...}/list"), true},
		{"two catch-alls disjoint by prefix", pub(MatchTemplate, "/a/{m...}"), priv(MatchTemplate, "/b/{n...}"), false},
		{"catch-all boundary length too short is disjoint", pub(MatchTemplate, "/{m...}/@v/list"), priv(MatchExact, "/a/b"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := ValidateRoutes([]Route{tt.a, tt.b})
			flagged := false
			for _, finding := range diags {
				if finding.Code == ErrorCodeVisibilityOverlap {
					flagged = true
				} else if diag.HasErrors([]diag.Diagnostic{finding}) {
					t.Fatalf("unexpected diagnostic: %v", finding)
				}
			}
			if flagged != tt.overlap {
				t.Fatalf("overlap flagged = %v, want %v (diags=%v)", flagged, tt.overlap, diags)
			}
		})
	}
}

// TestCatchAllOverlapMethodDisjoint pins that a public catch-all over a private
// route with disjoint methods is not a visibility overlap.
func TestCatchAllOverlapMethodDisjoint(t *testing.T) {
	base := Provenance{Project: "example/app", SourceKind: SourceManual}
	routes := []Route{
		{Match: MatchTemplate, Path: "/{m...}", Methods: []string{"GET"}, PublicEdge: true, Provenance: base},
		{Match: MatchExact, Path: "/internal/health", Methods: []string{"POST"}, PublicEdge: false, Provenance: base},
	}
	if diags := ValidateRoutes(routes); diag.HasErrors(diags) {
		t.Fatalf("method-disjoint catch-all must not flag: %v", diags)
	}
}

func bytesToLowerASCII(value string) string {
	data := []byte(value)
	for i, c := range data {
		if c >= 'A' && c <= 'Z' {
			data[i] = c - 'A' + 'a'
		}
	}
	return string(data)
}
