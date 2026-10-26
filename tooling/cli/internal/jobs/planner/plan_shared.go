package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Plan-level SHARED EXECUTIONS: one physical run serving several commands.
//
// Two commands can schedule separate DAG nodes over the SAME manifest task, the
// SAME project and the same declared inputs: `putnami test,build` plans the Go
// `build-generate` and `build-describe` once for each command. The cache cannot
// collapse them: a task-owned entry is addressed partly by the JOB NAME
// (`cachekey.CacheTaskName`), so one command's node can never reach the other's
// entry no matter how identical the work is.
//
// A shared node keeps BOTH logical nodes — their keys, their entries, their
// session rows, their dependents' keys — and only removes the second PHYSICAL
// execution. Normalizing the cache key instead would move every existing
// entry's address and hide the saving from the ledger.
//
// This file is the plan half: it decides WHICH nodes are one shared execution,
// stamps them with a shared id and orders them. scheduler_shared.go is the
// runtime half: it makes exactly one member execute and hands its result to the
// others.
//
// A group's first member by key LEADS and the others FOLLOW. A follower that
// adopts its leader's result executes nothing and writes nothing: it captures
// its own entry from the tree its leader left. So the followers run right after
// their leader, one after another, take its place in the write order, and every
// reader in that place waits for them (serializeWriteResources). That is what
// lets a task that writes the project `gen` subtree share: no other declared
// access to the subtree runs between the leader's write and a follower's
// capture.
//
// Deliberately NOT stamped here:
//
//   - Any step carrying a `with:` binding (TypeScript `test`'s `mode: "test"`,
//     `serve`/`run`'s `mode: "serve"`). A literal binding is a live task param,
//     which the producer identity below accounts for, but a context or result
//     binding has no single plan-time value, so one conservative gate keeps
//     every bound step out of direct sharing.
//   - A group whose write another command reads or writes with a write of its
//     own (ordersAnotherCommand). Taking the leader's place would move the
//     follower ahead of that command's node, and the follower's dependents
//     with it.
//
// ONE RESIDUAL, from writes into `.gen` that no task declares. `build-describe`
// also writes UNDECLARED into `.gen`: it merges the specs it promoted into the
// `exports` and `schemas` of `.gen/generate-result.json`. A follower describe
// does not repeat that merge, which a cache hit on that describe does not
// either. Nothing reads the manifest after the run: describe is its only
// reader, before it writes.

// sharedExecutionPrefix is the readable id every member of one shared node
// carries. The number is positional within a plan, assigned in deterministic
// group order, so two Plan calls over one workspace produce the same ids.
const sharedExecutionPrefix = "shared-"

// attachSharedExecutions groups the plan's content-identical nodes, stamps
// each group's members with a shared execution id, and returns the groups,
// each sorted by key.
//
// It runs after every functional edge is resolved, before write serialization
// orders the groups (serializeWithSharedExecutions), and BEFORE
// jobmodel.AttachIdentities, so a node's typed identity is stamped on the same
// value the scheduler later reads. It attaches the invocation relations first:
// a node that participates in one is never shared. It adds no edge and
// rewrites no dependency: every cache key and every artifact owner are exactly
// what they were without it.
func attachSharedExecutions(
	ws *workspace.Workspace,
	jobs []*jobmodel.ScheduledJob,
	commandParams map[string]any,
) [][]*jobmodel.ScheduledJob {
	if len(jobs) < 2 {
		return nil
	}
	ResolveInvocationRelations(jobs)

	producers := newSharedProducerIdentityResolver(ws, jobs, commandParams)
	byIdentity := make(map[string][]*jobmodel.ScheduledJob)
	for _, job := range jobs {
		identity, ok := sharedExecutionIdentity(ws, job, commandParams, producers)
		if !ok {
			continue
		}
		byIdentity[identity] = append(byIdentity[identity], job)
	}
	if len(byIdentity) == 0 {
		return nil
	}

	accessors := writingAccessors(jobs)
	identities := make([]string, 0, len(byIdentity))
	for identity, members := range byIdentity {
		// A group of one is not a shared node: nothing is saved and the runtime
		// would take a slot for a member that has no peer.
		if len(members) < 2 || ordersAnotherCommand(members, accessors) {
			continue
		}
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	groups := make([][]*jobmodel.ScheduledJob, 0, len(identities))
	for i, identity := range identities {
		id := sharedExecutionPrefix + strconv.Itoa(i+1)
		members := byIdentity[identity]
		sort.Slice(members, func(a, b int) bool { return members[a].Key() < members[b].Key() })
		for _, job := range members {
			job.SharedExecutionID = id
		}
		groups = append(groups, members)
	}
	return groups
}

// serializeWithSharedExecutions orders every declared resource access with the
// members of each shared execution group (attachSharedExecutions) as one
// writer.
//
// A group's first member by key LEADS: it runs after every member's ordering
// producers. The other members FOLLOW it, one after another in key order, and
// adopt its result. A follower writes nothing to the tree, so it takes its
// leader's place in the write order, and a reader of a follower's output reads
// what the leader wrote.
//
// Write serialization orders each resource on its own, so these edges can
// close a cycle across two resources that no single group predicts. When the
// ordering cannot be scheduled, the plan keeps no shared execution and is
// serialized as if none had been found: sharing never stops a plan from
// scheduling. The plan then records a plan.shared_executions_dropped warning
// on the workspace, which the run reports with the warnings of its load: the
// results are the same, and the run repeats the work sharing would have saved.
func serializeWithSharedExecutions(ws *workspace.Workspace, jobs []*jobmodel.ScheduledJob, groups [][]*jobmodel.ScheduledJob) {
	saved := make([][]string, len(jobs))
	for i, job := range jobs {
		saved[i] = append([]string(nil), job.SerializeAfter...)
	}
	leaders := orderSharedExecutions(jobs, groups)
	serializeWriteResources(jobs, leaders)
	orderContractClients(jobs, ws)
	if len(groups) == 0 || ValidatePlanDAG(jobs) == nil {
		return
	}
	for i, job := range jobs {
		job.SerializeAfter = saved[i]
		job.SharedExecutionID = ""
	}
	serializeWriteResources(jobs, nil)
	orderContractClients(jobs, ws)
	// A plan that cannot be scheduled without sharing either has a cycle of its
	// own, which Plan reports as an error, so only a plan that sharing broke
	// warns.
	if ValidatePlanDAG(jobs) != nil || ws.HasWarningCode(workspace.WarningCodeSharedExecutionsDropped) {
		return
	}
	ws.AddWarning(workspace.WarningCodeSharedExecutionsDropped, fmt.Sprintf(
		"the plan found %d groups of tasks that several commands could run once, could not order them, and runs each task on its own: the results are the same, and the run repeats that work",
		len(groups)))
}

// orderSharedExecutions adds the edges that make each group's leader run first
// and its followers one after another, in key order, and returns each
// follower's leader. At run time any member can write the outputs: a member
// whose own entry hits restores it, and the first member that misses executes.
// So no two members are left unordered, or one could write the outputs while
// another restores or captures them. Each follower waits for every member
// before it, not only the previous one, so a pass that removes a planned member
// later keeps the others ordered.
func orderSharedExecutions(jobs []*jobmodel.ScheduledJob, groups [][]*jobmodel.ScheduledJob) map[string]string {
	if len(groups) == 0 {
		return nil
	}
	byKey := JobsByPlanKey(jobs)
	leaders := make(map[string]string)
	for _, members := range groups {
		leader := members[0]
		own := make(map[string]bool, len(leader.DependsOn))
		for _, dep := range leader.DependsOn {
			own[dep] = true
		}
		for i, follower := range members[1:] {
			leaders[follower.Key()] = leader.Key()
			for _, earlier := range members[:i+1] {
				follower.SerializeAfter = append(follower.SerializeAfter, earlier.Key())
			}
			follower.SerializeAfter = jobmodel.DedupeSorted(follower.SerializeAfter)
			for _, dep := range follower.DependsOn {
				if producer := byKey[dep]; producer != nil && !own[dep] && orderingProducer(follower, producer) {
					leader.SerializeAfter = jobmodel.DedupeSorted(append(leader.SerializeAfter, dep))
				}
			}
		}
	}
	return leaders
}

// writingAccessors indexes, by conflict key, the nodes that read or write a
// resource and declare a write of their own.
func writingAccessors(jobs []*jobmodel.ScheduledJob) map[string][]*jobmodel.ScheduledJob {
	index := make(map[string][]*jobmodel.ScheduledJob)
	for _, job := range jobs {
		if job.Project == nil || job.JobDef == nil || len(job.JobDef.Writes) == 0 {
			continue
		}
		seen := make(map[string]bool)
		for _, refs := range [][]extension.ResourceRef{job.JobDef.Writes, job.JobDef.Reads} {
			for _, ref := range refs {
				key := ResourceConflictKey(job.Project.ID, ref)
				if !seen[key] {
					seen[key] = true
					index[key] = append(index[key], job)
				}
			}
		}
	}
	return index
}

// ordersAnotherCommand reports whether a node of a command no member belongs
// to reads or writes a resource the members write, and declares a write of its
// own. Without sharing, the later member is a writer that waits for such a
// node, and the member's dependents follow it: what the node writes can reach
// them through files they read, which no resource names. A follower takes its
// leader's earlier place in the write order instead, so the members stay
// apart. The members' own commands are the pipelines whose order a shared
// plan is pinned against.
func ordersAnotherCommand(members []*jobmodel.ScheduledJob, accessors map[string][]*jobmodel.ScheduledJob) bool {
	commands := make(map[string]bool, len(members))
	for _, member := range members {
		commands[member.CommandName()] = true
	}
	leader := members[0]
	for _, ref := range leader.JobDef.Writes {
		for _, other := range accessors[ResourceConflictKey(leader.Project.ID, ref)] {
			if !commands[other.CommandName()] {
				return true
			}
		}
	}
	return false
}

// sharedExecutionIdentity returns the value every member of one shared node must
// agree on, and whether this node may join one at all.
//
// The identity is deliberately built from DECLARED facts — the manifest task,
// its contract digest, the task's own file and env patterns, the declared
// params, the resolved deadline — and never from file CONTENT. Two nodes that
// agree on all of it run the same tool over the same project with the same
// inputs and the same recursively resolved producer identities, ordering
// producers left out (orderingProducer). That producer fold is what prevents
// two downstream nodes from being unified after behaviorally different
// upstream steps while still letting equivalent command-qualified producer
// chains compare equal.
//
// It intentionally EXCLUDES the command name, which is the only thing the two
// nodes disagree about and the only reason the cache cannot serve one from the
// other. It is not a cache key and never becomes one: each node keeps its own.
func sharedExecutionIdentity(
	ws *workspace.Workspace,
	job *jobmodel.ScheduledJob,
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
// with one vocabulary. It leaves out a project's per-command `filePatterns`
// and `envInputs`: they widen every key of one command and reach no task
// process, so two commands' nodes over one tree in one run do the same work.
func executionIdentityParts(
	ws *workspace.Workspace,
	job *jobmodel.ScheduledJob,
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
		"toolchain=" + toolchain.CacheToolchainVersion(job),
		"command=" + job.JobDef.Command,
		"args=" + strings.Join(job.JobDef.Args, "\x1f"),
		"cwd=" + job.JobDef.Cwd,
		"env=" + canonicalStrings(env),
		"sideEffects=" + job.JobDef.Traits.SideEffects,
		"timeout=" + strconv.Itoa(job.EffectiveTimeoutMs()),
		"files=" + canonicalStrings(job.JobDef.FilePatterns),
		"keyEnv=" + canonicalStrings(key.Env),
		"runtime=" + canonicalStrings(cachekey.TaskRuntimeIdentity(job)),
		"closure=" + canonicalStrings(key.ClosureFiles),
		"assets=" + canonicalStrings(cachekey.GenerateAssetFiles(ws, job)),
		"writes=" + canonicalResourceRefs(job.Project.ID, job.JobDef.Writes),
		"reads=" + canonicalResourceRefs(job.Project.ID, job.JobDef.Reads),
		"selected=" + canonicalStrings(cachekey.SelectedProjectIDs(job.SelectedProjects)),
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
	jobsByKey     map[string]*jobmodel.ScheduledJob
	memo          map[string]string
	visiting      map[string]bool
}

func newSharedProducerIdentityResolver(
	ws *workspace.Workspace,
	jobs []*jobmodel.ScheduledJob,
	commandParams map[string]any,
) *sharedProducerIdentityResolver {
	return &sharedProducerIdentityResolver{
		ws:            ws,
		commandParams: commandParams,
		jobsByKey:     JobsByPlanKey(jobs),
		memo:          make(map[string]string),
		visiting:      make(map[string]bool),
	}
}

// upstreamIdentity returns an order-independent identity for the job's direct
// functional producers. Each producer recursively carries its own producers,
// so a behavioral difference anywhere in the dependency chain reaches the
// candidate shared node.
func (r *sharedProducerIdentityResolver) upstreamIdentity(job *jobmodel.ScheduledJob) (string, bool) {
	if r == nil || job == nil || len(job.DependsOn) == 0 {
		return "", r != nil && job != nil
	}

	depKeys := jobmodel.DedupeSorted(append([]string(nil), job.DependsOn...))
	identities := make([]string, 0, len(depKeys))
	for _, depKey := range depKeys {
		producer := r.jobsByKey[depKey]
		if producer == nil {
			return "", false
		}
		if orderingProducer(job, producer) {
			continue
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

func (r *sharedProducerIdentityResolver) identity(job *jobmodel.ScheduledJob) (string, bool) {
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

// orderingProducer reports whether a producer only orders the consumer: it is
// in the consumer's project and declares no write resource. What it leaves in
// the tree reaches the consumer only through files the consumer reads, which
// the consumer's own key reads too. So it is left out of the producer
// identity, and orderSharedExecutions runs a group's leader after every
// member's ordering producers: the leader reads the files each of them left,
// and keys on them.
func orderingProducer(consumer, producer *jobmodel.ScheduledJob) bool {
	if consumer == nil || producer == nil || consumer.Project == nil || producer.Project == nil || producer.JobDef == nil {
		return false
	}
	return producer.Project.ID == consumer.Project.ID &&
		len(producer.JobDef.Writes) == 0 &&
		!jobmodel.IsFinalizerJob(producer) &&
		producer.InvocationProducer == nil && producer.Invocation == nil
}

// sharedProducerComparable is narrower than "has a task": cross-command
// equivalence needs a complete declared input set and a determinism promise.
// Literal bindings are comparable because BoundParams carries their resolved
// value. Context/result bindings are not materialized into that bag, so they
// remain opaque and fail closed.
func sharedProducerComparable(job *jobmodel.ScheduledJob) bool {
	if job == nil || job.Project == nil || job.Extension == nil || job.JobDef == nil || job.Step == nil {
		return false
	}
	if job.Step.Task == "" || job.JobDef.ContractDigest == "" ||
		job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil ||
		!cachekey.TaskIsDeterministic(job) || jobmodel.IsFinalizerJob(job) ||
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
func sharedExecutionEligible(job *jobmodel.ScheduledJob) bool {
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
	if !cachekey.TaskIsDeterministic(job) {
		return false
	}
	// A task that declares no write resource has side effects the planner cannot
	// serialize and this mechanism cannot reason about: sharing it would move an
	// undeclared write to a different point of the run.
	if len(job.JobDef.Writes) == 0 {
		return false
	}
	// A command-output directory resolves per command, so a leader never writes
	// the follower's, and a follower runs nothing, so it never empties it: the
	// follower would capture an earlier run's bytes under a key that says its
	// entry holds only its own run's files (ADR 0064).
	if DeclaresCommandOutput(job) {
		return false
	}
	// A finalizer is never dispatched by the DAG, and a participant in an
	// invocation relation is keyed on the producing ACTION — neither shape has a
	// second node it could be identical to.
	if jobmodel.IsFinalizerJob(job) || job.InvocationProducer != nil || job.Invocation != nil {
		return false
	}
	return true
}

// DeclaresCommandOutput reports whether a task owns bytes under the shared
// per-command directory. The scheduler uses it before a real execution to
// detach a stale cache symlink before the task can mutate it, and
// sharedExecutionEligible uses it to refuse every such task: a shared-node
// follower would capture a command-output directory its leader never wrote.
func DeclaresCommandOutput(job *jobmodel.ScheduledJob) bool {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return false
	}
	for _, output := range declaration.Outputs {
		if output.EffectiveRoot() == extension.OutputRootCommandOutput {
			return true
		}
	}
	return false
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
	job *jobmodel.ScheduledJob,
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
		configDefaults = ws.Config.GetCommandDefaults(jobmodel.JobCommandName(job), job.Extension.Name)
	}
	resolved := cachekey.ResolvedJobParams(job, commandParams, configDefaults)

	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		value, ok := resolved[name]
		if !ok && strings.Contains(name, "-") {
			value, ok = resolved[cachekey.KebabToCamel(name)]
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
	return strings.Join(jobmodel.DedupeSorted(append([]string(nil), values...)), "\x1f")
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
		keys = append(keys, ResourceConflictKey(projectID, ref))
	}
	return canonicalStrings(keys)
}
