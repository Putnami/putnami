package cli

import (
	"slices"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// removedCoreRoots are the command roots the core CLI no longer serves. An
// extension may serve each of them under its own command group, so the core
// names no destination.
var removedCoreRoots = []string{"channel", "ci"}

// refuseRemovedCoreRoot returns a usage error when command is a removed core
// root that no extension declares, as a command group or as a job, and nil
// otherwise. App.Run calls it after the parse and before the workspace
// bootstrap, so a refused root installs nothing and plans nothing.
func refuseRemovedCoreRoot(command string, groups map[string]bool, extensions []*extension.ExtensionDescription) error {
	if !slices.Contains(removedCoreRoots, command) || groups[command] {
		return nil
	}
	if len(extension.BuildJobMap(extensions)[command]) > 0 {
		return nil
	}
	return usageErrorf("`putnami %s` is no longer a core command: an extension may provide it under its own command group. "+
		"`putnami extensions list` names the extensions installed here", command)
}
