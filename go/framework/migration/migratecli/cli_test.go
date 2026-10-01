package migratecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/inject"
	"go.putnami.dev/migration"
	"go.putnami.dev/protocol/features/spectest"
)

// --- Stubs -----------------------------------------------------------------

type fakeSource struct {
	kind migration.Kind
	ns   string
}

func (f fakeSource) Kind() migration.Kind { return f.kind }
func (f fakeSource) Namespace() string    { return f.ns }

type fakeRunner struct {
	kind        migration.Kind
	apply       []migration.Record
	applyErr    error
	status      []migration.Record
	rollback    []migration.Record
	rollbackErr error
	report      migration.DriftReport
	gotApply    migration.ApplyOpts
	gotRollback migration.RollbackOpts
}

func (r *fakeRunner) Kind() migration.Kind { return r.kind }
func (r *fakeRunner) Apply(_ context.Context, opts migration.ApplyOpts) ([]migration.Record, error) {
	r.gotApply = opts
	return r.apply, r.applyErr
}
func (r *fakeRunner) Status(_ context.Context) ([]migration.Record, error) { return r.status, nil }
func (r *fakeRunner) Rollback(_ context.Context, opts migration.RollbackOpts) ([]migration.Record, error) {
	r.gotRollback = opts
	return r.rollback, r.rollbackErr
}
func (r *fakeRunner) Verify(_ context.Context) (migration.DriftReport, error) { return r.report, nil }

// runnerSeedingPlugin is the test equivalent of the database plugin —
// it installs a fake runner into the per-app registry during Configure.
type runnerSeedingPlugin struct {
	runner migration.Runner
}

func (p *runnerSeedingPlugin) Name() string { return "runner-seed" }
func (p *runnerSeedingPlugin) Configure(_ context.Context, owner *app.Module) error {
	cc := owner.Container()
	value, err := cc.Get(injectTokenForRegistry())
	if err != nil {
		return err
	}
	return value.(*migration.Registry).RegisterRunner(p.runner)
}

type startProbePlugin struct {
	started *bool
}

func (p *startProbePlugin) Name() string { return "start-probe" }
func (p *startProbePlugin) Start(_ context.Context, _ *app.Module) error {
	*p.started = true
	return nil
}

// contribPlugin seeds Sources into the registry.
type contribPlugin struct {
	name    string
	sources []migration.Source
}

func (p *contribPlugin) Name() string                         { return p.name }
func (p *contribPlugin) MigrationSources() []migration.Source { return p.sources }

// builder constructs an Application stacked with the given plugins.
// Each call returns a fresh Application — important for the CLI's
// repeated invocations across subcommand tests.
func builder(plugins ...app.Plugin) AppBuilder {
	return func() *app.Application {
		a := app.New("migrate-test")
		for _, p := range plugins {
			a.Use(p)
		}
		return a
	}
}

// --- Tests -----------------------------------------------------------------

func TestRunWith_NoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWith(builder(), nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestRunWith_UnknownSubcommand(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "user-error-exits-1")
	var stdout, stderr bytes.Buffer
	code := RunWith(builder(), []string{"sideways"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("expected unknown-subcommand error, got %q", stderr.String())
	}
}

func TestRunWith_ReadCommandsRejectExtraArgs(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "read-commands-reject-extra-arguments")
	// status/verify/inspect take no positional args; a leftover token must be
	// rejected (exit 1) rather than silently ignored, matching up/down.
	for _, sub := range []string{"status", "verify", "inspect"} {
		var stdout, stderr bytes.Buffer
		code := RunWith(builder(), []string{sub, "unexpected"}, &stdout, &stderr)
		if code != 1 {
			t.Errorf("%s with extra arg: exit code = %d, want 1", sub, code)
		}
		if !strings.Contains(stderr.String(), "unexpected arguments") {
			t.Errorf("%s: expected an unexpected-arguments error, got %q", sub, stderr.String())
		}
	}
}

func TestRunWith_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWith(builder(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("expected usage on stdout, got %q", stdout.String())
	}
}

func TestRunWith_NilBuilder(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWith(nil, []string{"up"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunWith_PrepareFailureExits2(t *testing.T) {
	// A Source whose Kind has no Runner triggers CodeUnknownKind at
	// Prepare time (collectMigrationSources succeeds; the failure is
	// during the application's downstream Configure verifications —
	// here we just check it surfaces correctly).
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	// No runner-seeding plugin → orphan source → ApplyAll on `up` will
	// fail. Prepare itself succeeds.

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib), []string{"up"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (operational), got stderr=%q", code, stderr.String())
	}
}

func TestRunWith_PrepareFailureClosesEagerContainerResource(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "cancellation-cleanup", "prepare-failure-closes-eager-container-resource")
	closed := false

	var stdout, stderr bytes.Buffer
	code := RunWith(func() *app.Application {
		a := builder()()
		a.Provide(inject.Provide(
			inject.Named[string]("eager-resource"),
			func(inject.Resolver) (any, error) { return "open", nil },
			inject.WithOnClose(func() error {
				closed = true
				return nil
			}),
		))
		a.OnPostConfigure(func(context.Context, *inject.ContainerContext) error {
			return errors.New("post-configure failed")
		})
		return a
	}, []string{"status"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "prepare failed") {
		t.Fatalf("expected prepare failure on stderr, got %q", stderr.String())
	}
	if !closed {
		t.Fatal("eager DI resource was not closed after Prepare failed")
	}
}

func TestRunWith_OperationalErrorClosesPreparedContainer(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "cancellation-cleanup", "operational-error-closes-prepared-container")
	runner := &fakeRunner{
		kind:     migration.KindSQL,
		applyErr: errors.New("migration failed"),
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}
	closed := false

	var stdout, stderr bytes.Buffer
	code := RunWith(func() *app.Application {
		a := builder(contrib, seed)()
		a.Provide(inject.Provide(
			inject.Named[string]("prepared-resource"),
			func(inject.Resolver) (any, error) { return "open", nil },
			inject.WithOnClose(func() error {
				closed = true
				return nil
			}),
		))
		return a
	}, []string{"up"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !closed {
		t.Fatal("prepared DI resources were not closed after an operational error")
	}
}

func TestRunWith_PreparesWithoutStartingOrInvoking(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "prepare-only", "prepare-starts-no-plugin-and-runs-no-invoker")
	started := false
	invoked := false
	var stdout, stderr bytes.Buffer

	code := RunWith(func() *app.Application {
		a := builder(&startProbePlugin{started: &started})()
		a.InvokeFunc(func() { invoked = true })
		return a
	}, []string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if started {
		t.Fatal("runtime Start hook ran during migrate CLI preparation")
	}
	if invoked {
		t.Fatal("application invoker ran during migrate CLI preparation")
	}
}

func TestRunWith_UpAppliesAllRunners(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "prepare-only", "prepare-collects-the-migration-registry")
	runner := &fakeRunner{
		kind: migration.KindSQL,
		apply: []migration.Record{
			{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/001",
				Status: migration.StatusApplied, Target: "default"},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"up"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "iam/001") {
		t.Errorf("expected applied migration in output, got %q", stdout.String())
	}
}

func TestRunWith_UpToParsesTarget(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "up-forwards-explicit-target")
	runner := &fakeRunner{kind: migration.KindSQL}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"up", "to", "iam/001"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	// The target must reach the runner via ApplyOpts.To — symmetric with the
	// `down to` path asserted in TestRunWith_DownToForwardsTarget. A regression
	// dropping the target on `up` must fail here, not pass silently.
	if runner.gotApply.To != "iam/001" {
		t.Errorf("up to must forward target into ApplyOpts.To, got %q", runner.gotApply.To)
	}
	// `up` always applies forward through the target inclusively, so Force is set.
	if !runner.gotApply.Force {
		t.Errorf("up must set ApplyOpts.Force, got %v", runner.gotApply.Force)
	}
}

func TestRunWith_UpRejectsBareTargetWithoutTo(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "up-rejects-bare-target-without-to")
	runner := &fakeRunner{kind: migration.KindSQL}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"up", "iam/001"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (user error)", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Errorf("expected argument error, got %q", stderr.String())
	}
}

func TestRunWith_VerifyExits3OnDrift(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "drift-exits-3")
	runner := &fakeRunner{
		kind: migration.KindSQL,
		report: migration.DriftReport{
			Kind: migration.KindSQL,
			HashDrifts: []migration.HashDrift{
				{Namespace: "iam", Name: "iam/001",
					StoredHash: "aaaaaaaaaaaa", CurrentHash: "bbbbbbbbbbbb"},
			},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"verify"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (drift detected); stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "DRIFT DETECTED") {
		t.Errorf("expected DRIFT DETECTED in stdout, got %q", stdout.String())
	}
}

func TestRunWith_VerifyExits0OnClean(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "clean-verify-exits-0")
	runner := &fakeRunner{kind: migration.KindSQL} // empty report
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"verify"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no drift") {
		t.Errorf("expected 'no drift' in stdout, got %q", stdout.String())
	}
}

func TestRunWith_InspectIsValidJSON(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "inspect-emits-valid-json")
	runner := &fakeRunner{kind: migration.KindSQL}
	contrib := &contribPlugin{
		name: "iam",
		sources: []migration.Source{
			fakeSource{kind: migration.KindSQL, ns: "iam"},
			fakeSource{kind: migration.KindSQL, ns: "secrets"},
		},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"inspect"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}

	var view struct {
		Kinds []struct {
			Kind             string
			RunnerRegistered bool
			Sources          []struct {
				Namespace string
			}
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(view.Kinds) != 1 || view.Kinds[0].Kind != "sql" {
		t.Fatalf("unexpected kinds: %+v", view.Kinds)
	}
	if !view.Kinds[0].RunnerRegistered {
		t.Error("expected runner to be registered")
	}
	if len(view.Kinds[0].Sources) != 2 {
		t.Errorf("expected 2 sources, got %d", len(view.Kinds[0].Sources))
	}
}

func TestRunWith_StatusPropagatesRunnerOutput(t *testing.T) {
	runner := &fakeRunner{
		kind: migration.KindSQL,
		status: []migration.Record{
			{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/001",
				Status: migration.StatusApplied, Target: "default"},
			{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/002",
				Status: migration.StatusPending, Target: "default",
				Source: "embed:iam/migrations/002.up.sql"},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "iam/001") || !strings.Contains(out, "iam/002") {
		t.Errorf("expected both migrations in status output, got %q", out)
	}
	if !strings.Contains(out, "pending") || !strings.Contains(out, "applied") {
		t.Errorf("expected both statuses in output, got %q", out)
	}
}

func TestRunWith_UpSurfacesRunnerError(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "operational-failure-exits-2")
	runner := &fakeRunner{
		kind:     migration.KindSQL,
		applyErr: errors.New("boom"),
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"up"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (operational), got stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("expected runner error in stderr, got %q", stderr.String())
	}
}

func TestRunWith_DownRollsBackPerKind(t *testing.T) {
	runner := &fakeRunner{
		kind: migration.KindSQL,
		rollback: []migration.Record{
			{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/002",
				Status: migration.StatusRolledBack, Target: "default"},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "iam/002") {
		t.Errorf("expected rolled-back migration in output, got %q", stdout.String())
	}
	if runner.gotRollback.To != "" {
		t.Errorf("bare down must pass empty RollbackOpts.To, got %q", runner.gotRollback.To)
	}
}

func TestRunWith_DownToForwardsTarget(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "down-forwards-explicit-target")
	runner := &fakeRunner{
		kind: migration.KindSQL,
		rollback: []migration.Record{
			{Kind: migration.KindSQL, Name: "iam/002", Status: migration.StatusRolledBack},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down", "to", "iam/001"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	if runner.gotRollback.To != "iam/001" {
		t.Errorf("down to must forward target into RollbackOpts.To, got %q", runner.gotRollback.To)
	}
}

func TestRunWith_DownRejectsBareTargetWithoutTo(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "command-contract", "down-rejects-bare-target-without-to")
	runner := &fakeRunner{kind: migration.KindSQL}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down", "iam/001"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (user error)", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Errorf("expected argument error, got %q", stderr.String())
	}
}

func TestRunWith_DownSurfacesRunnerError(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "records-completed-before-an-error-are-emitted")
	runner := &fakeRunner{
		kind: migration.KindSQL,
		rollback: []migration.Record{
			{Kind: migration.KindSQL, Name: "iam/002", Status: migration.StatusRolledBack},
		},
		rollbackErr: errors.New("rollback boom"),
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (operational), got stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "rollback boom") {
		t.Errorf("expected runner error in stderr, got %q", stderr.String())
	}
	// Partial records returned before the error must still be printed.
	if !strings.Contains(stdout.String(), "iam/002") {
		t.Errorf("expected partial rolled-back record in stdout, got %q", stdout.String())
	}
}

func TestRunWith_DownNothingToRollBack(t *testing.T) {
	runner := &fakeRunner{kind: migration.KindSQL} // empty rollback slice
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nothing to roll back") {
		t.Errorf("expected 'nothing to roll back', got %q", stdout.String())
	}
}

func TestRunWith_DownAggregatesAcrossKinds(t *testing.T) {
	sqlRunner := &fakeRunner{
		kind: migration.KindSQL,
		rollback: []migration.Record{
			{Kind: migration.KindSQL, Name: "iam/002", Status: migration.StatusRolledBack},
		},
	}
	gcsRunner := &fakeRunner{
		kind: migration.Kind("gcs"),
		rollback: []migration.Record{
			{Kind: migration.Kind("gcs"), Name: "blobs/002", Status: migration.StatusRolledBack},
		},
	}
	sqlContrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	gcsContrib := &contribPlugin{
		name:    "blobs",
		sources: []migration.Source{fakeSource{kind: migration.Kind("gcs"), ns: "blobs"}},
	}

	var stdout, stderr bytes.Buffer
	code := RunWith(
		builder(sqlContrib, gcsContrib,
			&runnerSeedingPlugin{runner: sqlRunner},
			&runnerSeedingPlugin{runner: gcsRunner}),
		[]string{"down"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "iam/002") || !strings.Contains(out, "blobs/002") {
		t.Errorf("expected both kinds' rolled-back records, got %q", out)
	}
}

// --- machine-readable output (--output=json|jsonl) ------------------------

// twoRecords is a fixed pair used by the output-format tests; the second
// record exercises every Record JSON tag the CLI propagates (Source +
// Target + a non-applied Status) so the assertions catch tag drift.
func twoRecords() []migration.Record {
	return []migration.Record{
		{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/001",
			Status: migration.StatusApplied, Target: "default"},
		{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/002",
			Status: migration.StatusPending, Target: "default",
			Source: "embed:iam/migrations/002.up.sql"},
	}
}

// decodeJSONL parses one JSON object per non-empty line.
func decodeJSONL(t *testing.T, s string) []migration.Record {
	t.Helper()
	var out []migration.Record
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var r migration.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("jsonl line is not a parseable JSON object: %v\nline: %q", err, line)
		}
		out = append(out, r)
	}
	return out
}

func setupRecordRunner(status []migration.Record) (*contribPlugin, *runnerSeedingPlugin) {
	runner := &fakeRunner{kind: migration.KindSQL, status: status, apply: status}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	return contrib, &runnerSeedingPlugin{runner: runner}
}

func TestRunWith_StatusOutputJSONL(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "status-emits-jsonl")
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status", "--output=jsonl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	// No tabwriter header may leak into machine-readable output.
	if strings.Contains(out, "KIND\t") || strings.Contains(out, "KIND ") {
		t.Errorf("jsonl output must not contain the table header, got %q", out)
	}
	recs := decodeJSONL(t, out)
	if len(recs) != 2 {
		t.Fatalf("expected 2 jsonl records, got %d: %q", len(recs), out)
	}
	if recs[0].Name != "iam/001" || recs[0].Status != migration.StatusApplied {
		t.Errorf("record[0] fields wrong: %+v", recs[0])
	}
	if recs[1].Name != "iam/002" || recs[1].Status != migration.StatusPending ||
		recs[1].Source != "embed:iam/migrations/002.up.sql" || recs[1].Target != "default" {
		t.Errorf("record[1] fields wrong: %+v", recs[1])
	}
}

func TestRunWith_StatusOutputJSONArray(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "status-emits-json-array")
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status", "--output=json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	var recs []migration.Record
	if err := json.Unmarshal(stdout.Bytes(), &recs); err != nil {
		t.Fatalf("json output is not a parseable array: %v\n%s", err, stdout.String())
	}
	if len(recs) != 2 || recs[0].Name != "iam/001" || recs[1].Name != "iam/002" {
		t.Fatalf("unexpected records: %+v", recs)
	}
	// Verify the array carries the documented JSON tags verbatim.
	var raw []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	for _, want := range []string{"kind", "name", "status"} {
		if _, ok := raw[0][want]; !ok {
			t.Errorf("missing JSON tag %q in object %v", want, raw[0])
		}
	}
	if v, ok := raw[1]["source"]; !ok || v != "embed:iam/migrations/002.up.sql" {
		t.Errorf("expected source tag on record[1], got %v", raw[1])
	}
}

func TestRunWith_UpOutputJSONL(t *testing.T) {
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"up", "--output=jsonl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	recs := decodeJSONL(t, stdout.String())
	if len(recs) != 2 {
		t.Fatalf("expected 2 jsonl records from up, got %d: %q", len(recs), stdout.String())
	}
	if recs[0].Kind != migration.KindSQL {
		t.Errorf("expected kind tag populated, got %+v", recs[0])
	}
}

func TestRunWith_DownOutputJSONL(t *testing.T) {
	runner := &fakeRunner{
		kind: migration.KindSQL,
		rollback: []migration.Record{
			{Kind: migration.KindSQL, Namespace: "iam", Name: "iam/002",
				Status: migration.StatusRolledBack, Target: "default"},
		},
	}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"down", "--output=jsonl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	recs := decodeJSONL(t, stdout.String())
	if len(recs) != 1 || recs[0].Name != "iam/002" || recs[0].Status != migration.StatusRolledBack {
		t.Fatalf("unexpected down jsonl records: %+v (%q)", recs, stdout.String())
	}
}

// The --output flag must be stripped before parseToArg runs, so it composes
// with the positional `to <name>` form (and the space-separated spelling).
func TestRunWith_UpToWithOutputFlag(t *testing.T) {
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed),
		[]string{"up", "to", "iam/002", "--output", "jsonl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d (flag should not be seen as a positional arg), stderr=%q",
			code, stderr.String())
	}
	if len(decodeJSONL(t, stdout.String())) != 2 {
		t.Errorf("expected jsonl records, got %q", stdout.String())
	}
}

func TestRunWith_DefaultStillTable(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "default-output-is-table")
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "KIND") || !strings.Contains(out, "NAMESPACE") {
		t.Errorf("default output must be the tabwriter table, got %q", out)
	}
	// Default output must NOT be valid JSON (proves the format branch is gated).
	if json.Valid(bytes.TrimSpace(stdout.Bytes())) {
		t.Errorf("default output unexpectedly parses as JSON: %q", out)
	}
}

func TestRunWith_UnknownOutputValueIsUserError(t *testing.T) {
	contrib, seed := setupRecordRunner(twoRecords())

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status", "--output=yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (user error)", code)
	}
	if !strings.Contains(stderr.String(), "unknown --output value") {
		t.Errorf("expected unknown-output error, got %q", stderr.String())
	}
}

// An empty result set in a machine-readable format must still be parseable:
// json => "[]", jsonl => zero lines.
func TestRunWith_EmptyResultJSONIsEmptyArray(t *testing.T) {
	spectest.Proves(t, "go/migration-cli", "machine-output", "empty-result-is-an-empty-json-array")
	runner := &fakeRunner{kind: migration.KindSQL} // empty status slice
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	seed := &runnerSeedingPlugin{runner: runner}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, seed), []string{"status", "--output=json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	var recs []migration.Record
	if err := json.Unmarshal(stdout.Bytes(), &recs); err != nil {
		t.Fatalf("empty json output is not a parseable array: %v\n%q", err, stdout.String())
	}
	if len(recs) != 0 {
		t.Errorf("expected empty array, got %+v", recs)
	}
	// The human "no migrations registered" line must not leak in json mode.
	if strings.Contains(stdout.String(), "no migrations registered") {
		t.Errorf("human message leaked into json output: %q", stdout.String())
	}
}

func TestRunWith_DownRejectsOrphanSources(t *testing.T) {
	// A source whose kind has no registered runner must fail `down` just as
	// it fails up/status/verify — `down` no longer tolerates orphan sources.
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}
	// No runner-seeding plugin → orphan source.

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib), []string{"down"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (operational); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "migrate down:") {
		t.Errorf("expected 'migrate down:' error prefix in stderr, got %q", stderr.String())
	}
}

// signalingRunner sends SIGTERM to this process from inside Apply, then blocks
// until the context it was handed is canceled. Blocking is what makes the test
// safe as well as meaningful: the signal is delivered while RunWith's
// NotifyContext handler is still installed, so it can never fall through to the
// default disposition and kill the test binary.
type signalingRunner struct {
	kind      migration.Kind
	sawCancel bool
	timedOut  bool
}

func (r *signalingRunner) Kind() migration.Kind { return r.kind }

func (r *signalingRunner) Apply(ctx context.Context, _ migration.ApplyOpts) ([]migration.Record, error) {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		r.sawCancel = true
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		r.timedOut = true
		return nil, errors.New("registry context was never canceled by SIGTERM")
	}
}

func (r *signalingRunner) Status(context.Context) ([]migration.Record, error) { return nil, nil }
func (r *signalingRunner) Rollback(context.Context, migration.RollbackOpts) ([]migration.Record, error) {
	return nil, nil
}
func (r *signalingRunner) Verify(context.Context) (migration.DriftReport, error) {
	return migration.DriftReport{}, nil
}

// TestRunWith_SignalCancelsInFlightRegistryWork asserts an operator's Ctrl-C
// reaches a running migration through the context the CLI hands the registry,
// so a long apply can unwind rather than being killed mid-statement.
func TestRunWith_SignalCancelsInFlightRegistryWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a process cannot send itself SIGTERM on Windows")
	}
	spectest.Proves(t, "go/migration-cli", "cancellation-cleanup", "signal-cancels-in-flight-registry-work")

	runner := &signalingRunner{kind: migration.KindSQL}
	contrib := &contribPlugin{
		name:    "iam",
		sources: []migration.Source{fakeSource{kind: migration.KindSQL, ns: "iam"}},
	}

	var stdout, stderr bytes.Buffer
	code := RunWith(builder(contrib, &runnerSeedingPlugin{runner: runner}),
		[]string{"up"}, &stdout, &stderr)

	if runner.timedOut {
		t.Fatal("SIGTERM did not cancel the context handed to the registry")
	}
	if !runner.sawCancel {
		t.Fatal("the runner never observed cancellation")
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (operational); stderr=%q", code, stderr.String())
	}
}
