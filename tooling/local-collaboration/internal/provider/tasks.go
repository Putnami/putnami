package provider

import (
	"context"
	"slices"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

func (p *Provider) findTasks(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.TaskFindInput](call)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	state, failure := readState(s)
	if failure != nil {
		return nil, failure
	}
	var matches []collab.Task
	for _, task := range state.Tasks {
		if input.Query != "" && !containsFold(task.Title, input.Query) && !containsFold(task.Body, input.Query) {
			continue
		}
		if len(input.States) > 0 && !slices.Contains(input.States, task.State) {
			continue
		}
		if !hasLabels(task.Labels, input.Labels) {
			continue
		}
		matches = append(matches, task)
	}
	items, next, failure := page(matches, func(task collab.Task) string { return task.Ref.ID }, input.Page)
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskListResult{Items: items, Page: next}, nil
}

func hasLabels(have, want []string) bool {
	for _, label := range want {
		if !slices.Contains(have, label) {
			return false
		}
	}
	return true
}

func (p *Provider) getTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.TaskRefInput](call)
	if failure != nil {
		return nil, failure
	}
	ref := input.Ref
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	state, failure := readState(s)
	if failure != nil {
		return nil, failure
	}
	index := taskIndex(state, ref)
	if index < 0 {
		return nil, notFound("task", ref)
	}
	return &collab.TaskResult{Task: state.Tasks[index]}, nil
}

func taskIndex(state *store.State, ref collab.Ref) int {
	if foreign(state, ref) {
		return -1
	}
	return slices.IndexFunc(state.Tasks, func(task collab.Task) bool { return task.Ref.ID == ref.ID })
}

func (p *Provider) createTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	request, failure := inputOf[*collab.TaskCreateInput](call)
	if failure != nil {
		return nil, failure
	}
	input := *request
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	key := keyDigest("tasks.create", input.IdempotencyKey)
	digest := requestDigest(struct {
		Title  string           `json:"title"`
		Body   string           `json:"body"`
		Labels []string         `json:"labels"`
		State  collab.TaskState `json:"state"`
	}{input.Title, input.Body, input.Labels, input.State})
	var created collab.Task
	replayed := false
	failure = update(s, func(state *store.State) error {
		if prior, used := state.Keys[key]; used {
			if prior.Kind != "task" || prior.Digest != digest {
				return refuse(collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
					"idempotency key %q was already used for a different request", input.IdempotencyKey))
			}
			index := taskIndex(state, collab.Ref{Source: state.Source(), ID: prior.ID})
			if index < 0 {
				return refuse(collab.Fail(collab.OutcomeUnavailable, "store.inconsistent",
					"idempotency key %q names task %s, which the store no longer holds", input.IdempotencyKey, prior.ID))
			}
			created, replayed = state.Tasks[index], true
			return errNothingToWrite
		}
		taskState := input.State
		if taskState == "" {
			taskState = collab.TaskStateOpen
		}
		state.Next.Task++
		created = collab.Task{
			Ref:       collab.Ref{Source: state.Source(), ID: "T-" + strconv.Itoa(state.Next.Task)},
			Revision:  "r1",
			Title:     input.Title,
			Body:      input.Body,
			State:     taskState,
			Labels:    input.Labels,
			UpdatedAt: p.timestamp(),
		}
		state.Tasks = append(state.Tasks, created)
		state.Keys[key] = store.Key{Kind: "task", ID: created.Ref.ID, Digest: digest}
		return nil
	})
	if replayed {
		return &collab.TaskCreateResult{Task: created, Created: false}, nil
	}
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskCreateResult{Task: created, Created: true}, nil
}

// changeTask applies one change to a task under the store lock, honoring
// expectedRevision atomically. A change that leaves the task as it was keeps
// its revision, so repeating an update does not move it.
func (p *Provider) changeTask(call collab.Call, ref collab.Ref, expected string, change func(*collab.Task)) (any, *collab.Failure) {
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	var result collab.Task
	failure = update(s, func(state *store.State) error {
		index := taskIndex(state, ref)
		if index < 0 {
			return refuse(notFound("task", ref))
		}
		task := &state.Tasks[index]
		if expected != "" && task.Revision != expected {
			return refuse(conflictOn(task.Revision, "task %s is at revision %s, not %s", ref.ID, task.Revision, expected))
		}
		before := requestDigest(task)
		change(task)
		if requestDigest(task) != before {
			task.Revision = nextRevision(task.Revision)
			task.UpdatedAt = p.timestamp()
		}
		result = *task
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: result}, nil
}

func (p *Provider) updateTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.TaskUpdateInput](call)
	if failure != nil {
		return nil, failure
	}
	return p.changeTask(call, input.Ref, input.ExpectedRevision, func(task *collab.Task) {
		if input.Title != nil {
			task.Title = *input.Title
		}
		if input.Body != nil {
			task.Body = *input.Body
		}
		if input.Labels != nil {
			task.Labels = *input.Labels
		}
	})
}

func (p *Provider) transitionTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.TaskTransitionInput](call)
	if failure != nil {
		return nil, failure
	}
	return p.changeTask(call, input.Ref, input.ExpectedRevision, func(task *collab.Task) {
		task.State = input.State
	})
}
