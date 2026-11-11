package lint

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// recordedESLint is one ESLint run a test observed.
type recordedESLint struct {
	program string
	args    []string
}

// fakeESLint replaces the subprocess with one that answers result, and
// records each run.
func fakeESLint(t *testing.T, result *exec.Result) *[]recordedESLint {
	t.Helper()
	var runs []recordedESLint
	t.Cleanup(SetExecRunForTesting(func(program string, args []string, _ ...exec.Option) (*exec.Result, error) {
		runs = append(runs, recordedESLint{program: program, args: append([]string(nil), args...)})
		return result, nil
	}))
	return &runs
}

func eslintViolationJSON(file string) string {
	return `[{"filePath":` + quote(file) + `,"messages":[{"ruleId":"prefer-const","severity":2,"message":"'x' is never reassigned. Use 'const' instead.","line":1,"column":5}],"errorCount":1,"warningCount":0}]`
}

func TestESLint_AViolationFailsWithFileLineAndRule(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-runs-eslint", "an-eslint-violation-fails-with-file-line-and-rule")
	root := t.TempDir()
	project := filepath.Join(root, "packages", "app")
	writeProjectFile(t, project, "src/a.js", "let x = 1;\n")
	runs := fakeESLint(t, &exec.Result{Success: false, ExitCode: 1, Stdout: eslintViolationJSON(filepath.Join(project, "src", "a.js"))})

	report, ok, err := ESLint("/ws/node_modules/.bin/eslint", "/usr/bin/node", project, root, false, "warn")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ESLint() ok = true, want a failure for an error")
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", report.Diagnostics)
	}
	d := report.Diagnostics[0]
	if d.File != "packages/app/src/a.js" || d.Line != 1 || d.Column != 5 || d.Category != "prefer-const" || d.Severity != "error" {
		t.Errorf("diagnostic = %+v, want packages/app/src/a.js:1:5 prefer-const error", d)
	}
	if len(*runs) != 1 {
		t.Fatalf("runs = %+v, want one", *runs)
	}
	args := (*runs)[0].args
	for _, want := range []string{"--format", "json", "--no-error-on-unmatched-pattern", "."} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v miss %q", args, want)
		}
	}

	// A clean run passes.
	fakeESLint(t, &exec.Result{Success: true, Stdout: "[]"})
	if _, ok, err := ESLint("/ws/node_modules/.bin/eslint", "/usr/bin/node", project, root, false, "warn"); err != nil || !ok {
		t.Errorf("clean ESLint() = %v, %v, want a pass", ok, err)
	}
}

func TestESLint_FixAppliesFixesAndFixFalseOnlyReports(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-runs-eslint", "fix-applies-eslint-fixes-and-fix-false-only-reports")
	project := t.TempDir()
	runs := fakeESLint(t, &exec.Result{Success: true, Stdout: "[]"})

	if _, _, err := ESLint("eslint", "node", project, "", true, "warn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ESLint("eslint", "node", project, "", false, "error"); err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(*runs))
	}
	if !slices.Contains((*runs)[0].args, "--fix") {
		t.Errorf("fix run args %v miss --fix", (*runs)[0].args)
	}
	if slices.Contains((*runs)[1].args, "--fix") {
		t.Errorf("report-only run args %v hold --fix", (*runs)[1].args)
	}
	if !slices.Contains((*runs)[1].args, "--quiet") || slices.Contains((*runs)[0].args, "--quiet") {
		t.Errorf("only the error diagnostic level passes --quiet: %v / %v", (*runs)[0].args, (*runs)[1].args)
	}
}

func TestESLint_SkipsNestedProjects(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-runs-eslint", "eslint-skips-nested-projects")
	project := t.TempDir()
	writeProjectFile(t, project, "src/a.ts", "export {};\n")
	writeProjectFile(t, project, "packages/inner/putnami.json", "{}\n")
	writeProjectFile(t, project, "packages/inner/deeper/putnami.json", "{}\n")
	writeProjectFile(t, project, "node_modules/dep/putnami.json", "{}\n")
	runs := fakeESLint(t, &exec.Result{Success: true, Stdout: "[]"})

	if _, _, err := ESLint("eslint", "node", project, "", false, "warn"); err != nil {
		t.Fatal(err)
	}
	args := strings.Join((*runs)[0].args, " ")
	for _, want := range []string{"--ignore-pattern packages/inner/**", "--ignore-pattern .gen/**"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q miss %q", args, want)
		}
	}
	for _, unwanted := range []string{"deeper", "node_modules/dep"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("args %q name %q", args, unwanted)
		}
	}
}

func TestESLint_ACrashReportsStderr(t *testing.T) {
	project := t.TempDir()
	fakeESLint(t, &exec.Result{Success: false, ExitCode: 2, Stderr: "Oops! Something went wrong! ESLint couldn't find a configuration file."})
	_, ok, err := ESLint("eslint", "node", project, "", false, "warn")
	if err == nil || ok || !strings.Contains(err.Error(), "couldn't find a configuration file") || !strings.Contains(err.Error(), "exit 2") {
		t.Errorf("ESLint() = %v, %v, want the failure with ESLint's stderr", ok, err)
	}
}
