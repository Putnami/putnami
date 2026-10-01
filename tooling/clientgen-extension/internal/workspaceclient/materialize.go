package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	registry "go.putnami.dev/protocol/registry"
)

// MaterializeWorkspaceContracts builds every clientgen PROVIDER through the
// spawning Putnami CLI, for the sync and adopt commands, which regenerate every
// target in place and need each provider's authoritative built contract and
// generation config first. The selection is explicit so it includes providers
// carrying a workspace-default excluded tag (--all would preserve those
// exclusions).
//
// It is the PROVIDERS rather than "*", and the difference is not an
// optimization. Building every project rewrote committed schema artifacts of
// samples that declare no client at all — measured on this repository: five
// sample documents changed or created by a command that regenerates none of
// them. The providers are the only projects whose contract these commands
// read, so they are the only ones they build.
//
// The check does NOT call this. `clientgen-check`, and the `validate`
// contribution that runs it in every gate, read committed inputs only
// (InspectCommitted): whether a provider's committed clients are what its
// current contract generates is judged by the engine on the generator tasks'
// declared outputs (protocols/extension ADR 0004), and a nested build of every
// provider on every `putnami validate` was the 10 s that made the guard miss
// its 5 s target.
func MaterializeWorkspaceContracts(workspaceRoot string) error {
	arguments := WorkspaceBuildArgs(workspaceRoot)
	if len(arguments) == 0 {
		return nil
	}
	return runWorkspaceBuild(workspaceRoot, "materialize provider contracts with Putnami", arguments)
}

// CompileWorkspaceConsumers rebuilds the WHOLE workspace after generated
// artifacts and any manifest-proven import moves are in place. This is the
// synchronize command's compile gate: adoption cannot report success while
// leaving consumer imports or module dependencies broken, and a consumer is
// any project, so the selection here really is every project. It is not on the
// check path and therefore not on the `validate` path.
func CompileWorkspaceConsumers(workspaceRoot string) error {
	return runWorkspaceBuild(workspaceRoot, "compile generated-client consumers with Putnami",
		WorkspaceCompileArgs())
}

// WorkspaceCompileArgs is the exact invocation the synchronize command's
// compile gate makes.
//
// Its --no-cache stays run-wide, unlike the materialization's, because the
// selection here IS every project: there is no dependency closure outside it to
// spare, so scoping the flag to the selection would name the same set twice.
func WorkspaceCompileArgs() []string {
	return []string{"build", "--projects", "*", "--no-cache"}
}

// WorkspaceBuildArgs is the exact invocation the materialization step makes. It
// is exported so the plan assertion and the code under test read one
// declaration: a test that retyped the arguments would keep passing after the
// selection changed under it.
//
// The provider list comes from the project documents rather than from
// discovery, because discovery reads `.gen/clientgen/config.json` — which is
// what this build PRODUCES. Deriving the selection from the artifact would make
// a cold tree discover no provider, build nothing, and report a clean workspace
// because it looked at nothing. Reading the declaration has no such
// chicken-and-egg: a project either names this extension or it does not.
//
// Three answers, and the difference between the last two matters for a
// workspace that merely INSTALLS this extension:
//
//   - providers found: build exactly those, and refuse cache reuse for those
//     alone — the body says why the bypass is scoped rather than run-wide;
//   - the project index cannot be read: fall back to every project, because a
//     narrowed selection derived from an unreadable index would silently match
//     nothing (discovery reports the unreadable index too);
//   - the index is readable and no project declares the extension: build
//     NOTHING. There is no provider, so there is no contract to materialize,
//     and a whole-workspace build on every `putnami validate` would be a cost
//     charged to a workspace with nothing to verify.
func WorkspaceBuildArgs(workspaceRoot string) []string {
	providers, err := ClientgenProviderProjects(workspaceRoot)
	if err != nil {
		return WorkspaceCompileArgs()
	}
	if len(providers) == 0 {
		return nil
	}
	selection := strings.Join(providers, ",")
	// The cache bypass is SCOPED to the providers, not to the run.
	//
	// `--projects <providers>` plans the providers AND their whole dependency
	// closure, because a project cannot build against unbuilt dependencies. A
	// run-wide --no-cache recompiled that closure cold: measured on this
	// repository, 131 of the 141 planned tasks were dependencies of the two real
	// providers, 93 % of the work. None of that recompilation has value for a
	// regeneration: a dependency restored from cache is byte-identical to the
	// one a cold build would produce, and the CLI extends the bypass to anything
	// downstream of a bypassed project anyway. The bypass stays WHOLE-PROJECT;
	// narrowing it to the tasks that produce the two files sync reads was
	// measured and rejected — the CLI has no task-scoped bypass, and the
	// nested run's floor (planning 141 tasks, restoring 131) exceeded what a
	// narrowing could save. That floor is why the check stopped building
	// altogether rather than building less.
	return []string{"build", "--projects", selection, "--no-cache-projects", selection}
}

// ClientgenProviderProjects returns the indexed projects that declare this
// extension, as workspace-relative project ids, sorted. Reading the declaration
// rather than the generated configuration is what keeps a cold tree honest.
func ClientgenProviderProjects(workspaceRoot string) ([]string, error) {
	projectPaths, err := indexedProjectPaths(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var providers []string
	for _, projectRel := range projectPaths {
		data, readErr := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(projectRel), "putnami.json")) //nolint:gosec // indexed project document
		if readErr != nil {
			continue
		}
		var document struct {
			Extensions []string `json:"extensions"`
		}
		if json.Unmarshal(data, &document) != nil {
			continue
		}
		for _, name := range document.Extensions {
			if name == ClientgenExtensionName || name == ClientgenExtensionProjectID {
				providers = append(providers, "/"+strings.TrimPrefix(filepath.ToSlash(projectRel), "/"))
				break
			}
		}
	}
	sort.Strings(providers)
	return providers, nil
}

// ClientgenExtensionName and ClientgenExtensionProjectID are the two spellings
// a project may use to declare this extension: the published package name and
// the in-repository project id.
const (
	ClientgenExtensionName      = "@putnami/clientgen"
	ClientgenExtensionProjectID = "/tooling/clientgen-extension"
)

func runWorkspaceBuild(workspaceRoot, action string, arguments []string) error {
	executable := strings.TrimSpace(os.Getenv(registry.CLIExecutableEnv))
	if executable == "" || !filepath.IsAbs(executable) {
		return fmt.Errorf("%s must name the absolute Putnami CLI that launched clientgen", registry.CLIExecutableEnv)
	}
	if err := requireExecutable(executable); err != nil {
		return fmt.Errorf("resolve spawning Putnami CLI: %w", err)
	}
	command := exec.Command(executable, arguments...) //nolint:gosec // absolute executable authenticated by the CLI-owned environment contract
	command.Dir = workspaceRoot
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", action, err, output)
	}
	return nil
}
