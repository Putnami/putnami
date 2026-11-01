package golang

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-go"

// demo is the layout of Putnami/demo-observe-go after its pull request: a
// go.work with two modules, api requiring pricing through a local replace.
var demo = map[string]string{
	"go.work": "go 1.23\n\nuse (\n\t./api\n\t./pricing\n)\n",
	"api/go.mod": `module github.com/Putnami/demo-observe-go/api

go 1.23

require (
	github.com/Putnami/demo-observe-go/pricing v0.0.0
	github.com/google/uuid v1.5.0
)

require golang.org/x/text v0.21.0 // indirect

replace github.com/Putnami/demo-observe-go/pricing => ../pricing
`,
	"api/go.sum":         "github.com/google/uuid v1.5.0 h1:x=\n",
	"api/quote.go":       "package main\n",
	"pricing/go.mod":     "module github.com/Putnami/demo-observe-go/pricing\n\ngo 1.23\n\nrequire golang.org/x/text v0.21.0\n",
	"pricing/go.sum":     "golang.org/x/text v0.21.0 h1:x=\n",
	"pricing/pricing.go": "package pricing\n",
	"README.md":          "# demo\n",
}

func tree(t *testing.T, files map[string]string) string {
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

func with(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func module(t *testing.T, d Detection, dir string) Module {
	t.Helper()
	for _, m := range d.Modules {
		if m.Dir == dir {
			return m
		}
	}
	t.Fatalf("no module %q in %+v", dir, d.Modules)
	return Module{}
}

func codes(list []diag.Diagnostic) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.Code)
	}
	return out
}

func TestGoWorkMembersAreWorkspaceComponents(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "go-work-members-are-workspace-components")
	d := Detect(tree(t, demo))
	if d.Workspace != GoWork || len(d.Diagnostics) != 0 {
		t.Fatalf("workspace %q, diagnostics %v", d.Workspace, d.Diagnostics)
	}
	want := []evidence.Component{
		{ID: "api", Name: "github.com/Putnami/demo-observe-go/api", Ecosystem: "go", Source: evidence.SourceWorkspace, DependsOn: []string{"pricing"}},
		{ID: "pricing", Name: "github.com/Putnami/demo-observe-go/pricing", Ecosystem: "go", Source: evidence.SourceWorkspace},
	}
	if got := d.Components(); !reflect.DeepEqual(got, want) {
		t.Fatalf("components\n got %+v\nwant %+v", got, want)
	}
	if m := module(t, d, "pricing"); m.GoMod != "pricing/go.mod" || m.GoSum != "pricing/go.sum" {
		t.Fatalf("pricing files %q %q", m.GoMod, m.GoSum)
	}
}

func TestAGoModOutsideGoWorkIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "a-go-mod-outside-go-work-is-a-manifest-component")
	root := tree(t, with(demo, map[string]string{
		"tools/go.mod":              "module example.com/tools\n",
		"go.mod":                    "module example.com/root\n",
		"api/testdata/x/go.mod":     "module example.com/fixture\n",
		".github/go.mod":            "module example.com/hidden\n",
		"_old/go.mod":               "module example.com/old\n",
		"node_modules/a/go.mod":     "module example.com/npm\n",
		"vendor/example.com/go.mod": "module example.com/vendored\n",
	}))
	d := Detect(root)
	dirs := make([]string, 0, len(d.Modules))
	for _, m := range d.Modules {
		dirs = append(dirs, m.Dir+":"+m.Source)
	}
	want := []string{".:manifest", "api:workspace", "pricing:workspace", "tools:manifest"}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("modules %v, want %v", dirs, want)
	}
}

func TestAGoModAloneIsTheRootComponent(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "a-go-mod-outside-go-work-is-a-manifest-component")
	d := Detect(tree(t, map[string]string{"go.mod": "module example.com/one\n\ngo 1.23\n", "main.go": "package main\n"}))
	if d.Workspace != "" || len(d.Modules) != 1 || d.Modules[0].Dir != evidence.RootComponent || d.Modules[0].Source != evidence.SourceManifest {
		t.Fatalf("detection %+v", d)
	}
	if owner, ok := d.Owner("main.go"); !ok || owner != "." {
		t.Fatalf("owner %q %v", owner, ok)
	}
}

func TestAFileBelongsToItsInnermostModule(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "a-file-belongs-to-its-innermost-module")
	d := Detect(tree(t, with(demo, map[string]string{
		"go.mod":                 "module example.com/root\n",
		"pricing/rates/go.mod":   "module example.com/rates\n",
		"pricing/rates/rates.go": "package rates\n",
	})))
	cases := map[string]string{
		"pricing/pricing.go":     "pricing",
		"pricing/go.sum":         "pricing",
		"pricing/rates/rates.go": "pricing/rates",
		"api/go.mod":             "api",
		"README.md":              ".",
		"pricingx/a.go":          ".",
	}
	for file, want := range cases {
		if got, ok := d.Owner(file); !ok || got != want {
			t.Errorf("Owner(%q) = %q %v, want %q", file, got, ok, want)
		}
	}
	noRoot := Detect(tree(t, demo))
	if got, ok := noRoot.Owner("README.md"); ok {
		t.Fatalf("README.md belongs to %q in a repository without a root module", got)
	}
	if got, ok := noRoot.Owner("go.work"); ok {
		t.Fatalf("go.work belongs to %q", got)
	}
}

func TestAnUnreadableManifestIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "an-unreadable-manifest-is-a-diagnostic")
	root := tree(t, with(demo, map[string]string{
		"go.work":       "go 1.23\nuse (\n",
		"broken/go.mod": "module example.com/broken\nrequire (\n",
		// A directive a newer go added: the module and its requires still count.
		"future/go.mod": "module example.com/future\nfrobnicate on\nrequire github.com/Putnami/demo-observe-go/pricing v0.0.0\n",
	}))
	d := Detect(root)
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestInvalid, CodeManifestInvalid, CodeManifestInvalid}) {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	future := module(t, d, "future")
	if future.Path != "example.com/future" || !reflect.DeepEqual(future.DependsOn, []string{"pricing"}) {
		t.Fatalf("future module %+v", future)
	}
	broken := module(t, d, "broken")
	if broken.Path != "" || broken.DependsOn != nil || broken.Source != evidence.SourceManifest {
		t.Fatalf("broken module %+v", broken)
	}
	// Without go.work, api and pricing are still found by their go.mod.
	if module(t, d, "api").Source != evidence.SourceManifest {
		t.Fatal("api is not a manifest component when go.work cannot be parsed")
	}

	unreadable := tree(t, demo)
	if err := os.Chmod(filepath.Join(unreadable, "pricing", "go.mod"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(unreadable, "pricing", "go.mod"), 0o644) })
	if data, err := os.ReadFile(filepath.Join(unreadable, "pricing", "go.mod")); err == nil && len(data) > 0 {
		t.Skip("the file stays readable: the test runs with privileges")
	}
	d = Detect(unreadable)
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	if len(d.Modules) != 1 || d.Modules[0].Dir != "api" || d.Modules[0].DependsOn != nil {
		t.Fatalf("modules %+v", d.Modules)
	}
}

func TestADuplicateModulePathIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "an-unreadable-manifest-is-a-diagnostic")
	d := Detect(tree(t, with(demo, map[string]string{
		"copy/go.mod": "module github.com/Putnami/demo-observe-go/pricing\n",
	})))
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{CodeDuplicateModule}) {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	if got := module(t, d, "api").DependsOn; !reflect.DeepEqual(got, []string{"pricing"}) {
		t.Fatalf("api depends on %v", got)
	}
}

func TestAGoWorkMemberNamesItsModulePath(t *testing.T) {
	spectest.Proves(t, feature, "go-module-edges", "a-require-of-a-repository-module-is-an-edge")
	spectest.Proves(t, feature, "go-report-paths", "an-import-path-names-its-file-and-module")
	// api requires pricing without a replace: go.work resolves it to the
	// member, not to copy/, which sorts first and declares the same path.
	d := Detect(tree(t, with(demo, map[string]string{
		"api/go.mod":  "module github.com/Putnami/demo-observe-go/api\nrequire github.com/Putnami/demo-observe-go/pricing v0.0.0\n",
		"copy/go.mod": "module github.com/Putnami/demo-observe-go/pricing\n",
	})))
	if len(d.Diagnostics) != 1 || d.Diagnostics[0].Code != CodeDuplicateModule || d.Diagnostics[0].Field != "copy/go.mod" {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	if got := module(t, d, "api").DependsOn; !reflect.DeepEqual(got, []string{"pricing"}) {
		t.Fatalf("api depends on %v", got)
	}
	if file, component, ok := d.Locate("github.com/Putnami/demo-observe-go/pricing/pricing.go"); !ok || file != "pricing/pricing.go" || component != "pricing" {
		t.Fatalf("Locate = %q %q %v", file, component, ok)
	}

	// Without go.work, the first directory in path order names the path.
	manifests := Detect(tree(t, map[string]string{
		"a/go.mod":   "module example.com/dup\n",
		"b/go.mod":   "module example.com/dup\n",
		"app/go.mod": "module example.com/app\nrequire example.com/dup v0.0.0\n",
	}))
	if got := module(t, manifests, "app").DependsOn; !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("app depends on %v", got)
	}
	if len(manifests.Diagnostics) != 1 || manifests.Diagnostics[0].Field != "b/go.mod" {
		t.Fatalf("diagnostics %v", manifests.Diagnostics)
	}
}

func TestAnEscapingPathHasNoComponent(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "a-file-belongs-to-its-innermost-module")
	spectest.Proves(t, feature, "go-report-paths", "an-import-path-names-its-file-and-module")
	d := Detect(tree(t, with(demo, map[string]string{"go.mod": "module github.com/Putnami/demo-observe-go\n"})))
	for _, file := range []string{"", ".", "..", "../outside.go", "/etc/passwd", "pricing/../../x.go"} {
		if got, ok := d.Owner(file); ok {
			t.Errorf("Owner(%q) = %q", file, got)
		}
	}
	for _, importPath := range []string{
		"github.com/Putnami/demo-observe-go/../x.go",
		"github.com/Putnami/demo-observe-go/pricing/../../x.go",
		"github.com/Putnami/demo-observe-go/./x.go",
		"github.com/Putnami/demo-observe-go//x.go",
		"github.com/Putnami/demo-observe-go/",
	} {
		if file, component, ok := d.Locate(importPath); ok {
			t.Errorf("Locate(%q) = %q %q", importPath, file, component)
		}
	}
}

func TestARequireOfARepositoryModuleIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "go-module-edges", "a-require-of-a-repository-module-is-an-edge")
	d := Detect(tree(t, map[string]string{
		"go.work":        "go 1.23\nuse (\n\t./api\n\t./pricing\n\t./core\n)\n",
		"api/go.mod":     "module example.com/api\nrequire example.com/pricing v0.0.0\n",
		"pricing/go.mod": "module example.com/pricing\nrequire (\n\texample.com/core v0.1.0\n\texample.com/pricing v0.0.0\n)\n",
		"core/go.mod":    "module example.com/core\n",
	}))
	edges := map[string][]string{}
	for _, m := range d.Modules {
		edges[m.Dir] = m.DependsOn
	}
	want := map[string][]string{"api": {"pricing"}, "pricing": {"core"}, "core": nil}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("edges %v, want %v", edges, want)
	}
}

func TestALocalReplaceIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "go-module-edges", "a-local-replace-is-an-edge")
	d := Detect(tree(t, map[string]string{
		// No go.work: the replace alone links api to pricing.
		"api/go.mod":       "module example.com/api\nrequire example.com/pricing v0.0.0\nreplace example.com/pricing => ../pricing\nreplace example.com/unused => ../tools\n",
		"pricing/go.mod":   "module example.com/pricing\n",
		"tools/go.mod":     "module example.com/tools\n",
		"web/go.mod":       "module example.com/web\nrequire example.com/shared v1.0.0\n",
		"internal/go.mod":  "module example.com/internal-shared\n",
		"go.work":          "go 1.23\nuse ./web\nreplace example.com/shared => ./internal\n",
		"remote/go.mod":    "module example.com/remote\nrequire example.com/pricing v1.0.0\nreplace example.com/pricing v1.0.0 => example.com/other v1.0.0\n",
		"versioned/go.mod": "module example.com/versioned\nrequire example.com/x v1.2.0\nreplace example.com/x v1.0.0 => ../pricing\n",
	}))
	edges := map[string][]string{}
	for _, m := range d.Modules {
		edges[m.Dir] = m.DependsOn
	}
	want := map[string][]string{
		"api":      {"pricing", "tools"},
		"pricing":  nil,
		"tools":    nil,
		"web":      {"internal"},
		"internal": nil,
		// A replace of pricing by an outside module drops the edge to pricing.
		"remote": nil,
		// A local replace in the module's own go.mod is an edge, even for a
		// version it does not require.
		"versioned": {"pricing"},
	}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("edges %v, want %v", edges, want)
	}
}

func TestAnOutsideModuleIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "go-module-edges", "an-outside-module-is-no-edge")
	d := Detect(tree(t, map[string]string{
		"go.mod":     "module example.com/root\nrequire (\n\tgolang.org/x/text v0.21.0\n\texample.com/root v0.0.0\n)\nreplace example.com/far => ../outside\nreplace example.com/abs => /opt/abs\n",
		"sub/go.mod": "module example.com/root/sub\nrequire example.com/root v0.0.0\n",
	}))
	if got := module(t, d, ".").DependsOn; got != nil {
		t.Fatalf("root depends on %v", got)
	}
	if got := module(t, d, "sub").DependsOn; !reflect.DeepEqual(got, []string{"."}) {
		t.Fatalf("sub depends on %v", got)
	}
}

func TestAnImportPathNamesItsFileAndModule(t *testing.T) {
	spectest.Proves(t, feature, "go-report-paths", "an-import-path-names-its-file-and-module")
	d := Detect(tree(t, with(demo, map[string]string{
		"go.mod":               "module github.com/Putnami/demo-observe-go\n",
		"pricing/rates/go.mod": "module github.com/Putnami/demo-observe-go/pricing/rates\n",
	})))
	cases := []struct{ in, file, component string }{
		// A cover profile line names the file by import path.
		{"github.com/Putnami/demo-observe-go/pricing/format.go", "pricing/format.go", "pricing"},
		// A gotestsum suite or a go test -json event names the package.
		{"github.com/Putnami/demo-observe-go/api", "api", "api"},
		{"github.com/Putnami/demo-observe-go/api/internal/quote", "api/internal/quote", "api"},
		{"github.com/Putnami/demo-observe-go/pricing/rates/rates.go", "pricing/rates/rates.go", "pricing/rates"},
		{"github.com/Putnami/demo-observe-go/cmd/tool/main.go", "cmd/tool/main.go", "."},
		{"github.com/Putnami/demo-observe-go", "", "."},
	}
	for _, c := range cases {
		file, component, ok := d.Locate(c.in)
		if !ok || file != c.file || component != c.component {
			t.Errorf("Locate(%q) = %q %q %v, want %q %q", c.in, file, component, ok, c.file, c.component)
		}
	}
	for _, outside := range []string{"github.com/Putnami/demo-observe-gox/a.go", "golang.org/x/text/cases", ""} {
		if file, component, ok := d.Locate(outside); ok {
			t.Errorf("Locate(%q) = %q %q", outside, file, component)
		}
	}
}

func TestGoModRequiresAreTheLockedVersions(t *testing.T) {
	spectest.Proves(t, feature, "go-locked-versions", "go-mod-requires-are-the-locked-versions")
	d := Detect(tree(t, demo))
	if got := d.Lockfiles(); !reflect.DeepEqual(got, []Lockfile{
		{Path: "api/go.mod", Component: "api", Ecosystem: "go"},
		{Path: "pricing/go.mod", Component: "pricing", Ecosystem: "go"},
	}) {
		t.Fatalf("lockfiles %+v", got)
	}
	base := []byte("module github.com/Putnami/demo-observe-go/api\nrequire (\n\tgithub.com/Putnami/demo-observe-go/pricing v0.0.0\n\tgithub.com/google/uuid v1.5.0\n)\nrequire golang.org/x/text v0.20.0 // indirect\nreplace github.com/Putnami/demo-observe-go/pricing => ../pricing\n")
	locked, err := d.ReadLock("api/go.mod", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Locked{
		{Name: "github.com/google/uuid", Version: "v1.5.0"},
		{Name: "golang.org/x/text", Version: "v0.20.0", Indirect: true},
	}
	if !reflect.DeepEqual(locked, want) {
		t.Fatalf("locked %+v, want %+v", locked, want)
	}
	if CompareVersions("v0.20.0", "v0.21.0") >= 0 || CompareVersions("v0.21.0", "v0.20.0") <= 0 || CompareVersions("v1.0.0", "v1.0.0") != 0 {
		t.Fatal("semantic order")
	}
	if CompareVersions("v0.0.0-20240101000000-abcdefabcdef", "v0.1.0") >= 0 {
		t.Fatal("a pseudo version sorts below the release")
	}
	if CompareVersions("latest", "v1.0.0") != 0 {
		t.Fatal("an invalid version compares as equal")
	}
}

func TestReplacesSelectTheLockedVersion(t *testing.T) {
	spectest.Proves(t, feature, "go-locked-versions", "go-mod-requires-are-the-locked-versions")
	d := Detect(tree(t, demo))
	locked, err := d.ReadLock("go.mod", []byte(`module example.com/m
require (
	example.com/a v1.0.0
	example.com/b v1.0.0
	example.com/c v1.0.0
	example.com/d v1.0.0
	example.com/e v1.0.0
)
replace example.com/a => example.com/a v1.1.0
replace example.com/b v1.0.0 => example.com/fork v2.0.0
replace example.com/c v0.9.0 => example.com/c v0.9.1
replace example.com/d => ./d
replace example.com/e => ../../outside
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Locked{
		{Name: "example.com/a", Version: "v1.1.0"},
		{Name: "example.com/b", Version: "example.com/fork@v2.0.0"},
		{Name: "example.com/c", Version: "v1.0.0"},
	}
	if !reflect.DeepEqual(locked, want) {
		t.Fatalf("locked %+v, want %+v", locked, want)
	}
	if _, err := d.ReadLock("go.mod", []byte("require (\n"), nil); err == nil {
		t.Fatal("a go.mod that cannot be parsed is not an error")
	}
	if _, err := d.ReadLock("go.mod", []byte("module example.com/m\n"), []byte("use (\n")); err == nil {
		t.Fatal("a go.work that cannot be parsed is not an error")
	}
	// A directive a newer go added: the requires are still read.
	lax, err := d.ReadLock("go.mod", []byte("module example.com/m\nfrobnicate on\nrequire example.com/a v1.0.0\n"), nil)
	if err != nil || !reflect.DeepEqual(lax, []Locked{{Name: "example.com/a", Version: "v1.0.0"}}) {
		t.Fatalf("lenient read %+v %v", lax, err)
	}
}

func TestARepositoryModuleIsNotALockedDependency(t *testing.T) {
	spectest.Proves(t, feature, "go-locked-versions", "a-repository-module-is-not-a-locked-dependency")
	// go.work resolves pricing without a replace in api's go.mod.
	apiGoMod := "module github.com/Putnami/demo-observe-go/api\nrequire github.com/Putnami/demo-observe-go/pricing v0.0.0\nrequire golang.org/x/text v0.21.0 // indirect\n"
	d := Detect(tree(t, with(demo, map[string]string{"api/go.mod": apiGoMod})))
	if got := module(t, d, "api").DependsOn; !reflect.DeepEqual(got, []string{"pricing"}) {
		t.Fatalf("api depends on %v", got)
	}
	locked, err := d.ReadLock("api/go.mod", []byte(apiGoMod), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range locked {
		if l.Name == "github.com/Putnami/demo-observe-go/pricing" {
			t.Fatalf("a repository module is locked: %+v", locked)
		}
	}
	if !reflect.DeepEqual(locked, []Locked{{Name: "golang.org/x/text", Version: "v0.21.0", Indirect: true}}) {
		t.Fatalf("locked %+v", locked)
	}
}

func TestTheDetectionIsAValidProbeAnswer(t *testing.T) {
	spectest.Proves(t, feature, "go-probe-answer", "the-detection-is-a-valid-probe-answer")
	d := Detect(tree(t, with(demo, map[string]string{"go.mod": "module github.com/Putnami/demo-observe-go\n"})))
	answer := d.Probe()
	if problems := wsproto.ValidateProbeResult(&answer); len(problems) != 0 {
		t.Fatalf("probe answer does not validate: %v", problems)
	}
	if answer.Extension != ProbeExtension || !reflect.DeepEqual(answer.WatchedFiles, []string{"go.work"}) {
		t.Fatalf("answer %+v", answer)
	}
	var api wsproto.ProbeProject
	for _, p := range answer.Projects {
		if p.Path == "api" {
			api = p
		}
	}
	if api.SourceName != "github.com/Putnami/demo-observe-go/api" || api.SourceFile != "api/go.mod" ||
		!reflect.DeepEqual(api.Dependencies, []string{"pricing"}) ||
		!reflect.DeepEqual(api.DependencySources, map[string]wsproto.DependencySource{"pricing": wsproto.DependencySourceGoModule}) ||
		!reflect.DeepEqual(api.WatchedFiles, []string{"api/go.mod", "api/go.sum"}) ||
		string(api.Metadata) != `{"ecosystem":"go","source":"workspace"}` {
		t.Fatalf("api project %+v", api)
	}
	if answer.Projects[0].Path != "." || answer.Projects[0].DependencySources != nil {
		t.Fatalf("root project %+v", answer.Projects[0])
	}
}

// TestTheDemoChangeMapsToItsModuleAndDependents walks the Go demo's pull
// request: each changed file maps to its module, and the reverse edges name
// api as the dependent of pricing.
func TestTheDemoChangeMapsToItsModuleAndDependents(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "a-file-belongs-to-its-innermost-module")
	spectest.Proves(t, feature, "go-module-edges", "a-local-replace-is-an-edge")
	d := Detect(tree(t, demo))
	changed := []string{"api/go.mod", "api/go.sum", "pricing/go.mod", "pricing/go.sum", "pricing/pricing.go", "pricing/pricing_test.go"}
	owners := map[string]string{}
	for _, file := range changed {
		owner, ok := d.Owner(file)
		if !ok {
			t.Fatalf("%s has no module", file)
		}
		owners[file] = owner
	}
	want := map[string]string{"api/go.mod": "api", "api/go.sum": "api", "pricing/go.mod": "pricing", "pricing/go.sum": "pricing", "pricing/pricing.go": "pricing", "pricing/pricing_test.go": "pricing"}
	if !reflect.DeepEqual(owners, want) {
		t.Fatalf("owners %v", owners)
	}
	dependents := map[string][]string{}
	for _, c := range d.Components() {
		for _, target := range c.DependsOn {
			dependents[target] = append(dependents[target], c.ID)
		}
	}
	if !reflect.DeepEqual(dependents, map[string][]string{"pricing": {"api"}}) {
		t.Fatalf("dependents %v", dependents)
	}
}

func TestGoWorkReplacesOverrideTheGoModAndAVersionedReplaceWins(t *testing.T) {
	spectest.Proves(t, feature, "go-module-edges", "a-local-replace-is-an-edge")
	spectest.Proves(t, feature, "go-locked-versions", "go-mod-requires-are-the-locked-versions")
	d := Detect(tree(t, map[string]string{
		"go.work":         "go 1.23\nuse ./app\nreplace example.com/lib => ./lib-work\n",
		"app/go.mod":      "module example.com/app\nrequire example.com/lib v1.0.0\nreplace example.com/lib => ../lib-mod\n",
		"lib-work/go.mod": "module example.com/lib-work\n",
		"lib-mod/go.mod":  "module example.com/lib-mod\n",
	}))
	// go.work sends the require to lib-work; the go.mod's own local replace
	// stays an edge as well.
	if got := module(t, d, "app").DependsOn; !reflect.DeepEqual(got, []string{"lib-mod", "lib-work"}) {
		t.Fatalf("app depends on %v", got)
	}

	goMod := []byte("module example.com/m\nrequire example.com/x v1.2.0\nreplace example.com/x => example.com/x v1.3.0\nreplace example.com/x v1.2.0 => example.com/x v1.2.1\n")
	locked, err := d.ReadLock("m/go.mod", goMod, nil)
	if err != nil || !reflect.DeepEqual(locked, []Locked{{Name: "example.com/x", Version: "v1.2.1"}}) {
		t.Fatalf("exact-version replace: %+v %v", locked, err)
	}
	locked, err = d.ReadLock("m/go.mod", goMod, []byte("go 1.23\nreplace example.com/x => example.com/x v1.4.0\n"))
	if err != nil || !reflect.DeepEqual(locked, []Locked{{Name: "example.com/x", Version: "v1.4.0"}}) {
		t.Fatalf("go.work replace: %+v %v", locked, err)
	}
	// The detection's own go.work does not reach a read given no go.work.
	locked, err = d.ReadLock("app/go.mod", []byte("module example.com/app\nrequire example.com/lib v1.0.0\n"), nil)
	if err != nil || !reflect.DeepEqual(locked, []Locked{{Name: "example.com/lib", Version: "v1.0.0"}}) {
		t.Fatalf("read without go.work: %+v %v", locked, err)
	}
	locked, err = d.ReadLock("m/go.mod", goMod, []byte("go 1.23\nreplace example.com/x => ./x\n"))
	if err != nil || len(locked) != 0 {
		t.Fatalf("go.work local replace: %+v %v", locked, err)
	}
}

func TestAGoWorkUseOutsideTheRepositoryIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "an-unreadable-manifest-is-a-diagnostic")
	d := Detect(tree(t, with(demo, map[string]string{"go.work": "go 1.23\nuse (\n\t./api\n\t./pricing\n\t../elsewhere\n)\n"})))
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestInvalid}) {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	if len(d.Modules) != 2 {
		t.Fatalf("modules %+v", d.Modules)
	}
}

func TestAnUnreadableDirectoryIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "go-modules-are-components", "an-unreadable-manifest-is-a-diagnostic")
	root := tree(t, with(demo, map[string]string{"locked/inner/go.mod": "module example.com/inner\n"}))
	dir := filepath.Join(root, "locked")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("the directory stays readable: the test runs with privileges")
	}
	d := Detect(root)
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) || d.Diagnostics[0].Field != "locked" {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	if len(d.Modules) != 2 {
		t.Fatalf("modules %+v", d.Modules)
	}
}

func TestADetectionDecodedFromJSONLocates(t *testing.T) {
	spectest.Proves(t, feature, "go-report-paths", "an-import-path-names-its-file-and-module")
	data, err := json.Marshal(Detect(tree(t, with(demo, map[string]string{
		"copy/go.mod": "module github.com/Putnami/demo-observe-go/pricing\n",
	}))))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Detection
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if file, component, ok := decoded.Locate("github.com/Putnami/demo-observe-go/pricing/format.go"); !ok || file != "pricing/format.go" || component != "pricing" {
		t.Fatalf("Locate = %q %q %v", file, component, ok)
	}
}
