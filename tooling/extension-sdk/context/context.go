// Package context parses the Putnami job context file.
//
// The orchestrator passes a JSON context file via --putnamiContext that contains
// workspace, project, extension, and job metadata needed by job implementations.
//
// The contract is owned by go.putnami.dev/protocol/job
// (protocols/job); this package re-exports the protocol types
// so extension code keeps its existing imports.
package context

import (
	"encoding/json"
	"path/filepath"

	job "go.putnami.dev/protocol/job"
)

// Context represents the Putnami job execution context.
type Context = job.Context

// Workspace holds workspace-level metadata.
type Workspace = job.Workspace

// Project holds project-level metadata.
type Project = job.Project

// ProjectRef identifies one project in the resolved command selection.
type ProjectRef = job.ProjectRef

// Selection is how the command invocation chose the projects in scope: the
// mode, whether the run is narrowed, the baseline `--impacted` resolved to, and
// the sorted project ids.
//
// A task reads it to know what its verdict may claim. ProjectRef answers WHICH
// projects and reaches only the workspace-scoped jobs; this answers what the
// user asked for and how it resolved, and reaches every job. It is nil when the
// orchestrator resolved nothing — an older CLI, say — and a consumer must then
// NOT assume the run was unscoped.
type Selection = job.Selection

// Selection modes, the closed vocabulary Selection.Mode reports.
const (
	// SelectionModeAll is the unscoped whole-workspace projection.
	SelectionModeAll = job.SelectionModeAll
	// SelectionModeProjects is an explicit selector, with or without filters.
	SelectionModeProjects = job.SelectionModeProjects
	// SelectionModeImpacted is the `--impacted` projection.
	SelectionModeImpacted = job.SelectionModeImpacted
)

// UserScope is set only when the job runs outside any workspace, from an
// extension pinned in the user scope. CallerDir is the directory the command
// was invoked from and the job's working directory; the workspace paths of the
// context then name the user-scope directory, not a workspace. A nil UserScope
// means the job runs in a workspace.
type UserScope = job.UserScope

// Extension identifies the Putnami extension running this job.
type Extension = job.Extension

// Invocation identifies the non-secret private artifact root delivered to one
// finalizes relation's producer, consumers, and finalizer.
type Invocation = job.Invocation

// Job identifies the current job being executed.
type Job = job.Job

// Version holds git version info from the orchestrator.
type Version = job.Version

// Params holds typed and untyped job parameters.
type Params = job.Params

// Parse reads and parses a Putnami context JSON file.
func Parse(path string) (*Context, error) {
	return job.ParseFile(path)
}

// GenerateSchemaCommit reports the project's options.generate.schema setting:
// whether generated schema artifacts should be committed to the project tree
// (true) or kept under the gitignored .gen/ fallback (false). The second
// return is false when the project does not declare the option.
func GenerateSchemaCommit(ctx *Context) (commit bool, declared bool) {
	if ctx == nil {
		return false, false
	}
	raw, ok := ctx.Project.Options["generate"]
	if !ok {
		return false, false
	}
	var opts struct {
		Schema *bool `json:"schema"`
	}
	if json.Unmarshal(raw, &opts) != nil || opts.Schema == nil {
		return false, false
	}
	return *opts.Schema, true
}

// ResolvePath resolves a path argument the user typed, as the user meant it.
//
// An interactive command runs in the directory its task names, often the
// workspace root, not in the directory the user typed it in. A relative path
// argument therefore resolves against ctx.InvocationDir, the directory the
// orchestrator recorded for that launch. The result is, in order:
//
//   - p unchanged when it is empty or absolute;
//   - p joined to ctx.InvocationDir when the launch recorded one;
//   - p joined to ctx.UserScope.CallerDir when the launch has none but ran
//     outside a workspace, where the caller's directory is the working
//     directory anyway;
//   - p unchanged otherwise (a nil ctx, a planned job, an older CLI), so the
//     path resolves against the process's working directory as before.
//
// It is a pure string function: it reads no file and no environment variable.
// A command that takes "-" for standard input, or a non-path token, must test
// for that before calling it.
func ResolvePath(ctx *Context, p string) string {
	if p == "" || filepath.IsAbs(p) || ctx == nil {
		return p
	}
	switch {
	case ctx.InvocationDir != "":
		return filepath.Join(ctx.InvocationDir, p)
	case ctx.UserScope != nil && ctx.UserScope.CallerDir != "":
		return filepath.Join(ctx.UserScope.CallerDir, p)
	}
	return p
}
