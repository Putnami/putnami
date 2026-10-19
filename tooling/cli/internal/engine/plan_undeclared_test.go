package engine

import (
	"errors"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// declaringDiscovery is a discovery that loaded one extension declaring
// commands as jobs.
func declaringDiscovery(commands ...string) *extension.DiscoveryResult {
	jobs := make(map[string]*extension.JobDefinition, len(commands))
	for _, command := range commands {
		jobs[command] = &extension.JobDefinition{Name: command}
	}
	return &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/lang", Jobs: jobs},
	}}
}

// A run that names a command no loaded extension declares as a job is a usage
// error that names every such command once, in the order the run names them,
// and says how to add an extension. Nothing is refused when what the
// extensions declare is not known: a workspace lifecycle run, an extension
// that did not load, a configured extension that is missing.
func TestRefuseUndeclaredCommands(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/impact-plan", "named-commands", "an-undeclared-command-is-refused")
	spectest.Proves(t, "cli/job-planning-execution", "no-core-development-verb", "an-undeclared-job-command-is-refused-once-in-every-entry-point")

	err := refuseUndeclaredCommands(&Request{Commands: []string{"build", "lint", "bundle", "build"}}, declaringDiscovery("lint"))
	const want = "no extension of this workspace declares build, bundle; name a language extension in putnami.workspace.json, then run: putnami extensions install"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %v, want %q", err, want)
	}
	if !errors.Is(err, protocolcli.ErrUsage) || protocolcli.ExitCodeForError(err) != ExitUsage {
		t.Errorf("refusal %v exits %d, want a usage error that exits %d", err, protocolcli.ExitCodeForError(err), ExitUsage)
	}
	if got := UndeclaredCommandsError([]string{"build", "bundle"}); got.Error() != want {
		t.Errorf("help's refusal = %q, want the run's %q", got, want)
	}

	// Control: a run of declared commands passes.
	if err := refuseUndeclaredCommands(&Request{Commands: []string{"lint"}}, declaringDiscovery("lint")); err != nil {
		t.Errorf("declared lint refused: %v", err)
	}

	unknown := map[string]struct {
		req        *Request
		discovered *extension.DiscoveryResult
	}{
		"a workspace lifecycle run": {
			&Request{Commands: []string{"build"}, WorkspaceLifecycle: true}, declaringDiscovery(),
		},
		"no discovery": {&Request{Commands: []string{"build"}}, nil},
		"an extension that did not load": {
			&Request{Commands: []string{"build"}},
			&extension.DiscoveryResult{Skipped: []extension.SkippedExtension{{Name: "@acme/dev", Reason: errors.New("bad manifest")}}},
		},
		"a configured extension that is missing": {
			&Request{Commands: []string{"build"}, Config: &wsproto.Config{
				Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@acme/lang": "", "@acme/go": "1.0.0"}},
			}},
			declaringDiscovery("lint"),
		},
		"a dependency that is missing": {
			&Request{Commands: []string{"build"}, Config: &wsproto.Config{}},
			&extension.DiscoveryResult{
				Extensions:   declaringDiscovery("lint").Extensions,
				Dependencies: []extension.GraphDependency{{Name: "@acme/dev", RequiredBy: []string{"@acme/lang"}}},
			},
		},
	}
	for name, test := range unknown {
		if err := refuseUndeclaredCommands(test.req, test.discovered); err != nil {
			t.Errorf("%s: refused %v: %v", name, test.req.Commands, err)
		}
	}
}
