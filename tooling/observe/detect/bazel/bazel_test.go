package bazel

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-bazel"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// shop is a Bazel workspace with //pricing, //api that depends on it, and
// //web, plus a directory that holds no BUILD file.
func shop(extra map[string]string) map[string]string {
	files := map[string]string{
		"MODULE.bazel":           "module(name = \"shop\")\n",
		"pricing/BUILD.bazel":    "go_library(name = \"pricing\")\n",
		"pricing/pricing.go":     "package pricing\n",
		"api/BUILD.bazel":        "go_library(name = \"api\")\n",
		"api/api.go":             "package api\n",
		"web/BUILD":              "js_library(name = \"web\")\n",
		"docs/README.md":         "# docs\n",
		".cache/BUILD.bazel":     "",
		"node_modules/x/BUILD":   "",
		"bazel-out/k8/BUILD":     "",
		"third_party/old/BUILD":  "",
		".bazelignore":           "# vendored\nthird_party/old\n",
		"web/sub/BUILD.bazel/.k": "",
	}
	for name, content := range extra {
		files[name] = content
	}
	return files
}

// streamed is the export of the shop as bazel query 'deps(//...)'
// --output=streamed_jsonproto writes it: one target per line.
const streamed = `{"type":"RULE","rule":{"name":"//pricing:pricing","ruleClass":"go_library","ruleInput":["//pricing:pricing.go","@rules_go//go:stdlib","@@rules_go~//go/toolchain:sdk"]}}
{"type":"SOURCE_FILE","sourceFile":{"name":"//pricing:pricing.go"}}
{"type":"RULE","rule":{"name":"//pricing:pricing_test","ruleClass":"go_test","ruleInput":["//pricing:pricing","//pricing:pricing_test.go"]}}
{"type":"RULE","rule":{"name":"//api:api","ruleClass":"go_library","ruleInput":["//api:api.go","//pricing","@maven//:com_google_guava_guava"]}}
{"type":"RULE","rule":{"name":"@@//api:api_test","ruleClass":"go_test","ruleInput":["@//api:api","@//pricing:pricing"]}}
{"type":"RULE","rule":{"name":"//web:web","ruleClass":"js_library","ruleInput":["//docs:README.md"]}}
{"type":"RULE","rule":{"name":"@maven//:com_google_guava_guava","ruleClass":"jvm_import"}}
{"type":"RULE","rule":{"name":"//gone:gone","ruleClass":"filegroup"}}
`

func codes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, string(d.Severity)+" "+d.Code)
	}
	return out
}

func TestAQueryExportGivesThePackagesAndTheirEdges(t *testing.T) {
	spectest.Proves(t, feature, "bazel-packages-are-components", "a-query-export-gives-the-packages-and-their-edges")
	spectest.Proves(t, feature, "bazel-package-edges", "an-edge-between-targets-is-an-edge-between-packages")
	root := writeTree(t, shop(map[string]string{GraphFile: streamed}))
	d := Detect(root, "")
	if len(d.Diagnostics) != 0 || !d.FromExport {
		t.Fatalf("diagnostics %v, from the export %v", d.Diagnostics, d.FromExport)
	}
	want := []Package{
		{Dir: "api", Label: "//api", DependsOn: []string{"pricing"}, Tested: true},
		{Dir: "pricing", Label: "//pricing", Tested: true},
		{Dir: "web", Label: "//web"},
	}
	if !reflect.DeepEqual(d.Packages, want) {
		t.Errorf("packages %+v, want %+v", d.Packages, want)
	}
	if got := BuildFile(root, "web"); got != "web/BUILD" {
		t.Errorf("BuildFile(web) = %q", got)
	}
	if got := BuildFile(root, "docs"); got != "" {
		t.Errorf("BuildFile(docs) = %q, want none", got)
	}
}

func TestTheJSONProtoQueryResultIsAnExportToo(t *testing.T) {
	spectest.Proves(t, feature, "bazel-packages-are-components", "a-query-export-gives-the-packages-and-their-edges")
	export := `{"target":[{"type":"RULE","rule":{"name":"//api:api","ruleInput":["//pricing:pricing"]}},{"type":"RULE","rule":{"name":"//pricing:pricing"}},{"type":"RULE","rule":{"name":"//:root","ruleInput":["//api"]}}]}`
	root := writeTree(t, shop(map[string]string{"graphs/query.json": export, "BUILD.bazel": ""}))
	d := Detect(root, "graphs/query.json")
	want := []Package{
		{Dir: ".", Label: "//", DependsOn: []string{"api"}},
		{Dir: "api", Label: "//api", DependsOn: []string{"pricing"}},
		{Dir: "pricing", Label: "//pricing"},
	}
	if !reflect.DeepEqual(d.Packages, want) || len(d.Diagnostics) != 0 {
		t.Errorf("packages %+v, diagnostics %v", d.Packages, d.Diagnostics)
	}
}

func TestAnExternalRepositoryIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "bazel-package-edges", "an-external-repository-is-no-edge")
	for label, want := range map[string]bool{
		"//pricing:pricing":   true,
		"//pricing":           true,
		"@//pricing:pricing":  true,
		"@@//pricing:pricing": true,
		"//:root":             true,
		"@maven//:guava":      false,
		"@npm//react":         false,
		"@@rules_go~//go:sdk": false,
		"//../escape:x":       false,
		"//":                  false,
		"pricing.go":          false,
	} {
		if _, _, ok := mainLabel(label); ok != want {
			t.Errorf("mainLabel(%q) = %v, want %v", label, ok, want)
		}
	}
	root := writeTree(t, shop(map[string]string{GraphFile: streamed}))
	for _, p := range Detect(root, "").Packages {
		for _, dep := range p.DependsOn {
			if strings.HasPrefix(dep, "@") || dep == "docs" || dep == p.Dir {
				t.Errorf("%s depends on %s", p.Dir, dep)
			}
		}
	}
}

func TestABazelWorkspaceIsRecognizedByItsRootFile(t *testing.T) {
	spectest.Proves(t, feature, "bazel-packages-are-components", "a-bazel-workspace-is-recognized-by-its-root-file")
	for _, marker := range []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"} {
		root := writeTree(t, map[string]string{marker: "", "pricing/BUILD": ""})
		if !IsWorkspace(root) || len(Detect(root, "").Packages) != 1 {
			t.Errorf("%s does not make a Bazel workspace", marker)
		}
	}
	// A workspace without a package names no export command.
	if d := Detect(writeTree(t, map[string]string{"MODULE.bazel": "", "go.mod": "module shop\n"}), ""); len(d.Packages) != 0 || len(d.Diagnostics) != 0 {
		t.Errorf("a workspace without a BUILD file: %+v", d)
	}
	root := writeTree(t, map[string]string{"pricing/BUILD": "", "pricing/MODULE.bazel": "", GraphFile: streamed})
	if d := Detect(root, ""); IsWorkspace(root) || len(d.Packages) != 0 || len(d.Diagnostics) != 0 {
		t.Errorf("a repository without a root marker: %+v", d)
	}
}

func TestWithoutAnExportTheBuildFilesGiveThePackages(t *testing.T) {
	spectest.Proves(t, feature, "bazel-without-export", "without-an-export-the-build-files-give-the-packages")
	root := writeTree(t, shop(nil))
	d := Detect(root, "")
	want := []Package{{Dir: "api", Label: "//api"}, {Dir: "pricing", Label: "//pricing"}, {Dir: "web", Label: "//web"}}
	if !reflect.DeepEqual(d.Packages, want) || d.FromExport {
		t.Errorf("packages %+v, want %+v", d.Packages, want)
	}
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{"info " + CodeGraphMissing}) {
		t.Fatalf("diagnostics %v", got)
	}
	if !strings.Contains(d.Diagnostics[0].Message, ExportCommand) {
		t.Errorf("the diagnostic names no export command: %s", d.Diagnostics[0].Message)
	}
}

func TestADirectoryRepoBazelIgnoresHoldsNoPackage(t *testing.T) {
	spectest.Proves(t, feature, "bazel-without-export", "a-directory-repo-bazel-ignores-holds-no-package")
	root := writeTree(t, shop(map[string]string{
		"REPO.bazel": "repo(default_visibility = [\"//visibility:public\"])\n" +
			"ignore_directories([\n    \"**/testdata\",\n    'tools/fixtures',\n])\n",
		"api/testdata/fixture/BUILD.bazel": "",
		"tools/fixtures/BUILD":             "",
		"tools/fixtures/deep/BUILD":        "",
		"tools/gen/BUILD.bazel":            "",
	}))
	var got []string
	for _, p := range Detect(root, "").Packages {
		got = append(got, p.Dir)
	}
	if want := []string{"api", "pricing", "tools/gen", "web"}; !reflect.DeepEqual(got, want) {
		t.Errorf("packages %v, want %v", got, want)
	}
	for dir, want := range map[string]bool{
		"api": false, "tools/gen": false, ".": false, "api/testdata/fixture": true,
		"tools/fixtures/deep": true, "third_party/old": true, ".cache": true, "bazel-out/k8": true,
	} {
		if got := Ignored(root, dir); got != want {
			t.Errorf("Ignored(%s) = %v, want %v", dir, got, want)
		}
	}
}

func TestAMalformedExportIsAWarning(t *testing.T) {
	spectest.Proves(t, feature, "bazel-without-export", "a-malformed-export-is-a-warning")
	for name, files := range map[string]map[string]string{
		"not json":         {GraphFile: "//pricing:pricing\n"},
		"truncated":        {GraphFile: streamed[:200]},
		"external only":    {GraphFile: `{"type":"RULE","rule":{"name":"@maven//:guava"}}`},
		"empty":            {GraphFile: ""},
		"named and absent": {},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, shop(files))
			graph := ""
			if name == "named and absent" {
				graph = "missing.json"
			}
			d := Detect(root, graph)
			if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{"warning " + CodeGraphUnreadable}) {
				t.Fatalf("diagnostics %v", d.Diagnostics)
			}
			if len(d.Packages) != 3 {
				t.Errorf("the BUILD files do not answer: %+v", d.Packages)
			}
			for _, p := range d.Packages {
				if len(p.DependsOn) != 0 {
					t.Errorf("%s has edges %v", p.Dir, p.DependsOn)
				}
			}
		})
	}
}

func TestATestLogGoesToThePackageOfItsTestTarget(t *testing.T) {
	spectest.Proves(t, feature, "bazel-reports", "a-test-log-goes-to-the-package-of-its-test-target")
	export := streamed + `{"type":"RULE","rule":{"name":"//pricing:rules/tax_test"}}` + "\n" +
		`{"type":"RULE","rule":{"name":"//pricing/rules:rules"}}` + "\n"
	root := writeTree(t, shop(map[string]string{GraphFile: export, "pricing/rules/BUILD": ""}))
	withExport := Detect(root, "")
	without := Detect(writeTree(t, shop(map[string]string{"pricing/rules/BUILD": "", "BUILD.bazel": ""})), "")
	for _, c := range []struct {
		d      Detection
		report string
		want   string
	}{
		{withExport, "bazel-testlogs/pricing/pricing_test/test.xml", "pricing"},
		{withExport, "bazel-testlogs/api/api_test/shard_1_of_2/test.xml", "api"},
		// The target pricing:rules/tax_test, not the package pricing/rules.
		{withExport, "bazel-testlogs/pricing/rules/tax_test/test.xml", "pricing"},
		{withExport, "bazel-testlogs/web/web_test/test.xml", "web"},
		{without, "bazel-testlogs/pricing/rules/tax_test/test.xml", "pricing/rules"},
		{without, "bazel-testlogs/api/api_test/test.xml", "api"},
		{without, "bazel-testlogs/root_test/test.xml", "."},
		{without, "bazel-testlogs/docs/x_test/test.xml", "."},
		{withExport, "reports/junit.xml", ""},
		{withExport, "bazel-testlogs/docs/x_test/test.xml", ""},
	} {
		got, ok := c.d.TestTarget(c.report)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("TestTarget(%s) = %q, %v, want %q", c.report, got, ok, c.want)
		}
	}
}
