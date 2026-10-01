package genresult

import (
	"go.putnami.dev/protocol/features/spectest"

	"path/filepath"
	"runtime"
	"testing"
)

// abs returns the absolute path made of parts under the host's root: "/" on
// Unix and a drive root on Windows, where a path without a volume is not
// absolute.
func abs(parts ...string) string {
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func TestRelativizePath(t *testing.T) {
	root := abs("ws", "checkout", "project")
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty stays empty", "", ""},
		{"absolute inside the project becomes relative",
			filepath.Join(root, ".gen", "schema", "openapi.json"), ".gen/schema/openapi.json"},
		{"the project root itself", root, "."},
		{"already relative is kept", "./loader.js", "./loader.js"},
		{"already relative is slash-normalized", filepath.Join("dist", "main.js"), "dist/main.js"},
		{"absolute outside the project is kept verbatim",
			abs("ws", "checkout", "other", "x.json"),
			abs("ws", "checkout", "other", "x.json")},
		{"absolute outside the workspace is kept verbatim",
			abs("tmp", "putnami-describe-123"), abs("tmp", "putnami-describe-123")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelativizePath(root, tc.value); got != tc.want {
				t.Errorf("RelativizePath(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestRelativizePathIsCheckoutIndependent is the invariant the package exists
// for: the same project generated under two different parent directories must
// serialize the same string, because that string is cached and restored into a
// third checkout.
func TestRelativizePathIsCheckoutIndependent(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "checkout-independent-generation-paths", "every-serialized-path-is-project-relative")
	a := abs("ws", "a", "go", "samples", "simple-api")
	b := abs("elsewhere", "deeper", "still", "go", "samples", "simple-api")

	got := RelativizePath(a, filepath.Join(a, ".gen", "schema", "capabilities.json"))
	want := RelativizePath(b, filepath.Join(b, ".gen", "schema", "capabilities.json"))
	if got != want {
		t.Fatalf("checkout-dependent manifest value: %q != %q", got, want)
	}
	if filepath.IsAbs(got) {
		t.Fatalf("manifest value is absolute: %q", got)
	}
}

func TestResolvePath(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "checkout-independent-generation-paths", "an-already-absolute-value-resolves-to-itself")
	root := abs("ws", "checkout", "project")
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty stays empty", "", ""},
		{"relative resolves against the project root",
			".gen/schema/openapi.json", filepath.Join(root, ".gen", "schema", "openapi.json")},
		{"dot-relative resolves against the project root",
			"./loader.js", filepath.Join(root, "loader.js")},
		{"legacy absolute value is returned as it stands",
			abs("other", "checkout", "project", ".gen", "x.json"),
			abs("other", "checkout", "project", ".gen", "x.json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePath(root, tc.value); got != tc.want {
				t.Errorf("ResolvePath(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestRelativizeResolveRoundTrip(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "checkout-independent-generation-paths", "relativizing-and-resolving-round-trip")
	root := abs("ws", "checkout", "project")
	abs := filepath.Join(root, ".gen", "src", "serve.bundled.ts")
	if got := ResolvePath(root, RelativizePath(root, abs)); got != abs {
		t.Errorf("round trip = %q, want %q", got, abs)
	}
}

func TestRelativizeAndResolveMapsAreNeverNil(t *testing.T) {
	if got := Relativize("/ws/p", nil); got == nil {
		t.Error("Relativize(nil) = nil, want an empty map")
	}
	if got := Resolve("/ws/p", nil); got == nil {
		t.Error("Resolve(nil) = nil, want an empty map")
	}
}

func TestRelativizeMapRewritesEveryValue(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "checkout-independent-generation-paths", "every-value-in-a-map-is-rewritten")
	root := abs("ws", "checkout", "project")
	got := Relativize(root, map[string]string{
		"openapi-spec": filepath.Join(root, ".gen", "schema", "openapi.json"),
		"version-info": filepath.Join(root, ".gen", "version.json"),
		"loader":       "./loader.js",
	})
	want := map[string]string{
		"openapi-spec": ".gen/schema/openapi.json",
		"version-info": ".gen/version.json",
		"loader":       "./loader.js",
	}
	if len(got) != len(want) {
		t.Fatalf("Relativize = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("Relativize[%q] = %q, want %q", key, got[key], value)
		}
	}
}

func TestRelativizeListPreservesNilAndRewritesValues(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "checkout-independent-generation-paths", "every-value-in-a-list-is-rewritten")
	root := abs("ws", "checkout", "project")
	if got := RelativizeList(root, nil); got != nil {
		t.Errorf("RelativizeList(nil) = %v, want nil", got)
	}
	got := RelativizeList(root, []string{
		filepath.Join(".gen", "schema", "openapi.json"),
		filepath.Join(root, "schema", "openapi.json"),
	})
	want := []string{".gen/schema/openapi.json", "schema/openapi.json"}
	if len(got) != len(want) {
		t.Fatalf("RelativizeList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RelativizeList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
