package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// runtimeComparisonWorkspace is a workspace-level extension /ext that imports
// /lib, and a plain project /app that runs under it.
func runtimeComparisonWorkspace(t *testing.T) *Workspace {
	t.Helper()
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/ext": ""}}}
	ws := NewWorkspace(tmp, cfg, []*Project{
		{ID: "/ext", Name: "ext", Path: "ext", Dependencies: []string{"/lib"}},
		{ID: "/lib", Name: "lib", Path: "lib"},
		{ID: "/app", Name: "app", Path: "app"},
	})
	libFile := filepath.Join(tmp, "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// The selection asks the comparer about the extensions it reached without a
// changed file of their own, walks again with the proof, and keeps the
// answers for its explanation: a consumer of an unchanged runtime is not
// selected, the extension and the library still are.
func TestImpactedSelection_AnUnchangedRuntimeLeavesItsConsumers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "an-unchanged-extension-runtime-leaves-its-consumers")
	ws := runtimeComparisonWorkspace(t)

	before, err := ImpactedSelectionWithOptions(ws, "main", ImpactedSelectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ownerIDs(before.Projects); !slices.Equal(got, []string{"/ext", "/lib", "/app"}) {
		t.Fatalf("without a comparer the change selected %v, want /ext, /lib and /app", got)
	}

	var asked []string
	selection, err := ImpactedSelectionWithOptions(ws, "main", ImpactedSelectionOptions{
		CompareRuntimes: func(candidates []string, diffBase string) []RuntimeComparison {
			asked = candidates
			if diffBase == "" {
				t.Error("the comparer got no diff base")
			}
			return []RuntimeComparison{{Project: "/ext", Unchanged: true}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(asked, "/ext") || slices.Contains(asked, "/lib") {
		t.Errorf("the comparer was asked about %v, want /ext and never the seed /lib", asked)
	}
	if got := ownerIDs(selection.Projects); !slices.Equal(got, []string{"/ext", "/lib"}) {
		t.Errorf("with /ext unchanged the change selected %v, want /ext and /lib", got)
	}
	if line := selection.RuntimeComparisonsExplanation(); line == "" {
		t.Error("the selection does not explain the unchanged runtime")
	}
	if got := selection.TraceRecord().RuntimeComparisons; len(got) != 1 || got[0].Project != "/ext" || !got[0].Unchanged {
		t.Errorf("the trace record carries the comparisons %+v, want /ext unchanged", got)
	}
}

// An answer that is not a proof leaves the selection as it was.
func TestImpactedSelection_ARuntimeThatCannotBeComparedKeepsItsConsumers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "an-unchanged-extension-runtime-leaves-its-consumers")
	ws := runtimeComparisonWorkspace(t)
	selection, err := ImpactedSelectionWithOptions(ws, "main", ImpactedSelectionOptions{
		CompareRuntimes: func([]string, string) []RuntimeComparison {
			return []RuntimeComparison{{Project: "/ext", Reason: "the base runtime is not in the artifact store"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ownerIDs(selection.Projects); !slices.Equal(got, []string{"/ext", "/lib", "/app"}) {
		t.Errorf("an unproven runtime selected %v, want /ext, /lib and /app", got)
	}
}

func ownerIDs(projects []*Project) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return ids
}
