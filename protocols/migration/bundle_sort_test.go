package migration

import (
	"reflect"
	"testing"
)

// TestSortOperations pins the canonical operation ordering exported for
// external emitters: kind, then target, then namespace, then orderKey, then
// name, with the up-payload hash breaking any remaining tie.
func TestSortOperations(t *testing.T) {
	op := func(kind OperationKind, target, ns, orderKey, name, hash string) BundleOperation {
		return BundleOperation{
			Kind:      kind,
			Target:    target,
			Namespace: ns,
			OrderKey:  orderKey,
			Name:      name,
			Up:        PayloadRef{Path: name + ".sql", Hash: hash},
		}
	}

	// canonical is in fully-sorted order; each entry differs from its
	// predecessor at exactly one tie-break level.
	canonical := []BundleOperation{
		op(KindDocument, "a", "", "k", "n", "h"), // kind: document < events
		op(KindEvents, "a", "", "k", "n", "h"),   // kind: events < sql
		op(KindSQL, "a", "", "k", "n", "h1"),     // target: a < b
		op(KindSQL, "b", "", "k", "n", "h"),      // namespace: "" < x
		op(KindSQL, "b", "x", "k", "n", "h"),     // orderKey: k < m
		op(KindSQL, "b", "x", "m", "a", "h"),     // name: a < b
		op(KindSQL, "b", "x", "m", "b", "aaa"),   // hash: aaa < bbb
		op(KindSQL, "b", "x", "m", "b", "bbb"),
	}

	// Feed a shuffled copy (fixed permutation, no randomness so the test is
	// deterministic) and assert SortOperations reproduces canonical order.
	perm := []int{5, 0, 7, 2, 4, 1, 6, 3}
	shuffled := make([]BundleOperation, len(canonical))
	for i, p := range perm {
		shuffled[i] = canonical[p]
	}

	SortOperations(shuffled)

	if !reflect.DeepEqual(shuffled, canonical) {
		t.Errorf("SortOperations did not produce canonical order\n got: %+v\nwant: %+v", shuffled, canonical)
	}
}
