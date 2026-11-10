package rust

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
	"go.putnami.dev/tooling/stacks/stackstest"
)

func TestTestPassesTheFilterToNextestAndKeysTheTaskOnIt(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "test-passes-the-filter-to-nextest-and-keys-the-task-on-it")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	params := job.Params{testFilterFlag: json.RawMessage(`"tests::lines"`)}
	out := filepath.Join(root, ".putnami/out/crates_api/test/test")
	run := func() string {
		t.Helper()
		_ = os.Remove(log)
		status, _, _, emitted, err := runJobParams(t, root, "crates/api", stacks.TaskTest, params)
		if status != "OK" || err != nil {
			t.Fatalf("test: %s %v\n%s", status, err, emitted)
		}
		ran, _ := os.ReadFile(log)
		return string(ran)
	}
	if ran := run(); !strings.Contains(ran, "cargo llvm-cov nextest --package api --profile ci --lcov --output-path "+out+"/lcov.info -E test(~tests::lines) --no-tests=pass\n") {
		t.Errorf("with coverage, test ran\n%s", ran)
	}
	bin := filepath.Dir(log)
	original, err := os.ReadFile(filepath.Join(bin, "cargo"))
	if err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, bin, "cargo", strings.Replace(string(original), "#!/bin/sh\n", "#!/bin/sh\ncase \"$*\" in *show-env*) exit 1;; esac\n", 1))
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ran := run(); !strings.Contains(ran, "cargo nextest run --package api --profile ci -E test(~tests::lines) --no-tests=pass\n") {
		t.Errorf("without coverage, test ran\n%s", ran)
	}
	t.Setenv("STUB_NEXTEST_WITHOUT_NO_TESTS", "1")
	if ran := run(); !strings.Contains(ran, "cargo nextest run --package api --profile ci -E test(~tests::lines)\n") {
		t.Errorf("a nextest without --no-tests, test ran\n%s", ran)
	}
	t.Setenv("STUB_NEXTEST_WITHOUT_NO_TESTS", "")
	params = nil
	if ran := run(); strings.Contains(ran, " -E ") || strings.Contains(ran, "--help") {
		t.Errorf("no filter, test ran\n%s", ran)
	}

	selected := stacks.RootStep{Dir: root, Project: root, Env: os.Environ(), Program: func(name string) string { return name }}
	step := []string{"cargo", "nextest", "run", "--package", "api"}
	filter := func(value string, step []string) []string {
		return testFilter(&pctx.Context{Params: job.Params{testFilterFlag: json.RawMessage(value)}}, selected, step)
	}
	if got := filter(`"a)b,c\\d"`, step); !slices.Equal(got, []string{"-E", `test(~a\)b\,c\\d)`, "--no-tests=pass"}) {
		t.Errorf("the value is one escaped name matcher: %q", got)
	}
	if got := filter(`"-x"`, step); !slices.Equal(got[:2], []string{"-E", "test(~-x)"}) {
		t.Errorf("a value that starts with a dash is no option: %q", got)
	}
	// The CLI names the value after the long flag, so the short name is no
	// parameter the job reads.
	if got := testFilter(&pctx.Context{Params: job.Params{"t": json.RawMessage(`"x"`)}}, selected, step); got != nil {
		t.Errorf("the job read the short name: %q", got)
	}
	// An override that runs another runner gets no argument.
	if got := filter(`"x"`, []string{"cargo", "test", "--package", "api"}); got != nil {
		t.Errorf("a cargo test step: %q", got)
	}
	if got := filter(`"x"`, []string{"/opt/bin/cargo-nextest", "nextest", "run"}); len(got) == 0 {
		t.Errorf("a cargo-nextest binary runs nextest: %q", got)
	}

	data, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	if flag := manifest.Commands["test"].Flags[testFilterFlag]; flag.Short != "-t" || flag.Type != "string" || !strings.Contains(flag.Description, "passed to nextest as") {
		t.Errorf("the test flag = %+v", flag)
	}
	if input := manifest.Tasks["test-run"].Inputs[testFilterFlag]; input.From != "params" {
		t.Errorf("the %s parameter is no input of the task's key: %+v", testFilterFlag, input)
	}
	if input, kept := manifest.Tasks["test-run"].Inputs["t"]; kept {
		t.Errorf("the task keys on the short name: %+v", input)
	}
}
