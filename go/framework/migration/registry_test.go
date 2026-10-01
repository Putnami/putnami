package migration

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/errors"
)

// --- test doubles ---------------------------------------------------------

type stubSource struct {
	kind Kind
	ns   string
}

func (s stubSource) Kind() Kind        { return s.kind }
func (s stubSource) Namespace() string { return s.ns }

type stubRunner struct {
	kind        Kind
	applied     []Record
	status      []Record
	rolled      []Record
	report      DriftReport
	applyErr    error
	rollbackErr error
	calls       struct {
		apply, status, rollback, verify int
		lastApply                       ApplyOpts
		lastRollback                    RollbackOpts
	}
}

func (r *stubRunner) Kind() Kind { return r.kind }

func (r *stubRunner) Apply(_ context.Context, opts ApplyOpts) ([]Record, error) {
	r.calls.apply++
	r.calls.lastApply = opts
	return r.applied, r.applyErr
}

func (r *stubRunner) Status(_ context.Context) ([]Record, error) {
	r.calls.status++
	return r.status, nil
}

func (r *stubRunner) Rollback(_ context.Context, opts RollbackOpts) ([]Record, error) {
	r.calls.rollback++
	r.calls.lastRollback = opts
	return r.rolled, r.rollbackErr
}

func (r *stubRunner) Verify(_ context.Context) (DriftReport, error) {
	r.calls.verify++
	return r.report, nil
}

// --- tests ----------------------------------------------------------------

func TestRegistry_AddSourceValidates(t *testing.T) {
	r := NewRegistry()

	if err := r.AddSource(nil); err == nil {
		t.Fatal("nil source must be rejected")
	}
	if err := r.AddSource(stubSource{kind: "", ns: "iam"}); err == nil {
		t.Fatal("empty Kind must be rejected")
	}
	if err := r.AddSource(stubSource{kind: KindSQL, ns: "   "}); err == nil {
		t.Fatal("blank Namespace must be rejected")
	}
}

func TestRegistry_RegisterRunnerDuplicate(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "runner-ownership", "exactly-one-runner-may-own-a-kind")
	r := NewRegistry()
	if err := r.RegisterRunner(&stubRunner{kind: KindSQL}); err != nil {
		t.Fatalf("first registration must succeed: %v", err)
	}
	err := r.RegisterRunner(&stubRunner{kind: KindSQL})
	if err == nil {
		t.Fatal("duplicate kind must be rejected")
	}
	var pe *errors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeDuplicateRunner {
		t.Fatalf("expected CodeDuplicateRunner, got %v", err)
	}
}

func TestRegistry_KindsLexicographicAndUnionsSourcesAndRunners(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "deterministic-order", "kinds-execute-in-lexicographic-order")
	r := NewRegistry()
	_ = r.RegisterRunner(&stubRunner{kind: "gcs"})
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})
	_ = r.AddSource(stubSource{kind: "document", ns: "wealth"})

	want := []Kind{"document", "gcs", "sql"}
	got := r.Kinds()
	if !slices.Equal(got, want) {
		t.Fatalf("Kinds order = %v, want %v", got, want)
	}
}

func TestRegistry_SourcesReturnsCopy(t *testing.T) {
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: KindSQL, ns: "iam"})

	got := r.Sources(KindSQL)
	if len(got) != 1 {
		t.Fatalf("Sources len = %d, want 1", len(got))
	}
	got[0] = stubSource{kind: KindSQL, ns: "EVIL"}

	// Mutating the returned slice must not affect the registry.
	again := r.Sources(KindSQL)
	if again[0].Namespace() != "iam" {
		t.Fatalf("Sources returned a shared slice; got %q after external mutation", again[0].Namespace())
	}
}

func TestRegistry_ApplyAllIteratesKindsInOrder(t *testing.T) {
	r := NewRegistry()
	rSQL := &stubRunner{kind: "sql", applied: []Record{{Kind: "sql", Name: "a"}}}
	rGCS := &stubRunner{kind: "gcs", applied: []Record{{Kind: "gcs", Name: "b"}}}
	_ = r.RegisterRunner(rSQL)
	_ = r.RegisterRunner(rGCS)

	recs, err := r.ApplyAll(context.Background(), ApplyOpts{Force: true})
	if err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// gcs < sql lexicographically.
	if recs[0].Kind != "gcs" || recs[1].Kind != "sql" {
		t.Fatalf("records out of order: %v", recs)
	}
	if rGCS.calls.apply != 1 || rSQL.calls.apply != 1 {
		t.Fatalf("each runner must Apply once, got gcs=%d sql=%d",
			rGCS.calls.apply, rSQL.calls.apply)
	}
	if !rGCS.calls.lastApply.Force || !rSQL.calls.lastApply.Force {
		t.Fatalf("ApplyOpts must be forwarded to runners: gcs=%+v sql=%+v",
			rGCS.calls.lastApply, rSQL.calls.lastApply)
	}
}

func TestRegistry_ApplyAllRejectsOrphanSourcesForExplicitApply(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "runner-ownership", "explicit-apply-fails-before-execution-without-a-runner")
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})
	// No SQL runner registered → explicit apply must error.

	_, err := r.ApplyAll(context.Background(), ApplyOpts{Force: true})
	if err == nil {
		t.Fatal("explicit ApplyAll must error on orphan sources")
	}
	var pe *errors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeUnknownKind {
		t.Fatalf("expected CodeUnknownKind, got %v", err)
	}
	if !strings.Contains(err.Error(), "iam") {
		t.Fatalf("error must name the contributing plugin, got %v", err)
	}
}

func TestRegistry_ApplyAllRejectsOrphanSourcesByDefault(t *testing.T) {
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})

	_, err := r.ApplyAll(context.Background(), ApplyOpts{})
	if err == nil {
		t.Fatal("default ApplyAll must error on orphan sources")
	}
	var pe *errors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeUnknownKind {
		t.Fatalf("expected CodeUnknownKind, got %v", err)
	}
}

func TestRegistry_ApplyAllAllowSourceOnlyToleratesOrphanSources(t *testing.T) {
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})
	// Lifecycle apply opts into allowing source-only kinds as static metadata
	// until a runner is present or the caller explicitly runs migrations.

	recs, err := r.ApplyAll(context.Background(), ApplyOpts{AllowSourceOnly: true})
	if err != nil {
		t.Fatalf("AllowSourceOnly ApplyAll must tolerate orphan sources: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected no records without a runner, got %v", recs)
	}
}

func TestRegistry_ApplyAllShortCircuitsButKeepsPriorRecords(t *testing.T) {
	r := NewRegistry()
	rGCS := &stubRunner{kind: "gcs", applied: []Record{{Kind: "gcs", Name: "ok"}}}
	rSQL := &stubRunner{
		kind:     "sql",
		applied:  []Record{{Kind: "sql", Name: "before-err"}},
		applyErr: errors.Newf(CodeApplyFailed, "boom"),
	}
	_ = r.RegisterRunner(rGCS)
	_ = r.RegisterRunner(rSQL)

	recs, err := r.ApplyAll(context.Background(), ApplyOpts{})
	if err == nil {
		t.Fatal("expected ApplyAll error")
	}
	// Must return: gcs success record + sql partial record before the error.
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (gcs + sql-before-err), got %d", len(recs))
	}
}

func TestRegistry_VerifyAllAggregatesPerKind(t *testing.T) {
	r := NewRegistry()
	rSQL := &stubRunner{
		kind:   "sql",
		report: DriftReport{Kind: "sql", HashDrifts: []HashDrift{{Name: "x"}}},
	}
	rGCS := &stubRunner{kind: "gcs", report: DriftReport{Kind: "gcs"}}
	_ = r.RegisterRunner(rSQL)
	_ = r.RegisterRunner(rGCS)

	reports, err := r.VerifyAll(context.Background())
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
	if reports[0].Kind != "gcs" || !reports[0].Empty() {
		t.Fatalf("first report should be empty gcs, got %+v", reports[0])
	}
	if reports[1].Kind != "sql" || reports[1].Empty() {
		t.Fatalf("second report should be non-empty sql, got %+v", reports[1])
	}
}

func TestRegistry_RollbackForwardsOpts(t *testing.T) {
	r := NewRegistry()
	rSQL := &stubRunner{kind: "sql"}
	_ = r.RegisterRunner(rSQL)

	opts := RollbackOpts{To: "iam/20260520120000_create_users"}
	if run, ok := r.Runner("sql"); ok {
		_, _ = run.Rollback(context.Background(), opts)
	}

	if rSQL.calls.rollback != 1 {
		t.Fatalf("Rollback must be called once, got %d", rSQL.calls.rollback)
	}
	if rSQL.calls.lastRollback.To != opts.To {
		t.Fatalf("RollbackOpts.To not forwarded: got %q want %q",
			rSQL.calls.lastRollback.To, opts.To)
	}
}

func TestRegistry_StatusAllAggregatesPerKind(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "deterministic-order", "per-kind-results-aggregate-in-that-same-order")
	r := NewRegistry()
	rSQL := &stubRunner{kind: "sql", status: []Record{{Kind: "sql", Name: "sql-status"}}}
	rGCS := &stubRunner{kind: "gcs", status: []Record{{Kind: "gcs", Name: "gcs-status"}}}
	_ = r.RegisterRunner(rSQL)
	_ = r.RegisterRunner(rGCS)

	recs, err := r.StatusAll(context.Background())
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// gcs < sql lexicographically.
	if recs[0].Kind != "gcs" || recs[1].Kind != "sql" {
		t.Fatalf("records out of order: %v", recs)
	}
	if rGCS.calls.status != 1 || rSQL.calls.status != 1 {
		t.Fatalf("each runner must Status once, got gcs=%d sql=%d",
			rGCS.calls.status, rSQL.calls.status)
	}
}

func TestRegistry_StatusAllRejectsOrphanSources(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "runner-ownership", "status-fails-before-execution-without-a-runner")
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})
	// No SQL runner registered → StatusAll must error.

	_, err := r.StatusAll(context.Background())
	if err == nil {
		t.Fatal("StatusAll must error on orphan sources")
	}
	var pe *errors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeUnknownKind {
		t.Fatalf("expected CodeUnknownKind, got %v", err)
	}
	if !strings.Contains(err.Error(), "iam") {
		t.Fatalf("error must name the contributing plugin, got %v", err)
	}
}

func TestRegistry_RollbackAllAggregatesPerKindAndForwardsOpts(t *testing.T) {
	r := NewRegistry()
	rSQL := &stubRunner{kind: "sql", rolled: []Record{{Kind: "sql", Name: "sql-rb"}}}
	rGCS := &stubRunner{kind: "gcs", rolled: []Record{{Kind: "gcs", Name: "gcs-rb"}}}
	_ = r.RegisterRunner(rSQL)
	_ = r.RegisterRunner(rGCS)

	opts := RollbackOpts{To: "iam/20260520120000_create_users"}
	recs, err := r.RollbackAll(context.Background(), opts)
	if err != nil {
		t.Fatalf("RollbackAll: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// gcs < sql lexicographically.
	if recs[0].Kind != "gcs" || recs[1].Kind != "sql" {
		t.Fatalf("records out of order: %v", recs)
	}
	if rGCS.calls.rollback != 1 || rSQL.calls.rollback != 1 {
		t.Fatalf("each runner must Rollback once, got gcs=%d sql=%d",
			rGCS.calls.rollback, rSQL.calls.rollback)
	}
	if rGCS.calls.lastRollback.To != opts.To || rSQL.calls.lastRollback.To != opts.To {
		t.Fatalf("RollbackOpts must be forwarded to runners: gcs=%+v sql=%+v",
			rGCS.calls.lastRollback, rSQL.calls.lastRollback)
	}
}

func TestRegistry_RollbackAllRejectsOrphanSources(t *testing.T) {
	spectest.Proves(t, "go/migration-execution", "runner-ownership", "rollback-fails-before-execution-without-a-runner")
	r := NewRegistry()
	_ = r.AddSource(stubSource{kind: "sql", ns: "iam"})
	// No SQL runner registered → RollbackAll must reject the orphan source,
	// matching ApplyAll/StatusAll/VerifyAll (the asymmetry runDown used to have).

	_, err := r.RollbackAll(context.Background(), RollbackOpts{})
	if err == nil {
		t.Fatal("RollbackAll must error on orphan sources")
	}
	var pe *errors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeUnknownKind {
		t.Fatalf("expected CodeUnknownKind, got %v", err)
	}
}

func TestRegistry_RollbackAllShortCircuitsButKeepsPriorRecords(t *testing.T) {
	r := NewRegistry()
	rGCS := &stubRunner{kind: "gcs", rolled: []Record{{Kind: "gcs", Name: "ok"}}}
	rSQL := &stubRunner{
		kind:        "sql",
		rolled:      []Record{{Kind: "sql", Name: "before-err"}},
		rollbackErr: errors.Newf(CodeRollbackFailed, "boom"),
	}
	_ = r.RegisterRunner(rGCS)
	_ = r.RegisterRunner(rSQL)

	recs, err := r.RollbackAll(context.Background(), RollbackOpts{})
	if err == nil {
		t.Fatal("expected RollbackAll error")
	}
	// Must return gcs success record + sql partial record before the error.
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (gcs + sql-before-err), got %d", len(recs))
	}
}

func TestRegistry_ConcurrentRegistrationAndReadsAreRaceFree(t *testing.T) {
	// Exercises the documented contract: AddSource and RegisterRunner are
	// safe to call concurrently with each other and with the read accessors
	// Sources, Runner, Kinds. Run under `go test -race` to catch a regression
	// that narrows the critical section. The drive methods (ApplyAll, etc.)
	// are documented as NOT safe to interleave with mutation, so they are not
	// driven concurrently here.
	r := NewRegistry()
	const n = 64
	var wg sync.WaitGroup

	for i := range n {
		kind := Kind(fmt.Sprintf("kind-%02d", i))
		ns := fmt.Sprintf("ns-%02d", i)
		wg.Add(2)
		// Writer: register a unique runner and contribute a source for it.
		go func() {
			defer wg.Done()
			if err := r.RegisterRunner(&stubRunner{kind: kind}); err != nil {
				t.Errorf("RegisterRunner(%s): %v", kind, err)
			}
			if err := r.AddSource(stubSource{kind: kind, ns: ns}); err != nil {
				t.Errorf("AddSource(%s): %v", kind, err)
			}
		}()
		// Reader: hammer the read accessors concurrently.
		go func() {
			defer wg.Done()
			_ = r.Kinds()
			_, _ = r.Runner(kind)
			_ = r.Sources(kind)
		}()
	}
	wg.Wait()

	if got := len(r.Kinds()); got != n {
		t.Fatalf("expected %d kinds after concurrent registration, got %d", n, got)
	}
}
