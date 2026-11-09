package ruby

import (
	"path/filepath"
	"regexp"
	"slices"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

const (
	// testFilterFlag is the test command's flag that selects the tests to run.
	testFilterFlag = "test"
	// testFilterShort is the name the CLI gives the flag's value when the
	// command line spells it -t.
	testFilterShort = "t"
)

// TestJob is the job of the test task: the profile's steps, with the test
// flag passed to the test runner as its filter.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunWith(stacks.TaskTest, testFilter)
}

// testFilter answers the filter arguments of the last test step: -e with the
// value for RSpec, which selects the examples whose description holds it, and
// -n with the value quoted as a regular expression for rails test. A step of
// another runner, which an override names, gets no argument.
func testFilter(ctx *pctx.Context, _ stacks.RootStep, lastStep []string) []string {
	filter := ctx.Params.String(testFilterFlag, testFilterShort)
	if filter == "" {
		return nil
	}
	programs := make([]string, 0, len(lastStep))
	for _, arg := range lastStep {
		programs = append(programs, filepath.Base(arg))
	}
	switch {
	case slices.Contains(programs, "rspec"):
		return []string{"-e", filter}
	case slices.Contains(programs, "rails") && slices.Contains(programs, "test"):
		return []string{"-n", "/" + regexp.QuoteMeta(filter) + "/"}
	}
	return nil
}
