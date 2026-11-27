package planner

import (
	"bytes"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
	// wsRoot is the workspace root the activation witnesses are kept under;
	// empty, an answer is computed and not kept.
	wsRoot   string
	projRoot string
	// projectType is the project's declared type, "" when it declares none.
	projectType  string
	closureRoots func() []string
	cache        *planCache
	// changedFiles are the run's changed files the project owns,
	// project-relative; nil when it owns none or the run measured no diff.
	changedFiles []string
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
		scope.wsRoot = ws.Root
		scope.projRoot = filepath.Join(ws.Root, proj.Path)
	}
	if proj != nil {
		scope.projectType = proj.Type
	}
	// Resolve the closure at most once per project, through core's single
	// closure definition so the gate and the runtime read the same set.
	scope.closureRoots = func() []string {
		if ws == nil || proj == nil {
			return nil
		}
		if cache == nil {
			return cachekey.ProjectClosureRoots(ws, proj)
		}
		if roots, ok := cache.closureRoots[proj.ID]; ok {
			return roots
		}
		roots := cachekey.ProjectClosureRoots(ws, proj)
		cache.closureRoots[proj.ID] = roots
		return roots
	}
	if cache != nil && proj != nil {
		scope.changedFiles = cache.ownedChangedFiles[proj.ID]
	}
	opts := extension.PipelineExpansionOptions{
		StepActive: func(step extension.PipelineStep) bool {
			return stepIsActive(step.Activation, scope)
		},
		// A project that owns no changed file binds none: the task receives
		// no parameter rather than an empty list.
		ChangedFiles: scope.changedFiles,
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
//
// The answer of a gate that reads the tree is decided once per plan for a
// project, and kept between runs with the witnesses of what it read (CLI ADR
// 0132): while every recorded stat holds, the next run answers it with a stat
// per path instead of a walk and a read per file.
func stepIsActive(act *extension.StepActivation, scope activationScope) bool {
	if act == nil {
		return true
	}
	// A change gate reads the diff, not the tree, so it needs no project root
	// and holds before the fallback below: a step that does work only for a
	// change is planned only where the project owns a matching changed file,
	// and never on a run that measured no diff.
	if len(act.ChangedFiles) > 0 && !extension.SelectsChangedFile(scope.changedFiles, act.ChangedFiles) {
		return false
	}
	// Without a resolvable project root we cannot probe files; keep the step so
	// it skips at runtime exactly as before rather than risk over-pruning.
	if scope.projRoot == "" {
		return true
	}
	key, witnessed := activationKey(act, scope)
	if scope.cache != nil {
		if v, ok := scope.cache.activation[key]; ok {
			return v
		}
	}
	var probe *workspace.ActivationProbe
	active, held := false, false
	if witnessed {
		active, held = workspace.HeldActivation(scope.wsRoot, key)
		if !held {
			probe = workspace.NewActivationProbe(scope.wsRoot)
		}
	}
	if !held {
		active = treeGateHolds(act, scope, probe)
		if witnessed {
			workspace.RecordActivation(scope.wsRoot, key, active, probe)
		}
	}
	if scope.cache != nil {
		scope.cache.activation[key] = active
	}
	return active
}

// activationKey names a gate's tree question for a project: its project, the
// gate without its change condition, and the facts outside the tree it
// depends on (the project's type, its closure, the workspace's project
// roots). witnessed is false when the gate also reads the diff below its top
// level, which no witness answers for.
func activationKey(act *extension.StepActivation, scope activationScope) (string, bool) {
	gate := *act
	gate.ChangedFiles = nil
	encoded, err := json.Marshal(gate)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(scope.wsRoot, scope.projRoot)
	if err != nil || scope.wsRoot == "" {
		rel = scope.projRoot
	}
	var b strings.Builder
	b.WriteString(filepath.ToSlash(rel))
	b.WriteByte(0)
	b.Write(encoded)
	// reads reports whether the gate, or with nested only one of its
	// alternatives at any depth, has the condition has selects.
	var reads func(g *extension.StepActivation, nested bool, has func(*extension.StepActivation) bool) bool
	reads = func(g *extension.StepActivation, nested bool, has func(*extension.StepActivation) bool) bool {
		if !nested && has(g) {
			return true
		}
		for i := range g.AnyOf {
			if reads(&g.AnyOf[i], false, has) {
				return true
			}
		}
		return false
	}
	if reads(act, false, func(g *extension.StepActivation) bool { return len(g.ProjectTypes) > 0 }) {
		b.WriteString("\x00type=")
		b.WriteString(scope.projectType)
	}
	if reads(act, false, func(g *extension.StepActivation) bool { return len(g.ClosureFiles) > 0 }) && scope.closureRoots != nil {
		b.WriteString("\x00closure=")
		for _, root := range scope.closureRoots() {
			if r, err := filepath.Rel(scope.wsRoot, root); err == nil {
				root = r
			}
			b.WriteString(filepath.ToSlash(root))
			b.WriteByte(1)
		}
	}
	if scope.cache != nil {
		b.WriteString("\x00projects=")
		b.WriteString(scope.cache.rootsDigest)
	}
	witnessed := scope.wsRoot != "" && !reads(act, true, func(g *extension.StepActivation) bool { return len(g.ChangedFiles) > 0 })
	return b.String(), witnessed
}

// treeGateHolds evaluates every condition of the gate but its top-level change
// condition, recording what it reads into probe, which may be nil. Conditions
// are read in a fixed order, and an alternative's first holding member ends
// the read, so one tree gives one witness.
func treeGateHolds(act *extension.StepActivation, scope activationScope, probe *workspace.ActivationProbe) bool {
	if len(act.ProjectTypes) > 0 && !slices.Contains(act.ProjectTypes, scope.projectType) {
		return false
	}
	var projectRoots map[string]bool
	if scope.cache != nil {
		projectRoots = scope.cache.projectRoots
	}
	if len(act.Files) > 0 && !workspace.HasActivationFilesProbed(scope.projRoot, act.Files, projectRoots, probe) {
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
			if workspace.HasActivationFilesProbed(root, act.ClosureFiles, projectRoots, probe) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Iterate content gates in sorted order for deterministic probing.
	// A missing or unreadable file reports false, so activation fails closed.
	for _, path := range slices.Sorted(maps.Keys(act.Contains)) {
		full := filepath.Join(scope.projRoot, path)
		probe.File(full)
		data, err := fileindex.ReadFile(full)
		if err != nil || !bytes.Contains(data, []byte(act.Contains[path])) {
			return false
		}
	}
	// An alternative's change condition reads the diff like the top-level one.
	if len(act.AnyOf) > 0 {
		for i := range act.AnyOf {
			alt := &act.AnyOf[i]
			if len(alt.ChangedFiles) > 0 && !extension.SelectsChangedFile(scope.changedFiles, alt.ChangedFiles) {
				continue
			}
			if treeGateHolds(alt, scope, probe) {
				return true
			}
		}
		return false
	}
	return true
}
