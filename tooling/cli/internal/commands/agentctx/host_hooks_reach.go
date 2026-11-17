package agentctx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/launch"
)

// HostHookGap is one way an agent host would run none of the extension hooks
// a workspace's explicit install writes (ADR 0098).
type HostHookGap = shared.HostHookGap

// hostHookLookPath finds putnami on the PATH. Tests replace it.
var hostHookLookPath = exec.LookPath

// UserHostHookGaps reports where the hooks the workspace at wsRoot asks for
// would not run: for each host, the events whose user hook file holds no
// Putnami handler, and putnami missing from the PATH. A workspace whose
// install writes no extension hook, and a run inside CI, which hosts no agent
// session, have none. It reads the workspace and the user's files, and writes
// nothing.
func UserHostHookGaps(wsRoot string, cfg *wsproto.Config) []HostHookGap {
	if cfg == nil || hostHookTruthy(hostHookGetenv("CI")) {
		return nil
	}
	set, err := collectHostHooks(wsRoot, cfg, func(string, ...any) {})
	if err != nil || len(set.user) == 0 {
		return nil
	}
	var gaps []HostHookGap
	for _, file := range hostHookUserFiles {
		desired := set.user[file.host]
		if len(desired) == 0 {
			continue
		}
		path, err := file.path()
		if err != nil {
			gaps = append(gaps, HostHookGap{Kind: shared.HostHookGapFile, Host: file.host, Reason: err.Error()})
			continue
		}
		missing, reason := missingHostHookEvents(path, file.host, set.paths, desired)
		if len(missing) > 0 {
			gaps = append(gaps, HostHookGap{Kind: shared.HostHookGapFile, Host: file.host, File: path, Events: missing, Reason: reason})
		}
	}
	if home, err := hostHookHomeDir(); err == nil && !launch.HostHookRepositoryRecorded(home, wsRoot) {
		gaps = append(gaps, HostHookGap{Kind: shared.HostHookGapRepository, Reason: "no putnami command recorded this repository, so the hooks do not serve it"})
	}
	if _, err := hostHookLookPath(hostHookUserLauncher); err != nil {
		gaps = append(gaps, HostHookGap{Kind: shared.HostHookGapPath, Reason: "putnami is not on the PATH, so the user's hooks cannot start it"})
	}
	return gaps
}

// missingHostHookEvents lists the events of desired, in the host's order,
// whose groups in the file at path hold no handler a declared path owns.
func missingHostHookEvents(path, host string, paths [][]string, desired map[string][]hostHookGroupJSON) ([]string, string) {
	var wanted []string
	for _, event := range extproto.HostHookEvents(host) {
		if len(desired[event]) > 0 {
			wanted = append(wanted, event)
		}
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wanted, "the file does not exist"
	case err != nil:
		return wanted, fmt.Sprintf("the file cannot be read: %v", err)
	}
	root, err := decodeJSONObject(data)
	if err != nil {
		return wanted, "the file is not a JSON object"
	}
	hooksValue, _ := singleJSONMember(root, "hooks")
	hooks, _ := decodeJSONObject(hooksValue)
	owns := func(command string) bool { return ownsHostHookCommand(command, paths) }
	var missing []string
	for _, event := range wanted {
		value, _ := singleJSONMember(hooks, event)
		groups, _ := decodeJSONArray(value)
		if !slices.ContainsFunc(groups, func(group []byte) bool {
			_, ours := splitHostHookGroup(group, owns, "  ")
			return ours
		}) {
			missing = append(missing, event)
		}
	}
	return missing, "the file holds no Putnami hook for these events"
}
