package swift

import (
	"os"

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
// flag passed to the last step as --filter when that step is the swiftpm-test
// or xcodebuild command, which hand it to swift test --filter and xcodebuild
// -only-testing. An override whose last step is another program gets no
// filter: its arguments are its own.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunWith(stacks.TaskTest, func(ctx *pctx.Context, _ stacks.RootStep, lastStep []string) []string {
		filter := ctx.Params.String(testFilterFlag, testFilterShort)
		if filter == "" || !readsFilter(stack, lastStep) {
			return nil
		}
		return []string{"--filter", filter}
	})
}

// readsFilter reports whether step runs a runtime command that reads
// --filter.
func readsFilter(stack stacks.Stack, step []string) bool {
	if len(step) < 2 || (step[1] != CommandSwiftPMTest && step[1] != CommandXcodebuild) {
		return false
	}
	runtime := stack.Runtime
	if runtime == "" {
		runtime, _ = os.Executable()
	}
	return step[0] == runtime
}
