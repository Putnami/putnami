package compose

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Project is one project of the workspace a composition plans against: what
// the job context records of it (job.ProjectRef) and its authored
// putnami.json.
type Project struct {
	// ID is the project's workspace id, such as /apps/web.
	ID string
	// Name is the project's resolved name.
	Name string
	// Path is the project directory relative to the workspace root.
	Path string
	// Type is the project's resolved classification.
	Type string
	// RunsWith names the workloads the project runs with, as its putnami.json
	// declares them, each by project name or id.
	RunsWith []string
	// Config is the project's authored putnami.json; nil reads as empty.
	Config *wsproto.ProjectConfig
}

// Workspace is the membership a composition plans against.
type Workspace struct {
	// Root is the workspace root directory.
	Root string
	// Config is the workspace configuration, whose disable.jobs a member
	// honors.
	Config *wsproto.Config
	// Projects are every project of the workspace.
	Projects []*Project
}

// NewWorkspace returns the workspace at root whose complete membership is
// projects, the job context's WorkspaceProjects. A nil cfg is an empty
// configuration. A project's authored putnami.json that does not decode is an
// error: a member's runsWith or disable.jobs would otherwise read as absent.
func NewWorkspace(root string, cfg *wsproto.Config, projects []job.ProjectRef) (*Workspace, error) {
	if cfg == nil {
		cfg = &wsproto.Config{}
	}
	ws := &Workspace{Root: root, Config: cfg, Projects: make([]*Project, 0, len(projects))}
	for _, ref := range projects {
		config := &wsproto.ProjectConfig{}
		if raw := bytes.TrimSpace(ref.Config); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if err := json.Unmarshal(raw, config); err != nil {
				return nil, fmt.Errorf("the job context's configuration of %s does not decode: %w", ref.ID, err)
			}
		}
		ws.Projects = append(ws.Projects, &Project{
			ID: ref.ID, Name: ref.Name, Path: ref.Path, Type: ref.Type,
			RunsWith: config.RunsWith, Config: config,
		})
	}
	return ws, nil
}

// ProjectByID returns the project whose id is id, or nil.
func (ws *Workspace) ProjectByID(id string) *Project {
	for _, project := range ws.Projects {
		if project.ID == id {
			return project
		}
	}
	return nil
}

// ProjectByName returns the project whose name is name, or nil.
func (ws *Workspace) ProjectByName(name string) *Project {
	for _, project := range ws.Projects {
		if project.Name == name {
			return project
		}
	}
	return nil
}
