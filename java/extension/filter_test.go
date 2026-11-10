package java

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

// filterFixture is the Gradle fixture with a component of each variant that
// tests: plain is gradle, api gradle-jacoco, droid android and app
// android-coverage.
func filterFixture(t *testing.T) string {
	t.Helper()
	g := gradleFixture(t)
	writeFile(t, g, "settings.gradle.kts", "rootProject.name = \"shop\"\ninclude(\":app\", \":api\", \":pricing\", \":libs:model\", \":bom\", \":catalog\", \":plain\", \":droid\")\n")
	writeFile(t, g, "plain/build.gradle.kts", "plugins {\n    `java-library`\n}\n")
	writeFile(t, g, "plain/src/test/java/com/acme/plain/PlainTest.java", "class PlainTest {}\n")
	writeFile(t, g, "droid/build.gradle.kts", "plugins {\n    alias(libs.plugins.android.library)\n}\n")
	writeFile(t, g, "droid/src/test/kotlin/com/acme/droid/DroidTest.kt", "class DroidTest\n")
	return g
}

func runFiltered(t *testing.T, root, rel, filter string) (string, map[string]any, error) {
	t.Helper()
	var params job.Params
	if filter != "" {
		params = job.Params{testFilterFlag: json.RawMessage(`"` + filter + `"`)}
	}
	return runProjectJob(t, root, rel, TestJob(stack(t)), params, "--report-dir", filepath.Join(root, ".putnami/out", rel))
}

func TestTheTestFilterReachesEachGradleVariantAndMaven(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "the-test-filter-reaches-each-gradle-variant-and-maven")
	g := filterFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	for rel, variant := range map[string]string{"plain": VariantGradle, "api": VariantGradleJaCoCo, "droid": VariantAndroid, "app": VariantAndroidCoverage} {
		if got := metadata(t, probe(t, g, rel)[rel]).Variant; got != variant {
			t.Fatalf("%s is variant %s, want %s", rel, got, variant)
		}
	}
	// The init script and the property are global options, after the last
	// task the step names: a task option such as --tests there would apply
	// to the report task of gradle-jacoco and android-coverage.
	for rel, tasks := range map[string]string{
		"plain": ":plain:test",
		"api":   ":api:test :api:jacocoTestReport",
		"droid": ":droid:testDebugUnitTest",
		"app":   ":app:testDebugUnitTest :app:createDebugUnitTestCoverageReport",
	} {
		_ = os.Remove(log)
		if status, _, err := runFiltered(t, g, rel, "com.acme.QuotesTest.labels total"); status != "OK" || err != nil {
			t.Fatalf("%s test: %s %v\n%s", rel, status, err, ran(t, log))
		}
		script := filepath.Join(g, ".putnami/out", rel, stacks.TaskTest, gradleInitScriptName)
		want := filepath.Join(g, rel) + ": gradlew --console=plain " + tasks + " --init-script " + script + " -Dputnami.testFilter=com.acme.QuotesTest.labels total\n"
		if got := ran(t, log); got != want {
			t.Errorf("%s ran\n%s\nwant\n%s", rel, got, want)
		}
		if got := ran(t, script); got != gradleInitScript {
			t.Errorf("%s: the init script holds %q", rel, got)
		}
	}
	// Without a filter, the steps run as the profile states them.
	_ = os.Remove(log)
	if status, _, err := runFiltered(t, g, "api", ""); status != "OK" || err != nil || ran(t, log) != filepath.Join(g, "api")+": gradlew --console=plain :api:test :api:jacocoTestReport\n" {
		t.Errorf("no filter: %s %v\n%s", status, err, ran(t, log))
	}

	m := mavenFixture(t)
	log = stubTools(t, m, "21.0.4", junitPass)
	if status, _, err := runFiltered(t, m, "pricing", "PricingTest#adds*"); status != "OK" || err != nil {
		t.Fatalf("maven test: %s %v", status, err)
	}
	base := filepath.Join(m, "pricing") + ": mvn --batch-mode --file " + m + "/pom.xml --projects com.acme:pricing "
	if got, want := ran(t, log), base+"clean\n"+base+"--also-make verify -Dtest=PricingTest#adds* -Dsurefire.failIfNoSpecifiedTests=false -DskipITs\n"; got != want {
		t.Errorf("maven ran\n%s\nwant\n%s", got, want)
	}

	data, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	if flag := manifest.Commands["test"].Flags[testFilterFlag]; flag.Short != "-t" || flag.Type != "string" {
		t.Errorf("the test flag = %+v", flag)
	}
	if input := manifest.Tasks["test-run"].Inputs[testFilterFlag]; input.From != "params" {
		t.Errorf("the %s parameter is no input of the task's key: %+v", testFilterFlag, input)
	}
	if input, kept := manifest.Tasks["test-run"].Inputs["t"]; kept {
		t.Errorf("the task keys on the short name: %+v", input)
	}
}

func TestTheGradleInitScriptAppliesTheFilterToEveryTestTask(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "the-gradle-init-script-applies-the-filter-to-every-test-task")
	// The filter reaches the script as a system property, never as script
	// text, and the provider API makes it a configuration cache input.
	for _, line := range []string{
		"def putnamiTestFilter = providers.systemProperty('putnami.testFilter').getOrNull()",
		"if (putnamiTestFilter) {",
		"tasks.withType(AbstractTestTask).configureEach { task ->",
		"task.setTestNameIncludePatterns([putnamiTestFilter])",
		"task.filter.failOnNoMatchingTests = false",
	} {
		if !strings.Contains(gradleInitScript, "    "+line+"\n") {
			t.Errorf("the init script has no line %q:\n%s", line, gradleInitScript)
		}
	}
	if !strings.HasPrefix(gradleInitScript, "// Written by @putnami/java") || strings.Count(gradleInitScript, "{") != strings.Count(gradleInitScript, "}") {
		t.Errorf("the init script:\n%s", gradleInitScript)
	}

	out := t.TempDir()
	selected := stacks.RootStep{Output: out}
	filter := func(value string) *pctx.Context {
		return &pctx.Context{Params: job.Params{testFilterFlag: json.RawMessage(value)}}
	}
	script := filepath.Join(out, gradleInitScriptName)
	gradle := []string{"--init-script", script, "-Dputnami.testFilter=two words 'and' $quotes"}
	maven := []string{"-Dtest=two words 'and' $quotes", "-Dsurefire.failIfNoSpecifiedTests=false", "-DskipITs"}
	for program, want := range map[string][]string{
		"/repo/gradlew":   gradle,
		"gradle":          gradle,
		"gradlew.bat":     gradle,
		"mvn":             maven,
		"/repo/mvnw":      maven,
		"mvnd.cmd":        maven,
		"/repo/run-tests": nil,
	} {
		if got := testFilter(filter(`"two words 'and' $quotes"`), selected, []string{program, "x"}); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", program, got, want)
		}
	}
	// The CLI names the value after the long flag, so the short name is no
	// parameter the job reads.
	if got := testFilter(&pctx.Context{Params: job.Params{"t": json.RawMessage(`"x"`)}}, selected, []string{"gradle"}); got != nil {
		t.Errorf("the job read the short name: %q", got)
	}
	if got := testFilter(filter(`""`), selected, []string{"gradle"}); got != nil {
		t.Errorf("an empty filter: %q", got)
	}
}

func TestAComponentInWhichTheFilterSelectsNoTestPasses(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "a-component-in-which-the-filter-selects-no-test-passes")
	g := filterFixture(t)
	stubTools(t, g, "21.0.4", junitPass)
	for _, rel := range []string{"plain", "api", "droid", "app"} {
		status, data, err := runFiltered(t, g, rel, "NoSuch")
		if status != "OK" || err != nil || data["testSummary"].(map[string]any)["total"] != 0 || data["coverageSummary"] != nil {
			t.Errorf("%s: %s %v %v", rel, status, err, data)
		}
	}
	m := mavenFixture(t)
	stubTools(t, m, "21.0.4", junitPass)
	if status, data, err := runFiltered(t, m, "pricing", "NoSuch"); status != "OK" || err != nil || data["testSummary"].(map[string]any)["total"] != 0 {
		t.Errorf("maven: %s %v %v", status, err, data)
	}
	// An unfiltered run that writes no report still fails.
	writeFile(t, g, "gradlew", "#!/bin/sh\nexit 0\n")
	if status, _, err := runFiltered(t, g, "api", ""); status != "FAILED" || err == nil {
		t.Errorf("an unfiltered run without a report: %s %v", status, err)
	}
	// So does a filtered step that fails.
	writeFile(t, g, "gradlew", "#!/bin/sh\nexit 1\n")
	if status, _, err := runFiltered(t, g, "api", "NoSuch"); status != "FAILED" || err == nil || err.Error() != stacks.CodeStepFailed {
		t.Errorf("a failed filtered step: %s %v", status, err)
	}
}
