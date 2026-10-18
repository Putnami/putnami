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
var removedCoreRoots = []string{"channel", "ci", "change-plan", "infra", "dev", "contributor"}

// removedRootSuccessors names, for a removed root whose commands the core still
// serves under another spelling, where they went. The core serves them in any
// workspace and outside one, so the refusal can name them without assuming
// either. A root whose commands went partly to an extension names only the core
// successors, and says an extension may provide the rest.
var removedRootSuccessors = map[string]string{
	"dev": "use `putnami extensions validate [path]` to validate an extension manifest, " +
		"and `putnami templates validate|test|package [path]` to validate, test or package a template",
	"contributor": "use `putnami doctor --machine` to read the machine load, " +
		"and an extension may provide the language check under its own command group",
}

// removedCoreSubcommands are the subcommands the core no longer serves under a
// command root it still serves, as the command path a user types. The core
// owns the root, so no extension serves the path itself; an extension may serve
// the command under its own command group. The refusal names no destination
// and cites the published reference that does.
var removedCoreSubcommands = []string{"tree verify"}

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
	return removedWithoutSuccessor(command)
}

// removedWithoutSuccessor is the refusal of a command path the core does not
// serve and names no successor for.
func removedWithoutSuccessor(path string) error {
	return usageErrorf("`putnami %s` is no longer a core command: an extension may provide it under its own command group. "+
		"%s lists where each moved command is now", path, commandmeta.CommandsThatLeftTheCoreURL)
}

// refuseRemovedCoreSubcommand returns a usage error when command and sub name a
// removed core subcommand, and nil otherwise. App.Run calls it beside
// refuseRemovedCoreRoot, and cmdHelp for `putnami help <command> <sub>`, so
// `putnami tree verify`, `putnami tree verify --help` and `putnami help tree
// verify` give the same answer before the workspace bootstrap, in a workspace
// and outside one.
func refuseRemovedCoreSubcommand(command, sub string) error {
	path := command + " " + sub
	if !slices.Contains(removedCoreSubcommands, path) {
		return nil
	}
	return removedWithoutSuccessor(path)
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
