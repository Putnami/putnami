package lifecycle

import (
	"os"

	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// No test run reaches the user's machine process (CLI ADR 0116), which is on
// by default: the setting is off before TestMain runs, for the CLI the tests
// run in this process and every process they start.
func init() {
	_ = os.Setenv(machineproc.Env, "off")
}
