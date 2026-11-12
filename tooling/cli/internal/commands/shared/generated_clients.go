package shared

import (
	"os"
	"path/filepath"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ClientGenerateCommand is the command that renders the generated client of
// every project that declares one. The @putnami/clientgen extension provides
// it. The first-run pass of the lifecycle vertical runs it, and the agentctx
// vertical writes the host hook that runs it (ADR 0087).
const ClientGenerateCommand = "client-generate"

// clientGeneratorConfigFile is the file a provider commits to generate its
// in-module Go client.
const clientGeneratorConfigFile = "clientgen.json"

// DeclaresGeneratedClient reports whether a project of ws declares a
// generated client. A project declares one when its putnami.json names a
// provider in `generatedFrom`, or when it holds a clientgen.json. These are
// the projects where client-generate plans a step.
func DeclaresGeneratedClient(ws *workspace.Workspace) bool {
	if ws == nil {
		return false
	}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if project.GeneratedClient != nil {
			return true
		}
		info, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(project.Path), clientGeneratorConfigFile))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}
