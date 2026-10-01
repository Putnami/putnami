package main

import (
	"fmt"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// Task error codes.
const (
	CodeTaskNotFound errors.Code = "task.not_found"
)

// Task represents a work item.
type Task struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"` // "pending", "in_progress", "completed"
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CreateTaskInput is the request body for creating a task. The `validate` tags
// are enforced by go.putnami.dev/schema (see handler.bindAndValidate) — the
// framework's struct-tag validation primitive — instead of hand-rolled checks.
type CreateTaskInput struct {
	Title       string `json:"title" validate:"required,minlen=1,maxlen=200"`
	Description string `json:"description" validate:"maxlen=2000"`
}

// UpdateTaskInput is the request body for updating a task. Every field is
// optional (a partial update); schema skips constraints on absent fields, so
// only the fields actually supplied are validated.
type UpdateTaskInput struct {
	Title       string `json:"title,omitempty" validate:"maxlen=200"`
	Description string `json:"description,omitempty" validate:"maxlen=2000"`
	Status      string `json:"status,omitempty" validate:"oneof=pending|in_progress|completed"`
}

// TaskStore is a thread-safe in-memory task repository.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	seq   int
}

// NewTaskStore creates a new in-memory task store.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[string]*Task),
	}
}

// Create adds a new task and returns it.
func (s *TaskStore) Create(input CreateTaskInput) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	now := time.Now().UTC()
	task := &Task{
		ID:          fmt.Sprintf("task-%d", s.seq),
		Title:       input.Title,
		Description: input.Description,
		Status:      "pending",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.tasks[task.ID] = task
	return task
}

// Get retrieves a task by ID.
func (s *TaskStore) Get(id string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[id]
	if !ok {
		return nil, errors.Newf(CodeTaskNotFound, "task %q not found", id)
	}
	return task, nil
}

// List returns all tasks sorted by creation time.
func (s *TaskStore) List() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	return tasks
}

// Update modifies an existing task.
func (s *TaskStore) Update(id string, input UpdateTaskInput) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return nil, errors.Newf(CodeTaskNotFound, "task %q not found", id)
	}
	if input.Title != "" {
		task.Title = input.Title
	}
	if input.Description != "" {
		task.Description = input.Description
	}
	if input.Status != "" {
		task.Status = input.Status
	}
	task.UpdatedAt = time.Now().UTC()
	return task, nil
}

// Delete removes a task by ID.
func (s *TaskStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
		return errors.Newf(CodeTaskNotFound, "task %q not found", id)
	}
	delete(s.tasks, id)
	return nil
}
