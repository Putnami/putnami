package pants

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// batchResult is what the test reads of one project's result in a batch.
type batchResult struct {
	ProjectID   string         `json:"projectId"`
	Status      string         `json:"status"`
	Data        map[string]any `json:"data"`
	Diagnostics []struct {
		Category    string `json:"category"`
		Description string `json:"description"`
	} `json:"diagnostics"`
}

// runBatchJob runs a task job of the stack as the leader of a batch over the
// packages at dirs, the way the CLI does, and returns each package's result
// by its directory.
func runBatchJob(t *testing.T, root, task string, dirs ...string) map[string]batchResult {
	t.Helper()
	s := stack(t)
	probed := map[string]json.RawMessage{}
	for _, p := range probe(t, root).Projects {
		probed[p.Path] = p.Metadata
	}
	var selected []pctx.ProjectRef
	for _, dir := range dirs {
		selected = append(selected, pctx.ProjectRef{ID: "/" + dir, Name: dir, Path: dir, FullPath: filepath.Join(root, filepath.FromSlash(dir)), OutputPath: filepath.Join(root, ".putnami/out", dir)})
	}
	ctx := &pctx.Context{WorkspaceRoot: root, SelectedProjects: selected}
	ctx.Project.Path = dirs[0]
	ctx.Project.FullPath = selected[0].FullPath
	ctx.Project.Metadata = map[string]json.RawMessage{ExtensionName: probed[dirs[0]]}
	status, data, err := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", selected[0].OutputPath})
	if status != "OK" || err != nil {
		t.Fatalf("batch status %s: %v", status, err)
	}
	raw, _ := json.Marshal(data["batchResults"])
	var results []batchResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("batch results %s: %v", raw, err)
	}
	out := map[string]batchResult{}
	for _, r := range results {
		out[strings.TrimPrefix(r.ProjectID, "/")] = r
	}
	if len(out) != len(dirs) {
		t.Fatalf("batch results = %s, want one per package", raw)
	}
	return out
}

// stubBatchPants puts first on PATH a pants that prints its version, logs
// each call with its working directory to $STUB_LOG, and for test writes the
// demo's JUnit reports of each package directory it names and its coverage
// report, prints $STUB_OUTPUT and exits with STUB_EXIT.
func stubBatchPants(t *testing.T, d demo) {
	t.Helper()
	bin, reports := t.TempDir(), t.TempDir()
	for rel, content := range d["ci"] {
		stackstest.WriteFile(t, reports, rel, content)
	}
	stackstest.WriteFile(t, bin, "pants", `#!/bin/sh
echo "$(pwd) pants $*" >> "$STUB_LOG"
case "$1" in --version) echo 2.25.0; exit 0;; esac
case " $* " in *" test "*)
  mkdir -p dist/test/reports dist/coverage/python
  for a in "$@"; do
    case "$a" in *:) ;; *) continue;; esac
    prefix="$(echo "${a%:}" | tr / .)."
    for f in "`+reports+`"/dist/test/reports/"$prefix"*; do [ -e "$f" ] && cp "$f" dist/test/reports/; done
  done
  cp "`+reports+`/dist/coverage/python/coverage.xml" dist/coverage/python/coverage.xml;;
esac
printf '%s\n' "$STUB_OUTPUT"
exit "${STUB_EXIT:-0}"
`)
	if err := os.Chmod(filepath.Join(bin, "pants"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

func TestAVerbRunsOnePantsGoalOverItsPackages(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-pants-command", "a-verb-runs-one-pants-goal-over-its-packages")
	d := readDemo(t)
	stubBatchPants(t, d)
	root := repository(t, d, true)
	rootReal, _ := filepath.EvalSymlinks(root)
	for task, want := range map[string]string{
		stacks.TaskLint:  "pants lint src/python/pricing: src/python/web:",
		stacks.TaskBuild: "pants package src/python/pricing: src/python/web:",
		stacks.TaskTest:  `pants --test-report --test-use-coverage --coverage-py-report=["xml"] --test-show-rerun-command test src/python/pricing: src/python/web:`,
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		results := runBatchJob(t, root, task, "src/python/pricing", "src/python/web")
		if log := stubLog(t); len(log) != 2 || !strings.HasSuffix(log[0], " pants --version") || log[1] != rootReal+" "+want {
			t.Errorf("%s ran %q, want the version check then %q", task, log, want)
		}
		for dir, r := range results {
			if r.Status != "OK" || r.Data["delegatedTo"] != "pants" {
				t.Errorf("%s %s = %+v, want a pass delegated to pants", task, dir, r)
			}
		}
		if task != stacks.TaskTest {
			continue
		}
		// Each package reads the JUnit reports of its own addresses, and the
		// coverage of its own sources.
		for dir, want := range map[string]float64{"src/python/pricing": 4, "src/python/web": 1} {
			if got := results[dir].Data["testSummary"].(map[string]any); got["total"] != want {
				t.Errorf("%s testSummary = %v, want its own %v cases", dir, got, want)
			}
		}
		if got := results["src/python/pricing"].Data["coverageSummary"].(map[string]any); got["covered"] != float64(17) || got["total"] != float64(17) {
			t.Errorf("pricing coverageSummary = %v, want its own 17 lines", got)
		}
	}
}

func TestAFailingTestTargetFailsItsPackageAlone(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-pants-command", "a-failing-test-target-fails-its-package-alone")
	d := readDemo(t)
	stubBatchPants(t, d)
	root := repository(t, d, true)
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_OUTPUT", "\x1b[32m✓\x1b[0m src/python/pricing/pricing_test.py:tests succeeded in 0.42s (cached locally).\n"+
		"\x1b[31m✕\x1b[0m src/python/web/pages_test.py:tests failed in 0.51s.\n\nTo rerun the failing tests, use:\n\n    pants test src/python/web/pages_test.py:tests\n")
	results := runBatchJob(t, root, stacks.TaskTest, "src/python/pricing", "src/python/web")
	web := results["src/python/web"]
	if web.Status != "FAILED" || len(web.Diagnostics) == 0 || web.Diagnostics[0].Description != "pants test: ✕ src/python/web/pages_test.py:tests failed in 0.51s." || web.Diagnostics[0].Category != stacks.CodeStepFailed {
		t.Errorf("web = %+v, want a failure with Pants's line", web)
	}
	if pricing := results["src/python/pricing"]; pricing.Status != "OK" || len(pricing.Diagnostics) != 0 {
		t.Errorf("pricing = %+v, want the pass Pants reported", pricing)
	}
}

func TestAPackagePantsDidNotReportFailsAsNotRun(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-pants-command", "a-package-pants-did-not-report-fails-as-not-run")
	d := readDemo(t)
	stubBatchPants(t, d)
	root := repository(t, d, true)
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_OUTPUT", "✕ src/python/web/pages_test.py:tests failed in 0.51s.\n\nTo rerun the failing tests, use:\n\n    pants test src/python/web/pages_test.py:tests\n")
	results := runBatchJob(t, root, stacks.TaskTest, "src/python/pricing", "src/python/web")
	if pricing := results["src/python/pricing"]; pricing.Status != "FAILED" || len(pricing.Diagnostics) == 0 || !strings.Contains(pricing.Diagnostics[0].Description, "did not run src/python/pricing") {
		t.Errorf("pricing = %+v, want a failure as not run", pricing)
	}
	// A failed lint names no package: each fails with the command's error.
	t.Setenv("STUB_OUTPUT", "✕ flake8 failed.\n✓ black succeeded.\n")
	for dir, r := range runBatchJob(t, root, stacks.TaskLint, "src/python/pricing", "src/python/web") {
		if r.Status != "FAILED" || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "exit status 1") {
			t.Errorf("lint %s = %+v, want the command's failure", dir, r)
		}
	}
}

func TestReadBatchReadsEachPackageFromPantsTestResults(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"src/a/t_test.py", "src/a/u_test.py", "src/a/b/t_test.py", "src/a/sub/s_test.py", "src/x/t_test.py"} {
		stackstest.WriteFile(t, root, f, "")
	}
	for _, tc := range []struct {
		name, output, want string
		// rerun lists the addresses of the rerun command, auto those of
		// the output's failed lines, none prints no rerun command.
		rerun string
	}{
		{"succeeded", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x/t_test.py:tests failed in 1.00s.", "passed", "auto"},
		{"cached", "✓ src/a/t_test.py:tests succeeded in 0.42s (cached locally).\n✕ src/x:t failed.", "passed", "auto"},
		{"after attempts", "+ src/a/t_test.py:tests succeeded after 2 attempts in 0.80s.\n✕ src/x:t failed.", "passed", "auto"},
		{"failed", "✕ src/a/t_test.py:tests failed in 0.42s.", "failed", "auto"},
		{"no elapsed time", "✓ src/a/t_test.py:tests succeeded .\n✕ src/x:t failed.", "passed", "auto"},
		{"no elapsed time, cached", "✓ src/a/t_test.py:tests succeeded  (cached locally).\n✕ src/x:t failed.", "passed", "auto"},
		{"failed with its exit code", "✕ src/a/t_test.py:tests failed (exit code 2) in 0.42s.", "failed", "auto"},
		{"one target of several failed", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/a/u_test.py:tests failed in 0.42s.", "failed", "auto"},
		{"a target address", "✕ src/a:tests failed in 0.42s.", "failed", "auto"},
		{"a generated target", "✕ src/a:tests#t_test.py failed in 0.42s.", "failed", "auto"},
		{"a parametrized target", "✕ src/a/t_test.py:tests@resolve=py failed in 0.42s.", "failed", "auto"},
		{"a default target of a file", "✕ src/a/t_test.py failed in 0.42s.", "failed", "auto"},
		{"a generated target of a default name", "✕ src/a#t failed in 0.42s.", "failed", "auto"},
		{"parameters of a default target", "✕ src/a/t_test.py@resolve=py failed in 0.42s.", "failed", "auto"},
		{"colored", "\x1b[31m✕\x1b[0m src/a/t_test.py:tests failed in 0.42s.", "failed", "auto"},
		{"not run", "✕ src/x/t_test.py:tests failed in 0.42s.", "not named", "auto"},
		{"a subdirectory", "✓ src/a/b/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "not named", "auto"},
		{"a batch of files", "✓ src/a/t_test.py:tests and 2 other files succeeded in 0.42s.\n✕ src/x:t failed.", "not named", "auto"},
		{"skipped", "- src/a/t_test.py:tests skipped.\n✕ src/x:t failed.", "not named", "auto"},
		{"a line of a tool", "✕ src/a failed to start.\n✕ src/x:t failed.", "not named", "src/x:t"},
		{"a failed batch of files", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/a/u_test.py:tests and 1 other files failed in 0.42s.", "failed", "src/a/u_test.py:tests src/x/t_test.py:tests"},
		{"a failed target with no line", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "failed", "src/a/u_test.py:tests src/x:t"},
		{"a quoted address", "✓ src/a/t_test.py:tests succeeded in 0.42s.", "failed", `'src/a/u_test.py:tests@py=3 x' 'src/x:it'"'"'s'`},
		{"a quoted address before another", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "failed", "'src/x:t' src/a/u_test.py:tests"},
		{"a file in a subdirectory of its target", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/a/sub/s_test.py:../tests failed in 0.42s.", "failed", "auto"},
		{"a file in a subdirectory of its target that passed", "✓ src/a/sub/s_test.py:../tests succeeded in 0.42s.\n✕ src/x:t failed.", "passed", "auto"},
		{"no rerun command", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "not named", "none"},
		{"a rerun of a target of no package", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "not named", "src/x:t src/z:t"},
		{"a rerun command with no address", "✓ src/a/t_test.py:tests succeeded in 0.42s.\n✕ src/x:t failed.", "not named", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := tc.output
			switch tc.rerun {
			case "none":
			case "-":
				output += "\n\nTo rerun the failing tests, use:\n\n    pants\n"
			case "auto":
				output += "\n\nTo rerun the failing tests, use:\n\n    pants test " + strings.Join(failedAddresses(tc.output), " ") + "\n"
			default:
				output += "\n\nTo rerun the failing tests, use:\n\n    pants test " + tc.rerun + "\n"
			}
			out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"src/a", "src/a/sub", "src/x"}, Failed: true, Output: []byte(output)})
			o, named := out["src/a"]
			got := "not named"
			switch {
			case named && o.Failed:
				got = "failed"
			case named:
				got = "passed"
			}
			if got != tc.want {
				t.Errorf("src/a is %s, want %s: %+v", got, tc.want, out)
			}
		})
	}
	failed := []byte("✕ src/a/t_test.py:tests failed in 0.42s.\n\nTo rerun the failing tests, use:\n\n    pants test src/a/t_test.py:tests\n")
	for _, report := range []stacks.BatchReport{
		{Task: stacks.TaskLint, Root: root, Components: []string{"src/a"}, Failed: true, Output: failed},
		{Task: stacks.TaskTest, Root: root, Components: []string{"src/a"}, Failed: false, Output: failed},
	} {
		if out := ReadBatch(report); out != nil {
			t.Errorf("%s, failed %v: answered %+v, want nothing", report.Task, report.Failed, out)
		}
	}
}

// failedAddresses are the addresses of the failed lines of an output, as
// Pants's rerun command names them.
func failedAddresses(output string) []string {
	var out []string
	for _, line := range strings.Split(ansi.ReplaceAllString(output, ""), "\n") {
		if m := testResult.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[4] == "failed" {
			out = append(out, m[2])
		}
	}
	return out
}

func TestPathSafeSpecIsTheNamePantsGivesAReport(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a/b/c.txt", "root.txt", "a/b/subdir/c.txt", "a/b/subdir/f.txt", "a/b/subdir/dir2/f.txt", "a/b/f.ext", "src/a/sub/x_test.py"} {
		stackstest.WriteFile(t, root, f, "")
	}
	// The cases of test_address_spec in Pants's address_test.py, each address
	// as Pants prints it, and two of this repository's own.
	for printed, want := range map[string]string{
		"a/b:b":                             "a.b",
		"a/b:c":                             "a.b.c",
		"//:root":                           ".root",
		"a/b#generated":                     "a.b@generated",
		"a/b#generated/f.ext":               "a.b@generated.f.ext",
		"a/b:generator#generated":           "a.b.generator@generated",
		"a/b:generator#generated/f.ext":     "a.b.generator@generated.f.ext",
		"//:root#generated":                 ".root@generated",
		"a/b/c.txt:c":                       "a.b.c.txt.c",
		"//root.txt:root":                   ".root.txt.root",
		"a/b/subdir/c.txt:../original":      "a.b.subdir.c.txt@original",
		"a/b/c.txt":                         "a.b.c.txt",
		"a/b/subdir/f.txt:../b":             "a.b.subdir.f.txt@b",
		"a/b/subdir/dir2/f.txt:../../b":     "a.b.subdir.dir2.f.txt@@b",
		"//:template@k1=v,k2=v":             ".template@@k1=v,k2=v",
		"a/b@k1=v,k2=v":                     "a.b@@k1=v,k2=v",
		"a/b#gen@k=v":                       "a.b@gen@@k=v",
		"a/b/f.ext@k=v":                     "a.b.f.ext@@k=v",
		"src/a/sub/x_test.py:../tests":      "src.a.sub.x_test.py@tests",
		"src/a/sub/x_test.py:../tests@py=3": "src.a.sub.x_test.py@tests@@py=3",
	} {
		if got, ok := pathSafeSpec(root, printed); !ok || got != want {
			t.Errorf("pathSafeSpec(%s) = %q %v, want %q", printed, got, ok, want)
		}
	}
	if got, ok := pathSafeSpec(root, "a/b/c.txt:../../../x"); ok {
		t.Errorf("an address above the root = %q", got)
	}
}

func TestReportOwnerNamesThePackageOfAJUnitReport(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"src/a/t_test.py", "src/a/b/t_test.py", "src/a/b/u_test.py", "src/c/t_test.py", "src/d/t_test.py"} {
		stackstest.WriteFile(t, root, f, "")
	}
	// src/a's target tests holds b/t_test.py, a file of the nested package
	// src/a/b, whose own target tests holds u_test.py.
	output := "✓ src/a/t_test.py:tests succeeded in 0.42s.\n" +
		"✓ src/a/b/t_test.py:../tests succeeded in 0.42s.\n" +
		"✓ src/a/b/u_test.py:tests succeeded in 0.42s.\n" +
		"✓ src/d/t_test.py:tests succeeded in 0.42s.\n" +
		"✓ src/a/b/t_test.py:../tests@py=3 succeeded in 0.42s.\n" +
		"✓ src/a/b/u_test.py:tests@py=3 succeeded in 0.42s.\n"
	components := []string{"src/a", "src/a/b", "src/c"}
	for file, want := range map[string]string{
		"dist/test/reports/src.a.t_test.py.tests.xml":                  "src/a",
		"dist/test/reports/src.a.b.t_test.py@tests.xml":                "src/a",
		"dist/test/reports/src.a.b.u_test.py.tests.xml":                "src/a/b",
		"dist/test/reports/src.a.b.t_test.py@tests@@py=3.xml":          "src/a",
		"dist/test/reports/src.a.b.u_test.py.tests@@py=3.xml":          "src/a/b",
		"dist/test/reports/src.c.t_test.py.tests.xml":                  "src/c",
		"dist/test/reports/src.c.xml":                                  "src/c",
		"dist/test/reports/src.a.other.xml":                            "src/a",
		"dist/test/reports/src.a.b.other.xml":                          "",
		"dist/test/reports/src.d.t_test.py.tests.xml":                  "",
		"dist/test/reports/src.ab.t_test.py.tests.xml":                 "",
		"dist/test/reports/batch-of-src.a.t_test.py.tests+1-files.xml": "",
		"dist/test/reports/sub/src.a.t_test.py.tests.xml":              "",
		"dist/test/reports/src.a.t_test.py.tests.json":                 "",
		"dist/coverage/python/coverage.xml":                            "",
		"elsewhere/dist/test/reports/src.a.t_test.py.tests.xml":        "",
	} {
		// In any order of the command.
		for _, order := range [][]string{components, {components[2], components[1], components[0]}} {
			report := stacks.BatchReport{Root: root, Components: order, Output: []byte(output)}
			owner, ok := ReportOwner(report)(file)
			if owner != want || ok != (want != "") {
				t.Errorf("ReportOwner(%s, %v) = %q %v, want %q", file, order, owner, ok, want)
			}
		}
	}
	// The rerun command names the addresses that failed.
	report := stacks.BatchReport{Root: root, Components: components, Output: []byte("\nTo rerun the failing tests, use:\n\n    pants test src/a/b/t_test.py:../tests\n")}
	if owner, ok := ReportOwner(report)("dist/test/reports/src.a.b.t_test.py@tests.xml"); owner != "src/a" || !ok {
		t.Errorf("a file the rerun command names = %q %v, want src/a", owner, ok)
	}
}
