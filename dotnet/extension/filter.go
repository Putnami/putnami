package dotnet

import (
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks"
)

// testFilterFlag is the test command's flag that selects the tests to run. The
// CLI names the value after the long flag, whichever spelling the command line
// used, so -t reaches the job as test too.
const testFilterFlag = "test"

// TestJob is the job of the test task: the profile's steps, with the test flag
// passed to dotnet test as its filter. The filter also reaches the projects the
// selected ones depend on, where it may select no test: such a run passes.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunFiltered(stacks.TaskTest, testFilter)
}

// testFilter answers dotnet test's filter arguments, one argument for the whole
// value, or none without a filter. A bare value selects the tests whose fully
// qualified name contains it.
func testFilter(ctx *pctx.Context, _ stacks.RootStep, _ []string) []string {
	filter := ctx.Params.String(testFilterFlag)
	if filter == "" {
		return nil
	}
	return []string{"--filter", filter}
}
