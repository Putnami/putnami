package cli

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// The command catalog (internal/commandmeta) is the
// single vocabulary the help, man, Markdown, and structured-help surfaces are
// generated from. These tests bind it to the two facts package cli owns and the
// catalog cannot see: which names are registered, and what the handlers actually
// do about a missing workspace.

func TestCatalog_CoversCommandRegistry(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "the-registry-and-catalog-agree")
	cataloged := map[string]bool{}
	for _, command := range commandmeta.StructuredCommands() {
		root := command.Root()
		if _, registered := lookupCommand(root); !registered {
			t.Errorf("catalog documents %q but %q is not a registered command", command.Path, root)
		}
		if command.Path == root {
			cataloged[root] = true
		}
	}
	var missing []string
	for name := range commandRegistry {
		if !cataloged[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("registered commands with no catalog entry for the bare invocation: %v.\n"+
			"  Every registered name needs one so its workspace requirement is declared rather than assumed.", missing)
	}
}

// TestCatalog_WorkspaceRequirementMatchesHandlers proves the catalog's workspace
// data is not decoration: for every path the catalog says needs a workspace, the
// registered handler must actually refuse to run without one.
//
// Only this direction is exercised in-process, deliberately. Running an exempt
// invocation to observe that it does NOT refuse would mean really running it:
// `cache clean --all` wipes every repo's store, `cache gc` collects the
// machine-global caches, `upgrade --global` downloads and installs a binary,
// `init` and `migrate` write a workspace into the current directory, and
// `telemetry on` rewrites machine-global consent. The exempt cases are pinned as
// data instead — each carries a WorkspaceNeed.Note naming the handler branch it
// codifies, asserted by commandmeta's TestCatalog_WorkspaceRequirementsAreExplained.
func TestCatalog_WorkspaceRequirementMatchesHandlers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "a-missing-workspace-yields-an-actionable-init-error")
	// A correctly declared handler returns before touching anything, so these
	// invocations are inert. A MIS-declared one (catalog says required, handler
	// does not check) really runs, which is the failure this test exists to
	// catch — so it runs in an empty working directory with an empty HOME,
	// bounding the blast radius of the failing case to a temp tree.
	sandbox := t.TempDir()
	t.Chdir(sandbox)
	hometest.Set(t, sandbox)

	checked := 0
	for _, command := range commandmeta.StructuredCommands() {
		if !command.Workspace.RequiresWorkspace(nil) {
			continue
		}
		root, sub, _ := strings.Cut(command.Path, " ")
		registered, ok := lookupCommand(root)
		if !ok {
			t.Errorf("catalog documents %q but %q is not registered", command.Path, root)
			continue
		}
		// Every handler that requires a workspace returns before touching the
		// filesystem, the network, or the config, so this invocation is inert.
		err := registered.run(&CommandEnv{
			Ctx:    context.Background(),
			Cfg:    &wsproto.Config{},
			WsRoot: "",
			Sub:    sub,
		})
		if !errors.Is(err, cmderr.ErrNoWorkspace) {
			t.Errorf("catalog says %q needs a workspace, but its handler returned %v instead of ErrNoWorkspace.\n"+
				"  Either the handler stopped checking (a command now runs outside a workspace) or the catalog is wrong.",
				command.Path, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no workspace-requiring command was exercised — the catalog lost its workspace data")
	}
}

// TestCatalog_StructuredOutputGatesEveryCapablePath asserts the gate accepts
// exactly the paths the catalog marks StructuredOutput. Slice A1b deleted the
// second capability table this used to be compared against; the catalog flag is
// now the only source, so the assertion is that the GATE reads it, in both
// directions.
func TestCatalog_StructuredOutputGatesEveryCapablePath(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, path := range commandmeta.StructuredOutputPaths() {
		declared[path] = true
	}
	if len(declared) == 0 {
		t.Fatal("no catalog path declares StructuredOutput")
	}

	for _, command := range commandmeta.StructuredCommands() {
		root, sub, _ := strings.Cut(command.Path, " ")
		// A bare root is judged by the path it actually runs, which is the
		// whole point of DefaultSub — TestCatalog_StructuredOutputReachesBareRoots
		// below pins that expansion separately.
		effective := commandmeta.CanonicalPath(root, sub)
		_, rejected := rejectStructuredIfUnsupported("json", root, sub)
		if declared[effective] && rejected {
			t.Errorf("catalog says %q emits structured output, but --output=json is rejected for `putnami %s`", effective, command.Path)
		}
		if !declared[effective] && !rejected {
			t.Errorf("--output=json is accepted for `putnami %s`, which runs %q — a path the catalog does not mark StructuredOutput",
				command.Path, effective)
		}
	}
}

// TestCatalog_StructuredOutputReachesBareRoots is the regression the slice
// exists for: a bare `putnami <root>` whose handler runs a structured-output
// subcommand must accept --output=json|jsonl, because it produces exactly that
// subcommand's bytes. `putnami scopes --json` exited 2 while
// `putnami scopes list --json` worked (ADR 0001), and only the capability
// lookup differed.
func TestCatalog_StructuredOutputReachesBareRoots(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, root := range commandmeta.StructuredRoots() {
		if root.DefaultSub == "" {
			continue
		}
		expansion, ok := commandmeta.Lookup(root.Path + " " + root.DefaultSub)
		if !ok {
			continue // reported by TestCatalog_DefaultSubIsAnInvocablePath
		}
		bareCode, bareRejected := rejectStructuredIfUnsupported("json", root.Path, "")
		subCode, subRejected := rejectStructuredIfUnsupported("json", root.Path, root.DefaultSub)
		if bareRejected != subRejected || bareCode != subCode {
			t.Errorf("`putnami %s --json` and `putnami %s %s --json` disagree: (%d, %v) vs (%d, %v) — "+
				"the handler runs the same code for both", root.Path, root.Path, root.DefaultSub,
				bareCode, bareRejected, subCode, subRejected)
		}
		if expansion.StructuredOutput && bareRejected {
			t.Errorf("`putnami %s --json` is rejected even though a bare %s runs %q, which emits structured output",
				root.Path, root.Path, expansion.Path)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no root declares a DefaultSub — the catalog lost its bare-root expansions")
	}
}
