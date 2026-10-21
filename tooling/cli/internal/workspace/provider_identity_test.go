package workspace

import (
	"strings"
	"testing"
)

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

// A recorded provider also records what its adapter declares. A record whose
// install inputs or language differ from the declaration is re-asked, even
// when its implementation identity did not move; one that records the same
// set in another order, or an empty set for no declaration, is not.
func TestPlanProbe_ReprobesAProviderWhoseDeclarationMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := &Snapshot{
		Version: snapshotFormatVersion,
		Providers: []SnapshotProvider{
			{Extension: "@x/same", Implementation: "impl", InstallInputs: []string{"a.lock"}, Language: "go"},
			{Extension: "@x/reordered", Implementation: "impl", InstallInputs: []string{"b.lock", "a.lock", "a.lock"}},
			{Extension: "@x/empty", Implementation: "impl", InstallInputs: []string{}},
			{Extension: "@x/legacy", Implementation: "impl"},
			{Extension: "@x/inputs", Implementation: "impl", InstallInputs: []string{"bun.lock"}},
			{Extension: "@x/language", Implementation: "impl", Language: "typescript"},
			{Extension: "@x/dropped", Implementation: "impl", InstallInputs: []string{"go.work"}, Language: "go"},
		},
	}
	bindings := []ProviderBinding{
		{Scope: ProviderScope{Extension: "@x/same", InstallInputs: []string{"a.lock"}, Language: "go"}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/reordered", InstallInputs: []string{"a.lock", "b.lock"}}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/empty"}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/legacy", InstallInputs: []string{"uv.lock"}, Language: "python"}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/inputs", InstallInputs: []string{"bun.lock", "bun.lockb"}}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/language", Language: "javascript"}, Implementation: "impl"},
		{Scope: ProviderScope{Extension: "@x/dropped"}, Implementation: "impl"},
	}

	plan := planProbe(root, existing, bindings, false)
	if plan.fresh {
		t.Fatalf("plan is fresh; want every provider whose declaration moved stale: %+v", plan)
	}
	want := map[string]bool{"@x/legacy": true, "@x/inputs": true, "@x/language": true, "@x/dropped": true}
	for _, binding := range bindings {
		name := binding.Scope.Extension
		if plan.stale[name] != want[name] {
			t.Errorf("provider %s stale = %v, want %v", name, plan.stale[name], want[name])
		}
	}
	if !strings.Contains(plan.reason, "install inputs or language changed") {
		t.Errorf("reason = %q, want it to name the changed declaration", plan.reason)
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
