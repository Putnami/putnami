package agentctx

import (
	"os"
	"path/filepath"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
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
// blocking command) gets the blocking commands of ciproto.DefaultDocument,
// the default the CI runner applies. The protocol owns those names; the core
// names no command. The fallback reads no extension, so the guidance is a
// function of the committed tree alone: it does not change with the
// extensions installed on a machine or with the host a regeneration runs on.
func gateTasksForWorkspace(wsRoot string) string {
	if tasks := gateCommandNames(readCIDocument(wsRoot)); len(tasks) > 0 {
		return strings.Join(tasks, ",")
	}
	return strings.Join(gateCommandNames(ciproto.DefaultDocument()), ",")
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
// run, in the document's own order. A name that repeats counts once, and a
// blank name is skipped, so a hand-edited document cannot emit `lint,lint`.
func gateCommandNames(document ciproto.Document) []string {
	names := make([]string, 0, len(document.Commands))
	seen := make(map[string]struct{}, len(document.Commands))
	for _, command := range document.Commands {
		name := strings.TrimSpace(command.Name)
		if name == "" || !command.Blocking() {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// GateTasks is the exported derivation for the callers that must agree with
// the generated guidance about the gate: `cache verify` runs these commands,
// and the documented contributor recipe is compared against them.
func GateTasks(wsRoot string) string {
	return gateTasksForWorkspace(wsRoot)
}
