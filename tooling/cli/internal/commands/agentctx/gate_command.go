package agentctx

import (
	"os"
	"path/filepath"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// gateTasksForWorkspace derives the verification gate the generated guidance
// tells an agent to run, from the workspace's own CI document. Only the
// blocking commands join it: a `failOnError: false` entry never fails a run,
// so telling a contributor it gates their merge would be false. The commands
// keep the document's order, which is the order the CI runner executes them.
//
// The gate is guidance about THIS workspace: a hardcoded line would tell a
// workspace whose extensions declare more tasks to run less than its CI does,
// and tell another workspace to run commands that do not exist. Reading
// putnami.ci.json makes the sentence true by construction and keeps the
// generated files stable: a regeneration that does not change the document
// cannot change them.
//
// A workspace without a usable document (missing, unparseable, or with no
// blocking command) gets the blocking commands of ciproto.DefaultDocument, the
// protocol's default CI document, that a loaded extension declares, in the
// default order. The protocol owns those names; the core names no command. A
// workspace whose extensions declare test and build, as Bazel's does, is not
// told to run lint, which the engine would refuse as undeclared. No verb
// outside the default document joins the fallback. When no extension loads,
// or none declares a default verb, the whole default stays. The guidance
// follows the committed lock: context generation materializes the
// lock-pinned extensions before it writes.
func gateTasksForWorkspace(wsRoot string) string {
	if tasks := gateCommandNames(readCIDocument(wsRoot)); len(tasks) > 0 {
		return strings.Join(tasks, ",")
	}
	defaults := gateCommandNames(ciproto.DefaultDocument())
	declared := declaredJobs(wsRoot)
	kept := make([]string, 0, len(defaults))
	for _, name := range defaults {
		if declared[name] {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 {
		return strings.Join(defaults, ",")
	}
	return strings.Join(kept, ",")
}

// declaredJobs returns the job names the workspace's loaded extensions
// declare, discovered as a job run discovers them: from the workspace
// projects and the workspace extensions list. A workspace that does not load
// declares nothing.
func declaredJobs(wsRoot string) map[string]bool {
	var projectPaths []string
	if ws, err := workspace.Load(wsRoot); err == nil && ws != nil {
		projectPaths = ws.ProjectPaths()
	}
	extensions, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), projectPaths)
	if err != nil {
		return nil
	}
	declared := make(map[string]bool)
	for name := range extension.BuildJobMap(extensions) {
		declared[name] = true
	}
	return declared
}

// readCIDocument returns the workspace's CI document, or the zero document
// when the file is missing or does not parse.
func readCIDocument(wsRoot string) ciproto.Document {
	data, err := os.ReadFile(filepath.Join(wsRoot, ciproto.Filename))
	if err != nil {
		return ciproto.Document{}
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return ciproto.Document{}
	}
	return document
}

// gateCommandNames returns the names of the commands whose failure fails the
// run, in the document's own order. ciproto.Parse rejects blank and repeated
// names, so the list needs no cleaning.
func gateCommandNames(document ciproto.Document) []string {
	names := make([]string, 0, len(document.Commands))
	for _, command := range document.Commands {
		if command.Blocking() {
			names = append(names, command.Name)
		}
	}
	return names
}

// GateTasks is the exported derivation for the callers that must agree with
// the generated guidance about the gate: `cache verify` runs these commands,
// and the documented contributor recipe is compared against them.
func GateTasks(wsRoot string) string {
	return gateTasksForWorkspace(wsRoot)
}
