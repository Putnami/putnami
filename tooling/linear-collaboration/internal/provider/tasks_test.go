package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestEachWorkflowStateTypeReadsAsItsSemanticState(t *testing.T) {
	spectest.Proves(t, feature, "workflow-state-mapping", "each-workflow-state-type-reads-as-its-semantic-state")
	h := newHarness(t)
	cases := []struct {
		state, name string
		want        collab.TaskState
	}{
		{"st-1111-triage", "Triage", collab.TaskStateOpen},
		{"st-1111-backlog", "Backlog", collab.TaskStateOpen},
		{"st-1111-todo", "Todo", collab.TaskStateOpen},
		{"st-1111-doing", "In Progress", collab.TaskStateInProgress},
		{"st-1111-done", "Done", collab.TaskStateDone},
		{"st-1111-canceled", "Canceled", collab.TaskStateCanceled},
		{"st-1111-dup", "Duplicate", collab.TaskStateCanceled},
	}
	for i, c := range cases {
		id := "aaaaaaaa-0000-4000-8000-00000000000" + strconv.Itoa(i)
		h.seedIssue(id, "Seeded "+c.name, c.state)
		var got collab.TaskResult
		h.ok("get", `{"ref":{"source":"linear:acme","id":"`+id+`"}}`, &got)
		if got.Task.State != c.want || got.Task.ProviderState != c.name || !strings.HasPrefix(got.Task.Key, "ENG-") {
			t.Errorf("a %s issue reads as %s (%s, key %s), want %s", c.name, got.Task.State, got.Task.ProviderState, got.Task.Key, c.want)
		}
	}
}

func TestATransitionTakesTheFirstStateOfTheTargetType(t *testing.T) {
	spectest.Proves(t, feature, "workflow-state-mapping", "a-transition-takes-the-first-state-of-the-target-type")
	h := newHarness(t)
	task := h.createTask(`{"title":"Move me","idempotencyKey":"move"}`)
	steps := []struct {
		state collab.TaskState
		name  string
	}{
		{collab.TaskStateInProgress, "In Progress"},
		{collab.TaskStateDone, "Done"},
		{collab.TaskStateOpen, "Todo"},
		{collab.TaskStateCanceled, "Canceled"},
	}
	for _, step := range steps {
		var moved collab.TaskResult
		h.ok("transition", `{"ref":`+refJSON(task.Ref)+`,"state":"`+string(step.state)+`"}`, &moved)
		if moved.Task.State != step.state || moved.Task.ProviderState != step.name {
			t.Fatalf("a transition to %s reached %s (%s), want the first state of its type, %s", step.state, moved.Task.State, moved.Task.ProviderState, step.name)
		}
	}
}

func TestBlockedIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "workflow-state-mapping", "blocked-is-unsupported")
	h := newHarness(t)
	task := h.createTask(`{"title":"Stuck","idempotencyKey":"stuck"}`)
	failure := h.fails("transition", `{"ref":`+refJSON(task.Ref)+`,"state":"blocked"}`, collab.OutcomeUnsupported)
	if failure.Reason != reasonStateUnmapped {
		t.Fatalf("a transition to blocked: %+v", failure)
	}
	h.fails("create", `{"title":"Born stuck","state":"blocked","idempotencyKey":"born-stuck"}`, collab.OutcomeUnsupported)
	if h.fake.count("PutnamiUpdateIssue") != 0 || h.issueCount() != 1 {
		t.Fatal("an unsupported state wrote to Linear")
	}
}

func TestATaskAlreadyInTheStateIsLeftAsItIs(t *testing.T) {
	spectest.Proves(t, feature, "workflow-state-mapping", "a-task-already-in-the-state-is-left-as-it-is")
	h := newHarness(t)
	h.seedIssue("bbbbbbbb-0000-4000-8000-000000000001", "In triage", "st-1111-triage")
	var got collab.TaskResult
	h.ok("transition", `{"ref":{"source":"linear:acme","id":"bbbbbbbb-0000-4000-8000-000000000001"},"state":"open"}`, &got)
	if got.Task.ProviderState != "Triage" || h.fake.count("PutnamiUpdateIssue") != 0 {
		t.Fatalf("an open issue moved on a transition to open: %+v", got.Task)
	}
}

func TestACreateStartsInTheRequestedState(t *testing.T) {
	spectest.Proves(t, feature, "workflow-state-mapping", "a-transition-takes-the-first-state-of-the-target-type")
	h := newHarness(t)
	task := h.createTask(`{"title":"Already going","state":"in_progress","idempotencyKey":"going"}`)
	if task.State != collab.TaskStateInProgress || task.ProviderState != "In Progress" {
		t.Fatalf("created %+v", task)
	}
	open := h.createTask(`{"title":"Default","idempotencyKey":"default"}`)
	if open.State != collab.TaskStateOpen || open.ProviderState != "Backlog" {
		t.Fatalf("an open create left the team's default state: %+v", open)
	}
}

func TestARepeatedKeyReturnsTheFirstIssue(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "a-repeated-key-returns-the-first-issue")
	h := newHarness(t)
	arguments := `{"title":"Once","body":"Only once.","labels":["Bug"],"idempotencyKey":"once"}`
	first := h.createTask(arguments)
	if first.Ref.ID != keyedID(teamID, "once") || first.Body != "Only once." {
		t.Fatalf("created %+v", first)
	}
	var again collab.TaskCreateResult
	h.ok("create", arguments, &again)
	if again.Created || again.Task.Ref != first.Ref || again.Task.Revision != first.Revision {
		t.Fatalf("a repeat answered %+v after %+v", again, first)
	}
	if h.fake.count("PutnamiCreateIssue") != 1 || h.issueCount() != 1 {
		t.Fatalf("a repeat sent %d creates for %d issues", h.fake.count("PutnamiCreateIssue"), h.issueCount())
	}
}

func TestAKeyWithOtherContentIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "a-key-with-other-content-is-a-conflict")
	h := newHarness(t)
	h.createTask(`{"title":"First","idempotencyKey":"shared"}`)
	failure := h.fails("create", `{"title":"Second","idempotencyKey":"shared"}`, collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Retryable {
		t.Fatalf("a reused key: %+v", failure)
	}
	other := h.createTask(`{"title":"First","idempotencyKey":"another"}`)
	if other.Ref.ID == keyedID(teamID, "shared") {
		t.Fatal("two keys named one issue")
	}
}

func TestADeletedIssueHoldsItsKey(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "a-key-with-other-content-is-a-conflict")
	h := newHarness(t)
	task := h.createTask(`{"title":"Gone","idempotencyKey":"gone"}`)
	h.fake.mu.Lock()
	h.fake.issue(task.Ref.ID).trashed = true
	h.fake.mu.Unlock()
	h.fails("get", `{"ref":`+refJSON(task.Ref)+`}`, collab.OutcomeNotFound)
	failure := h.fails("create", `{"title":"Gone","idempotencyKey":"gone"}`, collab.OutcomeConflict)
	mustContain(t, failure.Message, "deleted")
}

func TestUpdateKeepsTheRecordedDigest(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "update-keeps-the-recorded-digest")
	h := newHarness(t)
	// Linear's markdown normalization escapes a line that opens with -->.
	h.fake.normalize = func(text string) string { return strings.ReplaceAll(text, "` -->", "` \\-->") }
	arguments := `{"title":"Keep","body":"Original.","idempotencyKey":"keep"}`
	task := h.createTask(arguments)
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+refJSON(task.Ref)+`,"body":"Rewritten."}`, &updated)
	if updated.Task.Body != "Rewritten." {
		t.Fatalf("the body shows %q", updated.Task.Body)
	}
	if _, recorded := splitMarker(h.stored(task.Ref).description); recorded == "" {
		t.Fatalf("the update dropped the digest: %q", h.stored(task.Ref).description)
	}
	var again collab.TaskCreateResult
	h.ok("create", arguments, &again)
	if again.Created || again.Task.Ref != task.Ref {
		t.Fatalf("a repeat after an update answered %+v", again)
	}
	h.fails("create", `{"title":"Other","idempotencyKey":"keep"}`, collab.OutcomeConflict)
}

func TestAnIssueWithoutItsMarkerStillAnswersForItsKey(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "a-repeated-key-returns-the-first-issue")
	h := newHarness(t)
	task := h.createTask(`{"title":"Edited","idempotencyKey":"edited"}`)
	h.fake.mu.Lock()
	h.fake.issue(task.Ref.ID).description = "Someone rewrote it in Linear."
	h.fake.mu.Unlock()
	var again collab.TaskCreateResult
	h.ok("create", `{"title":"Edited","idempotencyKey":"edited"}`, &again)
	if again.Created || again.Task.Ref != task.Ref {
		t.Fatalf("answered %+v", again)
	}
}

func TestTheRevisionCoversWhatATaskShows(t *testing.T) {
	spectest.Proves(t, feature, "checked-revision", "the-revision-covers-what-a-task-shows")
	h := newHarness(t)
	task := h.createTask(`{"title":"Revised","body":"Body.","labels":["zeta","Bug"],"idempotencyKey":"revised"}`)
	stored := h.stored(task.Ref)
	want := "v" + digest([]any{"Revised", stored.description, "Backlog", []string{"Bug", "zeta"}, []string{}})[:32]
	if task.Revision != want {
		t.Fatalf("revision %s, want %s", task.Revision, want)
	}
	h.fake.mu.Lock()
	h.fake.issue(task.Ref.ID).assignee = "33333333-3333-4333-8333-333333333333"
	h.fake.mu.Unlock()
	var got collab.TaskResult
	h.ok("get", `{"ref":`+refJSON(task.Ref)+`}`, &got)
	assigned := "v" + digest([]any{"Revised", stored.description, "Backlog", []string{"Bug", "zeta"}, []string{"33333333-3333-4333-8333-333333333333"}})[:32]
	if got.Task.Revision != assigned || !slices.Equal(got.Task.Assignees, []string{"user-3333"}) {
		t.Fatalf("after an assignment: revision %s assignees %v, want %s", got.Task.Revision, got.Task.Assignees, assigned)
	}
}

func TestAStaleRevisionIsAConflictBeforeAnyWrite(t *testing.T) {
	spectest.Proves(t, feature, "checked-revision", "a-stale-revision-is-a-conflict-before-any-write")
	h := newHarness(t)
	task := h.createTask(`{"title":"Guarded","idempotencyKey":"guarded"}`)
	var renamed collab.TaskResult
	h.ok("update", `{"ref":`+refJSON(task.Ref)+`,"expectedRevision":"`+task.Revision+`","title":"Renamed"}`, &renamed)
	writes := h.fake.count("PutnamiUpdateIssue")
	failure := h.fails("update", `{"ref":`+refJSON(task.Ref)+`,"expectedRevision":"`+task.Revision+`","title":"Stale"}`, collab.OutcomeConflict)
	if failure.Current != renamed.Task.Revision || failure.Reason != collab.ReasonRevisionConflict {
		t.Fatalf("a stale update: %+v", failure)
	}
	h.fails("transition", `{"ref":`+refJSON(task.Ref)+`,"state":"done","expectedRevision":"`+task.Revision+`"}`, collab.OutcomeConflict)
	if h.fake.count("PutnamiUpdateIssue") != writes {
		t.Fatal("a stale precondition wrote to Linear")
	}
}

func TestAMissingLabelIsCreatedInTheTeam(t *testing.T) {
	spectest.Proves(t, feature, "named-labels", "a-missing-label-is-created-in-the-team")
	h := newHarness(t)
	task := h.createTask(`{"title":"Labeled","labels":["bug","fresh"],"idempotencyKey":"labeled"}`)
	if !slices.Equal(task.Labels, []string{"Bug", "fresh"}) {
		t.Fatalf("labels %v", task.Labels)
	}
	h.fake.mu.Lock()
	var created []fakeLabel
	for _, label := range h.fake.labels {
		if label.name == "fresh" {
			created = append(created, *label)
		}
	}
	h.fake.mu.Unlock()
	if len(created) != 1 || created[0].team != teamID {
		t.Fatalf("the missing label was created as %+v", created)
	}
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+refJSON(task.Ref)+`,"labels":["FRESH"]}`, &updated)
	if !slices.Equal(updated.Task.Labels, []string{"fresh"}) || h.fake.count("PutnamiCreateLabel") != 1 {
		t.Fatalf("labels replace the set and reuse a label by name: %v after %d label creations", updated.Task.Labels, h.fake.count("PutnamiCreateLabel"))
	}
}

func TestALabelGroupIsNeverApplied(t *testing.T) {
	spectest.Proves(t, feature, "named-labels", "a-label-group-is-never-applied")
	h := newHarness(t)
	task := h.createTask(`{"title":"Grouped","labels":["Area"],"idempotencyKey":"grouped"}`)
	labels := h.stored(task.Ref).labels
	if len(labels) != 1 || labels[0] == "lb-group-area" {
		t.Fatalf("the issue carries %v, the label group included", labels)
	}
}

func TestFindRequiresEveryLabelTheStatesAndTheQuery(t *testing.T) {
	spectest.Proves(t, feature, "named-labels", "find-requires-every-label-the-states-and-the-query")
	h := newHarness(t)
	both := h.createTask(`{"title":"Both needle","labels":["a","b"],"idempotencyKey":"both"}`)
	h.createTask(`{"title":"Only a","labels":["a"],"idempotencyKey":"only-a"}`)
	done := h.createTask(`{"title":"Done one","body":"has the NEEDLE inside","labels":["a","b"],"state":"done","idempotencyKey":"done"}`)
	h.seedIssue("cccccccc-0000-4000-8000-000000000001", "Other team", "st-2222-todo")
	find := func(arguments string) []collab.Ref {
		var page collab.TaskListResult
		h.ok("find", arguments, &page)
		refs := make([]collab.Ref, 0, len(page.Items))
		for _, task := range page.Items {
			refs = append(refs, task.Ref)
		}
		return refs
	}
	if got := find(`{"labels":["A","b"]}`); !slices.Equal(got, []collab.Ref{both.Ref, done.Ref}) {
		t.Errorf("labels a and b matched %v", got)
	}
	if got := find(`{"labels":["a","b"],"states":["open"]}`); !slices.Equal(got, []collab.Ref{both.Ref}) {
		t.Errorf("open with a and b matched %v", got)
	}
	if got := find(`{"query":"needle"}`); !slices.Equal(got, []collab.Ref{both.Ref, done.Ref}) {
		t.Errorf("the query matched %v", got)
	}
	if got := find(`{"states":["blocked"]}`); len(got) != 0 {
		t.Errorf("blocked matched %v", got)
	}
	if got := find(`{}`); len(got) != 3 {
		t.Errorf("an unfiltered find listed %d tasks of the team, want 3", len(got))
	}
}
