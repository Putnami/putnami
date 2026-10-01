package darc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
)

// Update is one delivery of producer state to a projection.
//
// It carries what the contract's consistency block names: the source identity,
// the producer state version the ordering rule compares, and the idempotency key
// that makes a repeated delivery a no-op. ObservedAt is when the producer state
// was seen — not when it arrived here — because that is what the declared
// freshness bound is measured against.
type Update[T any] struct {
	// ID is the value of the declared source identity for this record.
	ID string
	// Value is the projected copy.
	Value T
	// SourceVersion is the producer state version, compared by the ordering rule.
	SourceVersion string
	// IdempotencyKey identifies this exact delivery. A repeat of the key most
	// recently applied to the same record is dropped.
	IdempotencyKey string
	// ObservedAt is when the producer state was observed. Zero means "now",
	// which is only correct for a delivery the consumer just fetched.
	ObservedAt time.Time
}

// Projection is a rebuildable local copy of another domain's facts, governed at
// runtime by the import contract that declares it.
//
// Reads apply the declared freshness bound and missing/stale behavior. Writes go
// through the single writer the local model names, in the declared order, with
// the declared late-event and deletion rules. Nothing here fetches: the consumer
// hands the projection its bootstrap and its updates over whichever carrier the
// contract declares, and this type governs what happens to them.
//
// A Projection is safe for concurrent use.
type Projection[T any] struct {
	contract  archproto.Import
	bound     time.Duration
	order     VersionOrder
	clock     func() time.Time
	bootstrap func(context.Context) ([]Update[T], error)
	replay    func(ctx context.Context, since string) ([]Update[T], error)

	mu            sync.RWMutex
	records       map[string]Record[T]
	lastKey       map[string]string
	deleteVersion map[string]string
	writerName    string
	writerTaken   bool
	bootstrapped  bool
}

// ProjectionOption configures a projection at construction.
type ProjectionOption[T any] func(*Projection[T])

// WithBootstrap supplies the initial-state source the declared bootstrap
// transport carries. It is required exactly when the local model's rebuild
// strategy names bootstrap, and rejected otherwise, so a projection can never
// claim a rebuild path it has no way to run.
func WithBootstrap[T any](load func(ctx context.Context) ([]Update[T], error)) ProjectionOption[T] {
	return func(p *Projection[T]) { p.bootstrap = load }
}

// WithReplay supplies the replay source the declared updates transport carries.
// It receives the newest source version the projection currently holds, or the
// empty string when it holds nothing, so a replay can resume rather than restart.
// It is required exactly when the rebuild strategy names replay.
func WithReplay[T any](load func(ctx context.Context, since string) ([]Update[T], error)) ProjectionOption[T] {
	return func(p *Projection[T]) { p.replay = load }
}

// WithVersionOrder replaces the source-version comparator. Pass one whenever the
// declared version is not lexically ordered — an unpadded decimal counter is the
// common case, where "10" sorts before "9" and the late-event rule would then
// report the opposite of what happened.
func WithVersionOrder[T any](order VersionOrder) ProjectionOption[T] {
	return func(p *Projection[T]) { p.order = order }
}

// WithClock replaces the clock the freshness bound is measured against. Tests
// use it; production does not need it.
func WithClock[T any](now func() time.Time) ProjectionOption[T] {
	return func(p *Projection[T]) { p.clock = now }
}

// NewProjection builds a projection from its declared contract.
//
// The import and both of its carriers must be active. A planned projection is
// still a target design and cannot expose a runtime copy.
//
// The contract is validated by the protocol itself, so every projection
// invariant the manifest is required to state — bootstrap and updates,
// consistency, deletion, the local model, projected fields matching the
// minimized facts — is checked here rather than assumed. On top of that, the
// rebuild strategy must have the hooks it names: a projection that declares
// `bootstrap` and is handed none would fail at the moment it was needed, which
// is the moment it is least able to.
func NewProjection[T any](contract archproto.Import, opts ...ProjectionOption[T]) (*Projection[T], error) {
	if err := validateContract(contract, archproto.ModeProjection); err != nil {
		return nil, err
	}
	if err := requireActive(contract, contract.Bootstrap, contract.Updates); err != nil {
		return nil, err
	}
	projection := &Projection[T]{
		contract:      contract,
		bound:         staleness(contract.Consistency),
		order:         lexicalVersionOrder,
		clock:         time.Now,
		records:       map[string]Record[T]{},
		lastKey:       map[string]string{},
		deleteVersion: map[string]string{},
	}
	for _, opt := range opts {
		opt(projection)
	}
	projection.writerName = contract.LocalModel.Writer
	if err := projection.checkRebuildHooks(); err != nil {
		return nil, err
	}
	return projection, nil
}

// checkRebuildHooks holds the rebuild strategy to the hooks it needs.
func (p *Projection[T]) checkRebuildHooks() error {
	rebuild := p.contract.LocalModel.Rebuild
	needsBootstrap := rebuild == archproto.RebuildBootstrap || rebuild == archproto.RebuildBootstrapAndReplay
	needsReplay := rebuild == archproto.RebuildReplay || rebuild == archproto.RebuildBootstrapAndReplay
	if needsBootstrap && p.bootstrap == nil {
		return &ContractError{
			Import: p.contract.ID,
			Reason: fmt.Sprintf("declares rebuild %q but no bootstrap source was supplied (see WithBootstrap)", rebuild),
		}
	}
	if needsReplay && p.replay == nil {
		return &ContractError{
			Import: p.contract.ID,
			Reason: fmt.Sprintf("declares rebuild %q but no replay source was supplied (see WithReplay)", rebuild),
		}
	}
	if !needsBootstrap && p.bootstrap != nil {
		return &ContractError{
			Import: p.contract.ID,
			Reason: fmt.Sprintf("declares rebuild %q, which does not bootstrap, but a bootstrap source was supplied", rebuild),
		}
	}
	if !needsReplay && p.replay != nil {
		return &ContractError{
			Import: p.contract.ID,
			Reason: fmt.Sprintf("declares rebuild %q, which does not replay, but a replay source was supplied", rebuild),
		}
	}
	return nil
}

// Contract returns the declaration this projection enforces.
func (p *Projection[T]) Contract() archproto.Import { return p.contract }

// Get returns one projected record and the verdict the contract calls for.
//
// The three answers are distinct on purpose:
//
//   - found=false, err=nil — the fact is absent and the contract declares
//     onMissing fail-open, so the caller continues without it;
//   - found=false, err=ErrMissing — absent under fail-closed or unavailable;
//   - found=true, record.Freshness=stale — present but past the declared bound,
//     returned because onStale is use-stale or fail-open. Under fail-closed the
//     record comes back WITH ErrStale, so a caller that wants to log what it
//     refused can.
//
// A tombstoned record is reported as absent: the producer deleted the fact, and
// the contract's tombstone strategy is about retaining the marker, not the value.
func (p *Projection[T]) Get(ctx context.Context, id string) (Record[T], bool, error) {
	if err := ctx.Err(); err != nil {
		return Record[T]{}, false, err
	}
	p.mu.RLock()
	record, found := p.records[id]
	bootstrapped := p.bootstrapped
	p.mu.RUnlock()

	if !found || record.Deleted {
		// A projection that was never bootstrapped holds nothing, and reporting
		// that as an ordinary miss would hide the difference between "the
		// producer has no such fact" and "this copy was never built". Both take
		// the declared missing behavior, and the message says which happened.
		if err := missingVerdict(p.contract); err != nil {
			if !bootstrapped {
				return Record[T]{}, false, fmt.Errorf("%w (the projection has not been rebuilt yet)", err)
			}
			return Record[T]{}, false, err
		}
		return Record[T]{}, false, nil
	}
	stampFreshness(&record, p.clock(), p.bound)
	return record, true, staleVerdict(record, p.bound, p.contract.Consistency.OnStale)
}

// All returns every live record, ordered by source identity, each stamped with
// its freshness. It applies no missing or stale verdict: a caller enumerating a
// projection is asking what it holds, and the per-record stamp is the answer.
func (p *Projection[T]) All() []Record[T] {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.clock()
	records := make([]Record[T], 0, len(p.records))
	for _, record := range p.records {
		if record.Deleted {
			continue
		}
		copied := record
		stampFreshness(&copied, now, p.bound)
		records = append(records, copied)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records
}

// Writer returns the one handle allowed to update this projection.
//
// The local model names a single writer, and this is where that stops being a
// sentence in a manifest: the name must match, and the handle is handed out
// once. A second component that wants to write has to change the declaration
// first, which is the review the invariant exists to force.
func (p *Projection[T]) Writer(name string) (*Writer[T], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name != p.writerName {
		return nil, fmt.Errorf("%w: import %s names %q, not %q", ErrNotTheWriter, p.contract.ID, p.writerName, name)
	}
	if p.writerTaken {
		return nil, fmt.Errorf("%w: %q already holds it for import %s", ErrWriterClaimed, p.writerName, p.contract.ID)
	}
	p.writerTaken = true
	return &Writer[T]{projection: p}, nil
}

// Rebuild reconstructs the projection through the declared strategy: bootstrap
// replaces what is held, replay resumes from the newest source version held, and
// bootstrap-and-replay does both in that order.
//
// It is the recovery path the contract promises, and it does not need the writer
// handle: a rebuild is the projector re-deriving its own state from the
// producer, not a second component writing into it.
func (p *Projection[T]) Rebuild(ctx context.Context) error {
	switch p.contract.LocalModel.Rebuild {
	case archproto.RebuildBootstrap:
		return p.runBootstrap(ctx)
	case archproto.RebuildReplay:
		return p.runReplay(ctx)
	case archproto.RebuildBootstrapAndReplay:
		if err := p.runBootstrap(ctx); err != nil {
			return err
		}
		return p.runReplay(ctx)
	default:
		return &ContractError{
			Import: p.contract.ID,
			Reason: fmt.Sprintf("declares rebuild %q, so it cannot be rebuilt", p.contract.LocalModel.Rebuild),
		}
	}
}

func (p *Projection[T]) runBootstrap(ctx context.Context) error {
	updates, err := p.bootstrap(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap projection %s: %w", p.contract.ID, err)
	}
	p.mu.Lock()
	p.records = make(map[string]Record[T], len(updates))
	p.lastKey = make(map[string]string, len(updates))
	p.deleteVersion = make(map[string]string)
	p.bootstrapped = true
	p.mu.Unlock()
	for _, update := range updates {
		if _, err := p.apply(update); err != nil {
			return fmt.Errorf("bootstrap projection %s: %w", p.contract.ID, err)
		}
	}
	return nil
}

func (p *Projection[T]) runReplay(ctx context.Context) error {
	updates, err := p.replay(ctx, p.newestVersion())
	if err != nil {
		return fmt.Errorf("replay projection %s: %w", p.contract.ID, err)
	}
	p.mu.Lock()
	p.bootstrapped = true
	p.mu.Unlock()
	for _, update := range updates {
		if _, err := p.apply(update); err != nil {
			return fmt.Errorf("replay projection %s: %w", p.contract.ID, err)
		}
	}
	return nil
}

// newestVersion returns the newest source version held, so a replay resumes
// instead of restarting.
func (p *Projection[T]) newestVersion() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	newest := ""
	for _, record := range p.records {
		if newest == "" || p.order(record.SourceVersion, newest) > 0 {
			newest = record.SourceVersion
		}
	}
	for _, version := range p.deleteVersion {
		if newest == "" || p.order(version, newest) > 0 {
			newest = version
		}
	}
	return newest
}

// apply is the ordered, idempotent write the contract describes. It returns
// whether the update changed the local model.
func (p *Projection[T]) apply(update Update[T]) (bool, error) {
	observed := update.ObservedAt
	if observed.IsZero() {
		observed = p.clock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Idempotency first: a repeated delivery of the update already applied is
	// not a late event, it is the same event, and reporting it as late would
	// make a retrying transport look like a broken producer.
	if key := update.IdempotencyKey; key != "" && p.lastKey[update.ID] == key {
		return false, nil
	}
	// A deletion already accepted at this exact producer state wins over a
	// delayed live delivery of the same state. Re-applying it would make the
	// meaning of one source version depend on transport arrival order.
	if deletedVersion, deleted := p.deletionVersionLocked(update.ID); deleted && p.order(update.SourceVersion, deletedVersion) == 0 {
		return false, nil
	}
	if currentVersion, found := p.currentVersionLocked(update.ID); found {
		if applied, err := p.orderVerdict(Record[T]{SourceVersion: currentVersion}, update); !applied || err != nil {
			return false, err
		}
	}
	p.records[update.ID] = Record[T]{
		ID:            update.ID,
		Value:         update.Value,
		Provenance:    p.contract.From.Export,
		ObservedAt:    observed,
		Freshness:     FreshnessFresh,
		SourceVersion: update.SourceVersion,
	}
	delete(p.deleteVersion, update.ID)
	if update.IdempotencyKey != "" {
		p.lastKey[update.ID] = update.IdempotencyKey
	}
	return true, nil
}

// orderVerdict decides what an update older than the local copy means, per the
// declared late-event strategy.
func (p *Projection[T]) orderVerdict(existing Record[T], update Update[T]) (bool, error) {
	if p.contract.Consistency.Ordering == archproto.OrderingNone {
		return true, nil
	}
	if p.order(update.SourceVersion, existing.SourceVersion) >= 0 {
		return true, nil
	}
	switch p.contract.Consistency.LateEvents {
	case archproto.LateEventApply:
		return true, nil
	case archproto.LateEventReject:
		return false, fmt.Errorf("%w: %s is older than the local %s for %s",
			ErrLateUpdate, update.SourceVersion, existing.SourceVersion, update.ID)
	default:
		// ignore-older: the update is dropped and the caller is told nothing
		// changed. It is not an error — an out-of-order delivery is what the
		// strategy exists to absorb.
		return false, nil
	}
}

// currentVersionLocked returns the ordering watermark for one source identity.
// A hard delete has no record to carry it, and retain deliberately keeps the
// older value, so accepted deletions are tracked separately from the visible
// copy. Callers must hold p.mu.
func (p *Projection[T]) currentVersionLocked(id string) (string, bool) {
	if version, found := p.deleteVersion[id]; found {
		return version, true
	}
	record, found := p.records[id]
	return record.SourceVersion, found
}

// deletionVersionLocked reports whether the source identity is already deleted
// at one producer state. Tombstone carries that state in the visible marker;
// hard-delete and retain carry it in deleteVersion. Callers must hold p.mu.
func (p *Projection[T]) deletionVersionLocked(id string) (string, bool) {
	if version, found := p.deleteVersion[id]; found {
		return version, true
	}
	record, found := p.records[id]
	if !found || !record.Deleted {
		return "", false
	}
	return record.SourceVersion, true
}

// Writer is the single component the local model names as allowed to update the
// projection. It is obtained once, from [Projection.Writer].
type Writer[T any] struct {
	projection *Projection[T]
}

// Name returns the writer identity the local model declares.
func (w *Writer[T]) Name() string { return w.projection.writerName }

// Apply records one update and reports whether it changed the local model.
//
// It returns false with no error for the two deliveries the contract says to
// absorb: a repeat of the update already applied, and — under an `ignore-older`
// late-event strategy — an update older than the copy held. Under `reject` the
// same older update returns ErrLateUpdate instead.
func (w *Writer[T]) Apply(ctx context.Context, update Update[T]) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return w.projection.apply(update)
}

// Delete propagates a producer deletion through the declared strategy:
//
//   - tombstone retains the record and marks it, so a later reader can tell
//     "deleted" from "never seen";
//   - hard-delete removes it;
//   - retain leaves the copy in place, which is what the strategy means — the
//     consumer keeps its own copy after the producer drops the fact;
//   - not-applicable refuses, because the contract says deletion does not
//     happen for these facts.
//
// The source version is compared exactly as an update is, so a late deletion
// obeys the same late-event rule. Once accepted, the deletion version is an
// ordering watermark: replaying that deletion or a live delivery at the same
// producer state is an idempotent no-op, while a newer state may recreate it.
func (w *Writer[T]) Delete(ctx context.Context, id, sourceVersion string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	projection := w.projection
	strategy := projection.contract.Deletion.Strategy
	if strategy == archproto.DeletionNotApplicable {
		return fmt.Errorf("%w: import %s", ErrDeletionNotApplicable, projection.contract.ID)
	}
	projection.mu.Lock()
	defer projection.mu.Unlock()
	existing, found := projection.records[id]
	if deletedVersion, deleted := projection.deletionVersionLocked(id); deleted && projection.order(sourceVersion, deletedVersion) == 0 {
		return nil
	}
	if currentVersion, ordered := projection.currentVersionLocked(id); ordered {
		if applied, err := projection.orderVerdict(Record[T]{SourceVersion: currentVersion}, Update[T]{ID: id, SourceVersion: sourceVersion}); !applied || err != nil {
			return err
		}
	}
	switch strategy {
	case archproto.DeletionHardDelete:
		delete(projection.records, id)
		delete(projection.lastKey, id)
		projection.deleteVersion[id] = sourceVersion
	case archproto.DeletionTombstone:
		if !found {
			existing = Record[T]{
				ID:         id,
				Provenance: projection.contract.From.Export,
				ObservedAt: projection.clock(),
				Freshness:  FreshnessFresh,
			}
		}
		existing.Deleted = true
		existing.DeletedAt = projection.clock()
		existing.SourceVersion = sourceVersion
		projection.records[id] = existing
	case archproto.DeletionRetain:
		// The visible copy stays exactly as it was, while the deletion version
		// advances the ordering watermark. A delayed pre-deletion write therefore
		// cannot mutate the retained value under ignore-older or reject.
		projection.deleteVersion[id] = sourceVersion
	}
	return nil
}
