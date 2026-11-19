package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// runsStore is the smallest provider that honors the missions v2 run
// operations: one lock around a map of runs. It serves one caller, so every
// run it started is the caller's. The run scenarios must pass against it.
type runsStore struct {
	mu   sync.Mutex
	next int
	runs map[string]*storedRun
	// starts maps an idempotency key to the run it started and the content
	// it started with.
	starts map[string]startRecord
	// reports holds the report that ended each run, to replay it.
	reports map[string]string
	// reportsTwice, when set, lets a later report end a run again: the fault
	// the run scenarios must catch.
	reportsTwice bool
}

type storedRun struct {
	run collab.Run
	seq int
}

type startRecord struct {
	run     string
	content string
}

func newRunsStore() *runsStore {
	return &runsStore{runs: map[string]*storedRun{}, starts: map[string]startRecord{}, reports: map[string]string{}}
}

func (s *runsStore) handlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMissions, Version: 2, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationStart):  s.start,
		key(collab.OperationReport): s.report,
		key(collab.OperationRuns):   s.list,
	}
}

func (s *runsStore) start(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MissionsStartInput)
	content := encode(struct {
		Repository, Key string
		Kind            collab.RunKind
		Input           collab.RunInput
		Session         *collab.SessionRef
	}{in.Mission.Repository, in.Mission.Key, in.Kind, in.Input, in.Session})
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.starts[in.IdempotencyKey]; ok {
		if record.content != content {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, "the key started another run")
		}
		return &collab.MissionsRunResult{Run: s.runs[record.run].run, Replayed: true}, nil
	}
	s.next++
	id := "run_" + strconv.Itoa(s.next)
	run := collab.Run{
		ID: id, Mission: collab.MissionName{Repository: in.Mission.Repository, Key: in.Mission.Key},
		Kind: in.Kind, State: collab.RunStateRunning, Input: in.Input, Session: in.Session,
		StartedAt: time.Date(2026, 11, 19, 9, 0, s.next, 0, time.UTC).Format(time.RFC3339),
	}
	s.runs[id] = &storedRun{run: run, seq: s.next}
	s.starts[in.IdempotencyKey] = startRecord{run: id, content: content}
	return &collab.MissionsRunResult{Run: run}, nil
}

func (s *runsStore) report(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MissionsReportInput)
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.runs[in.Run]
	if !ok {
		return nil, collab.Fail(collab.OutcomeNotFound, collab.ReasonRunNotFound, "no run %s", in.Run)
	}
	content := encode(in)
	if stored.run.State != collab.RunStateRunning && !s.reportsTwice {
		if s.reports[in.Run] == content {
			return &collab.MissionsRunResult{Run: stored.run, Replayed: true}, nil
		}
		return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonRunClosed, "run %s already ended", in.Run)
	}
	if in.Result != nil && in.Result.Kind() != stored.run.Kind {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRunKindMismatch, "the result is not of the run's kind")
	}
	stored.run.State, stored.run.Result, stored.run.Failure = in.State, in.Result, in.Failure
	stored.run.EndedAt = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	s.reports[in.Run] = content
	return &collab.MissionsRunResult{Run: stored.run}, nil
}

func (s *runsStore) list(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MissionsRunsInput)
	s.mu.Lock()
	defer s.mu.Unlock()
	var listed []*storedRun
	for _, stored := range s.runs {
		if stored.run.Mission.Repository == in.Mission.Repository && stored.run.Mission.Key == in.Mission.Key &&
			(in.Kind == "" || stored.run.Kind == in.Kind) {
			listed = append(listed, stored)
		}
	}
	// The latest start first: the store's clock ticks once per start.
	slices.SortFunc(listed, func(a, b *storedRun) int { return b.seq - a.seq })
	offset := 0
	if in.Page.Cursor != "" {
		offset, _ = strconv.Atoi(in.Page.Cursor)
	}
	result := &collab.MissionsRunsResult{Items: []collab.Run{}}
	for i := offset; i < len(listed) && len(result.Items) < in.Page.Size; i++ {
		result.Items = append(result.Items, listed[i].run)
	}
	if next := offset + len(result.Items); next < len(listed) {
		result.Page.Next = strconv.Itoa(next)
	}
	return result, nil
}

// runsTarget serves the runs of store under missions version 2 and the
// missions v1 operations of a fresh missions store under versions 1 and 2.
func runsTarget(t *testing.T, store *runsStore) Target {
	target := missionsTarget(t, newMissionsStore())
	handlers := store.handlers()
	for key, handler := range target.Handlers {
		handlers[key] = handler
		key.Version = 2
		handlers[key] = handler
	}
	target.Handlers = handlers
	return target
}

func TestTheRunScenariosPassAgainstACorrectProvider(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-run-semantics", "the-run-scenarios-pass-against-a-correct-provider")
	RunMissionRuns(t, func(t *testing.T) Target { return runsTarget(t, newRunsStore()) })
}

// TestTheRunScenariosRequireTheRunOperations: a target without a run
// operation fails every scenario.
func TestTheRunScenariosRequireTheRunOperations(t *testing.T) {
	if os.Getenv("PROVIDERTEST_RUNS_CHILD") == "missing" {
		RunMissionRuns(t, func(t *testing.T) Target {
			target := runsTarget(t, newRunsStore())
			delete(target.Handlers, collab.OperationKey{Contract: collab.ContractMissions, Version: 2, Operation: collab.OperationRuns})
			return target
		})
		return
	}
	output := runChild(t, "TestTheRunScenariosRequireTheRunOperations", "missing")
	if !strings.Contains(output, "a missions v2 target answers missions.runs@v2") {
		t.Fatalf("a target without runs did not fail on it:\n%s", output)
	}
}

// TestTheRunScenariosCatchAProviderThatReportsTwice runs the scenarios in a
// child test process against a provider that lets a second report end a run
// again, and checks that the closed-run scenario fails on it.
func TestTheRunScenariosCatchAProviderThatReportsTwice(t *testing.T) {
	if os.Getenv("PROVIDERTEST_RUNS_CHILD") == "twice" {
		RunMissionRuns(t, func(t *testing.T) Target {
			store := newRunsStore()
			store.reportsTwice = true
			return runsTarget(t, store)
		})
		return
	}
	spectest.Proves(t, "collaboration/provider-contract", "missions-run-semantics", "the-run-scenarios-catch-a-provider-that-reports-twice")
	output := runChild(t, "TestTheRunScenariosCatchAProviderThatReportsTwice", "twice")
	for _, want := range []string{
		"--- FAIL: TestTheRunScenariosCatchAProviderThatReportsTwice/another_report_on_an_ended_run_is_a_conflict",
		"--- PASS: TestTheRunScenariosCatchAProviderThatReportsTwice/a_report_on_an_unknown_run_is_not_found",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("the child run lacks %q:\n%s", want, output)
		}
	}
}

// runChild runs one test of this binary again with PROVIDERTEST_RUNS_CHILD
// set to mode, expects it to fail, and returns its verbose output.
func runChild(t *testing.T, test, mode string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.v")
	cmd.Env = append(os.Environ(), "PROVIDERTEST_RUNS_CHILD="+mode)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the scenarios passed against a faulty provider:\n%s", output)
	}
	return string(output)
}

// TestTheMissionsScenariosPassOnVersion2: the missions v1 scenarios pass
// against a provider that serves them under version 2 and is called on it.
func TestTheMissionsScenariosPassOnVersion2(t *testing.T) {
	RunMissions(t, func(t *testing.T) Target {
		store := newMissionsStore()
		target := missionsTarget(t, store)
		v2 := map[collab.OperationKey]collab.Handler{}
		for key, handler := range target.Handlers {
			key.Version = 2
			v2[key] = handler
		}
		maps.Copy(v2, newRunsStore().handlers())
		target.Handlers, target.MissionsVersion = v2, 2
		return target
	})
}

// TestTheRunStoreAnswersWhatTheContractAccepts guards the reference store: a
// start it answers parses as a version 2 result.
func TestTheRunStoreAnswersWhatTheContractAccepts(t *testing.T) {
	store := newRunsStore()
	in := &collab.MissionsStartInput{
		Mission: collab.MissionName{Repository: "acme/app", Key: "k"}, Kind: collab.RunKindBuild,
		Input: collab.RunInput{Build: &collab.BuildRunInput{Intention: "x"}}, IdempotencyKey: "k",
	}
	result, failure := store.start(context.Background(), collab.Call{Input: in})
	if failure != nil {
		t.Fatal(failure)
	}
	encoded, _ := json.Marshal(result)
	if _, diags := collab.ParseResult(collab.ContractMissions, 2, collab.OperationStart, encoded); diags != nil {
		t.Fatal(fmt.Sprint(diags))
	}
}
