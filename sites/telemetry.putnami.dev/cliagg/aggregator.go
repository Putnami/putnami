package cliagg

import (
	"context"
	"sync"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	telemetry "go.putnami.dev/protocol/telemetry"
)

// defaultFlushInterval is how often the aggregator drains the accumulator into
// Postgres. Aggregates are daily, so there is no latency pressure; a modest
// interval bounds in-memory cardinality between flushes and keeps the write
// path light.
const defaultFlushInterval = 60 * time.Second

// Aggregator is the emit-time tap that folds sanitized CLI-usage records into
// durable daily aggregates. One value plays three roles:
//
//   - Emitter tee target: Emit folds a batch into the in-memory Accumulator.
//   - app.MigrationContributor: it owns the aggregate tables' migrations, so a
//     build's describe phase emits the datasource requirement regardless of
//     whether a database is configured at build time.
//   - app.Starter/app.Stopper: it resolves the telemetry *database.Pool from DI
//     on Start, runs a periodic flush, and performs a final flush on Stop.
//
// It is best-effort and fail-silent: with no resolvable pool it no-ops (Emit is
// never teed in that case, and any drained batch is dropped) so the receiver
// still serves; a flush DB error drops that batch without retry.
type Aggregator struct {
	acc           *Accumulator
	log           *logger.Logger
	flushInterval time.Duration

	mu   sync.Mutex
	pool *database.Pool

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewAggregator builds an Aggregator with an empty accumulator.
func NewAggregator(log *logger.Logger) *Aggregator {
	if log == nil {
		log = logger.Default().Named("cli-usage-aggregator")
	}
	return &Aggregator{
		acc:           NewAccumulator(),
		log:           log,
		flushInterval: defaultFlushInterval,
	}
}

// Name identifies the plugin.
func (a *Aggregator) Name() string { return "cli-usage-aggregator" }

// Emit implements the receiver's Emitter contract: it folds the sanitized
// resource logs into the accumulator. It is non-blocking and never errors.
//
// With no resolved pool it drops instead of accumulating: flush is a no-op
// without a pool, so folding would grow the accumulator without bound for the
// life of the process. Start resolves the pool before any request flows, so a
// deployed workload with a binding never takes this path.
func (a *Aggregator) Emit(resourceLogs []telemetry.ResourceLogs) {
	a.mu.Lock()
	pool := a.pool
	a.mu.Unlock()
	if pool == nil {
		return
	}
	a.acc.Add(resourceLogs)
}

// MigrationSources implements app.MigrationContributor.
func (a *Aggregator) MigrationSources() []migration.Source {
	return []migration.Source{Source()}
}

// Pool returns the lazily resolved telemetry pool, or nil when none was
// resolved. The aggregate read route reads through this accessor so it shares
// the single named-datasource pool opened in Start rather than declaring a
// second (eagerly opened) datasource of its own.
func (a *Aggregator) Pool() *database.Pool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pool
}

// Start resolves the telemetry pool from the DI container and, when one is
// available, launches the periodic flush. Requests do not flow until after the
// start phase, so the pool set here is visible to every subsequent Emit/flush.
// With no pool it logs and no-ops so the receiver still runs.
func (a *Aggregator) Start(_ context.Context, owner *app.Module) error {
	pool, err := resolvePool(owner)
	if pool == nil {
		reason := "no telemetry datasource pool resolved"
		if err != nil {
			reason = err.Error()
		}
		a.log.Info("cli-usage aggregation disabled: " + reason)
		return nil
	}
	a.mu.Lock()
	a.pool = pool
	a.mu.Unlock()

	a.done = make(chan struct{})
	a.wg.Add(1)
	go a.flushLoop()
	return nil
}

// Stop halts the flush loop and performs a final flush, so a short-lived
// container does not silently drop its last window of aggregates.
func (a *Aggregator) Stop(ctx context.Context, _ *app.Module) error {
	a.closeOnce.Do(func() {
		if a.done != nil {
			close(a.done)
		}
	})
	a.wg.Wait()
	a.flush(ctx)
	return nil
}

func (a *Aggregator) flushLoop() {
	defer a.wg.Done()
	t := time.NewTicker(a.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.flush(context.Background())
		case <-a.done:
			return
		}
	}
}

// flush drains the accumulator up front and upserts it. Even an empty snapshot
// runs the bounded retention statement, so durable rows age out without
// depending on continued telemetry traffic. Draining first means a DB error
// drops the drained batch (no retry) while a concurrent Emit keeps accumulating.
func (a *Aggregator) flush(ctx context.Context) {
	a.mu.Lock()
	pool := a.pool
	a.mu.Unlock()
	if pool == nil {
		return
	}
	snap := a.acc.Drain()
	if err := Flush(ctx, pool, snap); err != nil {
		a.log.Warn("dropped cli-usage aggregate flush: " + err.Error())
	}
}

// resolvePool opens the named telemetry pool from the *Pools registry. The
// registry itself never opens a connection when it is built, so this — running
// in the Start phase, where a managed binding carried by the resolved "database"
// config section is available — is the first and only open attempt.
//
// It returns (nil, err) rather than failing startup, so a workload with no
// database still serves; the error explains why (datasource not declared, no
// binding resolved, or the connection failed) and the caller logs it.
func resolvePool(owner *app.Module) (*database.Pool, error) {
	if owner == nil || owner.Container() == nil {
		return nil, nil
	}
	value, err := owner.Container().Get(inject.TokenOf[*database.Pools]())
	if err != nil {
		return nil, err
	}
	pools, ok := value.(*database.Pools)
	if !ok || pools == nil {
		return nil, nil
	}
	return pools.For(Datasource)
}

// Compile-time lifecycle interface checks.
var (
	_ app.MigrationContributor = (*Aggregator)(nil)
	_ app.Starter              = (*Aggregator)(nil)
	_ app.Stopper              = (*Aggregator)(nil)
)
