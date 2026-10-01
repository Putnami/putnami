package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// WorkspaceDescribe shows workspace information including projects,
// extensions, and configuration.
func WorkspaceDescribe(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	if outputFormat == "jsonl" {
		return workspaceDescribeJSONL(ws, cfg)
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

func workspaceDescribeJSONL(ws *workspace.Workspace, cfg *wsproto.Config) error {
	type wsInfo struct {
		Name       string   `json:"name"`
		Root       string   `json:"root"`
		Projects   []string `json:"projects"`
		Extensions []string `json:"extensions,omitempty"`
	}

	info := wsInfo{
		Name:       ws.Name,
		Root:       ws.Root,
		Extensions: cfg.Extensions.Names(),
	}

	for _, p := range ws.Projects {
		info.Projects = append(info.Projects, p.Name)
	}

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal workspace info: %w", err)
	}
	iox.Fprintln(os.Stdout, string(data))
	return nil
}

func joinTags(tags []string) string {
	return strings.Join(tags, ", ")
}
