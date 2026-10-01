package providertest

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// memory is the smallest provider that honors the tasks and proposals
// contracts: the scenarios must pass against it, so a scenario that fails a
// correct provider fails here first.
type memory struct {
	mu        sync.Mutex
	tasks     []collab.Task
	proposals []collab.Proposal
	reviews   map[string][]collab.Review
	keys      map[string]keyed
}

type keyed struct {
	id     string
	digest string
}

const memorySource = "memory:scenarios"

func newMemory() *memory {
	return &memory{reviews: map[string][]collab.Review{}, keys: map[string]keyed{}}
}

func (m *memory) handlers() map[collab.OperationKey]collab.Handler {
	key := func(contract, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key("tasks", "find"):       m.findTasks,
		key("tasks", "get"):        m.getTask,
		key("tasks", "create"):     m.createTask,
		key("tasks", "update"):     m.updateTask,
		key("tasks", "transition"): m.transitionTask,
		key("proposals", "find"):   m.findProposals,
		key("proposals", "upsert"): m.upsertProposal,
		key("proposals", "status"): m.status,
		key("proposals", "review"): m.review,
	}
}

func bump(revision string) string {
	n, _ := strconv.Atoi(strings.TrimPrefix(revision, "r"))
	return "r" + strconv.Itoa(n+1)
}

func notFound(ref collab.Ref) *collab.Failure {
	return collab.Fail(collab.OutcomeNotFound, "item.missing", "no item %s in %s", ref.ID, ref.Source)
}

func stale(current, expected string) *collab.Failure {
	failure := collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, "at %s, not %s", current, expected)
	failure.Error.Current = current
	return failure
}

func pageOf[T any](items []T, request *collab.PageRequest) ([]T, collab.Page) {
	start := 0
	if request.Cursor != "" {
		start, _ = strconv.Atoi(request.Cursor)
	}
	end := min(start+request.Size, len(items))
	var next collab.Page
	if end < len(items) {
		next.Next = strconv.Itoa(end)
	}
	return items[start:end], next
}

func (m *memory) findTasks(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.TaskFindInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	var matches []collab.Task
	for _, task := range m.tasks {
		if in.Query != "" && !strings.Contains(strings.ToLower(task.Title+" "+task.Body), strings.ToLower(in.Query)) {
			continue
		}
		if len(in.States) > 0 && !slices.Contains(in.States, task.State) {
			continue
		}
		if slices.ContainsFunc(in.Labels, func(label string) bool { return !slices.Contains(task.Labels, label) }) {
			continue
		}
		matches = append(matches, task)
	}
	items, next := pageOf(matches, in.Page)
	return &collab.TaskListResult{Items: items, Page: next}, nil
}

func (m *memory) task(ref collab.Ref) int {
	if ref.Source != memorySource {
		return -1
	}
	return slices.IndexFunc(m.tasks, func(task collab.Task) bool { return task.Ref.ID == ref.ID })
}

func (m *memory) getTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.TaskRefInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.task(in.Ref)
	if index < 0 {
		return nil, notFound(in.Ref)
	}
	return &collab.TaskResult{Task: m.tasks[index]}, nil
}

func (m *memory) createTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.TaskCreateInput)
	digest := in.Title + "\x00" + in.Body + "\x00" + strings.Join(in.Labels, ",") + "\x00" + string(in.State)
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, used := m.keys["task:"+in.IdempotencyKey]; used {
		if prior.digest != digest {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, "key reused")
		}
		return &collab.TaskCreateResult{Task: m.tasks[m.task(collab.Ref{Source: memorySource, ID: prior.id})]}, nil
	}
	state := in.State
	if state == "" {
		state = collab.TaskStateOpen
	}
	task := collab.Task{Ref: collab.Ref{Source: memorySource, ID: strconv.Itoa(len(m.tasks) + 1)}, Revision: "r1",
		Title: in.Title, Body: in.Body, Labels: in.Labels, State: state}
	m.tasks = append(m.tasks, task)
	m.keys["task:"+in.IdempotencyKey] = keyed{id: task.Ref.ID, digest: digest}
	return &collab.TaskCreateResult{Task: task, Created: true}, nil
}

func (m *memory) change(ref collab.Ref, expected string, apply func(*collab.Task)) (any, *collab.Failure) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.task(ref)
	if index < 0 {
		return nil, notFound(ref)
	}
	task := &m.tasks[index]
	if expected != "" && expected != task.Revision {
		return nil, stale(task.Revision, expected)
	}
	apply(task)
	task.Revision = bump(task.Revision)
	return &collab.TaskResult{Task: *task}, nil
}

func (m *memory) updateTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.TaskUpdateInput)
	return m.change(in.Ref, in.ExpectedRevision, func(task *collab.Task) {
		if in.Title != nil {
			task.Title = *in.Title
		}
		if in.Body != nil {
			task.Body = *in.Body
		}
		if in.Labels != nil {
			task.Labels = *in.Labels
		}
	})
}

func (m *memory) transitionTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.TaskTransitionInput)
	return m.change(in.Ref, in.ExpectedRevision, func(task *collab.Task) { task.State = in.State })
}

func sameHead(proposal collab.Proposal, change collab.Change) bool {
	return proposal.Change.Base == change.Base && proposal.Change.Head == change.Head
}

func (m *memory) findProposals(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.ProposalFindInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	var matches []collab.Proposal
	for _, proposal := range m.proposals {
		if sameHead(proposal, in.Change) && (len(in.States) == 0 || slices.Contains(in.States, proposal.State)) {
			matches = append(matches, proposal)
		}
	}
	items, next := pageOf(matches, in.Page)
	return &collab.ProposalListResult{Items: items, Page: next}, nil
}

func (m *memory) upsertProposal(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.ProposalUpsertInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	index := slices.IndexFunc(m.proposals, func(proposal collab.Proposal) bool { return sameHead(proposal, in.Change) })
	if index < 0 {
		proposal := collab.Proposal{Ref: collab.Ref{Source: memorySource, ID: "p" + strconv.Itoa(len(m.proposals)+1)}, Revision: "r1",
			Change: collab.Change{Repository: "memory", Base: in.Change.Base, Head: in.Change.Head},
			Title:  in.Title, Body: in.Body, State: collab.ProposalStateOpen}
		m.proposals = append(m.proposals, proposal)
		return &collab.ProposalUpsertResult{Proposal: proposal, Created: true}, nil
	}
	proposal := &m.proposals[index]
	if in.ExpectedRevision != "" && in.ExpectedRevision != proposal.Revision {
		return nil, stale(proposal.Revision, in.ExpectedRevision)
	}
	proposal.Title, proposal.Body, proposal.Revision = in.Title, in.Body, bump(proposal.Revision)
	return &collab.ProposalUpsertResult{Proposal: *proposal}, nil
}

func (m *memory) proposal(ref collab.Ref) int {
	if ref.Source != memorySource {
		return -1
	}
	return slices.IndexFunc(m.proposals, func(proposal collab.Proposal) bool { return proposal.Ref.ID == ref.ID })
}

func (m *memory) status(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.ProposalRefInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.proposal(in.Ref)
	if index < 0 {
		return nil, notFound(in.Ref)
	}
	return &collab.ProposalStatusResult{Proposal: m.proposals[index], Reviews: m.reviews[in.Ref.ID],
		Checks: collab.Checks{State: collab.ChecksStateUnsupported, Detail: "the memory provider runs no checks"}}, nil
}

func (m *memory) review(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.ProposalReviewInput)
	digest := string(in.Verdict) + "\x00" + in.Body
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proposal(in.Ref) < 0 {
		return nil, notFound(in.Ref)
	}
	if prior, used := m.keys["review:"+in.IdempotencyKey]; used {
		if prior.digest != digest {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, "key reused")
		}
		for _, review := range m.reviews[in.Ref.ID] {
			if review.Ref.ID == prior.id {
				return &collab.ProposalReviewResult{Review: review}, nil
			}
		}
	}
	review := collab.Review{Ref: collab.Ref{Source: memorySource, ID: "v" + strconv.Itoa(len(m.keys)+1)}, Verdict: in.Verdict}
	m.reviews[in.Ref.ID] = append(m.reviews[in.Ref.ID], review)
	m.keys["review:"+in.IdempotencyKey] = keyed{id: review.Ref.ID, digest: digest}
	return &collab.ProposalReviewResult{Review: review, Created: true}, nil
}

// memoryTarget is a fresh in-memory provider. A scenario that creates items
// must report every one of them to Track.
func memoryTarget(t *testing.T, creates bool) Target {
	var tracked []string
	if creates {
		t.Cleanup(func() {
			if len(tracked) == 0 {
				t.Error("the scenario tracked nothing it created")
			}
		})
	}
	return Target{
		Handlers:      newMemory().handlers(),
		WorkspaceRoot: t.TempDir(),
		Base:          "main",
		Heads:         [2]string{"topic-a", "topic-b"},
		Track:         func(contract string, ref collab.Ref) { tracked = append(tracked, contract+":"+ref.ID) },
	}
}

func TestTheScenariosPassAgainstACorrectProvider(t *testing.T) {
	createsNothing := func(name string) bool {
		return strings.Contains(name, "empty_page") || strings.Contains(name, "finds_nothing") || strings.Contains(name, "another_source")
	}
	RunTasks(t, func(t *testing.T) Target { return memoryTarget(t, !createsNothing(t.Name())) })
	RunProposals(t, func(t *testing.T) Target { return memoryTarget(t, !createsNothing(t.Name())) })
}

func TestNormalizeGivesListsAnExplicitPage(t *testing.T) {
	if got := string(normalize("tasks", "find", `{}`, "")); got != `{"page":{"size":20}}` {
		t.Errorf("tasks.find normalized to %s", got)
	}
	if got := string(normalize("proposals", "find", `{"change":{"base":"b","head":"h"},"page":{"size":3}}`, "")); !strings.Contains(got, `"size":3`) {
		t.Errorf("an explicit size changed: %s", got)
	}
	if got := string(normalize("tasks", "get", `{"ref":"not an object"}`, "")); got != `{"ref":"not an object"}` {
		t.Errorf("an invalid request was rewritten: %s", got)
	}
}

func TestUniqueTokensDiffer(t *testing.T) {
	first, second := Unique(t), Unique(t)
	if first == second || len(first) != 8 {
		t.Errorf("tokens %q and %q", first, second)
	}
}
