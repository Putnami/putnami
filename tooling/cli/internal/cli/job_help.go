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
	flags := mergedJobCommandFlags(command, wsRoot, cfg, extensions)
	PrintCommandHelp(command, flags)
}

func mergedJobCommandFlags(
	command string,
	wsRoot string,
	cfg *wsproto.Config,
	extensions []*extension.ExtensionDescription,
) map[string]extension.FlagDefinition {
	if wsRoot == "" {
		return nil
	}
	if extensions == nil {
		extensions, _ = extension.DiscoverExtensions(wsRoot, cfg, projectPathsForRoot(wsRoot))
	}
	jobMap := extension.BuildJobMap(extensions)
	return extension.CollectCommandFlags(jobMap[command])
}
