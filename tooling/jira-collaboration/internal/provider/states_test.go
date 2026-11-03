package provider

import (
	"strconv"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func (h *harness) ref(issue *fakeIssue) string {
	return refJSON(collab.Ref{Source: "jira:acme.atlassian.net", ID: strconv.Itoa(issue.id)})
}

func (h *harness) setStatus(issue *fakeIssue, status, resolution string) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	issue.status, issue.resolution = status, resolution
}

func (h *harness) get(issue *fakeIssue) collab.Task {
	h.t.Helper()
	var got collab.TaskResult
	h.ok("get", `{"ref":`+h.ref(issue)+`}`, &got)
	return got.Task
}

func TestAStatusCategoryMapsToAState(t *testing.T) {
	spectest.Proves(t, feature, "state-by-category", "a-status-category-maps-to-a-state")
	spectest.Proves(t, feature, "state-by-category", "a-done-issue-resolved-wont-do-or-duplicate-is-canceled")
	h := newHarness(t)
	issue := h.seed("Mapped", "body")
	for _, c := range []struct {
		status, resolution string
		want               collab.TaskState
	}{
		{"To Do", "", collab.TaskStateOpen},
		{"In Progress", "", collab.TaskStateInProgress},
		{"Done", "Done", collab.TaskStateDone},
		{"Done", "", collab.TaskStateDone},
		{"Done", "Won't Do", collab.TaskStateCanceled},
		{"Done", "Won’t Do", collab.TaskStateCanceled},
		{"Done", "Duplicate", collab.TaskStateCanceled},
		{"Triage", "", collab.TaskStateOpen},
	} {
		h.setStatus(issue, c.status, c.resolution)
		task := h.get(issue)
		if task.State != c.want || task.ProviderState != c.status {
			t.Fatalf("%s resolved %q reads as %s (%s), want %s", c.status, c.resolution, task.State, task.ProviderState, c.want)
		}
	}
	task := h.get(issue)
	if task.Key != "ENG-1" || task.URL != "https://acme.atlassian.net/browse/ENG-1" || task.Title != "Mapped" || task.Body != "body" ||
		task.CreatedAt != "2026-09-24T08:00:00.000Z" {
		t.Fatalf("task %+v", task)
	}
}

func TestATransitionTakesTheFirstTransitionIntoTheTargetCategory(t *testing.T) {
	spectest.Proves(t, feature, "state-by-category", "a-transition-takes-the-first-transition-into-the-target-category")
	h := newHarness(t)
	issue := h.seed("Moving", "")
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"in_progress"}`, &moved)
	if moved.Task.State != collab.TaskStateInProgress || h.issue(strconv.Itoa(issue.id)).status != "In Progress" {
		t.Fatalf("moved %+v", moved.Task)
	}
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"done"}`, &moved)
	if got := h.issue(strconv.Itoa(issue.id)); moved.Task.State != collab.TaskStateDone || got.status != "Done" || got.resolution != "Done" {
		t.Fatalf("done %+v, issue %+v", moved.Task, got)
	}
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"open"}`, &moved)
	if got := h.issue(strconv.Itoa(issue.id)); moved.Task.State != collab.TaskStateOpen || got.resolution != "" {
		t.Fatalf("reopened %+v, issue %+v", moved.Task, got)
	}

	// A Done transition that requires a resolution gets Done.
	h.fake.mu.Lock()
	h.fake.resolutionRequired = true
	h.fake.mu.Unlock()
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"done"}`, &moved)
	if got := h.issue(strconv.Itoa(issue.id)); got.resolution != "Done" {
		t.Fatalf("a required resolution is %q", got.resolution)
	}
}

func TestACanceledTransitionSetsTheWontDoResolution(t *testing.T) {
	spectest.Proves(t, feature, "state-by-category", "a-canceled-transition-sets-the-wont-do-resolution")
	h := newHarness(t)
	issue := h.seed("Dropped", "")
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"canceled"}`, &moved)
	if got := h.issue(strconv.Itoa(issue.id)); moved.Task.State != collab.TaskStateCanceled || got.status != "Done" || got.resolution != "Won't Do" {
		t.Fatalf("canceled %+v, issue %+v", moved.Task, got)
	}

	other := h.seed("No cancel", "")
	h.fake.mu.Lock()
	h.fake.cancelOff = true
	h.fake.mu.Unlock()
	failure := h.fails("transition", `{"ref":`+h.ref(other)+`,"state":"canceled"}`, collab.OutcomeUnsupported)
	if failure.Reason != reasonNoTransition {
		t.Fatalf("a workflow without Won't Do: %+v", failure)
	}
	mustContain(t, failure.Message, "Done (done)")
	if h.fake.count("POST /rest/api/2/issue/"+strconv.Itoa(other.id)+"/transitions") != 0 {
		t.Fatal("an unsupported transition was sent")
	}
}

func TestATaskAlreadyInTheStateIsLeftAsItIs(t *testing.T) {
	spectest.Proves(t, feature, "state-by-category", "a-task-already-in-the-state-is-left-as-it-is")
	h := newHarness(t)
	issue := h.seed("Settled", "")
	before := h.get(issue)
	var same collab.TaskResult
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"open","expectedRevision":"`+before.Revision+`"}`, &same)
	if same.Task.Revision != before.Revision || h.fake.count("GET /rest/api/2/issue/"+strconv.Itoa(issue.id)+"/transitions") != 0 {
		t.Fatalf("a settled transition answered %+v", same.Task)
	}
}

func TestBlockedIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "state-by-category", "blocked-is-unsupported")
	h := newHarness(t)
	issue := h.seed("Blocked?", "")
	for _, failure := range []collab.Error{
		h.fails("transition", `{"ref":`+h.ref(issue)+`,"state":"blocked"}`, collab.OutcomeUnsupported),
		h.fails("create", `{"title":"Blocked","state":"blocked","idempotencyKey":"k-blocked"}`, collab.OutcomeUnsupported),
	} {
		if failure.Reason != reasonStateUnmapped {
			t.Fatalf("%+v", failure)
		}
	}
	searches := h.fake.count("POST /rest/api/2/search/jql")
	var page collab.TaskListResult
	h.ok("find", `{"states":["blocked"]}`, &page)
	if len(page.Items) != 0 || h.fake.count("POST /rest/api/2/search/jql") != searches {
		t.Fatalf("find blocked answered %+v", page)
	}
	if h.issueCount() != 1 {
		t.Fatal("a blocked create opened an issue")
	}
}

func TestTheRevisionCoversWhatATaskShows(t *testing.T) {
	spectest.Proves(t, feature, "content-revision", "the-revision-covers-what-a-task-shows")
	spectest.Proves(t, feature, "content-revision", "a-cancel-changes-the-revision")
	h := newHarness(t)
	issue := h.seed("Revisioned", "body", "a")
	seen := map[string]string{}
	record := func(change string) {
		t.Helper()
		revision := h.get(issue).Revision
		for earlier, at := range seen {
			if at == revision {
				t.Fatalf("%s left the revision of %s", change, earlier)
			}
		}
		seen[change] = revision
	}
	record("start")
	edit := func(change string, apply func(*fakeIssue)) {
		h.fake.mu.Lock()
		apply(issue)
		h.fake.mu.Unlock()
		record(change)
	}
	edit("summary", func(i *fakeIssue) { i.summary = "Renamed" })
	edit("description", func(i *fakeIssue) { text := "other"; i.description = &text })
	edit("labels", func(i *fakeIssue) { i.labels = []string{"a", "b"} })
	edit("assignee", func(i *fakeIssue) { i.assignee = "557058:someone" })
	edit("status", func(i *fakeIssue) { i.status = "In Progress" })
	edit("done", func(i *fakeIssue) { i.status, i.resolution = "Done", "Done" })
	edit("cancel", func(i *fakeIssue) { i.resolution = "Won't Do" })

	revision := h.get(issue).Revision
	h.fake.mu.Lock()
	issue.updated = issue.updated.Add(3600e9)
	issue.labels = []string{"b", "a"}
	h.fake.mu.Unlock()
	if h.get(issue).Revision != revision {
		t.Fatal("a new updated time or another label order moved the revision")
	}
}
