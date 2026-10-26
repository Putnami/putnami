package cli

import (
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// workloadExtensions is one loaded extension whose commands declare the given
// workloads; an empty workload declares none.
func workloadExtensions(workloads map[string]string) []*extension.ExtensionDescription {
	jobs := make(map[string]*extension.JobDefinition, len(workloads))
	for command, workload := range workloads {
		jobs[command] = &extension.JobDefinition{Name: command, Kind: "command", Traits: extension.CommandTraits{Workload: workload}}
	}
	return []*extension.ExtensionDescription{{Name: "@acme/lang", Jobs: jobs}}
}

// A hosted run that watches is refused as a usage error: --watch, and a
// command whose traits declare the serve workload, which watches by default,
// whatever its name. Every watch iteration starts after repository code ran,
// when no process may receive the run credential. A finite hosted run, a
// serve command that declares no workload, and any run without the
// credential are not refused.
func TestAHostedRunRefusesToWatch(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-hosted-run-refuses-a-serve-workload-command")
	firstParty := workloadExtensions(map[string]string{"serve": extension.WorkloadServe, "run": extension.WorkloadOnce})
	renamed := workloadExtensions(map[string]string{"daemon": extension.WorkloadServe, "serve": "", "run": extension.WorkloadOnce})
	for name, tc := range map[string]struct {
		hosted     bool
		watch      bool
		extensions []*extension.ExtensionDescription
		commands   []string
		refused    bool
	}{
		"hosted --watch":                {hosted: true, watch: true, commands: []string{"build"}, refused: true},
		"hosted serve":                  {hosted: true, extensions: firstParty, commands: []string{"serve"}, refused: true},
		"hosted build,serve":            {hosted: true, extensions: firstParty, commands: []string{"build", "serve"}, refused: true},
		"hosted daemon":                 {hosted: true, extensions: renamed, commands: []string{"daemon"}, refused: true},
		"hosted serve without workload": {hosted: true, extensions: renamed, commands: []string{"serve"}},
		"hosted serve, no extension":    {hosted: true, commands: []string{"serve"}},
		"hosted build":                  {hosted: true, extensions: firstParty, commands: []string{"build"}},
		"hosted run":                    {hosted: true, extensions: firstParty, commands: []string{"run"}},
		"local --watch":                 {watch: true, commands: []string{"build"}},
		"local serve":                   {extensions: firstParty, commands: []string{"serve"}},
		"local build --watch":           {watch: true, commands: []string{"build", "test"}},
		"hosted lint,test,run":          {hosted: true, extensions: firstParty, commands: []string{"lint", "test", "run"}},
	} {
		err := refuseHostedWatch(tc.hosted, &GlobalFlags{Watch: tc.watch}, tc.extensions, tc.commands)
		if !tc.refused {
			if err != nil {
				t.Errorf("%s: refused: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, engine.ErrHostedWatch) {
			t.Errorf("%s: want ErrHostedWatch, got %v", name, err)
		}
		if code := exitCodeForError(err); code != ExitUsage {
			t.Errorf("%s: exit code %d, want %d", name, code, ExitUsage)
		}
	}
}

// A hosted `putnami qualify <workload> --target <url>` is refused before
// anything starts, as every extension command group is on a hosted run: a
// usage error that names the group and says how to run it, without
// --credential-fd, from a job that is not a hosted run. The same invocation
// without the run credential is not refused.
func TestAHostedRunRefusesQualifyWithTheRemedy(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	groups := map[string]bool{"qualify": true}
	parsed := ParseArgs([]string{"qualify", "/apps/web", "--target", "https://pr-1.example"}, nil, groups)
	if parsed.Err != nil || len(parsed.Commands) == 0 || parsed.Commands[0] != "qualify" {
		t.Fatalf("parse = %+v, %v; want the qualify group", parsed.Commands, parsed.Err)
	}
	if err := resolveCommandCacheTrust(&parsed.Global, nil, nil, parsed.Commands, groups); err != nil {
		t.Fatalf("a local qualify was refused: %v", err)
	}

	defer runcredential.SetForTest("run-bearer")()
	err := resolveCommandCacheTrust(&parsed.Global, nil, nil, parsed.Commands, groups)
	if !errors.Is(err, ErrHostedExtensionCommand) || !errors.Is(err, cmderr.ErrUsage) {
		t.Fatalf("hosted qualify = %v, want the usage error ErrHostedExtensionCommand", err)
	}
	if code := exitCodeForError(err); code != ExitUsage {
		t.Errorf("hosted qualify exit code %d, want %d", code, ExitUsage)
	}
	for _, want := range []string{"putnami qualify: ", "run the extension command without " + runcredential.Flag + ", from a job that is not a hosted run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hosted qualify refusal %q lacks %q", err, want)
		}
	}
}
