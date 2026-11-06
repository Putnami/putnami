package lifecycle

import (
	"fmt"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// WorkspaceDescribe shows workspace information including projects,
// extensions, and configuration. The warnings workspace.Load recorded go to
// warnings (nil reports none).
func WorkspaceDescribe(wsRoot string, cfg *wsproto.Config, outputFormat string, warnings *workspace.WarningReport) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	warnings.Report(ws)

	if protocolcli.OutputMode(outputFormat).IsStructured() {
		return shared.WriteResult(outputFormat, "workspace describe", workspaceInfoFor(ws, cfg), warnings.Envelope())
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Workspace: %s\n", ws.Name)
	iox.Fprintf(os.Stdout, "  Root:      %s\n", ws.Root)
	iox.Fprintln(os.Stdout)

	// Projects
	iox.Fprintf(os.Stdout, "  Projects (%d):\n", len(ws.Projects))
	for _, p := range ws.Projects {
		tags := ""
		if len(p.Tags) > 0 {
			tags = fmt.Sprintf(" [%s]", joinTags(p.Tags))
		}
		iox.Fprintf(os.Stdout, "    %-30s %s%s\n", p.Name, p.Path, tags)
	}
	iox.Fprintln(os.Stdout)

	// Extensions
	extNames := cfg.Extensions.Names()
	if len(extNames) > 0 {
		iox.Fprintf(os.Stdout, "  Extensions (%d):\n", len(extNames))
		for _, ext := range extNames {
			iox.Fprintf(os.Stdout, "    %s\n", ext)
		}
		iox.Fprintln(os.Stdout)
	}

	// Aliases
	if len(cfg.Aliases) > 0 {
		iox.Fprintln(os.Stdout, "  Aliases:")
		for k, v := range cfg.Aliases {
			iox.Fprintf(os.Stdout, "    %s → %s\n", k, v)
		}
		iox.Fprintln(os.Stdout)
	}

	return nil
}

// wsInfo is the data of the workspace describe result.
type wsInfo struct {
	Name       string   `json:"name"`
	Root       string   `json:"root"`
	Projects   []string `json:"projects"`
	Extensions []string `json:"extensions,omitempty"`
}

func workspaceInfoFor(ws *workspace.Workspace, cfg *wsproto.Config) wsInfo {
	info := wsInfo{
		Name:       ws.Name,
		Root:       ws.Root,
		Extensions: cfg.Extensions.Names(),
	}

	for _, p := range ws.Projects {
		info.Projects = append(info.Projects, p.Name)
	}
	return info
}

func joinTags(tags []string) string {
	return strings.Join(tags, ", ")
}
