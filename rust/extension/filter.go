package rust

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
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
	// noTestsOption is nextest's option for a run that selects no test.
	noTestsOption = "--no-tests"
	// nextestHelpTimeout bounds the question of what the nextest that runs
	// accepts.
	nextestHelpTimeout = 30 * time.Second
)

// filtersetEscapes are the characters a nextest name matcher reads only
// escaped.
var filtersetEscapes = strings.NewReplacer(`\`, `\\`, `)`, `\)`, `,`, `\,`, "\n", `\n`, "\r", `\r`, "\t", `\t`)

// TestJob is the job of the test task: the profile's steps, with the test flag
// passed to nextest as its filter.
func TestJob(stack stacks.Stack) cli.JobFunc {
	return stack.RunWith(stacks.TaskTest, testFilter)
}

// testFilter answers nextest's filter arguments, with or without coverage: -E
// with the filterset that selects the tests whose name contains the value,
// then --no-tests=pass when the nextest that runs accepts it. The filter also
// reaches the crates the selected ones depend on, where it can select no
// test, which nextest fails by default. A last step that runs no nextest,
// which an override names, gets no argument.
func testFilter(ctx *pctx.Context, root stacks.RootStep, lastStep []string) []string {
	filter := ctx.Params.String(testFilterFlag)
	if filter == "" || !runsNextest(lastStep) {
		return nil
	}
	args := []string{"-E", "test(~" + filtersetEscapes.Replace(filter) + ")"}
	if nextestAccepts(root, noTestsOption) {
		args = append(args, noTestsOption+"=pass")
	}
	return args
}

// runsNextest reports whether step runs nextest: cargo nextest run, cargo
// llvm-cov nextest, or a cargo-nextest binary.
func runsNextest(step []string) bool {
	for _, arg := range step {
		if name := filepath.Base(arg); name == "nextest" || strings.HasPrefix(name, "cargo-nextest") {
			return true
		}
	}
	return false
}

// nextestAccepts reports whether the nextest the task's cargo runs lists
// option in the help of cargo nextest run. A nextest that does not answer
// accepts nothing.
func nextestAccepts(root stacks.RootStep, option string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nextestHelpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, root.Program("cargo"), "nextest", "run", "--help") //nolint:gosec // the selected toolchain
	cmd.Dir = root.Project
	cmd.Env = root.Env
	help, err := cmd.Output()
	return err == nil && bytes.Contains(help, []byte(option))
}
