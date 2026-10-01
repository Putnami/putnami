package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ProjectsList lists all projects in the workspace with their paths and tags.
func ProjectsList(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	projects := sortedProjects(ws.Projects)

	if outputFormat == "jsonl" {
		return projectsListJSONL(projects)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", "PROJECT", "NAME", "TAGS")
	iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", "-------", "----", "----")

	for _, p := range projects {
		tags := "-"
		if len(p.Tags) > 0 {
			tags = joinTags(p.Tags)
		}
		iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", p.ID, p.Name, tags)
	}
	iox.Fprintf(os.Stdout, "\n  %d projects\n\n", len(projects))
	return nil
}

// ProjectsDescribe shows detailed information about a specific project.
func ProjectsDescribe(wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	if len(args) < 1 {
		return cmderr.Usagef("project name required: projects describe <name>")
	}
	projectName := args[0]

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	proj := ws.ProjectByID(projectName)
	if proj == nil {
		proj = ws.ProjectByName(projectName)
	}
	if proj == nil {
		return cmderr.Classify(fmt.Errorf("project not found: %s", projectName), cmderr.ErrNotFound)
	}

	if outputFormat == "jsonl" {
		return projectDescribeJSONL(ws, proj)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Project:      %s\n", proj.ID)
	iox.Fprintf(os.Stdout, "  Name:         %s\n", proj.Name)
	iox.Fprintf(os.Stdout, "  Path:         %s\n", proj.Path)
	if proj.Version != "" {
		iox.Fprintf(os.Stdout, "  Version:      %s\n", proj.Version)
	}
	if len(proj.Tags) > 0 {
		iox.Fprintf(os.Stdout, "  Tags:         %s\n", joinTags(proj.Tags))
	}

	if len(proj.Dependencies) > 0 {
		iox.Fprintf(os.Stdout, "  Dependencies: %s\n", strings.Join(proj.Dependencies, ", "))
	}

	if len(proj.Extensions) > 0 {
		iox.Fprintf(os.Stdout, "  Extensions:   %s\n", strings.Join(proj.Extensions, ", "))
	}

	if len(proj.Publish) > 0 {
		iox.Fprintf(os.Stdout, "  Publish:      %s\n", strings.Join(proj.Publish, ", "))
	}

	// Show dependents
	dependents := ws.Graph.DependentsOf(proj.ID)
	if len(dependents) > 0 {
		iox.Fprintf(os.Stdout, "  Dependents:   %s\n", strings.Join(dependents, ", "))
	}

	// Declared `jobs` entries are listed, not described: the key is accepted by
	// the parser and then ignored, so printing a kind/command would advertise an
	// execution contract that no plan ever honors. Reported on presence, like the
	// loader warning — an empty block still declares the ignored surface, and
	// describe is where an operator looks to find out why their config does
	// nothing.
	if proj.Config != nil && proj.Config.Jobs != nil {
		jobNames := make([]string, 0, len(proj.Config.Jobs))
		for jobName := range proj.Config.Jobs {
			jobNames = append(jobNames, jobName)
		}
		sort.Strings(jobNames)
		if len(jobNames) == 0 {
			iox.Fprintln(os.Stdout, "  Jobs:         (declared but empty; ignored either way)")
		} else {
			iox.Fprintf(os.Stdout, "  Jobs:         %s (declared but ignored)\n", strings.Join(jobNames, ", "))
		}
	}

	iox.Fprintln(os.Stdout)
	return nil
}

// --- JSONL helpers ---

func projectsListJSONL(projects []*workspace.Project) error {
	type projectEntry struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Version      string   `json:"version,omitempty"`
		Path         string   `json:"path"`
		Tags         []string `json:"tags,omitempty"`
		Dependencies []string `json:"dependencies,omitempty"`
	}

	for _, p := range projects {
		entry := projectEntry{
			ID:           p.ID,
			Name:         p.Name,
			Version:      p.Version,
			Path:         p.Path,
			Tags:         p.Tags,
			Dependencies: p.Dependencies,
		}
		data, _ := json.Marshal(entry)
		iox.Fprintln(os.Stdout, string(data))
	}
	return nil
}

func sortedProjects(projects []*workspace.Project) []*workspace.Project {
	sorted := append([]*workspace.Project(nil), projects...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})
	return sorted
}

func projectDescribeJSONL(ws *workspace.Workspace, proj *workspace.Project) error {
	type projectDetail struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Version      string   `json:"version,omitempty"`
		Path         string   `json:"path"`
		Tags         []string `json:"tags,omitempty"`
		Dependencies []string `json:"dependencies,omitempty"`
		Dependents   []string `json:"dependents,omitempty"`
		Extensions   []string `json:"extensions,omitempty"`
		Publish      []string `json:"publish,omitempty"`
	}

	detail := projectDetail{
		ID:           proj.ID,
		Name:         proj.Name,
		Version:      proj.Version,
		Path:         proj.Path,
		Tags:         proj.Tags,
		Dependencies: proj.Dependencies,
		Dependents:   ws.Graph.DependentsOf(proj.ID),
		Extensions:   proj.Extensions,
		Publish:      proj.Publish,
	}

	data, _ := json.Marshal(detail)
	iox.Fprintln(os.Stdout, string(data))
	return nil
}
