package dart

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-dart"

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

func byID(d Detection) map[string]Component {
	out := map[string]Component{}
	for _, c := range d.Components {
		out[c.ID] = c
	}
	return out
}

func codes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Code)
	}
	slices.Sort(out)
	return out
}

// pubWorkspace is a pub workspace of Dart 3.6: the root lists its members,
// and each member opts in with resolution: workspace.
var pubWorkspace = map[string]string{
	"pubspec.yaml": `name: shop_workspace
publish_to: none
environment:
  sdk: ^3.6.0
workspace:
  - apps/mobile
  - packages/*
dev_dependencies:
  melos: ^7.0.0
melos:
  scripts:
    test: melos exec -- flutter test
`,
	"apps/mobile/pubspec.yaml": `name: mobile
publish_to: none
resolution: workspace
environment:
  sdk: ^3.6.0
dependencies:
  flutter:
    sdk: flutter
  pricing: ^1.0.0
  ui:
  http: ^1.2.2
dev_dependencies:
  flutter_test:
    sdk: flutter
`,
	"packages/pricing/pubspec.yaml": "name: pricing\nversion: 1.0.0\nresolution: workspace\n",
	"packages/ui/pubspec.yaml": `name: ui
resolution: workspace
dependencies:
  flutter: {sdk: flutter}
dev_dependencies:
  pricing: {version: ^1.0.0}
`,
	"packages/ui/example/pubspec.yaml":    "name: ui_example\ndependencies:\n  ui:\n    path: ..\n",
	"tools/gen/pubspec.yaml":              "name: gen\ndependencies:\n  pricing: ^1.0.0\n",
	"apps/mobile/build/web/pubspec.yaml":  "name: build_output\n",
	"apps/mobile/ios/Pods/pubspec.yaml":   "name: pods\n",
	"apps/mobile/.dart_tool/pubspec.yaml": "name: tool\n",
	"node_modules/x/pubspec.yaml":         "name: x\n",
}

func TestPubWorkspaceMembersAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "pub-workspace-members-are-components")
	spectest.Proves(t, feature, "dart-package-edges", "a-workspace-package-dependency-is-an-edge")
	d := Detect(writeTree(t, pubWorkspace))
	want := []Component{
		{ID: "apps/mobile", Name: "mobile", Source: evidence.SourceWorkspace, DependsOn: []string{"packages/pricing", "packages/ui"}, Flutter: true, PubWorkspace: "."},
		{ID: "packages/pricing", Name: "pricing", Source: evidence.SourceWorkspace, PubWorkspace: "."},
		{ID: "packages/ui", Name: "ui", Source: evidence.SourceWorkspace, DependsOn: []string{"packages/pricing"}, Flutter: true, PubWorkspace: "."},
		{ID: "packages/ui/example", Name: "ui_example", Source: evidence.SourceManifest, DependsOn: []string{"packages/ui"}},
		{ID: "tools/gen", Name: "gen", Source: evidence.SourceManifest},
	}
	if !reflect.DeepEqual(d.Components, want) {
		t.Errorf("components\n%+v\nwant\n%+v", d.Components, want)
	}
	if len(d.Diagnostics) != 0 {
		t.Errorf("diagnostics %v", d.Diagnostics)
	}
}

func TestMelosPackagesAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "melos-packages-are-components")
	spectest.Proves(t, feature, "dart-package-edges", "a-workspace-package-dependency-is-an-edge")
	d := Detect(writeTree(t, map[string]string{
		"melos.yaml": `name: shop
packages:
  - apps/*
  - packages/**
ignore:
  - packages/**/example
`,
		"pubspec.yaml":                      "name: shop_root\ndev_dependencies:\n  melos: ^6.3.0\n",
		"apps/mobile/pubspec.yaml":          "name: mobile\ndependencies:\n  pricing: any\n  design:\n    path: ../../packages/ui\n",
		"packages/pricing/pubspec.yaml":     "name: pricing\n",
		"packages/ui/pubspec.yaml":          "name: design\n",
		"packages/ui/example/pubspec.yaml":  "name: design_example\ndependencies:\n  design: any\n",
		"packages/nested/core/pubspec.yaml": "name: core\n",
	}))
	got := byID(d)
	for id, source := range map[string]string{
		"apps/mobile": evidence.SourceWorkspace, "packages/pricing": evidence.SourceWorkspace, "packages/ui": evidence.SourceWorkspace,
		"packages/nested/core": evidence.SourceWorkspace, "packages/ui/example": evidence.SourceManifest,
	} {
		if got[id].Source != source {
			t.Errorf("%s source %q, want %q", id, got[id].Source, source)
		}
	}
	if _, root := got["."]; root || len(d.Components) != 5 {
		t.Errorf("components %+v", d.Components)
	}
	if !slices.Equal(got["apps/mobile"].DependsOn, []string{"packages/pricing", "packages/ui"}) {
		t.Errorf("mobile edges %v", got["apps/mobile"].DependsOn)
	}
	// The example is no melos package, so naming design links nothing.
	if len(got["packages/ui/example"].DependsOn) != 0 {
		t.Errorf("example edges %v", got["packages/ui/example"].DependsOn)
	}
}

func TestAPubspecOutsideAWorkspaceIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "a-pubspec-outside-a-workspace-is-a-manifest-component")
	d := Detect(writeTree(t, map[string]string{
		"pubspec.yaml":         "name: app\ndependencies:\n  shared:\n    path: shared\n",
		"shared/pubspec.yaml":  "name: shared\n",
		"backend/package.json": "{}",
	}))
	want := []Component{
		{ID: ".", Name: "app", Source: evidence.SourceManifest, DependsOn: []string{"shared"}},
		{ID: "shared", Name: "shared", Source: evidence.SourceManifest},
	}
	if !reflect.DeepEqual(d.Components, want) {
		t.Errorf("components %+v", d.Components)
	}
	if empty := Detect(writeTree(t, map[string]string{"go.mod": "module x\n"})); len(empty.Components) != 0 || len(empty.Diagnostics) != 0 {
		t.Errorf("a repository without a pubspec.yaml: %+v", empty)
	}
}

func TestAPathDependencyIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "dart-package-edges", "a-path-dependency-is-an-edge")
	d := Detect(writeTree(t, map[string]string{
		"app/pubspec.yaml": `name: app
dependencies:
  core: {path: ../libs/core/}
dev_dependencies:
  testing:
    path: ../libs/testing
dependency_overrides:
  models:
    path: "..\\libs\\models"
`,
		"libs/core/pubspec.yaml":    "name: core\ndependencies:\n  self:\n    path: .\n",
		"libs/testing/pubspec.yaml": "name: testing\n",
		"libs/models/pubspec.yaml":  "name: models\n",
	}))
	got := byID(d)
	if !slices.Equal(got["app"].DependsOn, []string{"libs/core", "libs/models", "libs/testing"}) {
		t.Errorf("app edges %v", got["app"].DependsOn)
	}
	if len(got["libs/core"].DependsOn) != 0 {
		t.Errorf("a path to itself is an edge: %v", got["libs/core"].DependsOn)
	}
}

func TestAGitSDKOrHostedDependencyIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "dart-package-edges", "a-git-sdk-or-hosted-dependency-is-no-edge")
	d := Detect(writeTree(t, map[string]string{
		"pubspec.yaml": "name: ws\nworkspace: [app, lib]\n",
		"app/pubspec.yaml": `name: app
resolution: workspace
dependencies:
  lib:
    git:
      url: https://github.com/acme/lib.git
  lib2:
    hosted: https://pub.example.com
    version: ^1.0.0
  flutter:
    sdk: flutter
  outside:
    path: ../../elsewhere
  absolute:
    path: /opt/lib
  missing:
    path: ../nowhere
  empty:
    path:
`,
		"lib/pubspec.yaml": "name: lib\nresolution: workspace\n",
	}))
	if got := byID(d)["app"]; len(got.DependsOn) != 0 {
		t.Errorf("app edges %v", got.DependsOn)
	}
	if len(d.Components) != 2 {
		t.Errorf("components %+v", d.Components)
	}
}

func TestAMalformedPubspecIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "a-malformed-pubspec-is-a-diagnostic")
	d := Detect(writeTree(t, map[string]string{
		"pubspec.yaml":          "name: ws\nworkspace: packages\n",
		"melos.yaml":            "packages: apps\nignore:\n  - ../outside\n",
		"broken/pubspec.yaml":   "name: [unterminated\n",
		"nameless/pubspec.yaml": "description: no name\n",
		"good/pubspec.yaml":     "name: good\n",
	}))
	if len(d.Components) != 1 || d.Components[0].ID != "good" || d.Components[0].Source != evidence.SourceManifest {
		t.Errorf("components %+v", d.Components)
	}
	want := []string{CodeMelosInvalid, CodePatternInvalid, CodePubspecInvalid, CodePubspecInvalid, CodePubspecInvalid}
	if got := codes(d.Diagnostics); !slices.Equal(got, want) {
		t.Errorf("diagnostics %v, want %v", d.Diagnostics, want)
	}
	for _, d := range d.Diagnostics {
		if d.Severity != diag.Warning {
			t.Errorf("%s is not a warning", d.Code)
		}
	}
	broken := Detect(writeTree(t, map[string]string{"melos.yaml": "packages: [a\n", "a/pubspec.yaml": "name: a\n"}))
	if got := codes(broken.Diagnostics); !slices.Equal(got, []string{CodeMelosInvalid}) || !strings.Contains(broken.Diagnostics[0].Message, "does not read") {
		t.Errorf("diagnostics %v", broken.Diagnostics)
	}
	if len(broken.Components) != 1 || broken.Components[0].Source != evidence.SourceManifest {
		t.Errorf("components %+v", broken.Components)
	}
}

func TestAPackageThatSharesAMemberNameDoesNotTakeItsEdges(t *testing.T) {
	spectest.Proves(t, feature, "dart-package-edges", "a-workspace-package-dependency-is-an-edge")
	for name, files := range map[string]map[string]string{
		"pub workspace": {
			"pubspec.yaml":             "name: ws\nworkspace: [apps/mobile, packages/ui]\n",
			"apps/mobile/pubspec.yaml": "name: mobile\nresolution: workspace\ndependencies:\n  ui: ^1.0.0\n",
			"packages/ui/pubspec.yaml": "name: ui\nresolution: workspace\n",
			"_archive/ui/pubspec.yaml": "name: ui\n",
		},
		"melos": {
			"melos.yaml":               "packages: [apps/*, packages/*]\n",
			"apps/mobile/pubspec.yaml": "name: mobile\ndependencies:\n  ui: any\n",
			"packages/ui/pubspec.yaml": "name: ui\n",
			"examples/ui/pubspec.yaml": "name: ui\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := byID(Detect(writeTree(t, files)))
			if !slices.Equal(got["apps/mobile"].DependsOn, []string{"packages/ui"}) {
				t.Errorf("mobile edges %v", got["apps/mobile"].DependsOn)
			}
		})
	}
}

func TestABuildDirectoryIsWalkedOnlyWhenItIsAPackage(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "pub-workspace-members-are-components")
	d := Detect(writeTree(t, map[string]string{
		"pubspec.yaml":                    "name: ws\nworkspace: [build, app]\n",
		"build/pubspec.yaml":              "name: build\nresolution: workspace\n",
		"app/pubspec.yaml":                "name: app\nresolution: workspace\ndependencies:\n  build: any\n",
		"app/build/flutter_assets/x.json": "{}",
		"app/build/web/pubspec.yaml":      "name: copied\n",
	}))
	want := []Component{
		{ID: "app", Name: "app", Source: evidence.SourceWorkspace, DependsOn: []string{"build"}, PubWorkspace: "."},
		{ID: "build", Name: "build", Source: evidence.SourceWorkspace, PubWorkspace: "."},
	}
	if !reflect.DeepEqual(d.Components, want) {
		t.Errorf("components %+v", d.Components)
	}
}

func TestPatternsStayInTheRepository(t *testing.T) {
	for pattern, want := range map[string][]string{
		"packages/*":  {"packages", "*"},
		"./apps//x/":  {"apps", "x"},
		`apps\mobile`: {"apps", "mobile"},
		"../outside":  nil,
		"/abs":        nil,
		"":            nil,
		"packages/[a": nil,
		"a/../b":      nil,
	} {
		got, ok := patternSegments(pattern)
		if ok != (want != nil) || !slices.Equal(got, want) {
			t.Errorf("patternSegments(%q) = %v, %v; want %v", pattern, got, ok, want)
		}
	}
}

func TestAPackageThatNamesTheFlutterSDKIsAFlutterPackage(t *testing.T) {
	d := byID(Detect(writeTree(t, map[string]string{
		"app/pubspec.yaml":    "name: app\ndependencies:\n  flutter:\n    sdk: flutter\n",
		"tests/pubspec.yaml":  "name: tests\ndev_dependencies:\n  flutter_test:\n    sdk: flutter\n",
		"pinned/pubspec.yaml": "name: pinned\nenvironment:\n  sdk: ^3.7.0\n  flutter: \">=3.29.0\"\n",
		"cli/pubspec.yaml":    "name: cli\nenvironment:\n  sdk: ^3.7.0\ndependencies:\n  args: ^2.6.0\n  meta:\n    sdk: dart\n",
	})))
	for id, want := range map[string]bool{"app": true, "tests": true, "pinned": true, "cli": false} {
		if d[id].Flutter != want {
			t.Errorf("%s Flutter = %v, want %v", id, d[id].Flutter, want)
		}
	}
}

func TestANestedPubWorkspaceResolvesItsMembers(t *testing.T) {
	spectest.Proves(t, feature, "dart-packages-are-components", "pub-workspace-members-are-components")
	d := byID(Detect(writeTree(t, map[string]string{
		"mono/pubspec.yaml":            "name: mono\npublish_to: none\nworkspace:\n  - app\n  - core\n",
		"mono/app/pubspec.yaml":        "name: app\nresolution: workspace\ndependencies:\n  core:\n",
		"mono/core/pubspec.yaml":       "name: core\nresolution: workspace\n",
		"mono/tools/x/pubspec.yaml":    "name: x\n",
		"outer/pubspec.yaml":           "name: outer\nworkspace: [inner]\n",
		"outer/inner/pubspec.yaml":     "name: inner\nresolution: workspace\nworkspace: [lib]\n",
		"outer/inner/lib/pubspec.yaml": "name: deep\nresolution: workspace\n",
	})))
	for id, want := range map[string]Component{
		"mono":            {ID: "mono", Name: "mono", Source: evidence.SourceManifest, DeclaresWorkspace: true},
		"mono/app":        {ID: "mono/app", Name: "app", Source: evidence.SourceWorkspace, PubWorkspace: "mono", DependsOn: []string{"mono/core"}},
		"mono/core":       {ID: "mono/core", Name: "core", Source: evidence.SourceWorkspace, PubWorkspace: "mono"},
		"mono/tools/x":    {ID: "mono/tools/x", Name: "x", Source: evidence.SourceManifest},
		"outer/inner":     {ID: "outer/inner", Name: "inner", Source: evidence.SourceWorkspace, PubWorkspace: "outer", DeclaresWorkspace: true},
		"outer/inner/lib": {ID: "outer/inner/lib", Name: "deep", Source: evidence.SourceWorkspace, PubWorkspace: "outer"},
	} {
		if got := d[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", id, got, want)
		}
	}
}
