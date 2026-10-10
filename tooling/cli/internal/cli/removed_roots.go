package cli

import (
	"slices"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// removedCoreRoots are the command roots the core CLI no longer serves. An
// extension may serve each of them under its own command group, so the core
// names no destination: the refusal cites the published reference that does.
var removedCoreRoots = []string{"channel", "ci"}

// refuseRemovedCoreRoot returns a usage error when command is a removed core
// root that no extension declares, as a command group or as a job, and nil
// otherwise. App.Run calls it after the parse and before the workspace
// bootstrap, so a refused root runs no install and plans nothing. The message
// assumes no workspace, because the refusal also applies outside one.
func refuseRemovedCoreRoot(command string, groups map[string]bool, extensions []*extension.ExtensionDescription) error {
	if !slices.Contains(removedCoreRoots, command) || groups[command] {
		return nil
	}
	if len(extension.BuildJobMap(extensions)[command]) > 0 {
		return nil
	}
	return usageErrorf("`putnami %s` is no longer a core command: an extension may provide it under its own command group. "+
		"%s lists where each moved command is now", command, commandmeta.CommandsThatLeftTheCoreURL)
}

// refuseHelpOfRemovedCoreRoot applies refuseRemovedCoreRoot to `putnami help
// <command>`. help is a built-in command, so App.Run discovers no extension
// for it; this check discovers them as App.Run does for `putnami <command>`,
// so `putnami help ci` and `putnami ci --help` give the same answer. It
// discovers nothing for any other command.
func refuseHelpOfRemovedCoreRoot(env *CommandEnv, command string) error {
	if !slices.Contains(removedCoreRoots, command) {
		return nil
	}
	extensions, groups := discoverDispatchExtensions(env.Ctx, []string{command}, env.WsRoot, env.Cfg, false)
	return refuseRemovedCoreRoot(command, groups, extensions)
}
