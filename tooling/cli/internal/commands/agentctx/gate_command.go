package agentctx

import (
	"os"
	"path/filepath"
	"slices"
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
	return gateTasks(wsRoot, loadedJobs(wsRoot))
}

// gateTasks is gateTasksForWorkspace over the jobs the workspace's loaded
// extensions declare, read once by the caller.
func gateTasks(wsRoot string, jobs map[string][]*extension.JobDefinition) string {
	if tasks := gateCommandNames(readCIDocument(wsRoot)); len(tasks) > 0 {
		return strings.Join(tasks, ",")
	}
	defaults := gateCommandNames(ciproto.DefaultDocument())
	kept := make([]string, 0, len(defaults))
	for _, name := range defaults {
		if len(jobs[name]) > 0 {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 {
		return strings.Join(defaults, ",")
	}
	return strings.Join(kept, ",")
}

// coverageFlag is the flag the gate adds so the run fails below the projects'
// coverage thresholds.
const coverageFlag = "enforce-coverage"

// gateCommandForWorkspace is the argument list of the gate the generated
// guidance tells an agent to run: the gate tasks, over the impacted projects,
// with --enforce-coverage when a gate task a loaded extension declares
// accepts it. The engine refuses a flag no selected task declares, so a
// workspace whose extensions declare no coverage flag, as Bazel's and Pants'
// do not, is told the gate without it. When no loaded extension declares a
// gate task, nothing says which flags the tasks accept, and the flag stays, as
// the whole default stays in gateTasksForWorkspace.
func gateCommandForWorkspace(wsRoot string) string {
	jobs := loadedJobs(wsRoot)
	tasks := strings.Split(gateTasks(wsRoot, jobs), ",")
	command := strings.Join(tasks, ",") + " --impacted"
	if !slices.ContainsFunc(tasks, func(task string) bool { return len(jobs[task]) > 0 }) ||
		gateDeclaresFlag(tasks, jobs, coverageFlag) {
		command += " --" + coverageFlag
	}
	return command
}

// gateDeclaresFlag reports whether a gate task declares the flag, the
// surface a run of those tasks validates its flags against.
func gateDeclaresFlag(tasks []string, jobs map[string][]*extension.JobDefinition, flag string) bool {
	for _, task := range tasks {
		if _, ok := extension.CollectCommandFlags(jobs[task])[flag]; ok {
			return true
		}
	}
	return false
}

// loadedJobs returns the jobs the workspace's loaded extensions declare, by
// name, discovered as a job run discovers them: from the workspace projects
// and the workspace extensions list. A workspace that does not load declares
// nothing.
func loadedJobs(wsRoot string) map[string][]*extension.JobDefinition {
	var projectPaths []string
	if ws, err := workspace.Load(wsRoot); err == nil && ws != nil {
		projectPaths = ws.ProjectPaths()
	}
	extensions, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), projectPaths)
	if err != nil {
		return nil
	}
	return extension.BuildJobMap(extensions)
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

// GateCommand is the argument list of the gate the generated guidance names,
// after `putnami`, such as "lint,test,build --impacted --enforce-coverage".
func GateCommand(wsRoot string) string {
	return gateCommandForWorkspace(wsRoot)
}
