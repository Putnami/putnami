package cachekey

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// JobInputQueries lists the input selectors a job's task names in its project
// inputs, as queries for the extension that declares the task in the job's
// project directory: one per selector of each input, with the input's other
// patterns. A job whose task names none, or that runs no manifest task, has
// none.
func JobInputQueries(ws *workspace.Workspace, job *jobmodel.ScheduledJob) []store.InputQuery {
	if ws == nil || job == nil || job.Project == nil || job.Extension == nil {
		return nil
	}
	task, ok := jobTask(job)
	if !ok {
		return nil
	}
	inputs := extproto.TaskSelectorInputs(task)
	if len(inputs) == 0 {
		return nil
	}
	dir := filepath.Join(ws.Root, job.Project.Path)
	queries := make([]store.InputQuery, 0, len(inputs))
	for _, input := range inputs {
		queries = append(queries, store.InputQuery{
			Extension: job.Extension.Name,
			Dir:       dir,
			Selector:  input.Selector,
			Patterns:  input.Patterns,
		})
	}
	return queries
}

// jobTask returns the manifest task a job runs: its pipeline step's task, or,
// for a job with no step, a task whose cache key is the job's.
func jobTask(job *jobmodel.ScheduledJob) (extproto.TaskDefinition, bool) {
	if job.Step != nil && job.Step.Task != "" {
		task, ok := job.Extension.Tasks[job.Step.Task]
		return task, ok
	}
	if job.JobDef == nil || job.JobDef.TaskCachePolicy == nil {
		return extproto.TaskDefinition{}, false
	}
	return extproto.TaskDefinition{Cache: job.JobDef.TaskCachePolicy}, true
}

// PlanInputQueries lists JobInputQueries for every planned job and the
// producer whose invocation-scoped resource it consumes: every input selector
// the plan's keys read.
func PlanInputQueries(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob) []store.InputQuery {
	var queries []store.InputQuery
	for _, job := range planned {
		for current := job; current != nil; current = current.InvocationProducer {
			queries = append(queries, JobInputQueries(ws, current)...)
		}
	}
	return queries
}

// PrimePlanInputs asks the extensions once, through the resolver cache
// carries, for every input selector the plan's keys read, before the first key
// asks for one of them.
func PrimePlanInputs(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob, cache *store.CacheManager) {
	if resolver := cache.InputResolver(); resolver != nil {
		if queries := PlanInputQueries(ws, planned); len(queries) > 0 {
			resolver.PrimeInputs(queries)
		}
	}
}

// JobInputExpander returns the expander a job's cache key expands the input
// selectors of its file patterns with. It answers a selector the job's task
// names in a project input through resolver, in the job's project directory,
// with the union of the answers of every input that names it. It refuses every
// selector of a job whose project filePatterns or --filePatterns flag names
// one: only a task input of the manifest that lists a selector names it.
func JobInputExpander(ws *workspace.Workspace, job *jobmodel.ScheduledJob, params map[string]any, resolver store.InputResolver) store.InputExpander {
	expander := &jobInputExpander{resolver: resolver, queries: JobInputQueries(ws, job)}
	if ws != nil && job != nil && job.Project != nil {
		expander.dir = filepath.Join(ws.Root, job.Project.Path)
		for _, pattern := range append(ParamStrings(params, "filePatterns"), ProjectFilePatterns(ws, job)...) {
			if extproto.NamesInputSelector(pattern) {
				expander.refused = pattern
				break
			}
		}
	}
	return expander
}

// jobInputExpander is JobInputExpander's expander for one job.
type jobInputExpander struct {
	resolver store.InputResolver
	dir      string
	queries  []store.InputQuery
	// refused is a selector a filePatterns option or flag names.
	refused string
}

func (e *jobInputExpander) ExpandInput(dir, selector string) (store.InputExpansion, error) {
	if e.refused != "" {
		return store.InputExpansion{}, fmt.Errorf("%w: selector %q requires an extension task input, not filePatterns", store.ErrInputSelector, e.refused)
	}
	if filepath.Clean(dir) != e.dir {
		return store.InputExpansion{}, fmt.Errorf("%w: selector %q expands in its project directory %s, not %s", store.ErrInputSelector, selector, e.dir, dir)
	}
	if e.resolver == nil {
		return store.InputExpansion{}, fmt.Errorf("%w: no resolver expands selector %q", store.ErrInputSelector, selector)
	}
	var expansion store.InputExpansion
	named := false
	for _, query := range e.queries {
		if query.Selector != selector {
			continue
		}
		named = true
		answer, err := e.resolver.ResolveInput(query)
		if err != nil {
			return store.InputExpansion{}, err
		}
		expansion.Files = append(expansion.Files, answer.Files...)
		expansion.Basis.Content = append(expansion.Basis.Content, answer.Basis.Content...)
		expansion.Basis.Listing = append(expansion.Basis.Listing, answer.Basis.Listing...)
	}
	if !named {
		return store.InputExpansion{}, fmt.Errorf("%w: selector %q is named by no project input of the task", store.ErrInputSelector, selector)
	}
	slices.Sort(expansion.Files)
	expansion.Files = slices.Compact(expansion.Files)
	return expansion, nil
}

// Identity names the job's extension, project directory, selector inputs and
// refusal: two jobs with one identity expand every selector alike.
func (e *jobInputExpander) Identity() string {
	var b strings.Builder
	b.WriteString(e.dir)
	b.WriteString("\x00")
	b.WriteString(e.refused)
	for _, query := range e.queries {
		b.WriteString("\x00")
		b.WriteString(query.Extension)
		b.WriteString("\x01")
		b.WriteString(query.Selector)
		b.WriteString("\x01")
		b.WriteString(strings.Join(query.Patterns, "\x02"))
	}
	return b.String()
}
