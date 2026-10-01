// Package extension owns the contract exposed by extension manifests and task
// runtimes.
package extension

// TaskDeadlineMsEnv is exported by the CLI to a task subprocess when — and only
// when — that subprocess has a scheduler deadline. Its value is the resolved
// positive deadline in whole milliseconds, after project task tuning and batch
// scaling. Absence means the task is genuinely unbounded by the scheduler.
//
// A runtime that owns a second timeout should derive it below this value so the
// tool can report its own structured timeout before the scheduler kills the
// whole task. This is execution metadata, never a cache-key input.
const TaskDeadlineMsEnv = "PUTNAMI_TASK_DEADLINE_MS"
