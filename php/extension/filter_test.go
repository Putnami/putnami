package php

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

func TestTestPassesTheFilterToPHPUnitAndKeysTheTaskOnIt(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "test-passes-the-filter-to-phpunit-and-keys-the-task-on-it")
	root := fixture(t)
	log := stubTools(t, root, "8.4.10", 0)
	params := job.Params{testFilterFlag: json.RawMessage(`"Calculator::adds two"`)}
	run := func() string {
		t.Helper()
		_ = os.Remove(log)
		status, _, err := runProjectJob(t, root, "packages/pricing", TestJob(stack(t)), params, "--report-dir", filepath.Join(root, "out"))
		if status != "OK" || err != nil {
			t.Fatalf("test: %s %v", status, err)
		}
		ran, _ := os.ReadFile(log)
		return string(ran)
	}
	if ran := run(); !strings.Contains(ran, "--coverage-filter src tests --filter Calculator::adds two --do-not-fail-on-empty-test-suite\n") {
		t.Errorf("with coverage, test ran\n%s", ran)
	}
	t.Setenv("STUB_NO_COVERAGE_DRIVER", "1")
	if ran := run(); !strings.Contains(ran, "--no-coverage tests --filter Calculator::adds two --do-not-fail-on-empty-test-suite\n") {
		t.Errorf("without coverage, test ran\n%s", ran)
	}
	t.Setenv("STUB_PHPUNIT_WITHOUT_EMPTY_SUITE_OPTION", "1")
	if ran := run(); !strings.Contains(ran, "--no-coverage tests --filter Calculator::adds two\n") {
		t.Errorf("a PHPUnit without the option, test ran\n%s", ran)
	}
	t.Setenv("STUB_PHPUNIT_WITHOUT_EMPTY_SUITE_OPTION", "")
	params = nil
	if ran := run(); strings.Contains(ran, "--filter ") {
		t.Errorf("no filter, test ran\n%s", ran)
	}
	selected := stacks.RootStep{Dir: root, Project: root, Env: os.Environ(), Program: func(name string) string { return name }}
	step := []string{filepath.Join(root, "vendor/bin/phpunit"), "--log-junit", "junit.xml", "tests"}
	for _, name := range []string{testFilterFlag, testFilterShort} {
		got := testFilter(&pctx.Context{Params: job.Params{name: json.RawMessage(`"two words"`)}}, selected, step)
		if !slices.Equal(got, []string{"--filter", "two words", emptySuiteOption}) {
			t.Errorf("the value of %s is one argument: %q", name, got)
		}
	}

	// An overridden step answers for its own program, wherever it stands.
	writeFile(t, root, "tools/phpunit", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(root, "tools/phpunit"), 0o755); err != nil {
		t.Fatal(err)
	}
	override := &pctx.Context{Params: job.Params{testFilterFlag: json.RawMessage(`"x"`)}}
	if got := testFilter(override, selected, []string{filepath.Join(root, "tools/phpunit")}); !slices.Equal(got, []string{"--filter", "x"}) {
		t.Errorf("a program whose help lists no option: %q", got)
	}
	writeFile(t, root, "tools/phpunit", "#!/bin/sh\necho "+emptySuiteOption+"\n")
	if err := os.Remove(filepath.Join(root, "vendor/bin/phpunit")); err != nil {
		t.Fatal(err)
	}
	if got := testFilter(override, selected, []string{filepath.Join(root, "tools/phpunit")}); !slices.Equal(got, []string{"--filter", "x", emptySuiteOption}) {
		t.Errorf("a program outside vendor/bin that lists the option: %q", got)
	}

	data, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	if flag := manifest.Commands["test"].Flags[testFilterFlag]; flag.Short != "-t" {
		t.Errorf("the test flag = %+v", flag)
	}
	for _, name := range []string{testFilterFlag, testFilterShort} {
		if input := manifest.Tasks["test-run"].Inputs[name]; input.From != "params" {
			t.Errorf("the %s parameter is no input of the task's key: %+v", name, input)
		}
	}
}
