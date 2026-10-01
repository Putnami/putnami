package shared

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ResolveProjectSelector resolves selector against ws: a leading "/" is tried
// as a project ID first, then the selector is tried as a project name, and
// finally as a path-shaped ID ("/" + trimmed selector).
func ResolveProjectSelector(ws *workspace.Workspace, selector string) *workspace.Project {
	if strings.HasPrefix(selector, "/") {
		if proj := ws.ProjectByID(selector); proj != nil {
			return proj
		}
	}
	if proj := ws.ProjectByName(selector); proj != nil {
		return proj
	}
	pathSelector := "/" + strings.Trim(selector, "/")
	return ws.ProjectByID(pathSelector)
}
