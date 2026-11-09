package php

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"time"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

const (
	// testFilterFlag is the test command's flag that selects the tests to run.
	// The CLI names the value after the long flag, whichever spelling the
	// command line used, so -t reaches the job as test too.
	testFilterFlag = "test"
	// emptySuiteOption makes PHPUnit pass a run that selects no test.
	emptySuiteOption = "--do-not-fail-on-empty-test-suite"
	// stepHelpTimeout bounds the question of what a step's program accepts.
	stepHelpTimeout = 30 * time.Second
)

// TestJob is the job of the test task: the profile's steps, with the test
// flag passed to PHPUnit as its filter.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunWith(stacks.TaskTest, testFilter)
}

// testFilter answers PHPUnit's filter arguments, one argument for the whole
// value, or none without a filter. The filter also reaches the projects the
// selected ones depend on, where it can select no test: a PHPUnit that fails
// such a run by default offers the option that passes it.
func testFilter(ctx *pctx.Context, root stacks.RootStep, lastStep []string) []string {
	filter := ctx.Params.String(testFilterFlag)
	if filter == "" {
		return nil
	}
	args := []string{"--filter", filter}
	if stepAccepts(root, lastStep, emptySuiteOption) {
		args = append(args, emptySuiteOption)
	}
	return args
}

// stepAccepts reports whether the program of step lists option in its help,
// asked with the step's own arguments. A program that does not answer accepts
// nothing.
func stepAccepts(root stacks.RootStep, step []string, option string) bool {
	if len(step) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), stepHelpTimeout)
	defer cancel()
	args := append(slices.Clone(step[1:]), "--help")
	cmd := exec.CommandContext(ctx, root.Program(step[0]), args...) //nolint:gosec // the profile's own step
	cmd.Dir = root.Project
	cmd.Env = root.Env
	help, err := cmd.Output()
	return err == nil && bytes.Contains(help, []byte(option))
}
