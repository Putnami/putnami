package agentctx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// defaultGateTasks is the generic verification gate emitted for a workspace
// that declares no CI document. It matches the commands `putnami ci init`
// scaffolds, so a workspace that later adopts the default document keeps the
// same line.
const defaultGateTasks = "lint,test,build"

// sddExtensionName is the extension that owns `validate` and
// `validate-workspace`. A workspace that declares it plans both, so its
// verification gate is one word longer than the generic one.
const sddExtensionName = "@putnami/sdd"

// sddDefaultGateTasks is the gate a workspace without a CI document receives
// when it declares the SDD extension. `validate` alone is enough: the SDD
// manifest's `alsoRuns` plans `validate-workspace` with it, and this is the
// fixed gate Putnami Cloud's native runner executes for every workspace.
const sddDefaultGateTasks = defaultGateTasks + ",validate"

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
// The gate is guidance about THIS workspace, so hardcoding it is wrong in both
// directions: a workspace that declares extension tasks (`validate` and
// `validate-workspace` come from @putnami/sdd) gets told to run less than CI
// does, and a workspace without that extension would be told to run commands
// that do not exist. Reading putnami.ci.json makes the sentence true by
// construction and keeps the generated files stable — the CI document is the
// only input, and a regeneration that does not change it cannot change them.
//
// Every failure to read a usable job falls back to the workspace default: an
// absent, unreadable, or invalid document must leave the generic guidance
// exactly as it was before this derivation existed. The default itself is
// derived from the one declaration that decides whether `validate` exists at
// all — whether the workspace declares the SDD extension — because Putnami
// Cloud's native runner executes the same fixed gate for every workspace and
// consumes no CI document, so a workspace without one must still be told the
// gate that CI actually runs.
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

// defaultGateTasksForWorkspace returns the generic gate, plus `validate` when
// the workspace declares the SDD extension by name or as a local path whose
// manifest carries that name. Any unreadable input keeps the generic gate.
func defaultGateTasksForWorkspace(wsRoot string) string {
	if workspaceDeclaresExtension(wsRoot, sddExtensionName) {
		return sddDefaultGateTasks
	}
	return defaultGateTasks
}

// workspaceDeclaresExtension reports whether the workspace's own
// putnami.workspace.json declares the named extension, either directly or as a
// local path whose manifest names it. It reads the workspace file alone, never
// the global config, so the answer is a function of the committed tree.
func workspaceDeclaresExtension(wsRoot, name string) bool {
	data, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return false
	}
	cfg, diagnostics := wsproto.ParseWorkspaceConfig(data)
	if cfg == nil || len(diagnostics) != 0 {
		return false
	}
	for _, declared := range cfg.Extensions.Names() {
		if declared == name {
			return true
		}
		if !strings.HasPrefix(declared, "/") {
			continue
		}
		if localExtensionManifestName(filepath.Join(wsRoot, filepath.FromSlash(declared))) == name {
			return true
		}
	}
	return false
}

// localExtensionManifestName reads only the manifest's name. It deliberately
// avoids the full manifest loader: the gate derivation must not depend on the
// extension contract version the running CLI accepts.
func localExtensionManifestName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, extproto.ManifestFilename))
	if err != nil {
		return ""
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Name)
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
