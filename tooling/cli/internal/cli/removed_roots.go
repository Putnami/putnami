package cli

import (
	"slices"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// removedCoreRoots are the command roots the core CLI no longer serves. An
// extension may serve each of them under its own command group, so the core
// names no destination: the refusal cites the published reference that does.
// The exception is a root whose successors the core serves itself
// (removedRootSuccessors): its refusal names them.
var removedCoreRoots = []string{"channel", "ci", "change-plan", "infra", "dev"}

// removedRootSuccessors names, for a removed root whose commands the core still
// serves under another spelling, where they went. The core serves them in any
// workspace and outside one, so the refusal can name them without assuming
// either.
var removedRootSuccessors = map[string]string{
	"dev": "use `putnami extensions validate [path]` to validate an extension manifest, " +
		"and `putnami templates validate|test|package [path]` to validate, test or package a template",
}

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
	if successors, ok := removedRootSuccessors[command]; ok {
		return usageErrorf("`putnami %s` is no longer a core command: %s. "+
			"%s lists where each moved command is now", command, successors, commandmeta.CommandsThatLeftTheCoreURL)
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
