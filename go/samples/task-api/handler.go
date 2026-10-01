package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"

	"go.putnami.dev/events"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	"go.putnami.dev/parallel"
	"go.putnami.dev/schema"
)

// bindAndValidate decodes the JSON body into a map, checks it can be decoded
// into T, and validates it against T's `validate:` struct tags using
// go.putnami.dev/schema. It returns field errors (nil when valid), replacing
// hand-rolled checks like `if input.Title == ""`.
func bindAndValidate[T any](ctx *http.Context, out *T) []schema.FieldError {
	var raw map[string]any
	if err := ctx.Body(&raw); err != nil {
		return []schema.FieldError{{Field: "body", Message: "invalid JSON body"}}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return []schema.FieldError{{Field: "body", Message: "invalid JSON body"}}
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		return []schema.FieldError{{Field: "body", Message: "invalid field types"}}
	}
	if result := schema.Validate(reflect.TypeFor[T](), raw); result.HasErrors() {
		return result.Errors
	}
	return nil
}

// Event topics.
var (
	TaskCreatedTopic   = events.NewTopic[Task]("task.created")
	TaskCompletedTopic = events.NewTopic[Task]("task.completed")
)

// TaskHandler provides HTTP handlers for the task API.
type TaskHandler struct {
	store     *TaskStore
	publisher *events.Publisher[Task]
	log       *logger.Logger
}

// NewTaskHandler creates a new task handler with its dependencies.
func NewTaskHandler(store *TaskStore, publisher *events.Publisher[Task], log *logger.Logger) *TaskHandler {
	return &TaskHandler{
		store:     store,
		publisher: publisher,
		log:       log.Named("handler"),
	}
}

// List returns all tasks.
func (h *TaskHandler) List(ctx *http.Context) *http.Response {
	tasks := h.store.List()
	return http.JSON(tasks)
}

// Get returns a single task by ID.
func (h *TaskHandler) Get(ctx *http.Context) *http.Response {
	id := ctx.Param("id")
	task, err := h.store.Get(id)
	if err != nil {
		return http.NotFound()
	}
	return http.JSON(task)
}

// Create adds a new task and publishes a task.created event.
func (h *TaskHandler) Create(ctx *http.Context) *http.Response {
	var input CreateTaskInput
	if errs := bindAndValidate(ctx, &input); errs != nil {
		return http.JSONStatus(400, map[string]any{"error": "validation failed", "fields": errs})
	}

	task := h.store.Create(input)
	h.log.Info("task created", slog.String("id", task.ID), slog.String("title", task.Title))

	// Publish event asynchronously (fire-and-forget for the HTTP response).
	go func() {
		if err := h.publisher.Publish(context.Background(), *task); err != nil {
			h.log.Error("publish task.created", err, slog.String("id", task.ID))
		}
	}()

	return http.JSONStatus(201, task)
}

// Update modifies an existing task. Publishes task.completed when status changes to "completed".
func (h *TaskHandler) Update(ctx *http.Context) *http.Response {
	id := ctx.Param("id")

	var input UpdateTaskInput
	if errs := bindAndValidate(ctx, &input); errs != nil {
		return http.JSONStatus(400, map[string]any{"error": "validation failed", "fields": errs})
	}

	// Check previous status for completion event.
	prev, err := h.store.Get(id)
	if err != nil {
		return http.NotFound()
	}
	wasCompleted := prev.Status == "completed"

	task, err := h.store.Update(id, input)
	if err != nil {
		return http.NotFound()
	}
	h.log.Info("task updated", slog.String("id", task.ID))

	// Publish completion event if status just changed to "completed".
	if task.Status == "completed" && !wasCompleted {
		go func() {
			if err := h.publisher.Publish(context.Background(), *task); err != nil {
				h.log.Error("publish task.completed", err, slog.String("id", task.ID))
			}
		}()
	}

	return http.JSON(task)
}

// Delete removes a task.
func (h *TaskHandler) Delete(ctx *http.Context) *http.Response {
	id := ctx.Param("id")
	if err := h.store.Delete(id); err != nil {
		return http.NotFound()
	}
	h.log.Info("task deleted", slog.String("id", id))
	return http.NoContent()
}

// batchResult is one entry in a BatchGet response.
type batchResult struct {
	ID    string `json:"id"`
	Task  *Task  `json:"task,omitempty"`
	Found bool   `json:"found"`
}

// BatchGet looks up many tasks concurrently from a {"ids": [...]} body, capping
// concurrency at 8 workers via go.putnami.dev/parallel.MapBounded — the
// framework's primitive for bounded fan-out with cancellation and ordered
// results. Each lookup reports found/not-found rather than failing the batch.
func (h *TaskHandler) BatchGet(ctx *http.Context) *http.Response {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := ctx.Body(&req); err != nil {
		return http.JSONStatus(400, map[string]string{"error": "invalid request body"})
	}

	results, err := parallel.MapBounded(ctx.Context(), req.IDs, 8,
		func(_ context.Context, id string) (batchResult, error) {
			task, getErr := h.store.Get(id)
			if getErr != nil {
				return batchResult{ID: id, Found: false}, nil
			}
			return batchResult{ID: id, Task: task, Found: true}, nil
		})
	if err != nil {
		return http.JSONStatus(500, map[string]string{"error": err.Error()})
	}
	return http.JSON(results)
}
