package providertest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// memoryWriters is how many writers the concurrency scenarios race.
const memoryWriters = 6

// RunMemory runs the memory v1 scenarios, each against a fresh target. A
// memory target names its Workspace and can Reopen its store.
func RunMemory(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(t *testing.T, s *Session)
	}{
		{"an unknown mission has no record and no context", memoryUnknownMission},
		{"create, replay and refuse a second creation", memoryCreateAndReplay},
		{"a checkpoint moves the revision and a stale one is a conflict", memoryStaleRevision},
		{"another provider instance resumes the mission", memoryResume},
		{"concurrent checkpoints at one revision land once", memoryConcurrentCheckpoints},
		{"concurrent creations land once", memoryConcurrentCreations},
		{"pages cover the records exactly once", memoryPages},
		{"a record stays within its workspace", memoryOtherWorkspace},
		{"a maximal checkpoint is read back unchanged", memoryMaximalCheckpoint},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			target := newTarget(t)
			if target.Workspace == "" || target.Reopen == nil {
				t.Fatal("a memory target names its Workspace and can Reopen its store")
			}
			scenario.run(t, Open(t, target))
		})
	}
}

// reopen is a session on another instance of the provider, over the same
// store.
func (s *Session) reopen() *Session {
	s.t.Helper()
	target := s.target
	target.Handlers = s.target.Reopen(s.t)
	return &Session{t: s.t, target: target}
}

// checkpointArguments is a memory.checkpoint request. expected is a revision,
// or "" for mustNotExist; extra members are merged in.
func checkpointArguments(mission, key, content, expected string, extra map[string]any) string {
	precondition := map[string]any{"mustNotExist": true}
	if expected != "" {
		precondition = map[string]any{"expectedRevision": expected}
	}
	arguments := map[string]any{"mission": mission, "idempotencyKey": key, "content": content, "precondition": precondition}
	for name, value := range extra {
		arguments[name] = value
	}
	encoded, _ := json.Marshal(arguments)
	return string(encoded)
}

// checkpoint sends a checkpoint that must succeed, and checks that its record
// is the mission it names, in the served workspace, with the content sent.
func (s *Session) checkpoint(mission, content, arguments string) collab.MemoryCheckpointResult {
	s.t.Helper()
	var result collab.MemoryCheckpointResult
	s.OK(collab.ContractMemory, collab.OperationCheckpoint, arguments, &result)
	s.sameMission(result.Record, mission)
	if result.Record.Content != content {
		s.t.Fatalf("checkpoint of %q answered content %q, sent %q", mission, result.Record.Content, content)
	}
	if !result.Replayed {
		s.track(collab.ContractMemory, result.Record.Ref)
	}
	return result
}

// mission reads a mission that must exist.
func (s *Session) mission(name string) collab.MemoryRecord {
	s.t.Helper()
	var result collab.MemoryRecordResult
	s.OK(collab.ContractMemory, collab.OperationMission, `{"mission":`+quote(name)+`}`, &result)
	s.sameMission(result.Record, name)
	return result.Record
}

// sameMission fails unless a record is the mission record of name in the
// served workspace.
func (s *Session) sameMission(record collab.MemoryRecord, name string) {
	s.t.Helper()
	if record.Kind != collab.MemoryKindMission || record.Identity.Mission != name || record.Identity.Workspace != s.target.Workspace {
		s.t.Fatalf("record %s is the %s of mission %q in workspace %q; requested mission %q in workspace %q", record.Ref.ID,
			record.Kind, record.Identity.Mission, record.Identity.Workspace, name, s.target.Workspace)
	}
}

// conflict checks a refused checkpoint: its reason, that it is not retryable
// as is, and the current revision when the provider states one.
func conflict(t *testing.T, failure collab.Error, reason, current string) {
	t.Helper()
	if failure.Reason != reason || failure.Retryable || (failure.Current != "" && failure.Current != current) {
		t.Fatalf("a conflict answered %+v; want reason %s, not retryable, current revision %s", failure, reason, current)
	}
}

func memoryUnknownMission(t *testing.T, s *Session) {
	mission := "pt-" + Unique(t)
	var page collab.MemoryListResult
	s.OK(collab.ContractMemory, collab.OperationContext, `{"identity":{"mission":`+quote(mission)+`}}`, &page)
	if page.Items == nil {
		t.Fatal("a context page without records carries items: []")
	}
	for _, record := range page.Items {
		if record.Kind == collab.MemoryKindMission && record.Identity.Mission == mission {
			t.Fatalf("a mission nobody wrote has record %+v", record)
		}
	}
	s.Fails(collab.ContractMemory, collab.OperationMission, `{"mission":`+quote(mission)+`}`, collab.OutcomeNotFound)
}

func memoryCreateAndReplay(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	evidence := []collab.Evidence{{Kind: collab.EvidenceKindGate, Locator: ".putnami/sessions/" + run + "/session.json",
		Digest: "sha256:" + strings.Repeat("ab", 32)}}
	sources := []collab.Ref{{Source: "providertest:scenarios", ID: run}}
	extra := map[string]any{"title": "Scenario mission " + run, "evidence": evidence, "sources": sources}
	request := checkpointArguments(mission, "providertest-create-"+run, "Plan accepted.", "", extra)
	created := s.checkpoint(mission, "Plan accepted.", request)
	record := created.Record
	if created.Replayed || record.Title != "Scenario mission "+run || !slices.Equal(record.Evidence, evidence) || !slices.Equal(record.Sources, sources) {
		t.Fatalf("created %+v", created)
	}

	// The answer was lost: the identical request replays the write.
	replay := s.checkpoint(mission, "Plan accepted.", request)
	if !replay.Replayed || replay.Record.Ref != record.Ref || replay.Record.Revision != record.Revision {
		t.Fatalf("a repeated checkpoint answered %+v after %+v", replay, created)
	}
	failure := s.Fails(collab.ContractMemory, collab.OperationCheckpoint,
		checkpointArguments(mission, "providertest-create-"+run, "Other content.", record.Revision, nil), collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Retryable {
		t.Fatalf("a reused key with other content answered %+v", failure)
	}
	failure = s.Fails(collab.ContractMemory, collab.OperationCheckpoint,
		checkpointArguments(mission, "providertest-again-"+run, "A second creation.", "", nil), collab.OutcomeConflict)
	conflict(t, failure, collab.ReasonAlreadyExists, record.Revision)

	read := s.mission(mission)
	if read.Ref != record.Ref || read.Revision != record.Revision || read.Content != "Plan accepted." ||
		!slices.Equal(read.Evidence, evidence) || !slices.Equal(read.Sources, sources) {
		t.Fatalf("the mission reads %+v; the refused writes changed it, or the created %+v was not kept", read, record)
	}
}

func memoryStaleRevision(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	first := s.checkpoint(mission, "one", checkpointArguments(mission, "providertest-1-"+run, "one", "", nil)).Record
	second := s.checkpoint(mission, "two", checkpointArguments(mission, "providertest-2-"+run, "two", first.Revision, nil))
	if second.Replayed || second.Record.Ref != first.Ref || second.Record.Revision == first.Revision {
		t.Fatalf("a checkpoint at the current revision answered %+v after %+v", second, first)
	}
	failure := s.Fails(collab.ContractMemory, collab.OperationCheckpoint,
		checkpointArguments(mission, "providertest-3-"+run, "three", first.Revision, nil), collab.OutcomeConflict)
	conflict(t, failure, collab.ReasonRevisionConflict, second.Record.Revision)
	if failure.Current == "" {
		t.Fatalf("a stale checkpoint names no current revision: %+v", failure)
	}
	if read := s.mission(mission); read.Revision != second.Record.Revision || read.Content != "two" {
		t.Fatalf("the mission reads %+v; the latest checkpoint wrote %+v", read, second.Record)
	}
}

func memoryResume(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	written := s.checkpoint(mission, "Task 1 next.", checkpointArguments(mission, "providertest-plan-"+run, "Task 1 next.", "", nil)).Record

	other := s.reopen()
	resumed := other.mission(mission)
	if resumed.Ref != written.Ref || resumed.Revision != written.Revision || resumed.Content != "Task 1 next." {
		t.Fatalf("another instance reads %+v; the first wrote %+v", resumed, written)
	}
	var page collab.MemoryListResult
	other.OK(collab.ContractMemory, collab.OperationContext, `{"identity":{"mission":`+quote(mission)+`}}`, &page)
	listed := 0
	for _, record := range page.Items {
		if record.Kind == collab.MemoryKindMission && record.Identity.Mission == mission {
			listed++
			if record.Revision != written.Revision {
				t.Fatalf("context lists the mission at %s, it is at %s", record.Revision, written.Revision)
			}
		}
	}
	if listed != 1 {
		t.Fatalf("context lists the mission %d times", listed)
	}

	request := checkpointArguments(mission, "providertest-task-1-"+run, "Task 1 merged; task 2 next.", resumed.Revision, nil)
	next := other.checkpoint(mission, "Task 1 merged; task 2 next.", request)
	if next.Replayed || next.Record.Revision == written.Revision {
		t.Fatalf("the resumed checkpoint answered %+v", next)
	}
	if read := s.mission(mission); read.Revision != next.Record.Revision {
		t.Fatalf("the first instance reads %+v after the second wrote %+v", read, next.Record)
	}
	// The second instance's answer was lost: the first repeats its request
	// and the write is replayed, not made again.
	if replay := s.checkpoint(mission, "Task 1 merged; task 2 next.", request); !replay.Replayed || replay.Record.Revision != next.Record.Revision {
		t.Fatalf("a repeat from another instance answered %+v after %+v", replay, next)
	}
}

// race sends one checkpoint per writer at once and sorts the answers into
// the records that landed and the conflicts.
func (s *Session) race(requests []string) ([]collab.MemoryRecord, []collab.Error) {
	s.t.Helper()
	responses := make([]collab.Response, len(requests))
	errs := make([]error, len(requests))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, request := range requests {
		wg.Go(func() {
			<-start
			responses[i], errs[i] = s.do(collab.ContractMemory, collab.OperationCheckpoint, request)
		})
	}
	close(start)
	wg.Wait()
	var landed []collab.MemoryRecord
	var conflicts []collab.Error
	for i, response := range responses {
		if errs[i] != nil {
			s.t.Fatal(errs[i])
		}
		switch response.Outcome {
		case collab.OutcomeOK:
			var result collab.MemoryCheckpointResult
			if err := json.Unmarshal(response.Result, &result); err != nil {
				s.t.Fatal(err)
			}
			if result.Replayed {
				s.t.Fatalf("writer %d with its own key answered replayed: %+v", i, result)
			}
			s.track(collab.ContractMemory, result.Record.Ref)
			landed = append(landed, result.Record)
		case collab.OutcomeConflict:
			conflicts = append(conflicts, *response.Error)
		default:
			s.t.Fatalf("writer %d answered %s %+v", i, response.Outcome, response.Error)
		}
	}
	return landed, conflicts
}

func memoryConcurrentCheckpoints(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	base := s.checkpoint(mission, "base", checkpointArguments(mission, "providertest-base-"+run, "base", "", nil)).Record
	// A checkpoint of another mission lands meanwhile without touching the
	// record the writers compare.
	s.checkpoint("pt-bystander-"+run, "unrelated", checkpointArguments("pt-bystander-"+run, "providertest-bystander-"+run, "unrelated", "", nil))
	requests := make([]string, memoryWriters)
	for i := range requests {
		requests[i] = checkpointArguments(mission, fmt.Sprintf("providertest-writer-%d-%s", i, run), fmt.Sprintf("writer %d", i), base.Revision, nil)
	}
	landed, conflicts := s.race(requests)
	if len(landed) != 1 || len(conflicts) != memoryWriters-1 {
		t.Fatalf("%d writers landed and %d conflicted; exactly one lands", len(landed), len(conflicts))
	}
	for _, failure := range conflicts {
		conflict(t, failure, collab.ReasonRevisionConflict, landed[0].Revision)
	}
	if read := s.mission(mission); read.Revision != landed[0].Revision || read.Content != landed[0].Content {
		t.Fatalf("the mission reads %+v; the writer that landed wrote %+v", read, landed[0])
	}
}

func memoryConcurrentCreations(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	requests := make([]string, memoryWriters)
	for i := range requests {
		requests[i] = checkpointArguments(mission, fmt.Sprintf("providertest-creator-%d-%s", i, run), fmt.Sprintf("creator %d", i), "", nil)
	}
	landed, conflicts := s.race(requests)
	if len(landed) != 1 || len(conflicts) != memoryWriters-1 {
		t.Fatalf("%d creations landed and %d conflicted; exactly one lands", len(landed), len(conflicts))
	}
	for _, failure := range conflicts {
		conflict(t, failure, collab.ReasonAlreadyExists, landed[0].Revision)
	}
	if read := s.mission(mission); read.Revision != landed[0].Revision || read.Content != landed[0].Content {
		t.Fatalf("the mission reads %+v; the creation that landed wrote %+v", read, landed[0])
	}
}

func memoryPages(t *testing.T, s *Session) {
	run := Unique(t)
	missions := make([]string, 0, 5)
	for i := range 5 {
		mission := fmt.Sprintf("pt-%s-%d", run, i)
		missions = append(missions, mission)
		s.checkpoint(mission, "paged", checkpointArguments(mission, "providertest-page-"+mission, "paged", "", nil))
	}
	// traverse walks every page of the workspace's context, asking for the
	// sizes in turn, and keeps the records this scenario wrote.
	traverse := func(sizes ...int) []collab.Ref {
		var refs []collab.Ref
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 1000 {
				t.Fatalf("a traversal in pages of %v did not end after %d records", sizes, len(refs))
			}
			size := sizes[pages%len(sizes)]
			arguments := fmt.Sprintf(`{"page":{"size":%d}}`, size)
			if cursor != "" {
				arguments = fmt.Sprintf(`{"page":{"size":%d,"cursor":%s}}`, size, quote(cursor))
			}
			var page collab.MemoryListResult
			s.OK(collab.ContractMemory, collab.OperationContext, arguments, &page)
			if len(page.Items) > size {
				t.Fatalf("a page of size %d carried %d records", size, len(page.Items))
			}
			for _, record := range page.Items {
				if slices.Contains(missions, record.Identity.Mission) {
					refs = append(refs, record.Ref)
				}
			}
			if cursor = page.Page.Next; cursor == "" {
				return refs
			}
		}
	}
	first := traverse(2)
	if len(first) != len(missions) {
		t.Fatalf("the pages carried %d of the %d records written", len(first), len(missions))
	}
	seen := map[collab.Ref]bool{}
	for _, ref := range first {
		if seen[ref] {
			t.Fatalf("record %+v appears on two pages", ref)
		}
		seen[ref] = true
	}
	if second := traverse(2); !slices.Equal(first, second) {
		t.Fatalf("two traversals disagree on the order: %v then %v", first, second)
	}
	if mixed := traverse(1, 3); !slices.Equal(first, mixed) {
		t.Fatalf("a traversal whose page size changes disagrees: %v then %v", first, mixed)
	}
}

// memoryMaximalCheckpoint writes the largest checkpoint the contract accepts
// and reads it back: a store whose own bound is below what the contract
// accepts refuses the write, or writes a record it cannot read.
func memoryMaximalCheckpoint(t *testing.T, s *Session) {
	run := Unique(t)
	mission := MaximalToken("pt-" + run + "-")
	identity := collab.MemoryIdentity{Workspace: s.target.Workspace, Repository: MaximalToken("pt-repository-"), Scope: MaximalToken("pt-scope-")}
	request := MaximalCheckpoint(mission, "providertest-maximal-"+run, identity)
	arguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := request["content"].(string)
	title, _ := request["title"].(string)
	written := s.checkpoint(mission, content, string(arguments)).Record

	var result collab.MemoryRecordResult
	s.OK(collab.ContractMemory, collab.OperationMission,
		`{"mission":`+quote(mission)+`,"identity":{"repository":`+quote(identity.Repository)+`,"scope":`+quote(identity.Scope)+`}}`, &result)
	read := result.Record
	s.sameMission(read, mission)
	if read.Ref != written.Ref || read.Revision != written.Revision || read.Content != content || read.Title != title ||
		read.Identity.Repository != identity.Repository || read.Identity.Scope != identity.Scope ||
		len(read.Sources) != collab.MaxListMembers || len(read.Evidence) != collab.MaxListMembers ||
		!slices.Equal(read.Sources, written.Sources) || !slices.Equal(read.Evidence, written.Evidence) {
		t.Fatalf("the maximal mission read back at %s (%d sources, %d evidence) differs from the one written at %s",
			read.Revision, len(read.Sources), len(read.Evidence), written.Revision)
	}
}

func memoryOtherWorkspace(t *testing.T, s *Session) {
	run := Unique(t)
	mission := "pt-" + run
	s.checkpoint(mission, "ours", checkpointArguments(mission, "providertest-ours-"+run, "ours", "", nil))
	elsewhere := quote("pt-elsewhere-" + run)
	s.Fails(collab.ContractMemory, collab.OperationMission,
		`{"mission":`+quote(mission)+`,"identity":{"workspace":`+elsewhere+`}}`, collab.OutcomeNotFound)
	var page collab.MemoryListResult
	s.OK(collab.ContractMemory, collab.OperationContext, `{"identity":{"workspace":`+elsewhere+`,"mission":`+quote(mission)+`}}`, &page)
	for _, record := range page.Items {
		if record.Identity.Mission == mission {
			t.Fatalf("another workspace's context lists %+v", record)
		}
	}
}
