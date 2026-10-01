package app

import (
	"context"
	stderrors "errors"
	"testing"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/migration"
	"go.putnami.dev/protocol/features/spectest"
)

// fakeSource is a no-op Source used to assert contribution wiring.
type fakeSource struct {
	kind migration.Kind
	ns   string
}

func (s fakeSource) Kind() migration.Kind { return s.kind }
func (s fakeSource) Namespace() string    { return s.ns }

// contribPlugin satisfies both Plugin and MigrationContributor.
type contribPlugin struct {
	name    string
	sources []migration.Source
}

func (p *contribPlugin) Name() string                         { return p.name }
func (p *contribPlugin) MigrationSources() []migration.Source { return p.sources }

// fakeRunner records Apply / Status / Verify / Rollback invocations.
type fakeRunner struct {
	kind     migration.Kind
	applied  []migration.Record
	calls    int
	lastOpts migration.ApplyOpts
}

func (r *fakeRunner) Kind() migration.Kind { return r.kind }
func (r *fakeRunner) Apply(_ context.Context, opts migration.ApplyOpts) ([]migration.Record, error) {
	r.calls++
	r.lastOpts = opts
	return r.applied, nil
}
func (r *fakeRunner) Status(_ context.Context) ([]migration.Record, error) { return nil, nil }
func (r *fakeRunner) Rollback(_ context.Context, _ migration.RollbackOpts) ([]migration.Record, error) {
	return nil, nil
}
func (r *fakeRunner) Verify(_ context.Context) (migration.DriftReport, error) {
	return migration.DriftReport{}, nil
}

func TestApplication_MigrationRegistryWiredIntoDI(t *testing.T) {
	a := New("registry-in-di")

	if err := a.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer a.Stop(context.Background())

	if a.MigrationRegistry() == nil {
		t.Fatal("MigrationRegistry must be non-nil after Prepare")
	}
}

func TestApplication_ContributorSourcesAreCollected(t *testing.T) {
	a := New("contrib-collected")
	a.Use(&contribPlugin{
		name: "iam",
		sources: []migration.Source{
			fakeSource{kind: migration.KindSQL, ns: "iam"},
			fakeSource{kind: "gcs", ns: "iam"},
		},
	})
	a.Use(&contribPlugin{
		name:    "secrets",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "secrets"}},
	})

	if err := a.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer a.Stop(context.Background())

	reg := a.MigrationRegistry()
	if got := len(reg.Sources(migration.KindSQL)); got != 2 {
		t.Errorf("expected 2 SQL sources (iam + secrets), got %d", got)
	}
	if got := len(reg.Sources("gcs")); got != 1 {
		t.Errorf("expected 1 GCS source, got %d", got)
	}
}

func TestApplication_MigrateAppliesRegisteredRunners(t *testing.T) {
	a := New("migrate-applies")

	a.Use(&contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	})

	// A plugin registers the SQL runner during Configure — mirroring how
	// the real database plugin will work in Stage 4.
	runner := &fakeRunner{kind: migration.KindSQL}
	a.Use(&runnerRegistrarPlugin{runner: runner})

	if err := a.Migrate(context.Background(), migration.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected Apply to be called once, got %d", runner.calls)
	}
	if !runner.lastOpts.Force {
		t.Error("ApplyOpts.Force must propagate to runner")
	}
}

func TestApplication_StartInvokesRegisteredRunnersNonForced(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "phase-order", "migrations-apply-during-start")
	a := New("start-applies")
	a.Use(&contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	})
	runner := &fakeRunner{kind: migration.KindSQL}
	a.Use(&runnerRegistrarPlugin{runner: runner})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected Start to call Apply once, got %d", runner.calls)
	}
	if runner.lastOpts.Force {
		t.Fatal("Start must let runners honor their own AutoApply gate")
	}
	if !runner.lastOpts.AllowSourceOnly {
		t.Fatal("Start must mark lifecycle applies as source-only tolerant")
	}
}

func TestApplication_StartToleratesSourceOnlyKinds(t *testing.T) {
	a := New("orphan-source")
	a.Use(&contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	})
	// No SQL runner registered. Start uses a non-forced lifecycle apply, so a
	// source-only kind is static metadata until an explicit migrate command runs.

	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start must tolerate source-only migration kinds: %v", err)
	}
}

func TestApplication_MigrateDefaultFailsOnOrphanSource(t *testing.T) {
	a := New("orphan-source-default")
	a.Use(&contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	})
	// Programmatic Migrate is explicit even without Force; it should still
	// catch the missing runner while preserving runner-level AutoApply gating.

	err := a.Migrate(context.Background(), migration.ApplyOpts{})
	if err == nil {
		t.Fatal("default Migrate must fail when a source's Kind has no registered Runner")
	}
	if !hasCode(err, migration.CodeUnknownKind) {
		t.Fatalf("expected error chain to include CodeUnknownKind, got %v", err)
	}
}

func TestApplication_MigrateForceFailsOnOrphanSource(t *testing.T) {
	a := New("orphan-source-force")
	a.Use(&contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	})
	// Explicit migration still catches the missing runner.

	err := a.Migrate(context.Background(), migration.ApplyOpts{Force: true})
	if err == nil {
		t.Fatal("forced Migrate must fail when a source's Kind has no registered Runner")
	}
	if !hasCode(err, migration.CodeUnknownKind) {
		t.Fatalf("expected error chain to include CodeUnknownKind, got %v", err)
	}
}

// hasCode reports whether any error in err's chain has the given Code.
func hasCode(err error, code errors.Code) bool {
	for cur := err; cur != nil; cur = stderrors.Unwrap(cur) {
		var pe *errors.Error
		if stderrors.As(cur, &pe) && pe.Code() == code {
			return true
		}
	}
	return false
}

// runnerRegistrarPlugin installs a migration.Runner into the per-app
// registry from its Configure hook. This is the pattern Stage 4's
// database.Plugin will use: resolve *migration.Registry from DI, then
// RegisterRunner.
type runnerRegistrarPlugin struct {
	runner migration.Runner
}

func (p *runnerRegistrarPlugin) Name() string { return "runner-registrar" }
func (p *runnerRegistrarPlugin) Configure(_ context.Context, owner *Module) error {
	cc := owner.Container()
	if cc == nil {
		return errors.Newf(CodeConfigure, "no DI container available")
	}
	value, err := cc.Get(inject.TokenOf[*migration.Registry]())
	if err != nil {
		return errors.Wrap(err, CodeConfigure)
	}
	return value.(*migration.Registry).RegisterRunner(p.runner)
}
