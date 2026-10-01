package darc

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"testing"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
)

// releaseManifest is an attached producer state with no meaning of its own.
type releaseManifest struct {
	Release string
}

// snapshotContract is the shape the protocol requires of a snapshot import: one
// transport, and an explicit consistency block. It carries no local model,
// because an immutable attachment is not a projection.
func snapshotContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:      "cli.verification-observations.v1",
		Version: 1,
		From:    archproto.ExportReference{Domain: "extension-providers", Export: "extension-providers.verification-observations.v1"},
		As:      "cli.spec-gate-observations",
		Mode:    archproto.ModeSnapshot,
		Status:  archproto.StatusActive,
		Facts:   []string{"feature_verification_observations"},
		Transport: &archproto.Transport{
			Kind: archproto.TransportFile, Contract: "putnami.feature-verification.v1",
			Availability: archproto.StatusActive,
		},
		Consistency: &archproto.Consistency{
			MaxStaleness:   "1h",
			OnMissing:      archproto.FailureFailClosed,
			OnStale:        archproto.FailureFailClosed,
			Ordering:       archproto.OrderingNone,
			SourceVersion:  "session_id",
			IdempotencyKey: "task_key",
			LateEvents:     archproto.LateEventReject,
		},
		Justification: "The gate joins the criteria projection with the observations the test jobs emitted in the same session.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// TestSnapshotRefusesAContractItCannotEnforce pins the construction verdict for
// the snapshot mode: the protocol requires one transport and explicit freshness
// behavior, and a component that ran without them would enforce nothing.
func TestSnapshotRefusesAContractItCannotEnforce(t *testing.T) {
	if _, err := NewSnapshot[releaseManifest](snapshotContract(func(i *archproto.Import) {
		i.Transport = nil
	})); err == nil {
		t.Error("a snapshot with no transport was accepted")
	}
	if _, err := NewSnapshot[releaseManifest](snapshotContract(func(i *archproto.Import) {
		i.Consistency = nil
	})); err == nil {
		t.Error("a snapshot with no consistency block was accepted")
	}
	if _, err := NewSnapshot[releaseManifest](snapshotContract(func(i *archproto.Import) {
		i.Mode = archproto.ModeProjection
	})); err == nil {
		t.Error("a projection contract was accepted by the snapshot component")
	}
	if _, err := NewSnapshot[releaseManifest](snapshotContract(func(i *archproto.Import) {
		i.Status = archproto.StatusPlanned
		i.Transport.Availability = archproto.StatusPlanned
	})); !errors.Is(err, ErrNotActive) {
		t.Errorf("err = %v, want ErrNotActive for a planned snapshot", err)
	}
}

// TestSnapshotIsImmutableAndVersionAddressed is the whole point of the mode: a
// version identifies a producer state, so the same version may be redelivered
// but never changed, and a reader that named a version gets that state back.
func TestSnapshotIsImmutableAndVersionAddressed(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-snapshot-version-is-immutable-and-addressable")
	clock := newTestClock()
	snapshot, err := NewSnapshot[releaseManifest](snapshotContract(),
		WithSnapshotClock[releaseManifest](clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	if err := snapshot.Attach("s-1", releaseManifest{Release: "r1"}, clock.now); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Attach("s-2", releaseManifest{Release: "r2"}, clock.now); err != nil {
		t.Fatal(err)
	}

	// A redelivery of the same state is not an error: a carrier that retries
	// must not look like a producer that rewrote history.
	if err := snapshot.Attach("s-1", releaseManifest{Release: "r1"}, clock.now); err != nil {
		t.Errorf("re-attaching identical content failed: %v", err)
	}
	// Different content under the same version IS history being rewritten, and
	// every reader that already resolved s-1 would be wrong.
	if err := snapshot.Attach("s-1", releaseManifest{Release: "rewritten"}, clock.now); !errors.Is(err, ErrImmutable) {
		t.Errorf("err = %v, want ErrImmutable", err)
	}

	record, found := snapshot.At("s-1")
	if !found || record.Value.Release != "r1" {
		t.Errorf("At(s-1) = (%+v, %v), want the state attached under that version", record, found)
	}
	if record.Provenance != "extension-providers.verification-observations.v1" {
		t.Errorf("provenance = %q, want the producer export the import names", record.Provenance)
	}
	if _, found := snapshot.At("s-9"); found {
		t.Error("At returned a version nobody attached")
	}
	if got := snapshot.Versions(); len(got) != 2 || got[0] != "s-1" || got[1] != "s-2" {
		t.Errorf("versions = %v, want both, oldest first", got)
	}

	// An unversioned attach is refused: without a version there is nothing to
	// address, and the mode is version-addressed by definition.
	if err := snapshot.Attach("", releaseManifest{}, clock.now); err == nil {
		t.Error("an unversioned state was attached")
	}
}

// TestSnapshotLatestAppliesTheDeclaredConsistency pins that the LATEST read is
// where the contract's freshness and missing behavior apply — a version-addressed
// read is a caller naming the state it wants, so "this state is old" is not news.
func TestSnapshotLatestAppliesTheDeclaredConsistency(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-snapshot-latest-read-takes-the-declared-consistency")
	clock := newTestClock()
	snapshot, err := NewSnapshot[releaseManifest](snapshotContract(),
		WithSnapshotClock[releaseManifest](clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := snapshot.Latest(context.Background()); found || !errors.Is(err, ErrMissing) {
		t.Fatalf("empty Latest = (%v, %v), want the declared fail-closed miss", found, err)
	}

	if err := snapshot.Attach("s-1", releaseManifest{Release: "r1"}, clock.now); err != nil {
		t.Fatal(err)
	}
	record, found, err := snapshot.Latest(context.Background())
	if !found || err != nil || record.Freshness != FreshnessFresh {
		t.Fatalf("fresh Latest = (%+v, %v, %v)", record, found, err)
	}

	clock.advance(2 * time.Hour)
	record, found, err = snapshot.Latest(context.Background())
	if !errors.Is(err, ErrStale) {
		t.Fatalf("stale Latest err = %v, want ErrStale under a fail-closed contract", err)
	}
	if !found || record.Freshness != FreshnessStale {
		t.Errorf("record = (%+v, %v); a refusal still returns what it refused, stamped", record, found)
	}

	// A version-addressed read of the same state stays available, stamped.
	if addressed, ok := snapshot.At("s-1"); !ok || addressed.Freshness != FreshnessStale {
		t.Errorf("At(s-1) = (%+v, %v), want the state with an honest freshness stamp", addressed, ok)
	}
}

// TestSnapshotLatestFollowsTheVersionOrder pins that "latest" is decided by the
// configured comparator, not by attachment order.
func TestSnapshotLatestFollowsTheVersionOrder(t *testing.T) {
	clock := newTestClock()
	snapshot, err := NewSnapshot[releaseManifest](snapshotContract(),
		WithSnapshotClock[releaseManifest](clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Attach("s-2", releaseManifest{Release: "r2"}, clock.now); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Attach("s-1", releaseManifest{Release: "r1"}, clock.now); err != nil {
		t.Fatal(err)
	}
	record, _, err := snapshot.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Value.Release != "r2" {
		t.Errorf("latest = %q, want the newest version rather than the last attached", record.Value.Release)
	}
}

// TestSnapshotHonoursContextCancellation pins that a canceled caller is not
// answered from a dead request.
func TestSnapshotHonoursContextCancellation(t *testing.T) {
	snapshot, err := NewSnapshot[releaseManifest](snapshotContract())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := snapshot.Latest(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
