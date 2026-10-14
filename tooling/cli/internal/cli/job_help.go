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
	if wsRoot != "" && extensions == nil {
		// Discover once for the flags and the scope. A non-nil empty list is a
		// workspace with no extension, which the scope must not discover again.
		if discovered, err := extension.DiscoverExtensions(wsRoot, cfg, projectPathsForRoot(wsRoot)); err == nil {
			extensions = append([]*extension.ExtensionDescription{}, discovered...)
		}
	}
	flags := mergedJobCommandFlags(command, wsRoot, cfg, extensions)
	printCommandHelp(command, flags, newHelpScope(wsRoot, cfg, extensions))
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
