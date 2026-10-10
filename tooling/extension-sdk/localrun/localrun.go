// Package localrun makes a test binary run as a job of a local run.
//
// The engine sets extensionproto.OfflineDependenciesEnv to "1" in every job of
// a hosted run, the test job included, and a test binary inherits it. Code
// under test that reads a hosted-run variable then acts as a job of a hosted
// run, in a suite that expects a local one. A test package whose tests reach
// such code imports this package for its effect, in a test file:
//
//	import _ "go.putnami.dev/sdk/extension/localrun"
//
// The import removes HostedRunVars from the environment of the test binary
// before any test runs. A test of the hosted behavior sets the variables it
// needs with t.Setenv.
//
// Only the test binary that `go test` starts removes them. It sets
// StartedEntry in its environment, and a process that inherits StartedEntry
// keeps its environment: a test binary that a test starts again as a fixture
// program sees what that test set. A test that starts its test binary with an
// environment it builds, instead of the one it inherits, adds StartedEntry to
// it.
//
// The import has no effect in a process that is not a test binary.
// CheckModule fails a module whose test packages miss the import.
package localrun

import (
	"os"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// ImportPath is the import path of this package.
const ImportPath = "go.putnami.dev/sdk/extension/localrun"

// startedEnv is "1" in a test binary that removed HostedRunVars, and in every
// process that inherits its environment.
const startedEnv = "PUTNAMI_LOCALRUN_STARTED"

// StartedEntry is the environment entry that makes a test binary keep the
// hosted-run variables it is started with.
const StartedEntry = startedEnv + "=1"

// HostedRunVars returns the names of the variables the engine sets for a job
// of a hosted run and for no job of a local run.
func HostedRunVars() []string {
	return []string{
		extensionproto.JobCredentialFDEnv,
		extensionproto.OfflineDependenciesEnv,
	}
}

func init() {
	if testing.Testing() {
		start()
	}
}

// start removes HostedRunVars from the environment of this process and sets
// StartedEntry, unless the process was started with StartedEntry.
func start() {
	if os.Getenv(startedEnv) == "1" {
		return
	}
	for _, name := range HostedRunVars() {
		_ = os.Unsetenv(name)
	}
	_ = os.Setenv(startedEnv, "1")
}
