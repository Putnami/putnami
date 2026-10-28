package mapgen

import (
	"go.putnami.dev/cli/model/workspace"
)

// ResolveMap renders the complete map in memory and writes NOTHING. It builds
// every live project's fragment from its inputs on disk, so the render is
// always complete and always current.
func ResolveMap(ws *workspace.Workspace) (*RepoMap, error) {
	live := LiveProjects(ws)
	fragments := make(map[string]*Fragment, len(live))
	for _, p := range live {
		fragment, err := BuildFragment(ws.Root, p)
		if err != nil {
			return nil, err
		}
		fragments[p.ID] = fragment
	}
	return Reduce(ws, fragments)
}
