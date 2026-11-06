package provider

import (
	"slices"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestAnOpenedIssueReadsItsStateFromItsLabels(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "an-opened-issue-reads-its-state-from-its-labels")
	h := newHarness(t)
	cases := []struct {
		labels        []string
		state         collab.TaskState
		providerState string
		visible       []string
	}{
		{nil, collab.TaskStateOpen, "opened", nil},
		{[]string{"bug"}, collab.TaskStateOpen, "opened", []string{"bug"}},
		{[]string{"status::triage"}, collab.TaskStateOpen, "status::triage", nil},
		{[]string{"status::doing", "bug"}, collab.TaskStateInProgress, "status::doing", []string{"bug"}},
		{[]string{"status::doing", "status::blocked"}, collab.TaskStateBlocked, "status::blocked", nil},
		{[]string{"status::other"}, collab.TaskStateOpen, "opened", nil},
		{[]string{"Status::Doing"}, collab.TaskStateInProgress, "Status::Doing", nil},
	}
	for _, c := range cases {
		issue := h.seed("Labeled", "", c.labels...)
		task := h.get(refOf(issue.iid))
		if task.State != c.state || task.ProviderState != c.providerState || !slices.Equal(task.Labels, c.visible) {
			t.Errorf("labels %v read as %s (%s) with labels %v, want %s (%s) with %v",
				c.labels, task.State, task.ProviderState, task.Labels, c.state, c.providerState, c.visible)
		}
	}
	h.settings = `{"project":"` + fakeProject + `","host":"` + fakeHost + `"}`
	issue := h.seed("Unmapped", "", "status::doing")
	if task := h.get(refOf(issue.iid)); task.State != collab.TaskStateOpen || !slices.Equal(task.Labels, []string{"status::doing"}) {
		t.Errorf("without a mapping, a label is a label: %+v", task)
	}
}

func TestAClosedIssueIsDoneUnlessCanceled(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "a-closed-issue-is-done-unless-canceled")
	h := newHarness(t)
	done := h.seed("Done", "", "status::done")
	canceled := h.seed("Canceled", "", "status::canceled", "status::doing")
	duplicate := h.seed("Duplicate", "")
	plain := h.seed("Closed", "")
	for _, issue := range []*fakeIssue{done, canceled, duplicate, plain} {
		h.change(issue, func(i *fakeIssue) { i.state = "closed" })
	}
	h.change(duplicate, func(i *fakeIssue) { i.duplicateOf = done.iid })
	for _, c := range []struct {
		issue         *fakeIssue
		state         collab.TaskState
		providerState string
	}{
		{done, collab.TaskStateDone, "closed"},
		{plain, collab.TaskStateDone, "closed"},
		{canceled, collab.TaskStateCanceled, "status::canceled"},
		{duplicate, collab.TaskStateCanceled, "closed:duplicate"},
	} {
		if task := h.get(refOf(c.issue.iid)); task.State != c.state || task.ProviderState != c.providerState {
			t.Errorf("%s reads as %s (%s), want %s (%s)", c.issue.title, task.State, task.ProviderState, c.state, c.providerState)
		}
	}
}

func TestATransitionSwapsTheStateLabelsAndClosesOrReopens(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "a-transition-swaps-the-state-labels-and-closes-or-reopens")
	h := newHarness(t)
	issue := h.seed("Moving", "", "bug", "status::triage", "status::stale")
	ref := refJSON(refOf(issue.iid))
	steps := []struct {
		state  collab.TaskState
		closed bool
		labels []string
	}{
		{collab.TaskStateInProgress, false, []string{"bug", "status::doing"}},
		{collab.TaskStateBlocked, false, []string{"bug", "status::blocked"}},
		{collab.TaskStateDone, true, []string{"bug", "status::done"}},
		{collab.TaskStateCanceled, true, []string{"bug", "status::canceled"}},
		{collab.TaskStateOpen, false, []string{"bug", "status::ready"}},
		{collab.TaskStateCanceled, true, []string{"bug", "status::canceled"}},
		{collab.TaskStateInProgress, false, []string{"bug", "status::doing"}},
	}
	for _, step := range steps {
		var moved collab.TaskResult
		h.ok("transition", `{"ref":`+ref+`,"state":"`+string(step.state)+`"}`, &moved)
		stored := h.issue(issue.iid)
		if moved.Task.State != step.state || (stored.state == "closed") != step.closed || !slices.Equal(stored.labels, step.labels) {
			t.Fatalf("transition to %s left %s with %v, answered %s", step.state, stored.state, stored.labels, moved.Task.State)
		}
		if !slices.Equal(moved.Task.Labels, []string{"bug"}) {
			t.Fatalf("a task's labels show a state label: %v", moved.Task.Labels)
		}
	}

	// A transition to the state an opened issue reads as through another of
	// its labels applies the state's first label.
	triaged := h.seed("Triaged", "", "status::triage")
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+refJSON(refOf(triaged.iid))+`,"state":"open"}`, &moved)
	if stored := h.issue(triaged.iid); !slices.Equal(stored.labels, []string{"status::ready"}) || moved.Task.ProviderState != "status::ready" {
		t.Fatalf("open through another label kept %v (%s)", stored.labels, moved.Task.ProviderState)
	}

	// A closed duplicate reopened is open again.
	duplicate := h.seed("Duplicate", "")
	h.change(duplicate, func(i *fakeIssue) { i.state, i.duplicateOf = "closed", triaged.iid })
	h.ok("transition", `{"ref":`+refJSON(refOf(duplicate.iid))+`,"state":"in_progress"}`, &moved)
	if moved.Task.State != collab.TaskStateInProgress || h.issue(duplicate.iid).state != "opened" {
		t.Fatalf("a reopened duplicate reads %s", moved.Task.State)
	}
}

func TestAStateWithoutALabelIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "a-state-without-a-label-is-unsupported")
	h := newHarness(t)
	h.settings = `{"project":"` + fakeProject + `","host":"` + fakeHost + `"}`
	issue := h.seed("Plain", "")
	ref := refJSON(refOf(issue.iid))
	for _, state := range []string{"in_progress", "blocked", "canceled"} {
		failure := h.fails("transition", `{"ref":`+ref+`,"state":"`+state+`"}`, collab.OutcomeUnsupported)
		if failure.Reason != reasonStateUnmapped {
			t.Fatalf("transition to %s: %+v", state, failure)
		}
		h.fails("create", `{"title":"New","state":"`+state+`","idempotencyKey":"unmapped-`+state+`"}`, collab.OutcomeUnsupported)
	}
	if writes := h.fake.writes(); writes != 0 {
		t.Fatalf("an unsupported state sent %d writes", writes)
	}
	// open and done need no label: an opened issue without one is open, and
	// a closed one without one is done.
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+ref+`,"state":"done"}`, &moved)
	if moved.Task.State != collab.TaskStateDone || h.issue(issue.iid).state != "closed" {
		t.Fatalf("done without a label: %+v", moved.Task)
	}
	h.ok("transition", `{"ref":`+ref+`,"state":"open"}`, &moved)
	if moved.Task.State != collab.TaskStateOpen || h.issue(issue.iid).state != "opened" {
		t.Fatalf("open without a label: %+v", moved.Task)
	}
}

func TestATaskAlreadyInTheStateIsLeftAsItIs(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "a-task-already-in-the-state-is-left-as-it-is")
	h := newHarness(t)
	doing := h.seed("Doing", "", "status::doing")
	closed := h.seed("Closed", "")
	plain := h.seed("Plain", "")
	h.change(closed, func(i *fakeIssue) { i.state = "closed" })
	h.settings = `{"project":"` + fakeProject + `","host":"` + fakeHost + `","states":{"in_progress":["status::doing"]}}`
	for _, c := range []struct {
		issue *fakeIssue
		state string
	}{{doing, "in_progress"}, {closed, "done"}, {plain, "open"}} {
		var result collab.TaskResult
		h.ok("transition", `{"ref":`+refJSON(refOf(c.issue.iid))+`,"state":"`+c.state+`"}`, &result)
		if string(result.Task.State) != c.state {
			t.Fatalf("%s answered %s", c.issue.title, result.Task.State)
		}
	}
	if writes := h.fake.writes(); writes != 0 {
		t.Fatalf("a task already in its state was written %d times", writes)
	}
}

func TestAStateLabelChangesOnlyThroughTransition(t *testing.T) {
	spectest.Proves(t, feature, "label-states", "a-state-label-changes-only-through-transition")
	h := newHarness(t)
	for _, label := range []string{"status::doing", "Status::Done", "status::anything"} {
		failure := h.fails("create", `{"title":"New","labels":["`+label+`"],"idempotencyKey":"state-label"}`, collab.OutcomeInvalid)
		if failure.Reason != reasonStateLabel {
			t.Fatalf("create with %s: %+v", label, failure)
		}
	}
	failure := h.fails("create", `{"title":"New","labels":["a,b"],"idempotencyKey":"comma-label"}`, collab.OutcomeInvalid)
	if failure.Reason != reasonLabelComma {
		t.Fatalf("create with a comma: %+v", failure)
	}
	issue := h.seed("Labeled", "", "bug", "status::doing")
	ref := refJSON(refOf(issue.iid))
	h.fails("update", `{"ref":`+ref+`,"labels":["status::blocked"]}`, collab.OutcomeInvalid)
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+ref+`,"labels":["feature"]}`, &updated)
	if stored := h.issue(issue.iid); !slices.Equal(stored.labels, []string{"feature", "status::doing"}) || updated.Task.State != collab.TaskStateInProgress {
		t.Fatalf("an update replaced the state labels: %v, %s", stored.labels, updated.Task.State)
	}

	// create applies the first label of in_progress and blocked, and opens
	// then closes a task asked for in done or canceled.
	for _, c := range []struct {
		state  collab.TaskState
		closed bool
		labels []string
	}{
		{collab.TaskStateOpen, false, []string{"x"}},
		{collab.TaskStateInProgress, false, []string{"status::doing", "x"}},
		{collab.TaskStateBlocked, false, []string{"status::blocked", "x"}},
		{collab.TaskStateDone, true, []string{"status::done", "x"}},
		{collab.TaskStateCanceled, true, []string{"status::canceled", "x"}},
	} {
		created := h.create(`{"title":"In ` + string(c.state) + `","labels":["x"],"state":"` + string(c.state) + `","idempotencyKey":"create-` + string(c.state) + `"}`)
		iid := mustIID(t, created.Task.Ref)
		stored := h.issue(iid)
		if created.Task.State != c.state || (stored.state == "closed") != c.closed || !slices.Equal(stored.labels, c.labels) {
			t.Fatalf("create in %s left %s with %v, answered %s", c.state, stored.state, stored.labels, created.Task.State)
		}
	}
}

// TestASettingThatCannotBeAStateIsRefused holds the binding's states to
// GitLab's label names.
func TestASettingThatCannotBeAStateIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, settings := range []string{
		`{"project":"` + fakeProject + `","states":{"open":["a,b"]}}`,
		`{"project":"` + fakeProject + `","states":{"open":[" padded"]}}`,
		`{"project":"` + fakeProject + `","states":{"open":["x"],"done":["X"]}}`,
		`{"project":"` + fakeProject + `","stateLabelPrefix":" s"}`,
		`{"project":"` + fakeProject + `","keyScanPages":11}`,
		`{"project":"` + fakeProject + `","repository":"acme/app"}`,
		`{"project":"app"}`,
		`{"project":"acme/app.git"}`,
		`{"project":"acme/../app"}`,
		`{"project":"acme/app","host":"https://gitlab.example.com"}`,
	} {
		h.settings = settings
		if failure := h.fails("get", `{"ref":`+refJSON(refOf(1))+`}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
			t.Errorf("settings %s: %+v", settings, failure)
		}
	}
	if calls := h.fake.count(""); calls != 0 {
		t.Fatalf("invalid settings sent %d requests", calls)
	}
}
