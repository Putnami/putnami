package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A hosted run starts no `putnami cloud registry-token` for a
// module origin. That CLI would run without the run credential and load the
// workspace's path extensions, so ensureModuleOriginCredential asks nothing
// and records no repository code. Without a run credential, the same call
// starts the CLI once, so the hosted half is not vacuous.
func TestEnsureModuleOriginCredentialStartsNoProcessOnAHostedRun(t *testing.T) {
	record := filepath.Join(t.TempDir(), "runs")
	cli := fixtureproc.Write(t, filepath.Join(t.TempDir(), "putnami"), fixtureproc.Program{Record: record})
	t.Setenv(registry.CLIExecutableEnv, cli)
	// The SDK starts no process in a job of a hosted run either; this process
	// is no such job, so only the CLI's own check stands between the call and
	// the spawn.
	for _, name := range []string{extensionproto.OfflineDependenciesEnv, extensionproto.JobCredentialFDEnv} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRoot := t.TempDir()

	restore := runcredential.SetForTest("prc_module_origin_run_credential")
	outcome := ensureModuleOriginCredential(context.Background(), workspaceRoot, "hosted-origin.example")
	custody := runcredential.RequireCustody("a later holder")
	restore()
	if outcome.Kind != registrycred.KindSkipped {
		t.Errorf("hosted outcome = %+v, want skipped", outcome)
	}
	if runs := runArgs(t, record); len(runs) != 0 {
		t.Errorf("a hosted run started the credential CLI: %q", runs)
	}
	if custody != nil {
		t.Errorf("a hosted run that asked nothing recorded repository code: %v", custody)
	}

	ensureModuleOriginCredential(context.Background(), workspaceRoot, "local-origin.example")
	runs := runArgs(t, record)
	want := []string{registry.SeamParentCommand, registry.SeamSubcommand, "--" + registry.SeamHostFlag, "local-origin.example"}
	if len(runs) != 1 || len(runs[0]) < len(want) || !slices.Equal(runs[0][:len(want)], want) {
		t.Errorf("without a run credential the credential CLI ran as %q, want once with %q", runs, want)
	}
}

// runArgs is the arguments of each run the record holds. A run's environment
// stays out of the test output: it carries the machine's secrets.
func runArgs(t *testing.T, record string) [][]string {
	t.Helper()
	var args [][]string
	for _, run := range fixtureproc.Runs(t, record) {
		args = append(args, run.Args)
	}
	return args
}
