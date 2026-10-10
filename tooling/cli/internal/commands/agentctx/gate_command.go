package agentctx

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// defaultGateTasks is the part of the default gate every workspace without a
// usable CI document receives, whatever its extensions declare. It equals the
// gate of the protocol's default CI document.
const defaultGateTasks = "lint,test,build"

// validateTask joins the default gate when an extension the workspace
// discovers declares a job of that name. The gate names it alone: the
// manifest that declares it plans its companions (`alsoRuns`) with it.
const validateTask = "validate"

// gateTaskPrefix is the conventional putnami verification order: lint gives the
// fastest signal, test the strongest, build the slowest. The generated guidance
// keeps that order rather than the document's, so a workspace that authored its
// commands in another order still reads the idiomatic `lint,test,build` line.
var gateTaskPrefix = []string{"lint", "test", "build"}

// gateTasksForWorkspace derives the verification gate the generated guidance
// tells an agent to run, from the workspace's own CI document. Only the
// blocking commands join it: a `failOnError: false` entry never fails a run,
// so telling a contributor it gates their merge would be false.
//
// The gate is guidance about THIS workspace: a hardcoded line would tell a
// workspace whose extensions declare more tasks to run less than its CI does,
// and tell another workspace to run commands that do not exist. Reading
// putnami.ci.json makes the sentence true by construction and keeps the
// generated files stable: a regeneration that does not change the document
// cannot change them.
//
// Every failure to read a usable command falls back to the workspace default
// (defaultGateTasksForWorkspace).
func gateTasksForWorkspace(wsRoot string) string {
	data, err := os.ReadFile(filepath.Join(wsRoot, ciproto.Filename))
	if err != nil {
		return defaultGateTasksForWorkspace(wsRoot)
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return defaultGateTasksForWorkspace(wsRoot)
	}
	tasks := orderGateTasks(blockingCommandNames(document))
	if tasks == "" {
		return defaultGateTasksForWorkspace(wsRoot)
	}
	// A usable document is authoritative, even when it names only the generic
	// trio: its blocking commands are what every push and pull request runs.
	return tasks
}

// blockingCommandNames returns the names of the commands whose failure fails
// the run, in the document's own order.
func blockingCommandNames(document ciproto.Document) []string {
	names := make([]string, 0, len(document.Commands))
	for _, command := range document.Commands {
		if command.Blocking() {
			names = append(names, command.Name)
		}
	}
	return names
}

// GateTasks is the exported derivation for the callers that must agree with
// the generated guidance about the gate: the change plan's command list and
// the documented contributor recipe are both compared against it.
func GateTasks(wsRoot string) string {
	return gateTasksForWorkspace(wsRoot)
}

// defaultGateTasksForWorkspace returns the gate of a workspace without a
// usable CI document: lint, test and build always, then validate when an
// extension the workspace discovers declares a job of that name. Any failure
// to discover the extensions keeps lint, test and build.
func defaultGateTasksForWorkspace(wsRoot string) string {
	if workspaceDeclaresJob(wsRoot, validateTask) {
		return defaultGateTasks + "," + validateTask
	}
	return defaultGateTasks
}

// workspaceDeclaresJob reports whether an extension the workspace discovers
// declares the named job. Discovery reads the workspace's own
// putnami.workspace.json and never the global configuration, so the answer
// depends on the workspace and the extensions installed for it, not on the
// machine's user settings. A workspace without that file runs no discovery.
// An extension that discovery skips declares nothing.
func workspaceDeclaresJob(wsRoot, name string) bool {
	data, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return false
	}
	cfg, diagnostics := wsproto.ParseWorkspaceConfig(data)
	if cfg == nil || len(diagnostics) != 0 {
		return false
	}
	discovered, err := shared.DiscoverWorkspaceExtensions(wsRoot, cfg)
	if err != nil || discovered == nil {
		return false
	}
	_, declared := extension.BuildJobMap(discovered.Extensions)[name]
	return declared
}

// orderGateTasks renders the gate as the comma-joined argument list a
// putnami invocation takes, in a canonical order: the conventional
// lint,test,build prefix first (only the members the job actually declares),
// then every remaining task sorted. Sorting the tail rather than trusting the
// input keeps the result identical whatever order the document was authored in,
// and duplicates collapse so a hand-edited document cannot emit `lint,lint`.
func orderGateTasks(tasks []string) string {
	remaining := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		task = strings.TrimSpace(task)
		if task == "" {
			continue
		}
		remaining[task] = struct{}{}
	}

	ordered := make([]string, 0, len(remaining))
	for _, task := range gateTaskPrefix {
		if _, ok := remaining[task]; ok {
			ordered = append(ordered, task)
			delete(remaining, task)
		}
	}
	tail := make([]string, 0, len(remaining))
	for task := range remaining {
		tail = append(tail, task)
	}
	sort.Strings(tail)

	return strings.Join(append(ordered, tail...), ",")
}
