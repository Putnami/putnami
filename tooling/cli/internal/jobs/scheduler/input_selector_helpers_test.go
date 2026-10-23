package scheduler

import (
	"slices"
	"sync"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// stubInputResolver answers each input query with what answer returns for it,
// and counts the queries it answers.
type stubInputResolver struct {
	answer func(query store.InputQuery) (store.InputExpansion, error)

	mu     sync.Mutex
	asked  []store.InputQuery
	primed [][]store.InputQuery
}

func (r *stubInputResolver) ResolveInput(query store.InputQuery) (store.InputExpansion, error) {
	r.mu.Lock()
	r.asked = append(r.asked, query)
	r.mu.Unlock()
	return r.answer(query)
}

func (r *stubInputResolver) PrimeInputs(queries []store.InputQuery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primed = append(r.primed, slices.Clone(queries))
}

// listedInputs answers every query with expansion.
func listedInputs(expansion store.InputExpansion) *stubInputResolver {
	return &stubInputResolver{answer: func(store.InputQuery) (store.InputExpansion, error) { return expansion, nil }}
}

// nameSelectorInput makes job's manifest task read patterns as one project
// input, and has its extension list sel:build, as the @test/selectors
// extension does: the key reads patterns, and the task's input names the
// selector the resolver expands.
func nameSelectorInput(job *ScheduledJob, patterns ...string) *ScheduledJob {
	task := job.Extension.Tasks[job.Step.Task]
	task.Inputs = map[string]extension.TaskInputPort{
		"sources": {From: extension.TaskInputFromProject, Files: patterns},
	}
	job.Extension.Tasks[job.Step.Task] = task
	job.Extension.Runtime = &extension.RuntimeDefinition{InputSelectors: []string{"sel:build"}}
	job.JobDef.FilePatterns = patterns
	return job
}
