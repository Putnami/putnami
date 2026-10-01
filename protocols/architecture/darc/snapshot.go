package darc

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
)

// Snapshot is an immutable, version-addressed copy of another domain's facts.
//
// It is the mode between a reference and a projection: the consumer attaches a
// whole producer state under an exact version and reads it back by that version.
// Nothing is edited in place — a new producer state is a new version — which is
// what makes a snapshot safe to hold across a request without a freshness race
// inside it.
//
// The declared consistency block still governs the LATEST read: a snapshot older
// than `consistency.maxStaleness` takes the declared stale behavior, and an
// absent one takes the declared missing behavior. Attaching is the consumer's
// job over whichever transport the contract declares.
//
// A Snapshot is safe for concurrent use.
type Snapshot[T any] struct {
	contract archproto.Import
	bound    time.Duration
	order    VersionOrder
	clock    func() time.Time

	mu       sync.RWMutex
	versions map[string]Record[T]
	latest   string
}

// SnapshotOption configures a snapshot at construction.
type SnapshotOption[T any] func(*Snapshot[T])

// WithSnapshotVersionOrder replaces the version comparator that decides which
// attached version is the latest. The default is lexical; see [WithVersionOrder]
// for when that is wrong.
func WithSnapshotVersionOrder[T any](order VersionOrder) SnapshotOption[T] {
	return func(s *Snapshot[T]) { s.order = order }
}

// WithSnapshotClock replaces the clock the freshness bound is measured against.
func WithSnapshotClock[T any](now func() time.Time) SnapshotOption[T] {
	return func(s *Snapshot[T]) { s.clock = now }
}

// NewSnapshot builds a snapshot from its declared contract. The protocol
// requires a snapshot import to name one transport and an explicit consistency
// block. The import and transport must both be active; refusing a planned
// contract here keeps a target design from becoming runtime state.
func NewSnapshot[T any](contract archproto.Import, opts ...SnapshotOption[T]) (*Snapshot[T], error) {
	if err := validateContract(contract, archproto.ModeSnapshot); err != nil {
		return nil, err
	}
	if err := requireActive(contract, contract.Transport); err != nil {
		return nil, err
	}
	snapshot := &Snapshot[T]{
		contract: contract,
		bound:    staleness(contract.Consistency),
		order:    lexicalVersionOrder,
		clock:    time.Now,
		versions: map[string]Record[T]{},
	}
	for _, opt := range opts {
		opt(snapshot)
	}
	return snapshot, nil
}

// Contract returns the declaration this snapshot enforces.
func (s *Snapshot[T]) Contract() archproto.Import { return s.contract }

// Attach records one producer state under its exact version.
//
// Re-attaching a version with identical content is accepted and does nothing —
// a transport that redelivers is not an error. Re-attaching one with DIFFERENT
// content returns ErrImmutable: the version is the identity of the state, and
// letting it change would make every reader that already resolved it wrong.
//
// observedAt is when the producer state was observed; zero means now.
func (s *Snapshot[T]) Attach(version string, value T, observedAt time.Time) error {
	if version == "" {
		return fmt.Errorf("darc: snapshot %s cannot attach an unversioned state", s.contract.ID)
	}
	if observedAt.IsZero() {
		observedAt = s.clock()
	}
	record := Record[T]{
		ID:            version,
		Value:         value,
		Provenance:    s.contract.From.Export,
		ObservedAt:    observedAt,
		Freshness:     FreshnessFresh,
		SourceVersion: version,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, found := s.versions[version]; found {
		if !reflect.DeepEqual(existing.Value, value) {
			return fmt.Errorf("%w: %s version %s is already attached with different content",
				ErrImmutable, s.contract.ID, version)
		}
		return nil
	}
	s.versions[version] = record
	if s.latest == "" || s.order(version, s.latest) > 0 {
		s.latest = version
	}
	return nil
}

// At returns the state attached under an exact version.
//
// A version-addressed read carries no staleness verdict, and that is the point
// of the mode: the caller named the state it wants, so "this state is old" is
// not news. The record still reports its observation time, and Freshness is
// stamped against the declared bound so a caller that cares can see it.
func (s *Snapshot[T]) At(version string) (Record[T], bool) {
	s.mu.RLock()
	record, found := s.versions[version]
	s.mu.RUnlock()
	if !found {
		return Record[T]{}, false
	}
	stampFreshness(&record, s.clock(), s.bound)
	return record, true
}

// Latest returns the newest attached state and the verdict the contract calls
// for, with the same three-answer shape as [Projection.Get]: absent under
// fail-open is (false, nil), absent otherwise is ErrMissing, and a state past
// the declared bound is stamped stale or refused with ErrStale.
func (s *Snapshot[T]) Latest(ctx context.Context) (Record[T], bool, error) {
	if err := ctx.Err(); err != nil {
		return Record[T]{}, false, err
	}
	s.mu.RLock()
	record, found := s.versions[s.latest]
	s.mu.RUnlock()
	if !found {
		return Record[T]{}, false, missingVerdict(s.contract)
	}
	stampFreshness(&record, s.clock(), s.bound)
	return record, true, staleVerdict(record, s.bound, s.contract.Consistency.OnStale)
}

// Versions returns every attached version, oldest first under the configured
// comparator.
func (s *Snapshot[T]) Versions() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := make([]string, 0, len(s.versions))
	for version := range s.versions {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return s.order(versions[i], versions[j]) < 0 })
	return versions
}
