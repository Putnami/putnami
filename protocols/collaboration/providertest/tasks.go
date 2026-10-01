package providertest

import (
	"fmt"
	"slices"
	"strconv"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// RunTasks runs the tasks v1 scenarios, each against a fresh target.
func RunTasks(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(t *testing.T, s *Session)
	}{
		{"a label nobody carries finds an empty page", tasksEmptyPage},
		{"create, read and replay an idempotency key", tasksCreateAndReplay},
		{"pages cover the result set exactly once", tasksPages},
		{"state, label and text filters", tasksFilters},
		{"a stale revision is a conflict", tasksPreconditions},
		{"a reference from another source is not found", tasksForeignReferences},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.run(t, Open(t, newTarget(t)))
		})
	}
}

func (s *Session) createTask(title, labels, key string) collab.TaskCreateResult {
	s.t.Helper()
	var created collab.TaskCreateResult
	arguments := `{"title":` + quote(title) + `,"body":"Created by the provider contract scenarios.","labels":` + labels +
		`,"idempotencyKey":` + quote(key) + `}`
	s.OK("tasks", "create", arguments, &created)
	if !created.Created {
		s.t.Fatalf("a new idempotency key answered created: false: %+v", created)
	}
	s.track("tasks", created.Task.Ref)
	return created
}

func tasksEmptyPage(t *testing.T, s *Session) {
	var page collab.TaskListResult
	s.OK("tasks", "find", `{"labels":[`+quote("pt-none-"+Unique(t))+`]}`, &page)
	if page.Items == nil || len(page.Items) != 0 || page.Page.Next != "" {
		t.Fatalf("an unused label matched %+v", page)
	}
}

func tasksCreateAndReplay(t *testing.T, s *Session) {
	run := Unique(t)
	label := "pt-" + run
	key := "providertest-create-" + run
	first := s.createTask("Scenario task "+run, `[`+quote(label)+`]`, key)
	task := first.Task
	if task.State != collab.TaskStateOpen || task.Title != "Scenario task "+run || !slices.Contains(task.Labels, label) {
		t.Fatalf("created %+v", task)
	}

	var got collab.TaskResult
	s.OK("tasks", "get", `{"ref":`+refJSON(task.Ref)+`}`, &got)
	if got.Task.Ref != task.Ref || got.Task.Revision != task.Revision || got.Task.Title != task.Title ||
		got.Task.Body != task.Body || got.Task.State != task.State {
		t.Fatalf("get answered %+v for the task created as %+v", got.Task, task)
	}

	var replay collab.TaskCreateResult
	s.OK("tasks", "create", `{"title":`+quote("Scenario task "+run)+`,"body":"Created by the provider contract scenarios.","labels":[`+
		quote(label)+`],"idempotencyKey":`+quote(key)+`}`, &replay)
	if replay.Created || replay.Task.Ref != task.Ref {
		t.Fatalf("a repeated create made a second task: %+v then %+v", task, replay)
	}
	failure := s.Fails("tasks", "create", `{"title":"Something else","idempotencyKey":`+quote(key)+`}`, collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Retryable {
		t.Fatalf("a reused key with other content: %+v", failure)
	}

	var all collab.TaskListResult
	s.Eventually("tasks", "find", `{"labels":[`+quote(label)+`]}`, &all, func() string {
		if len(all.Items) != 1 || all.Items[0].Ref != task.Ref {
			return fmt.Sprintf("after a replay and a refused reuse the label carries %d tasks, want exactly the one created", len(all.Items))
		}
		return ""
	})
}

func tasksPages(t *testing.T, s *Session) {
	run := Unique(t)
	label := "pt-" + run
	var created []collab.Ref
	for i := 1; i <= 5; i++ {
		result := s.createTask("Paged task "+strconv.Itoa(i)+" "+run, `[`+quote(label)+`]`, "providertest-page-"+run+"-"+strconv.Itoa(i))
		created = append(created, result.Task.Ref)
	}
	// traverse walks every page, asking for the sizes in turn: a cursor names
	// a position, so the size may change between the pages of one traversal.
	traverse := func(sizes ...int) []collab.Ref {
		var refs []collab.Ref
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatalf("a traversal of 5 tasks in pages of %v did not end: %v", sizes, refs)
			}
			size := sizes[pages%len(sizes)]
			arguments := `{"labels":[` + quote(label) + `],"page":{"size":` + strconv.Itoa(size) + `}}`
			if cursor != "" {
				arguments = `{"labels":[` + quote(label) + `],"page":{"size":` + strconv.Itoa(size) + `,"cursor":` + quote(cursor) + `}}`
			}
			var page collab.TaskListResult
			s.OK("tasks", "find", arguments, &page)
			if len(page.Items) > size {
				t.Fatalf("a page of size %d carried %d tasks", size, len(page.Items))
			}
			for _, task := range page.Items {
				refs = append(refs, task.Ref)
			}
			if cursor = page.Page.Next; cursor == "" {
				return refs
			}
		}
	}
	var first []collab.Ref
	s.Eventually("tasks", "find", `{"labels":[`+quote(label)+`]}`, &collab.TaskListResult{}, func() string {
		first = traverse(2)
		if len(first) != len(created) {
			return fmt.Sprintf("the pages yielded %d tasks, want %d", len(first), len(created))
		}
		return ""
	})
	seen := map[collab.Ref]bool{}
	for _, ref := range first {
		if seen[ref] {
			t.Fatalf("task %+v appears on two pages", ref)
		}
		seen[ref] = true
	}
	for _, ref := range created {
		if !seen[ref] {
			t.Fatalf("task %+v is on no page", ref)
		}
	}
	if second := traverse(2); !slices.Equal(first, second) {
		t.Fatalf("two traversals disagree on the order: %v then %v", first, second)
	}
	if mixed := traverse(1, 3); !slices.Equal(first, mixed) {
		t.Fatalf("a traversal whose page size changes disagrees: %v then %v", first, mixed)
	}
}

func tasksFilters(t *testing.T, s *Session) {
	run := Unique(t)
	label := "pt-" + run
	word := "needle" + run
	open := s.createTask("Filtered "+word, `[`+quote(label)+`]`, "providertest-filter-open-"+run).Task
	closed := s.createTask("Filtered other "+run, `[`+quote(label)+`]`, "providertest-filter-done-"+run).Task
	var done collab.TaskResult
	s.OK("tasks", "transition", `{"ref":`+refJSON(closed.Ref)+`,"state":"done"}`, &done)
	if done.Task.Ref != closed.Ref || done.Task.State != collab.TaskStateDone {
		t.Fatalf("transition answered %+v", done.Task)
	}

	only := func(page *collab.TaskListResult, want collab.Ref) func() string {
		return func() string {
			if len(page.Items) != 1 || page.Items[0].Ref != want {
				return fmt.Sprintf("matched %d tasks, want only %s/%s", len(page.Items), want.Source, want.ID)
			}
			return ""
		}
	}
	var page collab.TaskListResult
	s.Eventually("tasks", "find", `{"labels":[`+quote(label)+`],"states":["open"]}`, &page, only(&page, open.Ref))
	s.Eventually("tasks", "find", `{"labels":[`+quote(label)+`],"states":["done"]}`, &page, only(&page, closed.Ref))
	s.Eventually("tasks", "find", `{"labels":[`+quote(label)+`],"query":`+quote(word)+`}`, &page, only(&page, open.Ref))
	s.OK("tasks", "find", `{"labels":[`+quote(label)+`,`+quote("pt-absent-"+run)+`]}`, &page)
	if len(page.Items) != 0 {
		t.Fatalf("every listed label must match; a label no task carries matched %d", len(page.Items))
	}
}

func tasksPreconditions(t *testing.T, s *Session) {
	run := Unique(t)
	task := s.createTask("Revisioned "+run, `[`+quote("pt-"+run)+`]`, "providertest-revision-"+run).Task
	ref := refJSON(task.Ref)

	var updated collab.TaskResult
	s.OK("tasks", "update", `{"ref":`+ref+`,"expectedRevision":`+quote(task.Revision)+`,"title":`+quote("Renamed "+run)+`}`, &updated)
	if updated.Task.Ref != task.Ref || updated.Task.Title != "Renamed "+run || updated.Task.Revision == task.Revision {
		t.Fatalf("update answered %+v after %+v", updated.Task, task)
	}
	failure := s.Fails("tasks", "update", `{"ref":`+ref+`,"expectedRevision":`+quote(task.Revision)+`,"title":"Stale"}`, collab.OutcomeConflict)
	if failure.Retryable || (failure.Current != "" && failure.Current != updated.Task.Revision) {
		t.Fatalf("a stale update: %+v, current revision %s", failure, updated.Task.Revision)
	}
	s.Fails("tasks", "transition", `{"ref":`+ref+`,"state":"done","expectedRevision":`+quote(task.Revision)+`}`, collab.OutcomeConflict)

	var moved collab.TaskResult
	s.OK("tasks", "transition", `{"ref":`+ref+`,"state":"done","expectedRevision":`+quote(updated.Task.Revision)+`}`, &moved)
	if moved.Task.Ref != task.Ref || moved.Task.State != collab.TaskStateDone || moved.Task.Revision == updated.Task.Revision {
		t.Fatalf("transition answered %+v", moved.Task)
	}
	var got collab.TaskResult
	s.OK("tasks", "get", `{"ref":`+ref+`}`, &got)
	if got.Task.State != collab.TaskStateDone || got.Task.Title != "Renamed "+run || got.Task.Revision != moved.Task.Revision {
		t.Fatalf("get answered %+v after the transition answered %+v", got.Task, moved.Task)
	}
}

func tasksForeignReferences(t *testing.T, s *Session) {
	ref := refJSON(foreignRef)
	s.Fails("tasks", "get", `{"ref":`+ref+`}`, collab.OutcomeNotFound)
	s.Fails("tasks", "update", `{"ref":`+ref+`,"title":"x"}`, collab.OutcomeNotFound)
	s.Fails("tasks", "transition", `{"ref":`+ref+`,"state":"done"}`, collab.OutcomeNotFound)
}
