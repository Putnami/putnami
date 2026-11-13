package workspace

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// delegatedCycleWorkspace is buildbuddy's shape: /proto and /proto/api/v1
// depend on each other, /proto depends on /third outside the cycle, and
// /server depends on /proto/api/v1. Every project names cycleExtension except
// /proto, which names protoExtensions.
func delegatedCycleWorkspace(protoExtensions ...string) *Workspace {
	delegated := []string{"@fixture/delegated"}
	return NewWorkspace("", nil, []*Project{
		{ID: "/third", Name: "third", Path: "third", Extensions: delegated},
		{ID: "/proto", Name: "proto", Path: "proto", Extensions: protoExtensions, Dependencies: []string{"/proto/api/v1", "/third"}},
		{ID: "/proto/api/v1", Name: "v1", Path: "proto/api/v1", Extensions: delegated, Dependencies: []string{"/proto"}},
		{ID: "/server", Name: "server", Path: "server", Extensions: delegated, Dependencies: []string{"/proto/api/v1"}},
	})
}

func delegating(names ...string) func() map[string]bool {
	return func() map[string]bool {
		set := make(map[string]bool, len(names))
		for _, name := range names {
			set[name] = true
		}
		return set
	}
}

func TestDependencyGraph_ComponentsGroupTheProjectsOfACycle(t *testing.T) {
	ws := delegatedCycleWorkspace("@fixture/delegated")
	g := ws.Graph

	if got, want := g.CyclicComponents(), [][]string{{"/proto", "/proto/api/v1"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("cyclic components = %v, want %v", got, want)
	}
	for id, want := range map[string][]string{
		"/proto":        {"/proto", "/proto/api/v1"},
		"/proto/api/v1": {"/proto", "/proto/api/v1"},
		"/server":       {"/server"},
		"/missing":      nil,
	} {
		if got := g.ComponentOf(id); !slices.Equal(got, want) {
			t.Errorf("ComponentOf(%s) = %v, want %v", id, got, want)
		}
	}
	// A member waits for what the component depends on outside it, and for
	// no peer; a project in no cycle waits for its dependencies.
	for id, want := range map[string][]string{
		"/proto":        {"/third"},
		"/proto/api/v1": {"/third"},
		"/server":       {"/proto/api/v1"},
		"/third":        {},
	} {
		if got := g.UpstreamOf(id); !slices.Equal(got, want) {
			t.Errorf("UpstreamOf(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestDependencyGraph_ASelfDependencyIsACycle(t *testing.T) {
	g := BuildGraph([]*Project{{ID: "/a", Name: "a", Path: "a", Dependencies: []string{"/a"}}})
	if got := g.CyclicComponents(); len(got) != 1 || !slices.Equal(got[0], []string{"/a"}) {
		t.Errorf("cyclic components = %v, want [[/a]]", got)
	}
	if got := g.UpstreamOf("/a"); len(got) != 0 {
		t.Errorf("UpstreamOf(/a) = %v, want none: a project never waits for itself", got)
	}
}

func TestCycleError_ACycleAmongDelegatedProjectsLoads(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "a-cycle-among-delegated-projects-loads")
	ws := delegatedCycleWorkspace("@fixture/delegated")
	ws.SetDelegatingExtensions(delegating("@fixture/delegated"))
	if err := ws.CycleError(); err != nil {
		t.Fatalf("CycleError = %v, want nil: a build tool runs every task of the cycle", err)
	}
}

func TestCycleError_ACycleWithATaskPutnamiOrdersFails(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "a-declared-cycle-is-reported")
	for _, tc := range []struct {
		name       string
		extensions []string
		resolve    func() map[string]bool
		reason     string
	}{
		{name: "an ordered extension", extensions: []string{"@fixture/delegated", "@fixture/go"},
			resolve: delegating("@fixture/delegated"), reason: "/proto runs @fixture/go, whose tasks Putnami orders"},
		{name: "no extension", resolve: delegating("@fixture/delegated"), reason: "/proto names no extension"},
		{name: "no delegation known", extensions: []string{"@fixture/delegated"},
			reason: "/proto runs @fixture/delegated, whose tasks Putnami orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := delegatedCycleWorkspace(tc.extensions...)
			if tc.resolve != nil {
				ws.SetDelegatingExtensions(tc.resolve)
			}
			err := ws.CycleError()
			if err == nil {
				t.Fatal("CycleError = nil, want the cycle refused")
			}
			for _, want := range []string{"dependency cycle detected: /proto → /proto/api/v1 → /proto", tc.reason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestCycleError_AWorkspaceWithNoCycleNeverReadsTheExtensions(t *testing.T) {
	ws := NewWorkspace("", nil, []*Project{
		{ID: "/a", Name: "a", Path: "a", Dependencies: []string{"/b"}},
		{ID: "/b", Name: "b", Path: "b"},
	})
	ws.SetDelegatingExtensions(func() map[string]bool {
		t.Error("an acyclic workspace read the extension manifests")
		return nil
	})
	if err := ws.CycleError(); err != nil {
		t.Fatalf("CycleError = %v", err)
	}
}

// A change to one project of a cycle selects the whole cycle and what depends
// on it, since every member reaches every other over a dependency edge.
func TestChangeImpact_AChangeInACycleSelectsTheWholeComponent(t *testing.T) {
	ws := delegatedCycleWorkspace("@fixture/delegated")
	for _, changed := range []string{"proto/api/v1/service.proto", "proto/BUILD"} {
		selected, _ := ChangeImpact(ws, []string{changed}, ChangeImpactOptions{})
		var got []string
		for _, p := range selected {
			got = append(got, p.ID)
		}
		slices.Sort(got)
		if want := []string{"/proto", "/proto/api/v1", "/server"}; !slices.Equal(got, want) {
			t.Errorf("%s selects %v, want %v", changed, got, want)
		}
	}
}
