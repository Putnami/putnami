package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/github-collaboration"

func TestTransitionsApplyTheConfiguredLabels(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "a-transition-applies-the-configured-label-and-removes-the-others")
	h := newHarness(t)
	seeded := h.seedIssue("Seeded", "", "bug", "status/confirmed", "status/evidence")
	ref := refJSON(collab.Ref{Source: "github:acme/app", ID: strconv.Itoa(seeded.number)})

	var task collab.TaskResult
	h.ok("tasks", "get", `{"ref":`+ref+`}`, &task)
	if task.Task.State != collab.TaskStateOpen || task.Task.ProviderState != "status/confirmed" || !slices.Equal(task.Task.Labels, []string{"bug"}) {
		t.Fatalf("a confirmed issue reads as %+v", task.Task)
	}

	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"in_progress","expectedRevision":"`+task.Task.Revision+`"}`, &task)
	if got := h.issue(seeded.number).labels; !slices.Equal(got, []string{"bug", "status/in-progress"}) {
		t.Fatalf("in_progress left labels %v; every state label goes and the primary one comes", got)
	}
	if task.Task.State != collab.TaskStateInProgress || task.Task.ProviderState != "status/in-progress" {
		t.Fatalf("transition answered %+v", task.Task)
	}

	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"blocked"}`, &task)
	if got := h.issue(seeded.number).labels; !slices.Equal(got, []string{"bug", "status/needs-review"}) {
		t.Fatalf("blocked left labels %v", got)
	}
	patches := h.fake.count("PATCH", `/issues/[0-9]+$`)
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"blocked"}`, &task)
	if h.fake.count("PATCH", `/issues/[0-9]+$`) != patches {
		t.Error("a transition to the state the task is in wrote to GitHub")
	}
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"open"}`, &task)
	if got := h.issue(seeded.number).labels; !slices.Equal(got, []string{"bug", "status/confirmed"}) || task.Task.State != collab.TaskStateOpen {
		t.Fatalf("open left labels %v, state %s", got, task.Task.State)
	}
}

// The reference mapping reads status/needs-design as blocked too. A delivery
// that moves a task to blocked must still leave status/needs-review, whether
// the issue was blocked by status/needs-design alone or picked it up on top
// of its claim.
func TestATransitionToTheStateReadThroughAnotherLabelAppliesTheFirstLabel(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "a-state-read-through-another-label-takes-its-first-label")
	h := newHarness(t)
	for _, start := range [][]string{
		{"bug", "status/needs-design"},
		{"bug", "status/in-progress", "status/needs-design"},
	} {
		seeded := h.seedIssue("Needs design", "", start...)
		ref := refJSON(collab.Ref{Source: "github:acme/app", ID: strconv.Itoa(seeded.number)})
		var task collab.TaskResult
		h.ok("tasks", "get", `{"ref":`+ref+`}`, &task)
		if task.Task.State != collab.TaskStateBlocked || task.Task.ProviderState != "status/needs-design" {
			t.Fatalf("%v reads as %+v", start, task.Task)
		}
		h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"blocked","expectedRevision":"`+task.Task.Revision+`","reason":"Proposal published for review"}`, &task)
		if got := h.issue(seeded.number).labels; !slices.Equal(got, []string{"bug", "status/needs-review"}) {
			t.Fatalf("a transition to blocked from %v left labels %v", start, got)
		}
		if task.Task.State != collab.TaskStateBlocked || task.Task.ProviderState != "status/needs-review" {
			t.Fatalf("the transition from %v answered %+v", start, task.Task)
		}
		patches := h.fake.count("PATCH", `/issues/[0-9]+$`)
		h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"blocked"}`, &task)
		if h.fake.count("PATCH", `/issues/[0-9]+$`) != patches {
			t.Errorf("a repeated transition from %v wrote again", start)
		}
	}

	// The secondary label of open gives way the same way; a closed issue is
	// never rewritten by a transition to the closed state it is in.
	finding := h.seedIssue("Finding", "", "status/audit-finding")
	ref := refJSON(collab.Ref{Source: "github:acme/app", ID: strconv.Itoa(finding.number)})
	var task collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"open"}`, &task)
	if got := h.issue(finding.number).labels; !slices.Equal(got, []string{"status/confirmed"}) || task.Task.ProviderState != "status/confirmed" {
		t.Fatalf("a transition to open from status/audit-finding left labels %v, answered %+v", got, task.Task)
	}
	duplicate := h.seedIssue("Duplicate", "", "status/needs-design")
	h.fake.mu.Lock()
	duplicate.state, duplicate.stateReason = "closed", "duplicate"
	h.fake.mu.Unlock()
	patches := h.fake.count("PATCH", `/issues/[0-9]+$`)
	h.ok("tasks", "transition", `{"ref":{"source":"github:acme/app","id":"`+strconv.Itoa(duplicate.number)+`"},"state":"canceled"}`, &task)
	if h.fake.count("PATCH", `/issues/[0-9]+$`) != patches || task.Task.ProviderState != "closed:duplicate" {
		t.Fatalf("a transition to canceled rewrote an issue closed as a duplicate: %+v", task.Task)
	}
}

// create in open applies no state label, and an issue without one reads as
// open; a transition to open still gives it the first label of open, as it
// gives every issue the first label of the state it moves to. A binding
// whose open state has no label writes nothing.
func TestATransitionToOpenGivesAnIssueWithoutAStateLabelTheFirstLabel(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "a-transition-to-open-gives-an-issue-without-a-state-label-the-first-label")
	h := newHarness(t)
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Untriaged","labels":["bug"],"idempotencyKey":"untriaged"}`, &created)
	if created.Task.State != collab.TaskStateOpen || created.Task.ProviderState != "open" {
		t.Fatalf("a create in open answered %+v", created.Task)
	}
	ref := refJSON(created.Task.Ref)
	patches := h.fake.count("PATCH", `/issues/[0-9]+$`)
	var task collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"open","expectedRevision":"`+created.Task.Revision+`"}`, &task)
	if got := h.issue(number(t, created.Task.Ref)).labels; !slices.Equal(got, []string{"bug", "status/confirmed"}) ||
		task.Task.ProviderState != "status/confirmed" || h.fake.count("PATCH", `/issues/[0-9]+$`) != patches+1 {
		t.Fatalf("a transition to open of an issue without a state label left labels %v, answered %+v", got, task.Task)
	}
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"open"}`, &task)
	if h.fake.count("PATCH", `/issues/[0-9]+$`) != patches+1 {
		t.Error("a repeated transition to open wrote again")
	}

	h.tasks = `{"repository":"acme/app","states":{"in_progress":["status/in-progress"]},"stateLabelPrefix":"status/"}`
	h.ok("tasks", "create", `{"title":"Plain","idempotencyKey":"plain"}`, &created)
	patches = h.fake.count("PATCH", `/issues/[0-9]+$`)
	h.ok("tasks", "transition", `{"ref":`+refJSON(created.Task.Ref)+`,"state":"open"}`, &task)
	if h.fake.count("PATCH", `/issues/[0-9]+$`) != patches || task.Task.State != collab.TaskStateOpen {
		t.Fatalf("a transition to open without an open label wrote, or answered %+v", task.Task)
	}
}

func TestDoneAndCanceledCloseTheIssue(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "done-and-canceled-close-the-issue-with-their-reason")
	h := newHarness(t)
	task := h.createTask("Close me", "close-me")
	ref := refJSON(task.Ref)
	n := number(t, task.Ref)

	var moved collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"done"}`, &moved)
	if issue := h.issue(n); issue.state != "closed" || issue.stateReason != "completed" || moved.Task.ProviderState != "closed:completed" {
		t.Fatalf("done: GitHub state %s/%s, answered %+v", issue.state, issue.stateReason, moved.Task)
	}
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"canceled"}`, &moved)
	if issue := h.issue(n); issue.state != "closed" || issue.stateReason != "not_planned" || moved.Task.State != collab.TaskStateCanceled {
		t.Fatalf("canceled: GitHub state %s/%s", issue.state, issue.stateReason)
	}
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"in_progress"}`, &moved)
	if issue := h.issue(n); issue.state != "open" || issue.stateReason != "reopened" || moved.Task.State != collab.TaskStateInProgress {
		t.Fatalf("reopening: GitHub state %s/%s, labels %v", issue.state, issue.stateReason, issue.labels)
	}

	duplicate := h.seedIssue("Duplicate", "")
	h.fake.mu.Lock()
	duplicate.state, duplicate.stateReason = "closed", "duplicate"
	h.fake.mu.Unlock()
	var read collab.TaskResult
	h.ok("tasks", "get", `{"ref":{"source":"github:acme/app","id":"`+strconv.Itoa(duplicate.number)+`"}}`, &read)
	if read.Task.State != collab.TaskStateCanceled || read.Task.ProviderState != "closed:duplicate" {
		t.Fatalf("an issue closed as a duplicate reads as %+v", read.Task)
	}

	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Already done","state":"done","idempotencyKey":"done-at-birth"}`, &created)
	if issue := h.issue(number(t, created.Task.Ref)); !created.Created || issue.state != "closed" || created.Task.State != collab.TaskStateDone {
		t.Fatalf("a create in state done: %+v, GitHub state %s", created, issue.state)
	}
}

func TestAnUnmappedStateIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "an-unmapped-state-is-unsupported")
	h := newHarness(t)
	h.tasks = `{"repository":"acme/app"}`
	task := h.createTask("Plain", "plain")
	posts := h.fake.count("PATCH", `/issues/`)
	failure := h.fails("tasks", "transition", `{"ref":`+refJSON(task.Ref)+`,"state":"in_progress"}`, collab.OutcomeUnsupported)
	if failure.Reason != reasonStateUnmapped || h.fake.count("PATCH", `/issues/`) != posts {
		t.Fatalf("an unmapped state: %+v", failure)
	}
	h.fails("tasks", "create", `{"title":"t","state":"blocked","idempotencyKey":"k-blocked"}`, collab.OutcomeUnsupported)
	var done collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+refJSON(task.Ref)+`,"state":"done"}`, &done)
	h.ok("tasks", "transition", `{"ref":`+refJSON(task.Ref)+`,"state":"open"}`, &done)
	if done.Task.State != collab.TaskStateOpen {
		t.Fatalf("open needs no label: %+v", done.Task)
	}
}

func TestStateLabelsChangeOnlyThroughTransition(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "state-labels-change-only-through-transition")
	h := newHarness(t)
	failure := h.fails("tasks", "create", `{"title":"t","labels":["status/in-progress"],"idempotencyKey":"k"}`, collab.OutcomeInvalid)
	if failure.Reason != reasonStateLabel {
		t.Fatalf("create with a state label: %+v", failure)
	}
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"t","labels":["bug"],"state":"in_progress","idempotencyKey":"k"}`, &created)
	n := number(t, created.Task.Ref)
	if got := h.issue(n).labels; !slices.Equal(got, []string{"bug", "status/in-progress"}) || created.Task.State != collab.TaskStateInProgress {
		t.Fatalf("a create in progress carries %v", got)
	}
	ref := refJSON(created.Task.Ref)
	h.fails("tasks", "update", `{"ref":`+ref+`,"labels":["STATUS/whatever"]}`, collab.OutcomeInvalid)
	var updated collab.TaskResult
	h.ok("tasks", "update", `{"ref":`+ref+`,"labels":["enhancement"]}`, &updated)
	if got := h.issue(n).labels; !slices.Equal(got, []string{"enhancement", "status/in-progress"}) ||
		!slices.Equal(updated.Task.Labels, []string{"enhancement"}) || updated.Task.State != collab.TaskStateInProgress {
		t.Fatalf("an update replaced the labels with %v and answered %+v; the state label stays", got, updated.Task)
	}

	h.fake.mu.Lock()
	h.fake.issues[n].labels = append(h.fake.issues[n].labels, "status/needs-design")
	h.fake.mu.Unlock()
	h.ok("tasks", "get", `{"ref":`+ref+`}`, &updated)
	if updated.Task.State != collab.TaskStateBlocked {
		t.Fatalf("blocked outranks in_progress when an issue carries both: %+v", updated.Task)
	}
}

func TestAnUpdateKeepsTheIdempotencyRecord(t *testing.T) {
	h := newHarness(t)
	task := h.createTask("Keyed", "keyed")
	n := number(t, task.Ref)
	mustContain(t, h.issue(n).body, "putnami-idempotency")
	if strings.Contains(task.Body, "putnami-idempotency") {
		t.Fatalf("the marker leaked into the task body %q", task.Body)
	}
	var updated collab.TaskResult
	h.ok("tasks", "update", `{"ref":`+refJSON(task.Ref)+`,"body":"A new body","title":"Renamed"}`, &updated)
	if updated.Task.Body != "A new body" || !strings.HasPrefix(h.issue(n).body, "A new body\n\n<!-- putnami-idempotency") {
		t.Fatalf("the body became %q (GitHub %q)", updated.Task.Body, h.issue(n).body)
	}
	var unchanged collab.TaskResult
	patches := h.fake.count("PATCH", `/issues/`)
	h.ok("tasks", "update", `{"ref":`+refJSON(task.Ref)+`,"body":"A new body"}`, &unchanged)
	if unchanged.Task.Revision != updated.Task.Revision || h.fake.count("PATCH", `/issues/`) != patches {
		t.Error("an update that changes nothing wrote or moved the revision")
	}
	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Keyed","idempotencyKey":"keyed"}`, &replay)
	if replay.Created || replay.Task.Ref != task.Ref || replay.Task.Title != "Renamed" {
		t.Fatalf("a replay after an update: %+v", replay)
	}
	if count := h.issueCount(); count != 1 {
		t.Fatalf("%d issues", count)
	}
}

func TestAssignmentIsInformation(t *testing.T) {
	h := newHarness(t)
	task := h.createTask("Assigned", "assigned")
	ref := refJSON(task.Ref)
	var assigned collab.TaskResult
	h.ok("tasks", "assign", `{"ref":`+ref+`,"assignees":["@me","octocat","stranger"],"expectedRevision":"`+task.Revision+`"}`, &assigned)
	if !slices.Equal(assigned.Task.Assignees, []string{"putnami-bot", "octocat"}) || assigned.Task.Holder != "" {
		t.Fatalf("assign answered %+v; GitHub drops a login it cannot assign and nobody holds the task", assigned.Task)
	}
	patches := h.fake.count("PATCH", `/issues/`)
	h.ok("tasks", "assign", `{"ref":`+ref+`,"assignees":["octocat","putnami-bot"]}`, &assigned)
	if h.fake.count("PATCH", `/issues/`) != patches {
		t.Error("an assignment that changes nothing wrote")
	}
	h.fails("tasks", "assign", `{"ref":`+ref+`,"assignees":[],"expectedRevision":"`+task.Revision+`"}`, collab.OutcomeConflict)
	h.ok("tasks", "assign", `{"ref":`+ref+`,"assignees":[]}`, &assigned)
	if len(assigned.Task.Assignees) != 0 {
		t.Fatalf("clearing assignees left %v", assigned.Task.Assignees)
	}
}

func TestHierarchyThroughSubIssues(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "the-parent-is-outside-the-revision-and-link-says-so")
	h := newHarness(t)
	parent := h.createTask("Epic", "epic")
	child := h.createTask("Slice", "slice")
	other := h.createTask("Other epic", "other-epic")
	ref := refJSON(child.Ref)

	var linked collab.TaskResult
	h.ok("tasks", "link", `{"ref":`+ref+`,"parent":`+refJSON(parent.Ref)+`}`, &linked)
	if linked.Task.Parent == nil || *linked.Task.Parent != parent.Ref {
		t.Fatalf("link answered %+v", linked.Task)
	}
	// The parent is outside the revision, as the link tool declares: a
	// precondition taken before the link still holds after it.
	if linked.Task.Revision != child.Revision {
		t.Fatalf("link moved the revision from %s to %s", child.Revision, linked.Task.Revision)
	}
	var got collab.TaskResult
	h.ok("tasks", "get", `{"ref":`+ref+`}`, &got)
	if got.Task.Parent == nil || *got.Task.Parent != parent.Ref || got.Task.Revision != linked.Task.Revision {
		t.Fatalf("get after link: %+v", got.Task)
	}
	posts := h.fake.count("POST", `/sub_issues$`)
	h.ok("tasks", "link", `{"ref":`+ref+`,"parent":`+refJSON(parent.Ref)+`}`, &linked)
	if h.fake.count("POST", `/sub_issues$`) != posts {
		t.Error("linking to the current parent wrote")
	}
	h.ok("tasks", "link", `{"ref":`+ref+`,"parent":`+refJSON(other.Ref)+`}`, &linked)
	if linked.Task.Parent == nil || *linked.Task.Parent != other.Ref {
		t.Fatalf("moving to another parent: %+v", linked.Task)
	}
	h.ok("tasks", "link", `{"ref":`+ref+`,"unlink":true}`, &linked)
	if linked.Task.Parent != nil {
		t.Fatalf("unlink answered %+v", linked.Task)
	}
	h.ok("tasks", "link", `{"ref":`+ref+`,"unlink":true}`, &linked)
	h.fails("tasks", "link", `{"ref":`+ref+`,"parent":`+ref+`}`, collab.OutcomeInvalid)
	h.fails("tasks", "link", `{"ref":`+ref+`,"parent":{"source":"github:acme/other","id":"1"}}`, collab.OutcomeNotFound)
}

func TestPagesFollowGitHubPaginationExactlyOnce(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "pages-follow-github-pagination-exactly-once")
	h := newHarness(t)
	for i := 1; i <= 230; i++ {
		labels := []string{"bulk"}
		if i%3 == 0 {
			labels = append(labels, "status/in-progress")
		}
		h.seedIssue("Bulk "+strconv.Itoa(i), "", labels...)
		if i%50 == 0 {
			h.fake.mu.Lock()
			h.fake.issues[i].pull = true
			h.fake.mu.Unlock()
		}
	}
	want := map[string]bool{}
	for i := 1; i <= 230; i++ {
		if i%3 != 0 && i%50 != 0 {
			want[strconv.Itoa(i)] = true
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 60 {
			t.Fatal("the traversal did not end")
		}
		arguments := `{"labels":["bulk"],"states":["open"],"page":{"size":7}}`
		if cursor != "" {
			arguments = `{"labels":["bulk"],"states":["open"],"page":{"size":7,"cursor":"` + cursor + `"}}`
		}
		var page collab.TaskListResult
		h.ok("tasks", "find", arguments, &page)
		if len(page.Items) > 7 {
			t.Fatalf("a page of 7 carried %d", len(page.Items))
		}
		for _, task := range page.Items {
			if seen[task.Ref.ID] || task.State != collab.TaskStateOpen {
				t.Fatalf("task %s returned twice or in state %s", task.Ref.ID, task.State)
			}
			seen[task.Ref.ID] = true
		}
		if pages == 3 {
			// Concurrent writers: an issue created, one already returned closed.
			late := h.seedIssue("Late", "", "bulk")
			h.fake.mu.Lock()
			h.fake.issues[1].state = "closed"
			h.fake.mu.Unlock()
			want[strconv.Itoa(late.number)] = true
		}
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("open issue %s was on no page", id)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("returned %d tasks, want %d", len(seen), len(want))
	}
	h.fails("tasks", "find", `{"page":{"cursor":"not-ours"}}`, collab.OutcomeInvalid)
}

func TestAQueryReadsTheSearchIndexWithoutQualifiers(t *testing.T) {
	h := newHarness(t)
	h.seedIssue("Flaky gate on CI", "")
	h.seedIssue("Unrelated", "")
	var page collab.TaskListResult
	h.ok("tasks", "find", `{"query":"flaky"}`, &page)
	if len(page.Items) != 1 || page.Items[0].Title != "Flaky gate on CI" {
		t.Fatalf("a query matched %+v", page.Items)
	}
	h.ok("tasks", "find", `{"query":"flaky repo:other/place"}`, &page)
	if len(page.Items) != 0 {
		t.Fatalf("a qualifier in the query text escaped the repository: %+v", page.Items)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, call := range h.fake.calls {
		if strings.Contains(call, "other/place") {
			t.Fatal("a path carried the query")
		}
	}
}

func TestAMissingItemIsNotFound(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-missing-item-is-not-found")
	h := newHarness(t)
	h.fails("tasks", "get", `{"ref":{"source":"github:acme/app","id":"404"}}`, collab.OutcomeNotFound)
	h.fails("tasks", "get", `{"ref":{"source":"github:acme/app","id":"007"}}`, collab.OutcomeNotFound)
	h.fails("tasks", "get", `{"ref":{"source":"github:acme/other","id":"1"}}`, collab.OutcomeNotFound)
	h.fails("tasks", "transition", `{"ref":{"source":"github:acme/app","id":"404"},"state":"done"}`, collab.OutcomeNotFound)
	proposal := h.openProposal("A pull request")
	if failure := h.fails("tasks", "get", `{"ref":`+refJSON(proposal.Ref)+`}`, collab.OutcomeNotFound); failure.Reason != reasonPullRequest {
		t.Fatalf("a pull request read as a task: %+v", failure)
	}
	task := h.createTask("An issue", "an-issue")
	h.fails("proposals", "status", `{"ref":`+refJSON(task.Ref)+`}`, collab.OutcomeNotFound)
	h.fails("proposals", "review", `{"ref":{"source":"github:acme/app","id":"999"},"verdict":"comment","body":"x","idempotencyKey":"k"}`, collab.OutcomeNotFound)
}

func TestAReferenceSourceIgnoresCase(t *testing.T) {
	h := newHarness(t)
	task := h.createTask("Mixed case", "mixed-case")
	var got collab.TaskResult
	h.ok("tasks", "get", `{"ref":{"source":"github:ACME/App","id":"`+task.Ref.ID+`"}}`, &got)
	if got.Task.Ref != task.Ref {
		t.Fatalf("a mixed-case source reads the same task under its canonical reference: %+v", got.Task.Ref)
	}
	h.fails("tasks", "get", `{"ref":{"source":"github:acme/other","id":"`+task.Ref.ID+`"}}`, collab.OutcomeNotFound)
}
