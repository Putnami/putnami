package cli

import (
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func printJobCommandHelpForWorkspace(
	command string,
	wsRoot string,
	cfg *wsproto.Config,
	extensions []*extension.ExtensionDescription,
) int {
	printJobCommandHelp(command, wsRoot, cfg, extensions)
	return ExitSuccess
}

func printJobCommandHelp(
	command string,
	wsRoot string,
	cfg *wsproto.Config,
	extensions []*extension.ExtensionDescription,
) {
	scope, discovered := discoverHelpScope(wsRoot, cfg)
	// Outside a workspace, job help lists no extension flag.
	if wsRoot == "" || extensions == nil {
		extensions = discovered
	}
	printCommandHelp(command, jobCommandFlags(command, extensions), scope)
}

func jobCommandFlags(command string, extensions []*extension.ExtensionDescription) map[string]extension.FlagDefinition {
	if extensions == nil {
		return nil
	}
	jobMap := extension.BuildJobMap(extensions)
	return extension.CollectCommandFlags(jobMap[command])
}
