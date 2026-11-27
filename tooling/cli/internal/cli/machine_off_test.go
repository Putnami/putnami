package cli

import (
	"os"

	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// No test run reaches the user's machine process (CLI ADR 0116): the setting
// is off before TestMain runs.
func init() {
	_ = os.Setenv(machineproc.Env, "off")
}
