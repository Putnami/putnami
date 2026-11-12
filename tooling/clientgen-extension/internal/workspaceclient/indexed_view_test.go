package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// indexedView reads the disk and finds the members in the project index a
// Putnami run records under .putnami. Tests build their fixtures with it; the
// check reads the committed view.
func indexedView(workspaceRoot string) workspaceView {
	projects, err := indexedProjectPaths(workspaceRoot)
	return workspaceView{workspaceFiles: workspaceFiles{root: workspaceRoot}, projects: projects, membershipErr: err}
}

type workspaceProjectIndex struct {
	Version  int `json:"version"`
	Projects []struct {
		Path string `json:"path"`
	} `json:"projects"`
}

func indexedProjectPaths(workspaceRoot string) ([]string, error) {
	path := filepath.Join(workspaceRoot, ".putnami", "workspace-index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Putnami workspace project index: %w", err)
	}
	var index workspaceProjectIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse Putnami workspace project index: %w", err)
	}
	if index.Version <= 0 || index.Projects == nil {
		return nil, fmt.Errorf("putnami workspace project index is incomplete; run `putnami projects sync`")
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(index.Projects))
	for _, project := range index.Projects {
		clean, pathErr := safeInventoryProject(project.Path)
		if pathErr != nil || clean != project.Path || seen[project.Path] {
			return nil, fmt.Errorf("putnami workspace project index contains invalid or duplicate path %q", project.Path)
		}
		seen[project.Path] = true
		paths = append(paths, project.Path)
	}
	sort.Strings(paths)
	return paths, nil
}
