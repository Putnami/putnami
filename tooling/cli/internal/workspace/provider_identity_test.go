package workspace

import "testing"

// A recorded provider answer stands on its inputs AND on the implementation
// that produced it: the same inputs read by a rebuilt provider are a
// different answer.
func TestPlanProbe_ReprobesAProviderWhoseImplementationMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := &Snapshot{
		Version: snapshotFormatVersion,
		Providers: []SnapshotProvider{
			{Extension: "@x/same", Implementation: "impl-1"},
			{Extension: "@x/moved", Implementation: "impl-2"},
			{Extension: "@x/legacy"},
			{Extension: "@x/unknown", Implementation: "impl-4"},
		},
	}
	bindings := []ProviderBinding{
		{Scope: ProviderScope{Extension: "@x/same"}, Implementation: "impl-1"},
		{Scope: ProviderScope{Extension: "@x/moved"}, Implementation: "impl-2-rebuilt"},
		{Scope: ProviderScope{Extension: "@x/legacy"}, Implementation: "impl-3"},
		{Scope: ProviderScope{Extension: "@x/unknown"}},
	}

	plan := planProbe(root, existing, bindings, false)
	if plan.fresh {
		t.Fatalf("plan is fresh; want @x/moved and @x/legacy stale: %+v", plan)
	}
	want := map[string]bool{"@x/moved": true, "@x/legacy": true}
	for name, stale := range plan.stale {
		if stale != want[name] {
			t.Errorf("provider %s stale = %v, want %v", name, stale, want[name])
		}
	}
	for name := range want {
		if !plan.stale[name] {
			t.Errorf("provider %s is not stale; its implementation moved (or was never recorded)", name)
		}
	}
	if plan.reason == "" {
		t.Error("a stale plan names no reason")
	}
}

// Unchanged inputs and unchanged implementations carry every recorded answer:
// nothing is re-probed on an ordinary command.
func TestPlanProbe_CarriesEveryProviderWhenNothingMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := &Snapshot{
		Version: snapshotFormatVersion,
		Providers: []SnapshotProvider{
			{Extension: "@x/a", Implementation: "impl-a"},
			{Extension: "@x/b", Implementation: "impl-b"},
		},
	}
	bindings := []ProviderBinding{
		{Scope: ProviderScope{Extension: "@x/a"}, Implementation: "impl-a"},
		{Scope: ProviderScope{Extension: "@x/b"}, Implementation: "impl-b"},
	}
	plan := planProbe(root, existing, bindings, false)
	if !plan.fresh || len(plan.stale) != 0 {
		t.Fatalf("plan = %+v, want fresh with no stale provider", plan)
	}
}

// A rebuilt snapshot whose only difference is a provider's implementation is
// not the recorded one: the record is rewritten even when the answer bytes
// happen to match.
func TestSnapshotStillHolds_SeesAMovedImplementation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := &Snapshot{Version: snapshotFormatVersion, Providers: []SnapshotProvider{{Extension: "@x/a", Digest: "d", Implementation: "impl-1"}}}
	rebuilt := &Snapshot{Version: snapshotFormatVersion, Providers: []SnapshotProvider{{Extension: "@x/a", Digest: "d", Implementation: "impl-2"}}}
	if snapshotStillHolds(root, existing, rebuilt) {
		t.Fatal("snapshot holds across a moved provider implementation")
	}
	rebuilt.Providers[0].Implementation = "impl-1"
	if !snapshotStillHolds(root, existing, rebuilt) {
		t.Fatal("snapshot does not hold for an identical record")
	}
}
