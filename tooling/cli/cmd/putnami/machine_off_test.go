package main

import (
	"os"

	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// No test run reaches the user's machine process (CLI ADR 0116), which is on
// by default: the setting is off before the tests run, for the CLI they run
// in this process and every process they start.
func init() {
	_ = os.Setenv(machineproc.Env, "off")
}
