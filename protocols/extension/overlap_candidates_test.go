package extension

import (
	"fmt"
	"reflect"
	"testing"
)

// TestOverlapCandidates_HoldsEveryOverlappingPair pins the index against the
// predicate: over refs that nest, alias by case, share a port, cede subpaths
// and sit under several roots, every pair OutputsOverlap reports is a
// candidate, and the candidates come in the order a pairwise scan visits them.
func TestOverlapCandidates_HoldsEveryOverlappingPair(t *testing.T) {
	t.Parallel()
	paths := []string{"", ".gen", ".GEN/client", ".gen/client/a.ts", ".gen/clients", "dist", "Dist/x", "/ws/a", "/ws/a/b", "/WS"}
	var refs []OutputRef
	for index := 0; index < 60; index++ {
		ref := OutputRef{Root: []string{"project", "workspace", "Project"}[index%3], Path: paths[(index*7)%len(paths)]}
		switch {
		case index%11 == 0:
			ref = OutputRef{Root: ref.Root, FromPort: []string{"out", "Out"}[index%2]}
		case index%5 == 0:
			ref.Excludes = []string{paths[(index*3)%len(paths)]}
		}
		refs = append(refs, ref)
	}
	candidates := OverlapCandidates(refs)
	position := make(map[[2]int]int, len(candidates))
	for index, pair := range candidates {
		if pair[0] <= pair[1] {
			t.Fatalf("candidate %v does not name the later ref first", pair)
		}
		if _, seen := position[pair]; seen {
			t.Fatalf("candidate %v appears twice", pair)
		}
		if index > 0 && (candidates[index-1][0] > pair[0] ||
			candidates[index-1][0] == pair[0] && candidates[index-1][1] > pair[1]) {
			t.Fatalf("candidate %v follows %v: not the pairwise scan's order", pair, candidates[index-1])
		}
		position[pair] = index
	}
	overlaps := 0
	for later := 1; later < len(refs); later++ {
		for earlier := 0; earlier < later; earlier++ {
			if !OutputsOverlap(refs[later], refs[earlier]) {
				continue
			}
			overlaps++
			if _, found := position[[2]int{later, earlier}]; !found {
				t.Errorf("refs %d %+v and %d %+v overlap but are not a candidate", later, refs[later], earlier, refs[earlier])
			}
		}
	}
	if overlaps == 0 {
		t.Fatal("the fixture holds no overlap; it no longer exercises the index")
	}
}

// TestOverlapCandidates_IsProportionalToTheRefs pins the cost: thousands of
// disjoint outputs, as a plan of this repository resolves under one root,
// yield only the pairs that can overlap, where a pairwise scan compares every
// one of the n(n-1)/2.
func TestOverlapCandidates_IsProportionalToTheRefs(t *testing.T) {
	t.Parallel()
	const projects = 3000
	refs := make([]OutputRef, 0, projects+2)
	for index := 0; index < projects; index++ {
		refs = append(refs, OutputRef{Root: "/", Path: fmt.Sprintf("/ws/projects/p%d/.putnami/out/test", index)})
	}
	// Two collisions: an ancestor declared after its descendant, and a second
	// spelling of one path.
	refs = append(refs,
		OutputRef{Root: "/", Path: "/ws/projects/p7"},
		OutputRef{Root: "/", Path: "/WS/projects/p9/.putnami/out/test"},
	)
	want := [][2]int{{projects, 7}, {projects + 1, 9}}
	if got := OverlapCandidates(refs); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}
