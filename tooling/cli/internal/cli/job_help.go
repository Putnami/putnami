package cli

import (
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// printJobCommandHelpForWorkspace prints the help of a job command for
// `putnami <command> --help` and returns the exit code: the usage error of a
// run of that command when no loaded extension declares it, and outside a
// workspace the error of such a run.
func printJobCommandHelpForWorkspace(
	command string,
	wsRoot string,
	cfg *wsproto.Config,
	extensions []*extension.ExtensionDescription,
) int {
	if wsRoot == "" {
		return noWorkspaceFound(os.Stderr)
	}
	scope, discovered := discoverHelpScope(wsRoot, cfg)
	if extensions == nil {
		extensions = discovered
	}
	if err := printDeclaredJobCommandHelp(command, extensions, scope); err != nil {
		printCommandError(os.Stderr, err)
		return exitCodeForError(err)
	}
	return ExitSuccess
}

// printDeclaredJobCommandHelp prints the help of a job command a loaded
// extension of scope declares, with the flags extensions give it. A command
// that none declares, an alias expansion of several commands included, has no
// help: it returns the usage error a run of it ends with
// (engine.UndeclaredCommandsError).
func printDeclaredJobCommandHelp(command string, extensions []*extension.ExtensionDescription, scope *helpScope) error {
	var undeclared []string
	for _, name := range strings.Split(command, ",") {
		if !scope.declares(name) {
			undeclared = append(undeclared, name)
		}
	}
	if len(undeclared) > 0 {
		return engine.UndeclaredCommandsError(undeclared)
	}
	printCommandHelp(command, jobCommandFlags(command, extensions), scope)
	return nil
}

func jobCommandFlags(command string, extensions []*extension.ExtensionDescription) map[string]extension.FlagDefinition {
	if extensions == nil {
		return nil
	}
	jobMap := extension.BuildJobMap(extensions)
	return extension.CollectCommandFlags(jobMap[command])
}
