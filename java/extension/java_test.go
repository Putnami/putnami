package java

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "java/stack-extension"

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gradleFixture writes the build of the observe gradle demo: :api depends on
// :pricing through projects.pricing and applies jacoco; :app, an Android
// library with unit test coverage, depends on it through project(":pricing");
// :pricing applies jacoco and locks its dependencies; :model has no tests.
func gradleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, ".java-version", "21\n")
	writeFile(t, root, "settings.gradle.kts", "rootProject.name = \"shop\"\nenableFeaturePreview(\"TYPESAFE_PROJECT_ACCESSORS\")\ninclude(\":app\", \":api\", \":pricing\", \":libs:model\", \":bom\", \":catalog\")\n")
	writeFile(t, root, "build.gradle.kts", "plugins {\n    alias(libs.plugins.android.library) apply false\n}\n")
	writeFile(t, root, "gradle/libs.versions.toml", "[versions]\nkotlin = \"2.0.21\"\n")
	writeFile(t, root, "gradle/wrapper/gradle-wrapper.properties", "distributionUrl=https\\://services.gradle.org/distributions/gradle-8.14.3-bin.zip\n")
	writeFile(t, root, "api/build.gradle.kts", "plugins {\n    alias(libs.plugins.kotlin.jvm)\n    jacoco\n}\ndependencies {\n    implementation(projects.pricing)\n}\n")
	writeFile(t, root, "api/src/main/kotlin/com/acme/api/Quotes.kt", "class Quotes\n")
	writeFile(t, root, "api/src/test/kotlin/com/acme/api/QuotesTest.kt", "class QuotesTest\n")
	writeFile(t, root, "app/build.gradle.kts", "plugins {\n    alias(libs.plugins.android.library)\n}\nandroid {\n    buildTypes {\n        debug {\n            enableUnitTestCoverage = true\n        }\n    }\n}\ndependencies {\n    implementation(project(\":pricing\"))\n}\n")
	writeFile(t, root, "app/src/main/kotlin/com/acme/app/CartScreen.kt", "class CartScreen\n")
	writeFile(t, root, "app/src/test/kotlin/com/acme/app/CartScreenTest.kt", "class CartScreenTest\n")
	writeFile(t, root, "pricing/build.gradle.kts", "plugins {\n    alias(libs.plugins.kotlin.jvm)\n    jacoco\n}\n")
	writeFile(t, root, "pricing/gradle.lockfile", "empty=\n")
	writeFile(t, root, "pricing/src/main/kotlin/com/acme/pricing/Pricing.kt", "object Pricing\n")
	writeFile(t, root, "pricing/src/test/kotlin/com/acme/pricing/PricingTest.kt", "class PricingTest\n")
	writeFile(t, root, "libs/model/build.gradle.kts", "plugins {\n    // jacoco\n    `java-library`\n}\n")
	writeFile(t, root, "libs/model/src/main/java/com/acme/model/Line.java", "class Line {}\n")
	writeFile(t, root, "bom/build.gradle.kts", "plugins {\n    `java-platform`\n}\n")
	writeFile(t, root, "catalog/build.gradle.kts", "plugins {\n    `version-catalog`\n}\n")
	return root
}

// mavenFixture writes the build of the observe maven demo: the aggregator
// lists pricing and api, and api depends on pricing.
func mavenFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "pom.xml", `<project><modelVersion>4.0.0</modelVersion><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version><packaging>pom</packaging><modules><module>pricing</module><module>api</module></modules></project>`)
	writeFile(t, root, "pricing/pom.xml", `<project><parent><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version></parent><artifactId>pricing</artifactId></project>`)
	writeFile(t, root, "pricing/src/main/java/com/acme/pricing/Pricing.java", "class Pricing {}\n")
	writeFile(t, root, "pricing/src/test/java/com/acme/pricing/PricingTest.java", "class PricingTest {}\n")
	writeFile(t, root, "api/pom.xml", `<project><parent><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version></parent><artifactId>api</artifactId><dependencies><dependency><groupId>${project.groupId}</groupId><artifactId>pricing</artifactId></dependency></dependencies></project>`)
	writeFile(t, root, "api/src/main/java/com/acme/api/Quotes.java", "class Quotes {}\n")
	return root
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probe(t *testing.T, root string, paths ...string) map[string]wsproto.ProbeProject {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		out[p.Path] = p
	}
	return out
}

func metadata(t *testing.T, p wsproto.ProbeProject) stacks.Metadata {
	t.Helper()
	var meta stacks.Metadata
	if err := json.Unmarshal(p.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestTheGradleProjectsAndMavenModulesBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "components-become-projects", "the-gradle-projects-and-maven-modules-become-projects-with-their-edges")
	type want struct {
		name, ref, source string
		deps              []string
	}
	check := func(got map[string]wsproto.ProbeProject, wants map[string]want) {
		t.Helper()
		if len(got) != len(wants) {
			t.Fatalf("projects = %v", got)
		}
		for path, w := range wants {
			p := got[path]
			meta := metadata(t, p)
			if p.SourceName != w.name || meta.Component != w.ref || p.SourceFile != w.source || !reflect.DeepEqual(p.Dependencies, w.deps) || meta.Root != "." {
				t.Errorf("%s = %+v, metadata %+v", path, p, meta)
			}
			if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"java"}) {
				t.Errorf("%s extensions %v tags %v", path, p.Extensions, p.Tags)
			}
		}
	}
	g := gradleFixture(t)
	got := probe(t, g, ".", "api", "app", "bom", "catalog", "docs", "libs/model", "pricing")
	check(got, map[string]want{
		"api":        {"api", ":api:", "api/build.gradle.kts", []string{"pricing"}},
		"app":        {"app", ":app:", "app/build.gradle.kts", []string{"pricing"}},
		"bom":        {"bom", ":bom:", "bom/build.gradle.kts", nil},
		"catalog":    {"catalog", ":catalog:", "catalog/build.gradle.kts", nil},
		"libs/model": {"libs/model", ":libs:model:", "libs/model/build.gradle.kts", nil},
		"pricing":    {"pricing", ":pricing:", "pricing/build.gradle.kts", nil},
	})
	if meta := metadata(t, got["api"]); meta.Toolchain != (stacks.Expected{Version: "21", Source: ".java-version"}) {
		t.Errorf("api toolchain = %+v", meta.Toolchain)
	}

	m := mavenFixture(t)
	check(probe(t, m, ".", "api", "pricing"), map[string]want{
		"api":     {"api", "com.acme:api", "api/pom.xml", []string{"pricing"}},
		"pricing": {"pricing", "com.acme:pricing", "pricing/pom.xml", nil},
	})

	// A single-project Gradle build is the root project, which keeps its
	// directory's name and runs its tasks by their root path.
	single := t.TempDir()
	writeFile(t, single, "settings.gradle", "rootProject.name = 'one'\n")
	writeFile(t, single, "build.gradle", "plugins { id 'java' }\n")
	writeFile(t, single, "src/main/java/A.java", "class A {}\n")
	check(probe(t, single, "."), map[string]want{".": {"", ":", "build.gradle", nil}})

	// A directory that is both a Gradle project and a Maven module runs as
	// the Gradle project, with a warning.
	writeFile(t, single, "pom.xml", `<project><groupId>com.acme</groupId><artifactId>one</artifactId></project>`)
	s := stack(t)
	result, _ := s.Probe(single, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"."}})
	if len(result.Projects) != 1 || metadata(t, result.Projects[0]).Variant != VariantGradle || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeBuildConflict {
		t.Errorf("a conflict = %+v", result)
	}
}

func TestEachComponentGetsTheVariantOfItsBuild(t *testing.T) {
	spectest.Proves(t, feature, "variants-follow-the-build", "each-component-gets-the-variant-of-its-build")
	got := probe(t, gradleFixture(t), "api", "app", "bom", "catalog", "libs/model", "pricing")
	for path, want := range map[string]string{"bom": VariantGradlePlatform, "catalog": VariantGradleCatalog, "api": VariantGradleJaCoCo, "app": VariantAndroidCoverage, "libs/model": VariantGradle, "pricing": VariantGradleJaCoCo} {
		if v := metadata(t, got[path]).Variant; v != want {
			t.Errorf("%s variant = %q, want %q", path, v, want)
		}
	}
	if v := metadata(t, probe(t, mavenFixture(t), "api")["api"]).Variant; v != VariantMaven {
		t.Errorf("a Maven module = %q", v)
	}
	for build, want := range map[string]string{
		"plugins { id(\"com.android.application\") }\nandroid { }":   VariantAndroid,
		"apply plugin: 'com.android.library'":                        VariantAndroid,
		"plugins { alias(libs.plugins.android.application) }":        VariantAndroid,
		"android {\n  testCoverageEnabled true\n}":                   VariantAndroidCoverage,
		"plugins {\n    id(\"jacoco\")\n}":                           VariantGradleJaCoCo,
		"plugins { id(\"org.gradle.version-catalog\") }":             VariantGradleCatalog,
		"plugins { id 'org.gradle.jacoco' }":                         VariantGradleJaCoCo,
		"apply plugin: 'jacoco'":                                     VariantGradleJaCoCo,
		"tasks.jacocoTestReport { }":                                 VariantGradle,
		"plugins { java }\n/* jacoco */":                             VariantGradle,
		"plugins { java }\n// id(\"com.android.library\")":           VariantGradle,
		"repositories { maven { url = uri(\"https://x/jacoco\") } }": VariantGradle,
		"plugins { id(\"version-catalog\") }":                        VariantGradleCatalog,
		"val p = \"/META-INF/*.kotlin_module\"\nval q = \"**/x\"\nandroid {\n  enableUnitTestCoverage = true\n}": VariantAndroidCoverage,
		"val s = \"\"\"a // b\"\"\"\napply plugin: 'jacoco'":                                                     VariantGradleJaCoCo,
	} {
		if got := gradleVariant(stripComments(build)); got != want {
			t.Errorf("%q = %s, want %s", build, got, want)
		}
	}
}

// stubTools puts first on PATH a java that prints version and a mvn that
// logs its arguments and writes the Surefire report, and writes a gradlew at
// root that logs its arguments and writes the reports Gradle's test,
// jacocoTestReport and the Android unit test tasks write. The real builds
// are the stack's own tools: the stubs stand at that boundary.
func stubTools(t *testing.T, root, version, junit string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "build.log")
	writeFile(t, bin, "javac", "#!/bin/sh\necho 'Picked up JAVA_TOOL_OPTIONS: -Dx=y' >&2\necho 'javac "+version+"'\n")
	report := "cat > \"$dir/TEST-stub.xml\" <<'XML'\n" + junit + "\nXML\n"
	jacoco := `<?xml version="1.0"?><report name="r"><package name="p"><sourcefile name="A.kt"><line nr="1" mi="0" ci="3"/><line nr="2" mi="2" ci="0"/><line nr="3" mi="0" ci="1"/></sourcefile></package></report>`
	writeFile(t, bin, "mvn", "#!/bin/sh\necho \"$PWD: mvn $*\" >> \""+log+"\"\ncase \"$*\" in *-DskipTests*) exit 0;; esac\ndir=target/surefire-reports\nmkdir -p $dir\n"+report)
	writeFile(t, root, "gradlew", `#!/bin/sh
echo "$PWD: gradlew $*" >> "`+log+`"
for a in "$@"; do
  case "$a" in
    *:test) dir=build/test-results/test;;
    *:testDebugUnitTest) dir=build/test-results/testDebugUnitTest;;
    *:jacocoTestReport) mkdir -p build/reports/jacoco/test; printf '%s' '`+jacoco+`' > build/reports/jacoco/test/jacocoTestReport.xml; continue;;
    *:createDebugUnitTestCoverageReport) mkdir -p build/reports/coverage/test/debug; printf '%s' '`+jacoco+`' > build/reports/coverage/test/debug/report.xml; continue;;
    *) continue;;
  esac
  [ "$prev" = "-x" ] || { mkdir -p $dir; `+report+`}
  prev="$a"
done
`)
	for _, f := range []string{filepath.Join(bin, "javac"), filepath.Join(bin, "mvn"), filepath.Join(root, "gradlew")} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const junitPass = `<?xml version="1.0"?>
<testsuite name="com.acme.QuotesTest">
  <testcase classname="com.acme.QuotesTest" name="quoteLabelsTheTotal()"/>
  <testcase classname="com.acme.QuotesTest" name="anEmptyQuoteIsZero()"/>
</testsuite>`

const junitFail = `<?xml version="1.0"?>
<testsuite name="com.acme.QuotesTest">
  <testcase classname="com.acme.QuotesTest" name="quoteLabelsTheTotal()"><failure message="expected 15.00 EUR"/></testcase>
</testsuite>`

// runJob runs a task of the component at rel through the extension's job,
// the way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, error) {
	t.Helper()
	s := stack(t)
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: probe(t, root, rel)[rel].Metadata},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, runErr
}

func ran(t *testing.T, log string) string {
	t.Helper()
	data, _ := os.ReadFile(log) //nolint:gosec // the stub's own log
	return string(data)
}

func TestATaskRunsTheVariantStepsAfterTheJavaCheck(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-variant-steps-after-the-java-check")
	g := gradleFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	for _, rel := range []string{"api", "app", "bom", "catalog"} {
		for _, task := range stacks.TaskNames {
			if status, _, err := runJob(t, g, rel, task); status != "OK" || err != nil {
				t.Fatalf("%s %s: %s %v\n%s", rel, task, status, err, ran(t, log))
			}
		}
	}
	for _, want := range []string{
		filepath.Join(g, "api") + ": gradlew --console=plain :api:check -x :api:test",
		filepath.Join(g, "bom") + ": gradlew --console=plain :bom:check\n",
		filepath.Join(g, "bom") + ": gradlew --console=plain :bom:assemble\n",
		filepath.Join(g, "catalog") + ": gradlew --console=plain :catalog:generateCatalogAsToml\n",
		filepath.Join(g, "api") + ": gradlew --console=plain :api:test :api:jacocoTestReport",
		filepath.Join(g, "api") + ": gradlew --console=plain :api:assemble",
		filepath.Join(g, "app") + ": gradlew --console=plain :app:testDebugUnitTest :app:createDebugUnitTestCoverageReport",
		filepath.Join(g, "app") + ": gradlew --console=plain :app:assembleDebug",
	} {
		if !strings.Contains(ran(t, log), want) {
			t.Errorf("ran\n%s\nwant %q", ran(t, log), want)
		}
	}

	m := mavenFixture(t)
	log = stubTools(t, m, "21.0.4", junitPass)
	for _, task := range stacks.TaskNames {
		if status, _, err := runJob(t, m, "pricing", task); status != "OK" || err != nil {
			t.Fatalf("pricing %s: %s %v", task, status, err)
		}
	}
	for _, want := range []string{
		"mvn --batch-mode --file " + m + "/pom.xml --projects com.acme:pricing clean\n",
		"mvn --batch-mode --file " + m + "/pom.xml --projects com.acme:pricing --also-make verify -DskipTests",
		"mvn --batch-mode --file " + m + "/pom.xml --projects com.acme:pricing --also-make verify\n",
		"mvn --batch-mode --file " + m + "/pom.xml --projects com.acme:pricing --also-make package -DskipTests",
	} {
		if !strings.Contains(ran(t, log), filepath.Join(m, "pricing")+": "+want) {
			t.Errorf("ran\n%s\nwant %q", ran(t, log), want)
		}
	}

	stubTools(t, g, "17.0.9", junitPass)
	if status, _, err := runJob(t, g, "api", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a Java mismatch fails the task: %s %v", status, err)
	}
}

func TestJUnitAndJaCoCoBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "junit-and-jacoco-become-the-test-and-coverage-summary")
	g := gradleFixture(t)
	stubTools(t, g, "21.0.4", junitPass)
	for _, rel := range []string{"api", "app"} {
		status, data, err := runJob(t, g, rel, stacks.TaskTest)
		if status != "OK" || err != nil {
			t.Fatalf("%s test: %s %v", rel, status, err)
		}
		if got := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 {
			t.Errorf("%s testSummary = %v", rel, got)
		}
		if got := data["coverageSummary"].(map[string]any); got["covered"] != 2 || got["total"] != 3 {
			t.Errorf("%s coverageSummary = %v", rel, got)
		}
	}
	// Gradle writes the JaCoCo XML report only when the build asks for it:
	// without it the coverage is read as none.
	if status, data, err := runJobNoXML(t, g); status != "OK" || err != nil || data["coverageSummary"] != nil {
		t.Errorf("a JaCoCo build without its XML report: %s %v %v", status, err, data)
	}
	// A project without tests expects no report.
	if status, data, err := runJob(t, g, "libs/model", stacks.TaskTest); status != "OK" || err != nil || data["testSummary"] != nil {
		t.Errorf("a project without tests: %s %v %v", status, err, data)
	}

	// A Maven module without a JaCoCo report reads its Surefire report alone.
	m := mavenFixture(t)
	stubTools(t, m, "21.0.4", junitPass)
	status, data, err := runJob(t, m, "pricing", stacks.TaskTest)
	if status != "OK" || err != nil || data["testSummary"].(map[string]any)["total"] != 2 || data["coverageSummary"] != nil {
		t.Errorf("maven test: %s %v %v", status, err, data)
	}
	writeFile(t, m, "pricing/target/site/jacoco/jacoco.xml", `<report name="r"><package name="p"><sourcefile name="P.java"><line nr="1" mi="0" ci="1"/><line nr="2" mi="1" ci="0"/></sourcefile></package></report>`)
	_, data, _ = runJob(t, m, "pricing", stacks.TaskTest)
	if data["coverageSummary"] != nil {
		t.Errorf("a stale JaCoCo report is removed before the steps: %v", data["coverageSummary"])
	}

	stubTools(t, g, "21.0.4", junitFail)
	if status, data, err := runJob(t, g, "api", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}
}

func resolve(t *testing.T, root, project, selector string) wsproto.InputResolveAnswer {
	t.Helper()
	s := stack(t)
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	return result.Inputs[0]
}

func TestALockfileChangeKeepsTheLintKey(t *testing.T) {
	spectest.Proves(t, feature, "lint-skips-the-lockfiles", "a-lockfile-change-keeps-the-lint-key")
	g := gradleFixture(t)
	if got := resolve(t, g, "pricing", "java:lint"); !reflect.DeepEqual(got.Files, []string{"build.gradle.kts", "src/main/kotlin/com/acme/pricing/Pricing.kt", "src/test/kotlin/com/acme/pricing/PricingTest.kt"}) {
		t.Errorf("gradle lint reads what check reads, not the lockfile = %+v", got)
	}
	if got := resolve(t, g, "pricing", "java:test"); !slices.Contains(got.Files, "gradle.lockfile") {
		t.Errorf("gradle test reads the lockfile = %+v", got)
	}
	writeFile(t, g, "catalog/libs.versions.toml", "[versions]\n")
	writeFile(t, g, "catalog/build/version-catalog/libs.versions.toml", "[versions]\n")
	if got := resolve(t, g, "catalog", "java:build"); !reflect.DeepEqual(got.Files, []string{"build.gradle.kts", "libs.versions.toml"}) {
		t.Errorf("a catalog build reads its own TOML, not its output = %+v", got)
	}
	m := mavenFixture(t)
	for _, sel := range []string{"java:lint", "java:build"} {
		if got := resolve(t, m, "pricing", sel); !reflect.DeepEqual(got.Files, []string{"pom.xml", "src/main/java/com/acme/pricing/Pricing.java", "src/test/java/com/acme/pricing/PricingTest.java"}) {
			t.Errorf("maven %s reads the test sources -DskipTests compiles = %+v", sel, got)
		}
	}
	s := stack(t)
	for _, task := range stacks.TaskNames {
		if slices.Contains(s.Profile.ShippedRootPatterns(task), "gradle.lockfile") != (task != stacks.TaskLint) {
			t.Errorf("%s reads the root lockfile: %v", task, s.Profile.ShippedRootPatterns(task))
		}
	}
	if !slices.Contains(s.Profile.ShippedRootPatterns(stacks.TaskTest), "**/pom.xml") {
		t.Error("every POM of the build, parents included, is a Maven input")
	}
	for _, p := range s.Profile.ShippedRootPatterns(stacks.TaskBuild) {
		for _, dir := range []string{"buildSrc/", "build-logic/"} {
			if strings.HasPrefix(p, dir) && !strings.HasSuffix(p, "/src/**") && !strings.HasSuffix(p, ".kts") && !strings.HasSuffix(p, ".gradle") {
				t.Errorf("%s may select the build output of %s", p, dir)
			}
		}
	}
	for _, p := range []string{"build-logic/src/main/kotlin/A.kt", "build-logic/convention/src/main/kotlin/A.kt", "build-logic/plugins/java/build.gradle.kts"} {
		if !slices.ContainsFunc(s.Profile.ShippedRootPatterns(stacks.TaskBuild), func(pat string) bool { return stacks.MatchPattern(pat, p) }) {
			t.Errorf("%s reaches no key", p)
		}
	}
	for _, p := range []string{"build-logic/build/classes/A.class", "build-logic/.gradle/8.14/file.lock", "buildSrc/build/libs/buildSrc.jar"} {
		if slices.ContainsFunc(s.Profile.ShippedRootPatterns(stacks.TaskBuild), func(pat string) bool { return stacks.MatchPattern(pat, p) }) {
			t.Errorf("the build output %s reaches the key", p)
		}
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	g := gradleFixture(t)
	writeFile(t, g, "app/putnami.json", `{"options": {"@putnami/java": {
	  "inputs": {"tests": ["src/test/**", "src/testFree/**"]},
	  "tasks": {"test": {
	    "steps": [["{root}/gradlew", "{component}testFreeDebugUnitTest"]],
	    "reports": {"junit": ["build/test-results/testFreeDebugUnitTest/*.xml"], "coverage": []}
	  }}
	}}}`)
	writeFile(t, g, "app/src/testFree/kotlin/FreeTest.kt", "class FreeTest\n")
	if got := resolve(t, g, "app", "java:test"); !slices.Contains(got.Files, "src/testFree/kotlin/FreeTest.kt") || !slices.Contains(got.Files, "putnami.json") {
		t.Errorf("an input override is honored: %v", got.Files)
	}
	log := stubTools(t, g, "21.0.4", junitPass)
	if status, _, err := runJob(t, g, "app", stacks.TaskTest); status != "FAILED" || err == nil {
		t.Errorf("the flavored step writes no report where the override reads it: %s %v", status, err)
	}
	if !strings.Contains(ran(t, log), "app: gradlew :app:testFreeDebugUnitTest\n") {
		t.Errorf("a step override is honored: %s", ran(t, log))
	}

	writeFile(t, g, "app/putnami.json", `{"options": {"@putnami/java": {"variants": {}}}}`)
	if status, _, err := runJob(t, g, "app", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeProjectConfigInvalid {
		t.Errorf("an override cannot change the variants: %s %v", status, err)
	}
	writeFile(t, g, "app/putnami.json", `{"options": {"@putnami/java": {"inputs": {"sources": ["{root}/secrets.properties"]}}}}`)
	if got := resolve(t, g, "app", "java:lint"); got.Error == "" {
		t.Errorf("an override cannot add a stack-root input: %+v", got)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	s := stack(t)
	if got := s.Selectors(); !slices.Equal(got, []string{"java:build", "java:lint", "java:test"}) {
		t.Errorf("selectors = %v", got)
	}
	if got := s.Profile.VariantNames(); !slices.Equal(got, []string{VariantAndroid, VariantAndroidCoverage, VariantGradle, VariantGradleCatalog, VariantGradleJaCoCo, VariantGradlePlatform, VariantMaven}) {
		t.Errorf("variants = %v", got)
	}
}

// runJobNoXML runs the test task of api with a gradlew that writes the JUnit
// report but no JaCoCo XML report.
func runJobNoXML(t *testing.T, g string) (string, map[string]any, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g, "gradlew")) //nolint:gosec // the stub's own script
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, g, "gradlew", strings.Replace(string(data), "*:jacocoTestReport) mkdir", "*:jacocoTestReport) continue; mkdir", 1))
	defer writeFile(t, g, "gradlew", string(data))
	return runJob(t, g, "api", stacks.TaskTest)
}

func TestNestedBuildsHaveTheirOwnStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "nested-builds-are-stack-roots", "nested-gradle-and-maven-builds-are-their-components-stack-roots")
	root := t.TempDir()
	writeFile(t, root, "samples/gradle/settings.gradle.kts", "include(\":app\", \":lib\")\n")
	writeFile(t, root, "samples/gradle/app/build.gradle.kts", "plugins {\n    application\n}\ndependencies {\n    implementation(project(\":lib\"))\n}\n")
	writeFile(t, root, "samples/gradle/app/src/main/java/App.java", "class App {}\n")
	writeFile(t, root, "samples/gradle/lib/build.gradle.kts", "plugins {\n    `java-library`\n}\n")
	writeFile(t, root, "samples/gradle/lib/src/main/java/Lib.java", "class Lib {}\n")
	writeFile(t, root, "samples/gradle/buildSrc/settings.gradle.kts", "\n")
	writeFile(t, root, "samples/gradle/buildSrc/build.gradle.kts", "plugins { `kotlin-dsl` }\n")
	writeFile(t, root, "samples/maven/pom.xml", `<project><groupId>com.acme</groupId><artifactId>shop</artifactId><packaging>pom</packaging><modules><module>app</module><module>lib</module></modules></project>`)
	writeFile(t, root, "samples/maven/app/pom.xml", `<project><parent><groupId>com.acme</groupId></parent><artifactId>app</artifactId><dependencies><dependency><groupId>com.acme</groupId><artifactId>lib</artifactId></dependency></dependencies><build><plugins><plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build></project>`)
	writeFile(t, root, "samples/maven/lib/pom.xml", `<project><parent><groupId>com.acme</groupId></parent><artifactId>lib</artifactId></project>`)
	got := probe(t, root, "samples/gradle", "samples/gradle/app", "samples/gradle/buildSrc", "samples/gradle/lib", "samples/maven", "samples/maven/app", "samples/maven/lib")
	want := map[string]struct {
		root, ref, typ string
		deps           []string
	}{
		"samples/gradle":     {"samples/gradle", "", "", nil},
		"samples/gradle/app": {"samples/gradle", ":app:", TypeApplication, []string{"samples/gradle", "samples/gradle/lib"}},
		"samples/gradle/lib": {"samples/gradle", ":lib:", TypeLibrary, []string{"samples/gradle"}},
		"samples/maven":      {"samples/maven", "", "", nil},
		"samples/maven/app":  {"samples/maven", "com.acme:app", TypeApplication, []string{"samples/maven", "samples/maven/lib"}},
		"samples/maven/lib":  {"samples/maven", "com.acme:lib", TypeLibrary, []string{"samples/maven"}},
	}
	if len(got) != len(want) {
		t.Fatalf("projects = %v", got)
	}
	for path, w := range want {
		p, ok := got[path]
		if !ok {
			t.Errorf("no project %s", path)
			continue
		}
		meta := metadata(t, p)
		if meta.Root != w.root || meta.Component != w.ref || p.Type != w.typ || !reflect.DeepEqual(p.Dependencies, w.deps) {
			t.Errorf("%s = %+v, metadata %+v", path, p, meta)
		}
	}
	if got := resolve(t, root, "samples/gradle", "java:build"); !slices.Contains(got.Files, "settings.gradle.kts") || got.Error != "" {
		t.Errorf("the nested root reads its root files: %+v", got)
	}
}
