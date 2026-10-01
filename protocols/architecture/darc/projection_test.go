package darc

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"strings"
	"testing"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
)

// The projection invariants of protocols/architecture/README.md, one test per
// clause. Each states the DECLARED behavior it exercises, because the point of
// the component is that the manifest sentence and the code path are the same
// statement.

// workspaceContext is a projected value with no meaning of its own; the
// contract is the subject of every test here.
type workspaceContext struct {
	Region string
}

// projectionContract is the shape the protocol requires of a projection:
// bootstrap and updates, bounded staleness with explicit missing and stale
// behavior, monotone ordering with idempotency and a late-event rule, a local
// model with source identity, provenance, observation time, freshness and one
// writer, a rebuild strategy, and a deletion strategy.
func projectionContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:      "observability.workspace-context.v1",
		Version: 1,
		From:    archproto.ExportReference{Domain: "runtime", Export: "runtime.workspace-binding.v1"},
		As:      "observability.workspace-context",
		Mode:    archproto.ModeProjection,
		Status:  archproto.StatusActive,
		Facts:   []string{"workspace_id", "region", "source_version"},
		Bootstrap: &archproto.Transport{
			Kind: archproto.TransportAPI, Contract: "runtime.workspace-bindings.v1",
			Availability: archproto.StatusActive,
		},
		Updates: &archproto.Transport{
			Kind: archproto.TransportEvent, Contract: "runtime.workspace-binding-changed.v1",
			Availability: archproto.StatusActive,
		},
		Consistency: &archproto.Consistency{
			MaxStaleness:   "5m",
			OnMissing:      archproto.FailureFailClosed,
			OnStale:        archproto.FailureUseStale,
			Ordering:       archproto.OrderingSourceVersion,
			SourceVersion:  "source_version",
			IdempotencyKey: "event_id",
			LateEvents:     archproto.LateEventIgnoreOlder,
		},
		Deletion: &archproto.Deletion{Strategy: archproto.DeletionTombstone, TombstoneField: "deleted_at"},
		LocalModel: &archproto.LocalModel{
			Name:            "observability.workspace-context",
			Kind:            archproto.LocalModelProjection,
			SourceIdentity:  "workspace_id",
			ProjectedFields: []string{"workspace_id", "region", "source_version"},
			ProvenanceField: "source_contract",
			ObservedAtField: "observed_at",
			FreshnessField:  "freshness_state",
			Writer:          "observability.workspace-context-projector",
			Rebuildable:     true,
			Rebuild:         archproto.RebuildBootstrap,
		},
		Justification: "Local routing facts without making Runtime a per-request lookup service.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// testClock is a hand-wound clock, so a freshness bound is exercised by moving
// time rather than by sleeping through it.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

// bootstrapping builds a projection with a bootstrap source and rebuilds it, so
// a test that is about reads does not restate the write path.
func bootstrapping(t *testing.T, contract archproto.Import, clock *testClock, updates ...Update[workspaceContext]) *Projection[workspaceContext] {
	t.Helper()
	projection, err := NewProjection[workspaceContext](contract,
		WithClock[workspaceContext](clock.Now),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return updates, nil }))
	if err != nil {
		t.Fatalf("build the projection: %v", err)
	}
	if err := projection.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild the projection: %v", err)
	}
	return projection
}

// TestProjectionRefusesAContractItCannotEnforce pins the construction verdict:
// the protocol's own validation runs first, and a component never enforces a
// contract the gate would reject.
func TestProjectionRefusesAContractItCannotEnforce(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-component-refuses-a-contract-the-gate-would-reject")
	cases := []struct {
		name   string
		mutate func(*archproto.Import)
		code   string
	}{
		{
			name:   "a projection with no updates transport",
			mutate: func(i *archproto.Import) { i.Updates = nil },
			code:   archproto.ErrorCodeInvalidProjection,
		},
		{
			name:   "a projection whose projected fields are not its imported facts",
			mutate: func(i *archproto.Import) { i.LocalModel.ProjectedFields = []string{"workspace_id"} },
			code:   archproto.ErrorCodeInvalidProjection,
		},
		{
			name:   "an unbounded freshness window",
			mutate: func(i *archproto.Import) { i.Consistency.MaxStaleness = "eventually" },
			code:   archproto.ErrorCodeInvalidConsistency,
		},
		{
			name:   "an active import carried by a planned transport",
			mutate: func(i *archproto.Import) { i.Updates.Availability = archproto.StatusPlanned },
			code:   archproto.ErrorCodeInvalidStatus,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewProjection[workspaceContext](projectionContract(testCase.mutate),
				WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return nil, nil }))
			var refusal *ContractError
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a *ContractError", err)
			}
			found := false
			for _, d := range refusal.Diagnostics {
				if d.Code == testCase.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %v, want one %s", refusal.Diagnostics, testCase.code)
			}
		})
	}

	t.Run("a contract for another access mode", func(t *testing.T) {
		_, err := NewProjection[workspaceContext](projectionContract(func(i *archproto.Import) {
			i.Mode = archproto.ModeQuery
		}))
		if err == nil || !strings.Contains(err.Error(), "enforces projection access") {
			t.Fatalf("err = %v, want a mode refusal", err)
		}
	})

	t.Run("a projection contract that is not live", func(t *testing.T) {
		_, err := NewProjection[workspaceContext](projectionContract(func(i *archproto.Import) {
			i.Status = archproto.StatusPlanned
			i.Bootstrap.Availability = archproto.StatusPlanned
			i.Updates.Availability = archproto.StatusPlanned
		}), WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return nil, nil }))
		if !errors.Is(err, ErrNotActive) {
			t.Fatalf("err = %v, want ErrNotActive for a planned projection", err)
		}
	})
}

// TestProjectionRequiresTheRebuildHooksItDeclares is the "missing bootstrap
// fails closed" case: a projection that declares a rebuild strategy and cannot
// run it would fail at the moment it is needed, which is the moment it is least
// able to.
func TestProjectionRequiresTheRebuildHooksItDeclares(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-declared-rebuild-strategy-needs-its-source-at-construction")
	if _, err := NewProjection[workspaceContext](projectionContract()); err == nil {
		t.Fatal("a bootstrap-rebuildable projection was built with no bootstrap source")
	}

	replayContract := projectionContract(func(i *archproto.Import) {
		i.LocalModel.Rebuild = archproto.RebuildBootstrapAndReplay
	})
	if _, err := NewProjection[workspaceContext](replayContract,
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return nil, nil })); err == nil {
		t.Fatal("a bootstrap-and-replay projection was built with no replay source")
	}

	// The refusal runs in both directions: a hook the strategy does not name is
	// a projection whose code does something its declaration does not say.
	if _, err := NewProjection[workspaceContext](projectionContract(),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return nil, nil }),
		WithReplay(func(context.Context, string) ([]Update[workspaceContext], error) { return nil, nil })); err == nil {
		t.Fatal("a bootstrap-only projection accepted a replay source")
	}
}

// TestProjectionAppliesTheDeclaredMissingBehavior walks the three answers a read
// of an absent fact can give, and pins that the never-rebuilt case says so.
func TestProjectionAppliesTheDeclaredMissingBehavior(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"an-absent-fact-takes-the-declared-missing-behavior")
	clock := newTestClock()

	closed := bootstrapping(t, projectionContract(), clock)
	_, found, err := closed.Get(context.Background(), "ws-1")
	if found || !errors.Is(err, ErrMissing) {
		t.Fatalf("fail-closed read = (%v, %v), want ErrMissing", found, err)
	}

	open := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Consistency.OnMissing = archproto.FailureFailOpen
	}), clock)
	if _, found, err := open.Get(context.Background(), "ws-1"); found || err != nil {
		t.Fatalf("fail-open read = (%v, %v), want a clean miss", found, err)
	}

	unbuilt, err := NewProjection[workspaceContext](projectionContract(),
		WithClock[workspaceContext](clock.Now),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = unbuilt.Get(context.Background(), "ws-1")
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "not been rebuilt") {
		t.Errorf("err = %v, want a miss that says the projection was never built", err)
	}
}

// TestProjectionAppliesTheDeclaredStaleBehavior is the freshness bound: the same
// copy, past the same bound, is stamped under use-stale and refused under
// fail-closed.
func TestProjectionAppliesTheDeclaredStaleBehavior(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-copy-past-the-declared-bound-is-stamped-or-refused")
	clock := newTestClock()
	update := Update[workspaceContext]{
		ID: "ws-1", Value: workspaceContext{Region: "eu"},
		SourceVersion: "2026-09-01T12:00:00Z", IdempotencyKey: "e1", ObservedAt: clock.now,
	}

	useStale := bootstrapping(t, projectionContract(), clock, update)
	record, found, err := useStale.Get(context.Background(), "ws-1")
	if !found || err != nil || record.Freshness != FreshnessFresh {
		t.Fatalf("fresh read = (%+v, %v, %v), want a fresh hit", record, found, err)
	}
	if record.Provenance != "runtime.workspace-binding.v1" {
		t.Errorf("provenance = %q, want the producer export the import names", record.Provenance)
	}

	clock.advance(6 * time.Minute)
	record, found, err = useStale.Get(context.Background(), "ws-1")
	if !found || err != nil {
		t.Fatalf("use-stale read = (%v, %v), want the copy and no error", found, err)
	}
	if record.Freshness != FreshnessStale {
		t.Errorf("freshness = %q, want %q: use-stale returns the copy AND says it is stale",
			record.Freshness, FreshnessStale)
	}

	failClosed := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Consistency.OnStale = archproto.FailureFailClosed
	}), clock, update)
	record, found, err = failClosed.Get(context.Background(), "ws-1")
	if !errors.Is(err, ErrStale) {
		t.Fatalf("fail-closed read err = %v, want ErrStale", err)
	}
	if !found || record.Freshness != FreshnessStale {
		t.Errorf("record = (%+v, %v); a refusal still returns what it refused, stamped", record, found)
	}
}

// TestProjectionOrdersAndDeduplicatesUpdates covers the three consistency
// clauses that govern the write path: ordering, idempotency, and late events.
func TestProjectionOrdersAndDeduplicatesUpdates(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"updates-are-ordered-deduplicated-and-late-handled-as-declared")
	clock := newTestClock()
	projection := bootstrapping(t, projectionContract(), clock)
	writer, err := projection.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first := Update[workspaceContext]{ID: "ws-1", Value: workspaceContext{Region: "eu"},
		SourceVersion: "2026-09-01T12:00:00Z", IdempotencyKey: "e1", ObservedAt: clock.now}
	if applied, err := writer.Apply(ctx, first); !applied || err != nil {
		t.Fatalf("first apply = (%v, %v), want applied", applied, err)
	}

	// Idempotency: the same delivery again changes nothing and is not an error.
	// A retrying transport must not read as a broken producer.
	if applied, err := writer.Apply(ctx, first); applied || err != nil {
		t.Errorf("repeat apply = (%v, %v), want a silent no-op", applied, err)
	}

	// ignore-older: an update behind the local source version is absorbed.
	older := Update[workspaceContext]{ID: "ws-1", Value: workspaceContext{Region: "us"},
		SourceVersion: "2026-09-01T11:00:00Z", IdempotencyKey: "e0", ObservedAt: clock.now}
	if applied, err := writer.Apply(ctx, older); applied || err != nil {
		t.Errorf("late apply = (%v, %v), want a silent drop under ignore-older", applied, err)
	}
	record, _, _ := projection.Get(ctx, "ws-1")
	if record.Value.Region != "eu" {
		t.Errorf("region = %q, want the newer copy to survive a late update", record.Value.Region)
	}

	// reject: the same late update is an error under the other strategy.
	rejecting := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Consistency.LateEvents = archproto.LateEventReject
	}), clock, first)
	rejectWriter, err := rejecting.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rejectWriter.Apply(ctx, older); !errors.Is(err, ErrLateUpdate) {
		t.Errorf("err = %v, want ErrLateUpdate under a reject strategy", err)
	}

	// apply: the same late update overwrites under the third strategy.
	applying := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Consistency.LateEvents = archproto.LateEventApply
	}), clock, first)
	applyWriter, err := applying.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applyWriter.Apply(ctx, older); !applied || err != nil {
		t.Errorf("late apply = (%v, %v), want it applied under an apply strategy", applied, err)
	}
}

// TestProjectionVersionOrderIsReplaceable pins the trap the default carries: a
// lexical comparator mis-orders unpadded decimal counters, so a contract using
// them must supply its own.
func TestProjectionVersionOrderIsReplaceable(t *testing.T) {
	clock := newTestClock()
	numeric := func(left, right string) int {
		if len(left) != len(right) {
			return len(left) - len(right)
		}
		switch {
		case left < right:
			return -1
		case left > right:
			return 1
		}
		return 0
	}
	projection, err := NewProjection[workspaceContext](projectionContract(),
		WithClock[workspaceContext](clock.Now),
		WithVersionOrder[workspaceContext](numeric),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) {
			return []Update[workspaceContext]{{ID: "ws-1", SourceVersion: "9", IdempotencyKey: "e9"}}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	writer, err := projection.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := writer.Apply(context.Background(), Update[workspaceContext]{
		ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "10", IdempotencyKey: "e10",
	})
	if !applied || err != nil {
		t.Fatalf("apply = (%v, %v); version 10 is newer than 9 under the supplied comparator", applied, err)
	}
}

// TestProjectionHandsOutOneWriter is the single-writer invariant: the local
// model names one writer, and a second component has to change the declaration
// before it can write.
func TestProjectionHandsOutOneWriter(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"the-local-model-names-one-writer-and-a-second-is-refused")
	projection := bootstrapping(t, projectionContract(), newTestClock())

	if _, err := projection.Writer("observability.some-other-component"); !errors.Is(err, ErrNotTheWriter) {
		t.Fatalf("err = %v, want ErrNotTheWriter for a name the local model does not declare", err)
	}
	if _, err := projection.Writer("observability.workspace-context-projector"); err != nil {
		t.Fatalf("the declared writer was refused: %v", err)
	}
	if _, err := projection.Writer("observability.workspace-context-projector"); !errors.Is(err, ErrWriterClaimed) {
		t.Fatalf("err = %v, want ErrWriterClaimed for a second handle", err)
	}
}

// TestProjectionRebuildReplacesAndReplayResumes pins both rebuild strategies:
// bootstrap replaces what is held, and replay resumes from the newest source
// version rather than restarting.
func TestProjectionRebuildReplacesAndReplayResumes(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"rebuild-replaces-and-replay-resumes-from-the-newest-version")
	clock := newTestClock()
	generation := 0
	var replayedSince []string

	projection, err := NewProjection[workspaceContext](
		projectionContract(func(i *archproto.Import) {
			i.LocalModel.Rebuild = archproto.RebuildBootstrapAndReplay
		}),
		WithClock[workspaceContext](clock.Now),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) {
			generation++
			if generation == 1 {
				return []Update[workspaceContext]{
					{ID: "ws-1", Value: workspaceContext{Region: "eu"}, SourceVersion: "v1", IdempotencyKey: "b1"},
					{ID: "ws-2", Value: workspaceContext{Region: "us"}, SourceVersion: "v1", IdempotencyKey: "b2"},
				}, nil
			}
			return []Update[workspaceContext]{
				{ID: "ws-3", Value: workspaceContext{Region: "ap"}, SourceVersion: "v9", IdempotencyKey: "b3"},
			}, nil
		}),
		WithReplay(func(_ context.Context, since string) ([]Update[workspaceContext], error) {
			replayedSince = append(replayedSince, since)
			return nil, nil
		}))
	if err != nil {
		t.Fatal(err)
	}

	if err := projection.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(projection.All()); got != 2 {
		t.Fatalf("records = %d, want the two the first bootstrap produced", got)
	}
	if len(replayedSince) != 1 || replayedSince[0] != "v1" {
		t.Errorf("replay resumed from %v, want the newest version held", replayedSince)
	}

	// A second rebuild REPLACES: the records the first bootstrap produced are
	// gone, which is what makes a rebuild a recovery rather than a merge.
	if err := projection.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := projection.All()
	if len(records) != 1 || records[0].ID != "ws-3" {
		t.Errorf("records = %+v, want only what the second bootstrap produced", records)
	}
	if len(replayedSince) != 2 || replayedSince[1] != "v9" {
		t.Errorf("replay resumed from %v, want the newest version of the new generation", replayedSince)
	}
}

// TestProjectionRebuildRefusesWhenTheStrategyIsNotApplicable pins that a
// projection which declares no rebuild path cannot be rebuilt. The protocol
// already rejects such a projection, so this is reachable only by constructing
// the component around the protocol — and it still refuses.
func TestProjectionRebuildRefusesWhenTheStrategyIsNotApplicable(t *testing.T) {
	projection := bootstrapping(t, projectionContract(), newTestClock())
	projection.contract.LocalModel.Rebuild = archproto.RebuildNotApplicable
	if err := projection.Rebuild(context.Background()); err == nil {
		t.Fatal("a not-applicable rebuild strategy was rebuilt anyway")
	}
}

// TestProjectionAppliesTheDeclaredDeletionStrategy walks the four strategies. A
// tombstone is the one with a runtime obligation beyond removal: the marker is
// what lets a later reader tell "deleted" from "never seen".
func TestProjectionAppliesTheDeclaredDeletionStrategy(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"deletion-follows-the-declared-strategy")
	clock := newTestClock()
	seed := Update[workspaceContext]{ID: "ws-1", Value: workspaceContext{Region: "eu"},
		SourceVersion: "v1", IdempotencyKey: "e1", ObservedAt: clock.now}
	ctx := context.Background()

	tombstoned := bootstrapping(t, projectionContract(), clock, seed)
	writer, err := tombstoned.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
		t.Fatalf("tombstone delete: %v", err)
	}
	if _, found, err := tombstoned.Get(ctx, "ws-1"); found || !errors.Is(err, ErrMissing) {
		t.Errorf("read after tombstone = (%v, %v), want the declared missing behavior", found, err)
	}
	if got := len(tombstoned.All()); got != 0 {
		t.Errorf("All() returned %d live record(s) after a tombstone", got)
	}
	tombstoned.mu.RLock()
	marked := tombstoned.records["ws-1"]
	tombstoned.mu.RUnlock()
	if !marked.Deleted || marked.DeletedAt.IsZero() {
		t.Errorf("record = %+v, want a retained tombstone with its marker set", marked)
	}

	hard := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionHardDelete}
	}), clock, seed)
	hardWriter, err := hard.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if err := hardWriter.Delete(ctx, "ws-1", "v2"); err != nil {
		t.Fatal(err)
	}
	hard.mu.RLock()
	_, retained := hard.records["ws-1"]
	hard.mu.RUnlock()
	if retained {
		t.Error("a hard-delete kept the record")
	}

	retain := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionRetain}
	}), clock, seed)
	retainWriter, err := retain.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if err := retainWriter.Delete(ctx, "ws-1", "v2"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := retain.Get(ctx, "ws-1"); !found {
		t.Error("a retain strategy dropped the copy; retaining it IS the strategy")
	}

	none := bootstrapping(t, projectionContract(func(i *archproto.Import) {
		i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionNotApplicable}
	}), clock, seed)
	noneWriter, err := none.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	if err := noneWriter.Delete(ctx, "ws-1", "v2"); !errors.Is(err, ErrDeletionNotApplicable) {
		t.Errorf("err = %v, want ErrDeletionNotApplicable", err)
	}
}

// TestProjectionDeletionAdvancesTheOrderingWatermark pins the ordering state a
// deletion must leave behind. Hard-delete has no visible record to carry the
// version, while retain intentionally keeps a value from the earlier version;
// both still have to reject a delayed pre-deletion write.
func TestProjectionDeletionAdvancesTheOrderingWatermark(t *testing.T) {
	clock := newTestClock()
	seed := Update[workspaceContext]{ID: "ws-1", Value: workspaceContext{Region: "eu"},
		SourceVersion: "v1", IdempotencyKey: "e1", ObservedAt: clock.now}
	ctx := context.Background()

	t.Run("tombstone before value", func(t *testing.T) {
		projection := bootstrapping(t, projectionContract(), clock)
		writer, err := projection.Writer("observability.workspace-context-projector")
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatal(err)
		}
		projection.mu.RLock()
		marker, found := projection.records["ws-1"]
		projection.mu.RUnlock()
		if !found || !marker.Deleted || marker.SourceVersion != "v2" {
			t.Fatalf("marker = (%+v, %v), want the out-of-order tombstone retained", marker, found)
		}
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatalf("repeated tombstone: %v", err)
		}
		projection.mu.RLock()
		repeated := projection.records["ws-1"]
		projection.mu.RUnlock()
		if !repeated.DeletedAt.Equal(marker.DeletedAt) {
			t.Fatalf("repeated tombstone changed DeletedAt from %s to %s", marker.DeletedAt, repeated.DeletedAt)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v1", IdempotencyKey: "late-e1",
		}); applied || err != nil {
			t.Fatalf("late apply after tombstone = (%v, %v), want a silent drop", applied, err)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v2", IdempotencyKey: "same-e2",
		}); applied || err != nil {
			t.Fatalf("same-version apply after tombstone = (%v, %v), want an idempotent drop", applied, err)
		}
		if _, found, _ := projection.Get(ctx, "ws-1"); found {
			t.Error("a delayed older write resurrected a record deleted before it arrived")
		}
	})

	t.Run("hard-delete", func(t *testing.T) {
		projection := bootstrapping(t, projectionContract(func(i *archproto.Import) {
			i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionHardDelete}
		}), clock, seed)
		writer, err := projection.Writer("observability.workspace-context-projector")
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatal(err)
		}
		// Redelivery of the same deletion is idempotent even though the record is
		// already absent.
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatalf("repeated delete: %v", err)
		}
		if got := projection.newestVersion(); got != "v2" {
			t.Fatalf("newest version = %q, want the hard-delete watermark", got)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v1", IdempotencyKey: "late-e1",
		}); applied || err != nil {
			t.Fatalf("late apply after hard-delete = (%v, %v), want a silent drop", applied, err)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v2", IdempotencyKey: "same-e2",
		}); applied || err != nil {
			t.Fatalf("same-version apply after hard-delete = (%v, %v), want an idempotent drop", applied, err)
		}
		if _, found, _ := projection.Get(ctx, "ws-1"); found {
			t.Error("a delayed older write resurrected the hard-deleted record")
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "ap"}, SourceVersion: "v3", IdempotencyKey: "e3",
		}); !applied || err != nil {
			t.Fatalf("newer apply after hard-delete = (%v, %v), want a legitimate recreation", applied, err)
		}
	})

	t.Run("retain", func(t *testing.T) {
		projection := bootstrapping(t, projectionContract(func(i *archproto.Import) {
			i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionRetain}
		}), clock, seed)
		writer, err := projection.Writer("observability.workspace-context-projector")
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatal(err)
		}
		if err := writer.Delete(ctx, "ws-1", "v2"); err != nil {
			t.Fatalf("repeated delete: %v", err)
		}
		if got := projection.newestVersion(); got != "v2" {
			t.Fatalf("newest version = %q, want the retain watermark", got)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v1", IdempotencyKey: "late-e1",
		}); applied || err != nil {
			t.Fatalf("late apply after retain = (%v, %v), want a silent drop", applied, err)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "us"}, SourceVersion: "v2", IdempotencyKey: "same-e2",
		}); applied || err != nil {
			t.Fatalf("same-version apply after retain = (%v, %v), want an idempotent drop", applied, err)
		}
		record, found, err := projection.Get(ctx, "ws-1")
		if !found || err != nil || record.Value.Region != "eu" || record.SourceVersion != "v1" {
			t.Fatalf("retained record = (%+v, %v, %v), want the original visible copy", record, found, err)
		}
		if applied, err := writer.Apply(ctx, Update[workspaceContext]{
			ID: "ws-1", Value: workspaceContext{Region: "ap"}, SourceVersion: "v3", IdempotencyKey: "e3",
		}); !applied || err != nil {
			t.Fatalf("newer apply after retain = (%v, %v), want the new producer state", applied, err)
		}
	})
}

// TestProjectionHonoursContextCancellation pins that the read and write paths
// respect a canceled caller rather than answering from a dead request.
func TestProjectionHonoursContextCancellation(t *testing.T) {
	projection := bootstrapping(t, projectionContract(), newTestClock())
	writer, err := projection.Writer("observability.workspace-context-projector")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := projection.Get(ctx, "ws-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get err = %v, want context.Canceled", err)
	}
	if _, err := writer.Apply(ctx, Update[workspaceContext]{ID: "ws-1"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Apply err = %v, want context.Canceled", err)
	}
	if err := writer.Delete(ctx, "ws-1", "v2"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete err = %v, want context.Canceled", err)
	}
}
