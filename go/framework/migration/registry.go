package migration

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

// Registry is the per-application collection of contributed Sources and
// registered Runners. One Registry is held in DI by the app builder; it
// is built fresh at every boot — there is no package-level singleton.
//
// Concurrency contract:
//
//   - AddSource and RegisterRunner are safe to call concurrently with each
//     other and with the read accessors Sources, Runner, Kinds.
//   - The drive methods ApplyAll, StatusAll, VerifyAll are NOT safe to
//     interleave with AddSource or RegisterRunner. They take a snapshot of
//     Kinds under a read lock, release it, then iterate — concurrent
//     mutation between snapshot and iteration would observe a non-atomic
//     view. The application lifecycle guarantees this is fine in practice:
//     collectMigrationSources / runner registration both complete before
//     any drive method runs. Tests that exercise drive methods directly
//     should not start a goroutine that calls AddSource concurrently.
//
// Runners run one kind at a time in lexicographic order so cross-kind
// output is reproducible.
type Registry struct {
	mu      sync.RWMutex
	sources map[Kind][]Source
	runners map[Kind]Runner
	log     *logger.Logger
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		sources: make(map[Kind][]Source),
		runners: make(map[Kind]Runner),
		log:     migrationLoggerFrom(logger.Default()),
	}
}

// AddSource records a feature-plugin contribution. The same Source value
// must not be added twice (per-kind ordering is otherwise non-deterministic);
// per-(namespace, name) duplicate detection is delegated to the Runner once
// the source has been expanded into definitions.
func (r *Registry) AddSource(s Source) error {
	if s == nil {
		return errors.Newf(CodeInvalidSource, "nil migration source")
	}
	if s.Kind() == "" {
		return errors.Newf(CodeInvalidSource, "migration source has empty Kind")
	}
	if strings.TrimSpace(s.Namespace()) == "" {
		return errors.New(CodeInvalidSource, "migration source has empty Namespace",
			errors.String("kind", string(s.Kind())))
	}

	r.mu.Lock()
	r.sources[s.Kind()] = append(r.sources[s.Kind()], s)
	count := len(r.sources[s.Kind()])
	r.mu.Unlock()

	r.log.Debug("plugin contributed source", migrationAttr(map[string]any{
		"kind":           string(s.Kind()),
		"namespace":      s.Namespace(),
		"sourcesForKind": count,
	}))
	return nil
}

// RegisterRunner installs the Runner for one Kind. Exactly one Runner may
// register per Kind; a second call for the same Kind returns
// CodeDuplicateRunner.
func (r *Registry) RegisterRunner(run Runner) error {
	if run == nil {
		return errors.Newf(CodeInvalidSource, "nil migration runner")
	}
	if run.Kind() == "" {
		return errors.Newf(CodeInvalidSource, "migration runner has empty Kind")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runners[run.Kind()]; exists {
		return errors.Newf(CodeDuplicateRunner,
			"a runner for kind %q is already registered", run.Kind())
	}
	r.runners[run.Kind()] = run

	r.log.Debug("runner registered", migrationAttr(map[string]any{
		"kind":           string(run.Kind()),
		"sourcesForKind": len(r.sources[run.Kind()]),
	}))
	return nil
}

// Sources returns the contributed sources for one kind, in registration
// order. The returned slice is a copy — mutating it does not affect the
// registry.
func (r *Registry) Sources(k Kind) []Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.sources[k]
	if len(src) == 0 {
		return nil
	}
	out := make([]Source, len(src))
	copy(out, src)
	return out
}

// Runner returns the Runner registered for one Kind, if any.
func (r *Registry) Runner(k Kind) (Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runners[k]
	return run, ok
}

// Kinds returns every Kind that has either a registered Runner or
// contributed Sources, lexicographically sorted. This is the order
// ApplyAll/StatusAll/VerifyAll iterate, so callers see reproducible
// cross-kind output.
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[Kind]struct{}, len(r.runners)+len(r.sources))
	for k := range r.runners {
		seen[k] = struct{}{}
	}
	for k := range r.sources {
		seen[k] = struct{}{}
	}
	out := make([]Kind, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ApplyAll runs Runner.Apply for every Kind that has a registered Runner,
// in lexicographic order. By default, Sources contributed for a Kind with no
// registered Runner produce CodeUnknownKind; callers must explicitly set
// ApplyOpts.AllowSourceOnly for the automatic lifecycle apply that tolerates
// source-only static metadata.
//
// The first error short-circuits subsequent kinds; records already
// returned by earlier kinds are preserved in the response.
func (r *Registry) ApplyAll(ctx context.Context, opts ApplyOpts) ([]Record, error) {
	if !opts.AllowSourceOnly {
		if err := r.assertNoOrphanSources(); err != nil {
			return nil, err
		}
	}

	var out []Record
	for _, k := range r.Kinds() {
		run, ok := r.Runner(k)
		if !ok {
			continue
		}
		records, err := run.Apply(ctx, opts)
		out = append(out, records...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// RollbackAll runs Runner.Rollback for every Kind that has a registered
// Runner, in lexicographic order. Like explicit apply/status/verify it
// rejects orphan sources up front via assertNoOrphanSources, so operator
// commands still fail loudly when a source has no runner.
//
// With opts.To empty each runner rolls back only its most recently applied
// migration, so a bare `down` rolls back the most recent migration per
// kind. The first error short-circuits subsequent kinds; records already
// returned by earlier kinds are preserved in the response.
func (r *Registry) RollbackAll(ctx context.Context, opts RollbackOpts) ([]Record, error) {
	if err := r.assertNoOrphanSources(); err != nil {
		return nil, err
	}

	var out []Record
	for _, k := range r.Kinds() {
		run, ok := r.Runner(k)
		if !ok {
			continue
		}
		records, err := run.Rollback(ctx, opts)
		out = append(out, records...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// StatusAll aggregates Runner.Status across every registered kind.
func (r *Registry) StatusAll(ctx context.Context) ([]Record, error) {
	if err := r.assertNoOrphanSources(); err != nil {
		return nil, err
	}
	var out []Record
	for _, k := range r.Kinds() {
		run, ok := r.Runner(k)
		if !ok {
			continue
		}
		records, err := run.Status(ctx)
		out = append(out, records...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// VerifyAll aggregates Runner.Verify across every registered kind.
// Returns a slice of one DriftReport per kind, even when individual
// reports are empty, so the caller can render a complete per-kind view.
//
// Returned reports are defensively copied so callers can mutate them
// (e.g. append for aggregate views) without affecting the Runner's
// internal state on subsequent calls.
func (r *Registry) VerifyAll(ctx context.Context) ([]DriftReport, error) {
	if err := r.assertNoOrphanSources(); err != nil {
		return nil, err
	}
	kinds := r.Kinds()
	out := make([]DriftReport, 0, len(kinds))
	for _, k := range kinds {
		run, ok := r.Runner(k)
		if !ok {
			continue
		}
		report, err := run.Verify(ctx)
		out = append(out, cloneDriftReport(report))
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// cloneDriftReport returns a DriftReport whose slice fields share no
// backing array with the input. Used by VerifyAll so consumers can
// mutate or accumulate without affecting Runner-internal state.
func cloneDriftReport(d DriftReport) DriftReport {
	out := DriftReport{Kind: d.Kind}
	if len(d.HashDrifts) > 0 {
		out.HashDrifts = append([]HashDrift(nil), d.HashDrifts...)
	}
	if len(d.MissingFromRegistry) > 0 {
		out.MissingFromRegistry = append([]Record(nil), d.MissingFromRegistry...)
	}
	if len(d.MissingFromStore) > 0 {
		out.MissingFromStore = append([]Record(nil), d.MissingFromStore...)
	}
	return out
}

// assertNoOrphanSources returns CodeUnknownKind when a Source has been
// contributed for a Kind that has no registered Runner. Explicit migration
// operations use this to catch the common "plugin ships SQL migrations but the
// app forgot to Use() the database plugin" mistake without making source-only
// static metadata fatal during the automatic lifecycle apply.
func (r *Registry) assertNoOrphanSources() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	missing := make([]string, 0, len(r.sources))
	for k, srcs := range r.sources {
		if _, ok := r.runners[k]; ok {
			continue
		}
		if len(srcs) == 0 {
			continue
		}
		names := make([]string, len(srcs))
		for i, s := range srcs {
			names[i] = s.Namespace()
		}
		missing = append(missing,
			string(k)+" (contributed by: "+strings.Join(names, ", ")+")")
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return errors.Newf(CodeUnknownKind,
		"migration sources contributed for kinds with no registered Runner: %s",
		strings.Join(missing, "; "))
}
