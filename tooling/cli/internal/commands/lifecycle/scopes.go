package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ScopeInfo describes a scope for display and JSONL output.
type ScopeInfo struct {
	Path     string            `json:"path"`
	Projects []string          `json:"projects,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Groups   map[string]string `json:"groups,omitempty"`
	Aliases  map[string]string `json:"aliases,omitempty"`
	// Line is the git tag pattern of the version line this scope declares, or
	// empty when the scope declares none. Only the declaring scope reports it:
	// a line is not inherited.
	Line string `json:"line,omitempty"`
}

// ScopesList lists all scopes in the workspace with their paths, project counts, groups, and tags.
func ScopesList(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	scopes := collectScopeInfos(wsRoot, cfg, ws)

	if outputFormat == "jsonl" {
		return scopesListJSONL(scopes)
	}

	if len(scopes) == 0 {
		iox.Fprintln(os.Stdout, "  No scopes configured.")
		return nil
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  %-30s %-10s %-10s %-20s %s\n", "SCOPE", "PROJECTS", "GROUPS", "LINE", "TAGS")
	iox.Fprintf(os.Stdout, "  %-30s %-10s %-10s %-20s %s\n", "-----", "--------", "------", "----", "----")

	for _, s := range scopes {
		tags := "-"
		if len(s.Tags) > 0 {
			tags = strings.Join(s.Tags, ", ")
		}
		line := "-"
		if s.Line != "" {
			line = s.Line
		}
		iox.Fprintf(os.Stdout, "  %-30s %-10d %-10d %-20s %s\n", s.Path, len(s.Projects), len(s.Groups), line, tags)
	}
	iox.Fprintf(os.Stdout, "\n  %d scopes\n\n", len(scopes))
	return nil
}

// collectScopeInfos gathers scope information from all configured scopes.
func collectScopeInfos(wsRoot string, cfg *wsproto.Config, ws *workspace.Workspace) []ScopeInfo {
	var scopes []ScopeInfo

	for _, scopePath := range workspace.ScopePaths(wsRoot, cfg) {
		scopeDir := filepath.Join(wsRoot, scopePath)
		sc := wsproto.ReadScopeConfig(scopeDir)

		info := ScopeInfo{
			Path: scopePath,
		}

		if sc != nil {
			// Resolve project names from relative paths
			for _, relProj := range sc.IncludePaths() {
				fullRel := filepath.Join(scopePath, relProj)
				proj := ws.ProjectByPath(fullRel)
				if proj != nil {
					info.Projects = append(info.Projects, proj.Name)
				} else {
					info.Projects = append(info.Projects, fullRel)
				}
			}
			info.Tags = sc.Tags
			info.Groups = sc.Groups
			info.Aliases = sc.ProjectAliases
			if sc.IsLine() {
				info.Line = wsproto.LineTagPattern(scopePath, sc.Line)
			}
		}

		scopes = append(scopes, info)
	}

	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].Path < scopes[j].Path
	})

	return scopes
}

func scopesListJSONL(scopes []ScopeInfo) error {
	for _, s := range scopes {
		data, _ := json.Marshal(s)
		iox.Fprintln(os.Stdout, string(data))
	}
	return nil
}
