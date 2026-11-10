package java

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

const (
	// testFilterFlag is the test command's flag that selects the tests to run.
	// The CLI names the value after the long flag, whichever spelling the
	// command line used, so -t reaches the job as test too.
	testFilterFlag = "test"
	// gradleFilterProperty is the system property the init script reads the
	// filter from.
	gradleFilterProperty = "putnami.testFilter"
	// gradleInitScriptName is the file name of the init script in the task's
	// output directory.
	gradleInitScriptName = "putnami-test-filter.gradle"
)

// gradleInitScript applies the filter of the system property
// gradleFilterProperty to every test task of the build, as --tests applies it
// to one: the task runs the tests its own filter and this one both select.
// A task in which the filter selects no test passes. A build without the
// property is left as it is. The property is read through the provider API,
// so the configuration cache keys its entries on it.
const gradleInitScript = `// Written by @putnami/java for putnami test --test <filter>.
allprojects {
    def putnamiTestFilter = providers.systemProperty('` + gradleFilterProperty + `').getOrNull()
    if (putnamiTestFilter) {
        tasks.withType(AbstractTestTask).configureEach { task ->
            task.setTestNameIncludePatterns([putnamiTestFilter])
            task.filter.failOnNoMatchingTests = false
        }
    }
}
`

// TestJob is the job of the test task: the profile's steps, with the test
// flag passed to Gradle or Maven as its test filter. A component in which the
// filter selects no test passes with zero tests.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunFiltered(stacks.TaskTest, testFilter)
}

// testFilter answers the filter arguments of the task's last step, by its
// program:
//
//   - Gradle (gradlew or gradle) gets the init script, written into the
//     task's output directory, and the filter as the system property it
//     reads. Both are global options, which Gradle reads wherever they stand,
//     so they reach the test task of a step that runs a report task after it.
//   - Maven (mvn, mvnw or mvnd) gets -Dtest for Surefire, with the switch that
//     passes a module in which it selects no test, and -DskipITs, so Failsafe
//     runs no integration test.
//
// Any other program, which an override names, gets no filter.
func testFilter(ctx *pctx.Context, root stacks.RootStep, lastStep []string) []string {
	filter := ctx.Params.String(testFilterFlag)
	if filter == "" || len(lastStep) == 0 {
		return nil
	}
	switch program := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(lastStep[0]), ".bat"), ".cmd"); program {
	case "gradlew", "gradle":
		script := filepath.Join(root.Output, gradleInitScriptName)
		// A script that cannot be written fails the step: Gradle names the
		// init script it cannot find.
		_ = os.WriteFile(script, []byte(gradleInitScript), 0o600)
		return []string{"--init-script", script, "-D" + gradleFilterProperty + "=" + filter}
	case "mvn", "mvnw", "mvnd":
		return []string{"-Dtest=" + filter, "-Dsurefire.failIfNoSpecifiedTests=false", "-DskipITs"}
	}
	return nil
}
