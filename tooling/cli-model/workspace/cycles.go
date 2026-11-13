package workspace

import (
	"fmt"
	"strings"
	"sync"
)

// delegationView answers which extensions delegate their tasks to a build tool
// that orders its own targets. The answer is read once, on the first cycle a
// check meets, since a workspace with no cycle never needs it.
type delegationView struct {
	mu       sync.Mutex
	resolve  func() map[string]bool
	resolved bool
	names    map[string]bool
}

// SetDelegatingExtensions sets how the workspace learns which extensions
// delegate: resolve returns their names, and a workspace-path extension under
// its project ID too. An extension delegates when its manifest declares
// workspace.buildGraphEdges: every task it runs is a build tool's, and that
// tool states and orders the edges between its own targets.
//
// Without it, no extension delegates and every cycle fails the load.
func (ws *Workspace) SetDelegatingExtensions(resolve func() map[string]bool) {
	if ws == nil {
		return
	}
	ws.delegation.mu.Lock()
	defer ws.delegation.mu.Unlock()
	ws.delegation.resolve = resolve
	ws.delegation.resolved = false
	ws.delegation.names = nil
}

// delegatingExtensions returns the names SetDelegatingExtensions resolves.
func (ws *Workspace) delegatingExtensions() map[string]bool {
	ws.delegation.mu.Lock()
	defer ws.delegation.mu.Unlock()
	if !ws.delegation.resolved {
		ws.delegation.resolved = true
		if ws.delegation.resolve != nil {
			ws.delegation.names = ws.delegation.resolve()
		}
	}
	return ws.delegation.names
}

// CycleError returns why the dependency graph holds a cycle this workspace
// refuses, or nil.
//
// A cycle is accepted when every project in its strongly connected component
// runs only delegated tasks: the project names at least one extension, and
// every extension it names delegates. The build tool orders the targets inside
// the component, the scheduler orders the components, and selection follows
// every edge. Any other cycle fails: Putnami would have to order its tasks,
// and a cycle has no order. The message names the cycle and each project that
// keeps it from loading.
func (ws *Workspace) CycleError() error {
	if ws == nil || ws.Graph == nil {
		return nil
	}
	cyclic := ws.Graph.CyclicComponents()
	if len(cyclic) == 0 {
		return nil
	}
	delegating := ws.delegatingExtensions()
	for _, members := range cyclic {
		var reasons []string
		for _, id := range members {
			if reason := ws.undelegatedReason(id, delegating); reason != "" {
				reasons = append(reasons, reason)
			}
		}
		if len(reasons) == 0 {
			continue
		}
		return fmt.Errorf("dependency cycle detected: %s; a cycle loads only when a build tool runs every task of its projects "+
			"(each extension they name declares workspace.buildGraphEdges), and %s",
			strings.Join(ws.Graph.componentCycle(members), " → "), strings.Join(reasons, ", "))
	}
	return nil
}

// undelegatedReason says why a project runs a task no build tool orders, or ""
// when every extension it names delegates.
func (ws *Workspace) undelegatedReason(id string, delegating map[string]bool) string {
	project := ws.ProjectByID(id)
	if project == nil || len(project.Extensions) == 0 {
		return id + " names no extension"
	}
	var ordered []string
	for _, extension := range project.Extensions {
		if !delegating[extension] {
			ordered = append(ordered, extension)
		}
	}
	if len(ordered) == 0 {
		return ""
	}
	return fmt.Sprintf("%s runs %s, whose tasks Putnami orders", id, strings.Join(ordered, ", "))
}
