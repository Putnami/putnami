package sitecontent

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestPrefixesConflict_SegmentBoundary(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"/docs", "/docs/cloud", true},
		{"/docs/cloud", "/docs", true},
		{"/docs", "/docs", true},
		{"/docs", "/docs-cloud", false},
		{"/docs/cloud", "/docs/cli", false},
		{"/", "/docs", true},
	}
	for _, tt := range tests {
		if got := PrefixesConflict(tt.a, tt.b); got != tt.want {
			t.Errorf("PrefixesConflict(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestMountForPath(t *testing.T) {
	mounts := []Mount{{URLPrefix: "/docs/cloud"}}
	if _, ok := MountForPath(mounts, "docs/cloud/index.html"); !ok {
		t.Fatal("expected docs/cloud/index.html to be under /docs/cloud")
	}
	if _, ok := MountForPath(mounts, "docs/cloudish/index.html"); ok {
		t.Fatal("docs/cloudish must not match /docs/cloud")
	}
}

func TestValidateMergePlan_BundleVsSiteCollision(t *testing.T) {
	bundles := []Manifest{{
		Name:   "cloud-docs",
		Mounts: []Mount{{URLPrefix: "/docs/cloud"}},
	}}
	diags := ValidateMergePlan([]string{"/docs"}, bundles)
	if !hasCode(diags, ErrorCodeMountCollision) {
		t.Fatalf("want mount collision, got %v", diags)
	}
}

func TestValidateMergePlan_BundleVsBundleCollision(t *testing.T) {
	bundles := []Manifest{
		{Name: "cloud-docs", Mounts: []Mount{{URLPrefix: "/docs/cloud"}}},
		{Name: "cloud-api", Mounts: []Mount{{URLPrefix: "/docs/cloud/api"}}},
	}
	diags := ValidateMergePlan(nil, bundles)
	if !hasCode(diags, ErrorCodeMountCollision) {
		t.Fatalf("want mount collision, got %v", diags)
	}
}

func TestValidateMergePlan_CollisionHiddenByInterveningSibling(t *testing.T) {
	// A non-conflicting sibling ("/docs-cloud") sorts lexicographically between
	// an ancestor ("/docs") and its descendant ("/docs/cloud") because '-'
	// (0x2d) sorts before '/' (0x2f). The overlap between /docs and /docs/cloud
	// must still be detected: regression test for the segment-aware ordering in
	// the O(n log n) collision scan (a raw byte sort hid this).
	bundles := []Manifest{
		{Name: "sibling", Mounts: []Mount{{URLPrefix: "/docs-cloud"}}},
		{Name: "cloud", Mounts: []Mount{{URLPrefix: "/docs/cloud"}}},
	}
	diags := ValidateMergePlan([]string{"/docs"}, bundles)
	if !hasCode(diags, ErrorCodeMountCollision) {
		t.Fatalf("want mount collision between /docs and /docs/cloud, got %v", diags)
	}
}

func TestValidateMergePlan_Disjoint(t *testing.T) {
	bundles := []Manifest{
		{Name: "cloud-docs", Mounts: []Mount{{URLPrefix: "/docs/cloud"}}},
		{Name: "cli-docs", Mounts: []Mount{{URLPrefix: "/docs/cli"}}},
	}
	diags := ValidateMergePlan([]string{"/blog"}, bundles)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
}
