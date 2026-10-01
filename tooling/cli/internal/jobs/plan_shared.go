package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Plan-level SHARED EXECUTIONS: one physical run serving several commands.
//
// `putnami lint,test,build` schedules `build~describe`
// and `test~describe` as separate DAG nodes over the SAME manifest task, the
// SAME project and the same declared inputs. Their outputs are byte-identical
// across an 11-project set, and the duplication costs ~8% of a clean run's
// physical time. The cache
// cannot collapse it: a task-owned entry is addressed partly by the JOB NAME
// (`cacheTaskName`), so `test~describe` can never reach `build~describe`'s
// entry no matter how identical the work is.
//
// The epic settled the mechanism (refinement decision F3): a plan-level SHARED
// NODE rather than cache-key normalization. Normalizing the key would have
// moved every existing entry's address, merged two commands' cache identities
// permanently, and made the saving invisible to the ledger. A shared node keeps
// BOTH logical nodes — their keys, their entries, their session rows, their
// dependents' keys — and only removes the second PHYSICAL execution.
//
// This file is the plan half: it decides WHICH nodes are one shared execution
// and stamps them with a shared id. scheduler_shared.go is the runtime half: it
// makes exactly one member execute and hands its result to the others.
//
// Deliberately NOT stamped here (see the audit's blocker):
//
//   - `build-generate`. It declares the project `gen` subtree as its output and
//     its own manifest records that describe, config-merge and config-extract
//     all write inside that subtree without owning it — and on TypeScript
//     applications `test~test` rewrites `.gen/schema/capabilities.json` after
//     generate finished, deleting it outright when the suite fails. A shared
//     node captures each member's entry from the tree as it stands when that
//     member is served, so a subtree other tasks write into cannot be shared
//     until that ownership violation is fixed. `writesProjectGen` is the gate.
//   - Any step carrying literal `with:` bindings (`test`'s `mode: "test"`,
//     `serve`/`run`'s `mode: "serve"`). Those bindings are inert today — the
//     planner never merges them into task params — but a manifest that DECLARES
//     a difference must not be silently unified on the strength of an
//     implementation gap that a later slice is expected to close.
//
// In this workspace the predicate below therefore admits exactly one task, the
// Go `build-describe`, which is the (a) payload the audit authorized.
//
// ONE RESIDUAL, measured and bounded, from the same `.gen` ownership violation.
// `build-describe`'s DECLARED output is the generated client tree, and nothing
// else in a plan writes it — under the declarations, sharing it is exact. It
// also writes UNDECLARED into `.gen` (its own manifest says so), and one of
// those writes is a merge into `.gen/generate-result.json` that adds the specs
// it promoted to that manifest's `exports` and `schemas`. In a multi-command
// invocation the SECOND command's generate rewrites that manifest afterwards,
// so those merged entries survive only when the LAST describe of the invocation
// physically executes — which a shared node makes false, exactly as a cache hit
// on that last describe already made it false. Measured on
// /go/samples/simple-api: `putnami build --no-cache` is unchanged (3 exports, 6
// schemas) before and after; `putnami lint,test,build --no-cache` goes from 3/6
// to 1/1, which is what the same command already produced on any run with a
// populated cache. Nothing reads the manifest after the run — describe is its
// only reader, before it writes — and the committed `schema/*.json` sidecars,
// the `.gen/schema/*.json` copies and the worktree's git status are identical
// either way. The proper fix is the ownership violation itself, which the W2a
// audit filed as its own issue; it is not something a shared node can repair,
// because the only ordering that would preserve the merge (execute at the LAST
// member's position) is unreachable — the first command's compile depends on
// its describe, and the second command's generate is serialized after that
// compile, so a node running at the last member's position would sit on a
// cycle.

// sharedExecutionPrefix is the readable id every member of one shared node
// carries. The number is positional within a plan, assigned in deterministic
// group order, so two Plan calls over one workspace produce the same ids.
const sharedExecutionPrefix = "shared-"

// attachSharedExecutions groups the plan's content-identical nodes and stamps
// each group's members with a shared execution id.
//
// It runs after every edge is resolved and after the invocation relations are
// attached (a node that participates in one is never shared), and BEFORE
// attachIdentities, so a node's typed identity is stamped on the same value the
// scheduler later reads. It adds no edge and rewrites no dependency: the DAG,
// every cache key and every artifact owner are exactly what they were without
// it. The only thing it changes is how many subprocesses the scheduler spawns.
func attachSharedExecutions(ws *workspace.Workspace, jobs []*ScheduledJob, commandParams map[string]any) {
	if len(jobs) < 2 {
		return
	}

	producers := newSharedProducerIdentityResolver(ws, jobs, commandParams)
	byIdentity := make(map[string][]*ScheduledJob)
	for _, job := range jobs {
		identity, ok := sharedExecutionIdentity(ws, job, commandParams, producers)
		if !ok {
			continue
		}
		byIdentity[identity] = append(byIdentity[identity], job)
	}
	if len(byIdentity) == 0 {
		return
	}

	identities := make([]string, 0, len(byIdentity))
	for identity, members := range byIdentity {
		// A group of one is not a shared node: nothing is saved and the runtime
		// would take a slot for a member that has no peer.
		if len(members) < 2 {
			continue
		}
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	for i, identity := range identities {
		id := sharedExecutionPrefix + strconv.Itoa(i+1)
		for _, job := range byIdentity[identity] {
			job.SharedExecutionID = id
		}
	}
}

// sharedExecutionIdentity returns the value every member of one shared node must
// agree on, and whether this node may join one at all.
//
// The identity is deliberately built from DECLARED facts — the manifest task,
// its contract digest, the file and env patterns the project adds for this
// command, the declared params, the resolved deadline — and never from file
// CONTENT. Two nodes that agree on all of it run the same tool over the same
// project with the same inputs and the same recursively resolved producer
// identities. That producer fold is what prevents two downstream nodes from
// being unified after behaviorally different upstream steps while still
// letting equivalent command-qualified producer chains compare equal.
//
// It intentionally EXCLUDES the command name, which is the only thing the two
// nodes disagree about and the only reason the cache cannot serve one from the
// other. It is not a cache key and never becomes one: each node keeps its own.
func sharedExecutionIdentity(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	producers *sharedProducerIdentityResolver,
) (string, bool) {
	if !sharedExecutionEligible(job) {
		return "", false
	}

	upstream, ok := producers.upstreamIdentity(job)
	if !ok {
		// A missing producer or a cycle cannot occur after Plan's DAG gate, but
		// shared execution is an optimization: fail closed if a hand-built plan
		// or a future call order violates that assumption.
		return "", false
	}

	parts := executionIdentityParts(ws, job, commandParams)
	parts = append(parts, "upstream="+upstream)
	return sharedIdentityDigest(parts), true
}

// executionIdentityParts describes one task from declared and resolved plan
// facts, deliberately excluding the command name. It is shared by the node
// identity and the recursive producer identity so both answer "same work?"
// with one vocabulary.
func executionIdentityParts(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
) []string {
	// Non-nil for directly shareable/comparable tasks. Keep the local guard so
	// this helper remains fail-safe when exercised by hand-built test plans.
	key := &extension.TaskCacheKey{}
	if job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.Key != nil {
		key = job.JobDef.TaskCachePolicy.Key
	}
	env := make([]string, 0, len(job.JobDef.Env))
	for name, value := range job.JobDef.Env {
		env = append(env, name+"="+value)
	}

	return []string{
		"project=" + job.Project.ID,
		"extension=" + job.Extension.Name + "@" + job.Extension.Version,
		"task=" + job.Step.Task,
		"contract=" + job.JobDef.ContractDigest,
		"toolchain=" + cacheToolchainVersion(job),
		"command=" + job.JobDef.Command,
		"args=" + strings.Join(job.JobDef.Args, "\x1f"),
		"cwd=" + job.JobDef.Cwd,
		"env=" + canonicalStrings(env),
		"sideEffects=" + job.JobDef.Traits.SideEffects,
		"timeout=" + strconv.Itoa(job.EffectiveTimeoutMs()),
		"files=" + canonicalStrings(append(append([]string(nil), job.JobDef.FilePatterns...), projectFilePatterns(ws, job)...)),
		"envInputs=" + canonicalStrings(projectEnvInputs(ws, job)),
		"keyEnv=" + canonicalStrings(key.Env),
		"runtime=" + canonicalStrings(taskRuntimeIdentity(job)),
		"closure=" + canonicalStrings(key.ClosureFiles),
		"assets=" + canonicalStrings(generateAssetFiles(ws, job)),
		"writes=" + canonicalResourceRefs(job.Project.ID, job.JobDef.Writes),
		"reads=" + canonicalResourceRefs(job.Project.ID, job.JobDef.Reads),
		"selected=" + canonicalStrings(selectedProjectIDs(job.SelectedProjects)),
		"params=" + canonicalDeclaredParams(ws, job, commandParams),
	}
}

func sharedIdentityDigest(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// sharedProducerIdentityResolver gives every functional predecessor a stable
// behavioral identity. Raw dependency keys are insufficient: equivalent
// build/test producers have different command-qualified keys, while two
// producers with different literal bindings can execute the same manifest task
// under superficially identical downstream nodes.
type sharedProducerIdentityResolver struct {
	ws            *workspace.Workspace
	commandParams map[string]any
	jobsByKey     map[string]*ScheduledJob
	memo          map[string]string
	visiting      map[string]bool
}

func newSharedProducerIdentityResolver(
	ws *workspace.Workspace,
	jobs []*ScheduledJob,
	commandParams map[string]any,
) *sharedProducerIdentityResolver {
	return &sharedProducerIdentityResolver{
		ws:            ws,
		commandParams: commandParams,
		jobsByKey:     jobsByPlanKey(jobs),
		memo:          make(map[string]string),
		visiting:      make(map[string]bool),
	}
}

// upstreamIdentity returns an order-independent identity for the job's direct
// functional producers. Each producer recursively carries its own producers,
// so a behavioral difference anywhere in the dependency chain reaches the
// candidate shared node.
func (r *sharedProducerIdentityResolver) upstreamIdentity(job *ScheduledJob) (string, bool) {
	if r == nil || job == nil || len(job.DependsOn) == 0 {
		return "", r != nil && job != nil
	}

	depKeys := dedupeSorted(append([]string(nil), job.DependsOn...))
	identities := make([]string, 0, len(depKeys))
	for _, depKey := range depKeys {
		producer := r.jobsByKey[depKey]
		if producer == nil {
			return "", false
		}
		identity, ok := r.identity(producer)
		if !ok {
			return "", false
		}
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	return strings.Join(identities, "\x1f"), true
}

func (r *sharedProducerIdentityResolver) identity(job *ScheduledJob) (string, bool) {
	if job == nil || job.Project == nil || job.JobDef == nil {
		return "", false
	}
	key := job.Key()
	if identity, ok := r.memo[key]; ok {
		return identity, true
	}
	if r.visiting[key] {
		// Plan normally rejects cycles before this resolver runs. Keep the
		// optimization fail-closed for direct unit use and future call sites.
		return "", false
	}

	// An incompletely declared or nondeterministic producer cannot be equated
	// with another command's node from plan facts. Its exact plan key remains a
	// safe identity: two consumers may still share when they depend on the same
	// physical producer, but never when they depend on two merely similar ones.
	if !sharedProducerComparable(job) {
		identity := sharedIdentityDigest([]string{"opaque=" + key})
		r.memo[key] = identity
		return identity, true
	}

	r.visiting[key] = true
	defer delete(r.visiting, key)
	upstream, ok := r.upstreamIdentity(job)
	if !ok {
		return "", false
	}
	parts := executionIdentityParts(r.ws, job, r.commandParams)
	parts = append(parts, "upstream="+upstream)
	identity := sharedIdentityDigest(parts)
	r.memo[key] = identity
	return identity, true
}

// sharedProducerComparable is narrower than "has a task": cross-command
// equivalence needs a complete declared input set and a determinism promise.
// Literal bindings are comparable because BoundParams carries their resolved
// value. Context/result bindings are not materialized into that bag, so they
// remain opaque and fail closed.
func sharedProducerComparable(job *ScheduledJob) bool {
	if job == nil || job.Project == nil || job.Extension == nil || job.JobDef == nil || job.Step == nil {
		return false
	}
	if job.Step.Task == "" || job.JobDef.ContractDigest == "" ||
		job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil ||
		!taskIsDeterministic(job) || isFinalizerJob(job) ||
		job.InvocationProducer != nil || job.Invocation != nil {
		return false
	}
	for _, binding := range job.Step.With {
		if !binding.HasValue {
			return false
		}
	}
	return true
}

// sharedExecutionEligible is the conservative gate on the mechanism. Every
// condition removes a way one physical run could fail to be what the other node
// would have produced.
func sharedExecutionEligible(job *ScheduledJob) bool {
	if job == nil || job.Project == nil || job.Extension == nil || job.JobDef == nil || job.Step == nil {
		return false
	}
	// A shared node is defined by the MANIFEST TASK two commands schedule. A job
	// with no task (a non-pipeline command job) has no such identity.
	if job.Step.Task == "" || job.JobDef.ContractDigest == "" {
		return false
	}
	// A v3 task contract is what makes "the same declared inputs" a complete
	// statement: without declared inputs the task may read anything, and two
	// commands hand it different parameter bags.
	if job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil {
		return false
	}
	// Input-bound steps remain outside direct sharing. Literal values are live
	// task params and producer identities account for them, but context/result
	// bindings are not reducible to one plan-time value. Keeping one conservative
	// gate avoids two subtly different sharing rules for the union.
	if len(job.Step.With) > 0 {
		return false
	}
	// `cache.deterministic` is the manifest's own statement that identical
	// inputs produce identical outputs — precisely the claim a shared node makes
	// on the task's behalf.
	if !taskIsDeterministic(job) {
		return false
	}
	// A task that declares no write resource has side effects the planner cannot
	// serialize and this mechanism cannot reason about: sharing it would move an
	// undeclared write to a different point of the run.
	if len(job.JobDef.Writes) == 0 {
		return false
	}
	// The project `gen` subtree is written by tasks that do not own it (the
	// manifest says so, and the W2a audit measured the TypeScript instance), so a
	// capture of it taken on another node's behalf is not that node's output.
	if writesProjectGen(job) {
		return false
	}
	// A finalizer is never dispatched by the DAG, and a participant in an
	// invocation relation is keyed on the producing ACTION — neither shape has a
	// second node it could be identical to.
	if isFinalizerJob(job) || job.InvocationProducer != nil || job.Invocation != nil {
		return false
	}
	return true
}

// canonicalDeclaredParams renders the values of the params the TASK declared as
// inputs plus literal params the STEP binds, resolved through the same layer
// merge the task's own context uses.
//
// Only CONTRACT-declared names and explicit STEP literals participate.
// taskCacheParams additionally projects the owning command's flags as a
// fail-safe for tasks whose contracts are incomplete, and that fail-safe is
// precisely what a cross-command comparison must not read: `test` declares
// `--enforce-coverage` and `build` does not, so folding command flags in would
// report the CI gate's own describe nodes as different work. A task with a
// complete v3 contract can only read what it declared or what its step binds,
// which is the same premise the contract digest in every cache key rests on.
func canonicalDeclaredParams(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
) string {
	namesSet := make(map[string]struct{})
	if policy := job.JobDef.TaskCachePolicy; policy != nil && policy.Key != nil {
		for _, name := range policy.Key.Params {
			namesSet[name] = struct{}{}
		}
	}
	for name := range job.JobDef.BoundParams {
		namesSet[name] = struct{}{}
	}
	if len(namesSet) == 0 {
		return ""
	}

	var configDefaults map[string]any
	if ws != nil && ws.Config != nil {
		configDefaults = ws.Config.GetCommandDefaults(jobCommandName(job), job.Extension.Name)
	}
	resolved := resolvedJobParams(job, commandParams, configDefaults)

	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		value, ok := resolved[name]
		if !ok && strings.Contains(name, "-") {
			value, ok = resolved[kebabToCamel(name)]
		}
		if !ok {
			parts = append(parts, name+"=<absent>")
			continue
		}
		// %#v carries the Go TYPE as well as the value, so an int 1 and a string
		// "1" are different declared inputs here exactly as they are in
		// store.hashParams.
		parts = append(parts, fmt.Sprintf("%s=%#v", name, value))
	}
	return strings.Join(parts, "\x1f")
}

// canonicalStrings renders a set of declarations order-independently.
func canonicalStrings(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(dedupeSorted(append([]string(nil), values...)), "\x1f")
}

// canonicalResourceRefs renders declared resource accesses by the conflict key
// the planner serializes them on, so two nodes agree on identity only when they
// contend for exactly the same resources.
func canonicalResourceRefs(projectID string, refs []extension.ResourceRef) string {
	if len(refs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, resourceConflictKey(projectID, ref))
	}
	return canonicalStrings(keys)
}
