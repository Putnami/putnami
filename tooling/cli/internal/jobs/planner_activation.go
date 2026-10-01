package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// activationScope is the filesystem view one project's activation gates are
// probed against: the project's own root, and the roots of its whole dependency
// closure.
//
// The closure is resolved LAZILY, through a function the caller supplies,
// because almost no gate asks for it: eagerly walking the graph for every
// (project, command) pair would pay for an answer the common `files`/`contains`
// gate never reads.
type activationScope struct {
	projRoot     string
	closureRoots func() []string
	cache        *planCache
}

// pipelineExpansionOptions returns every project/invocation fact the pipeline
// expander consults: filesystem activation, the immutable provider/project
// command set with its resolved params, and the project's classification.
// Keeping that projection in one function ensures direct and dependency-
// emitted jobs cannot disagree.
//
// projRoot is computed once here so each step probe reuses it; filesystem
// results and the resolved closure are memoized in the shared *planCache.
func pipelineExpansionOptions(
	ws *workspace.Workspace,
	proj *workspace.Project,
	ext *extension.ExtensionDescription,
	namespace bool,
	cache *planCache,
) extension.PipelineExpansionOptions {
	scope := activationScope{cache: cache}
	if ws != nil && ws.Root != "" {
		scope.projRoot = filepath.Join(ws.Root, proj.Path)
	}
	// Resolve the closure at most once per project, through core's single
	// closure definition so the gate and the runtime read the same set.
	scope.closureRoots = func() []string {
		if ws == nil || proj == nil {
			return nil
		}
		if cache == nil {
			return projectClosureRoots(ws, proj)
		}
		if roots, ok := cache.closureRoots[proj.ID]; ok {
			return roots
		}
		roots := projectClosureRoots(ws, proj)
		cache.closureRoots[proj.ID] = roots
		return roots
	}
	opts := extension.PipelineExpansionOptions{
		StepActive: func(step extension.PipelineStep) bool {
			return stepIsActive(step.Activation, scope)
		},
	}
	if cache != nil && proj != nil {
		projectType := proj.Type
		contextKey := proj.ID
		if ext != nil {
			contextKey += "\x00" + ext.Name
		}
		commands := cache.effectiveCommands[contextKey]
		commandParams := cache.effectiveCommandParams[contextKey]
		opts.PlanContext = &extension.WhenContext{
			Commands:      commands,
			CommandParams: commandParams,
			ProjectType:   &projectType,
		}
	}
	if namespace && ext != nil {
		opts.Namespace = ext.Name
	}
	return opts
}

// stepIsActive reports whether a step's activation gate is satisfied. A nil
// gate (the common case) is always active.
func stepIsActive(act *extension.StepActivation, scope activationScope) bool {
	if act == nil {
		return true
	}
	// Without a resolvable project root we cannot probe files; keep the step so
	// it skips at runtime exactly as before rather than risk over-pruning.
	if scope.projRoot == "" {
		return true
	}
	if len(act.Files) > 0 && !hasActivationFiles(scope.projRoot, act.Files, scope.cache) {
		return false
	}
	// A closure gate asks the question the step's task will answer at runtime:
	// does ANY member of the dependency closure — this project included —
	// declare the file? A project whose need is entirely transitive answers yes
	// here and no to the project-local `files` gate, and it is precisely the
	// project that must keep the step. A scope with no resolvable closure falls
	// back to the project's own root, which is the closure of one an edgeless
	// project has.
	if len(act.ClosureFiles) > 0 {
		roots := []string{scope.projRoot}
		if scope.closureRoots != nil {
			if resolved := scope.closureRoots(); len(resolved) > 0 {
				roots = resolved
			}
		}
		matched := false
		for _, root := range roots {
			if hasActivationFiles(root, act.ClosureFiles, scope.cache) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Iterate content gates in sorted order for deterministic probing.
	paths := make([]string, 0, len(act.Contains))
	for path := range act.Contains {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if !fileContains(scope.projRoot, path, act.Contains[path], scope.cache) {
			return false
		}
	}
	return true
}

// fileContains reports whether the project-relative file contains the
// substring. Results are memoized per (absolute path, substring).
func fileContains(projRoot, relPath, substr string, cache *planCache) bool {
	full := filepath.Join(projRoot, relPath)
	if cache != nil {
		key := full + "\x00" + substr
		if v, ok := cache.fileContains[key]; ok {
			return v
		}
		v := fileContainsUncached(full, substr)
		cache.fileContains[key] = v
		return v
	}
	return fileContainsUncached(full, substr)
}

// fileContainsUncached reads the file and reports substring membership. A read
// error (missing/unreadable file) reports false so activation fails closed.
func fileContainsUncached(path, substr string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(substr))
}
