package gradle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-jvm"

// writeTree writes files, keyed by slash path, under a new directory; a key
// ending with / makes a directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(file, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func codes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func TestSettingsIncludesAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "gradle-projects-are-components", "settings-includes-are-components")
	root := writeTree(t, map[string]string{
		"settings.gradle.kts": `rootProject.name = "shop"
include(":app", ":api")
include("pricing") // a relative path is a root project
// include(":commented")
/* include(":blocked") */
include(":libs:core")
include(":legacy")
project(":legacy").projectDir = file("old/legacy-code")
`,
		"app/build.gradle.kts":      "plugins { id(\"com.android.library\") }\n",
		"api/":                      "",
		"pricing/":                  "",
		"libs/core/":                "",
		"old/legacy-code/":          "",
		"src/main/kotlin/Main.kt":   "fun main() {}\n",
		"buildSrc/build.gradle.kts": "",
	})
	got := Detect(root)
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics %+v", got.Diagnostics)
	}
	want := []Component{
		{ID: ".", Name: ":"},
		{ID: "api", Name: ":api"},
		{ID: "app", Name: ":app"},
		{ID: "libs/core", Name: ":libs:core"},
		{ID: "old/legacy-code", Name: ":legacy"},
		{ID: "pricing", Name: ":pricing"},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}
}

func TestGroovySettingsAreRead(t *testing.T) {
	spectest.Proves(t, feature, "gradle-projects-are-components", "settings-includes-are-components")
	root := writeTree(t, map[string]string{
		"settings.gradle": `rootProject.name = 'shop'
include ':app',
        ':api'
include(':pricing')
includeBuild 'build-logic'
project(':api').projectDir = new File(settingsDir, 'services/api')
`,
		"app/":          "",
		"services/api/": "",
		"pricing/":      "",
	})
	got := Detect(root)
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics %+v", got.Diagnostics)
	}
	want := []Component{{ID: "app", Name: ":app"}, {ID: "pricing", Name: ":pricing"}, {ID: "services/api", Name: ":api"}}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}
}

func TestAMissingProjectDirectoryIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "gradle-projects-are-components", "a-missing-project-directory-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"settings.gradle.kts": "include(\":gone\", \":kept\", \":outside\")\nproject(\":outside\").projectDir = file(\"../elsewhere\")\n",
		"kept/":               "",
	})
	got := Detect(root)
	if want := []Component{{ID: "kept", Name: ":kept"}}; !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}
	if want := []string{CodeProjectMissing, CodeProjectMissing}; !reflect.DeepEqual(codes(got.Diagnostics), want) {
		t.Errorf("diagnostics %+v, want %v", got.Diagnostics, want)
	}
	if got := Detect(t.TempDir()); len(got.Components) != 0 || len(got.Diagnostics) != 0 {
		t.Errorf("a directory without settings: %+v", got)
	}
}

func TestAComputedIncludeIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "gradle-dynamic-settings", "a-computed-include-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"settings.gradle.kts": `include(":api")
val modules = listOf("a", "b")
modules.forEach { include(it) }
include(":static", "dyn-$name")
file("plugins").listFiles()?.forEach { include(":plugins:${it.name}") }
project(":api").projectDir = file("services/${"api"}")
rootProject.children.forEach { it.projectDir = file("x/" + it.name) }
`,
		"api/":    "",
		"static/": "",
	})
	got := Detect(root)
	if want := []Component{{ID: "api", Name: ":api"}, {ID: "static", Name: ":static"}}; !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != CodeDynamicSettings || got.Diagnostics[0].Severity != diag.Warning || got.Diagnostics[0].Field != "settings.gradle.kts" {
		t.Fatalf("diagnostics %+v, want one %s warning", got.Diagnostics, CodeDynamicSettings)
	}
	if !strings.Contains(got.Diagnostics[0].Message, "5 include or projectDir") {
		t.Errorf("message %q, want 5 computed values", got.Diagnostics[0].Message)
	}
}

func TestProjectReferencesAreEdges(t *testing.T) {
	spectest.Proves(t, feature, "gradle-project-edges", "project-references-are-edges")
	root := writeTree(t, map[string]string{
		"settings.gradle": "include ':app', ':api', ':pricing', ':libs:core', ':libs:core:impl', ':libs:util'\n",
		"app/build.gradle": `plugins { id 'com.android.application' }
dependencies {
    implementation project(':pricing')
    implementation project(path: ':api', configuration: 'default')
    // implementation project(':libs:util')
    testImplementation project(":unknown")
}
`,
		"api/build.gradle.kts": `dependencies {
    implementation(project(path = ":libs:core"))
    implementation(project(":api")) // itself
}
`,
		"pricing/":               "",
		"libs/core/build.gradle": "dependencies { implementation project('impl'); api project(':libs:util') }\n",
		"libs/core/impl/":        "",
		"libs/util/":             "",
	})
	got := Detect(root)
	want := map[string][]string{
		"app":            {"api", "pricing"},
		"api":            {"libs/core"},
		"libs/core":      {"libs/core/impl", "libs/util"},
		"libs/core/impl": nil,
		"libs/util":      nil,
		"pricing":        nil,
	}
	if len(got.Components) != len(want) {
		t.Fatalf("components %+v", got.Components)
	}
	for _, c := range got.Components {
		if !reflect.DeepEqual(c.DependsOn, want[c.ID]) {
			t.Errorf("%s depends on %v, want %v", c.ID, c.DependsOn, want[c.ID])
		}
	}
}

func TestTypeSafeAccessorsAreEdges(t *testing.T) {
	spectest.Proves(t, feature, "gradle-project-edges", "type-safe-accessors-are-edges")
	root := writeTree(t, map[string]string{
		"settings.gradle.kts": "enableFeaturePreview(\"TYPESAFE_PROJECT_ACCESSORS\")\ninclude(\":app\", \":shared-model\", \":libs:core\", \":libs:snake_case\")\n",
		"app/build.gradle.kts": `dependencies {
    implementation(projects.sharedModel)
    implementation(projects.libs.core.dependencyProject)
    implementation(projects.libs.snakeCase)
    implementation(subprojects.first())
    implementation(projects.notAProject)
}
`,
		"shared-model/":    "",
		"libs/core/":       "",
		"libs/snake_case/": "",
	})
	got := Detect(root)
	if got.Components[0].ID != "app" || !reflect.DeepEqual(got.Components[0].DependsOn, []string{"libs/core", "libs/snake_case", "shared-model"}) {
		t.Errorf("components %+v", got.Components)
	}
}

func TestCommentsKeepStrings(t *testing.T) {
	text := stripComments("a(\"http://x\") // c\n/* b\nc */ d('/*') e(\"\"\"x // y\"\"\")")
	if want := "a(\"http://x\")     \n    \n     d('/*') e(\"\"\"x // y\"\"\")"; text != want {
		t.Errorf("stripComments = %q, want %q", text, want)
	}
	if got := accessor(":libs:foo-bar_baz"); got != "libs.fooBarBaz" {
		t.Errorf("accessor = %q", got)
	}
}
