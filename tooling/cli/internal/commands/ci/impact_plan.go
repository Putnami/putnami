// Package ci implements the impact-plan command. It plans one checked-out
// commit range through the impacted planner and emits the ImpactPlan; a
// consumer that needs a ChangePlan projects one with
// ciproto.ChangePlanFromImpactPlan.
package ci

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/changeplan"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/inputresolve"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// PlanOptions are the command-specific inputs of impact-plan after CLI
// parsing.
type PlanOptions struct {
	Base       string
	Head       string
	NoCache    bool
	CLIVersion string
}

// PlannerResult is the engine-shaped information a plan needs after the CLI
// adapter has planned the exact impacted job set. It deliberately exposes no
// engine request or invocation details: the command document can serialize
// project and task metadata, but not the mechanics used to plan it. Commands
// is the command list the planner planned, in its order, which is the list
// the plan and every diagnostic about it name.
type PlannerResult struct {
	Commands []string
	Complete bool
	Projects []*workspace.Project
	Jobs     []*jobs.ScheduledJob
	Versions cachekey.RunVersions
}

// Planner is supplied by the CLI adapter, which is the sole owner of
// Engine.Run and of the command list it plans. An error it returns names that
// command list. Keeping this callback small lets commands remain engine-free,
// so engine tests can exercise command-provided preflight seams without a
// cycle.
type Planner func(ctx context.Context, baseSHA string, noCache bool) (PlannerResult, error)

// EmitImpactPlan builds the impacted plan of the exact base-to-HEAD delta for
// the command list planner plans. The head must be the current checkout and
// the worktree must be clean: otherwise task planning can observe source bytes
// that the emitted revisions do not identify. It reads no remote: the
// repository identity a ChangePlan adds is the consumer's input to
// ciproto.ChangePlanFromImpactPlan.
func EmitImpactPlan(ctx context.Context, wsRoot string, opts PlanOptions, planner Planner) (ciproto.ImpactPlan, error) {
	baseSHA, headSHA, err := resolvePlanRange(wsRoot, opts)
	if err != nil {
		return ciproto.ImpactPlan{}, err
	}
	input, err := planRange(ctx, wsRoot, baseSHA, headSHA, opts, planner)
	if err != nil {
		return ciproto.ImpactPlan{}, err
	}
	return changeplan.BuildImpact(input)
}

// resolvePlanRange resolves the base and head of a plan to full commit IDs. It
// refuses a missing --base, a dirty worktree, a head other than the checked-out
// HEAD, and a base that is not its ancestor.
func resolvePlanRange(wsRoot string, opts PlanOptions) (baseSHA, headSHA string, err error) {
	if strings.TrimSpace(opts.Base) == "" {
		return "", "", cmderr.Usagef("impact-plan requires --base <commit>")
	}
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("impact-plan cannot inspect the worktree: %v", err)
	}
	if !clean {
		return "", "", cmderr.InvalidConfigf("impact-plan requires a clean worktree")
	}

	headRef := strings.TrimSpace(opts.Head)
	if headRef == "" {
		headRef = "HEAD"
	}
	headSHA, err = git.ResolveCommit(wsRoot, headRef)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("impact-plan cannot resolve --head: %v", err)
	}
	checkedOutSHA, err := git.HeadSHA(wsRoot)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("impact-plan cannot resolve checked-out HEAD: %v", err)
	}
	if headSHA != checkedOutSHA {
		return "", "", cmderr.InvalidConfigf("impact-plan --head must resolve to the checked-out HEAD")
	}
	baseSHA, err = git.ResolveCommit(wsRoot, opts.Base)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("impact-plan cannot resolve --base: %v", err)
	}
	if !git.CommitReachableFromHead(wsRoot, baseSHA) {
		return "", "", cmderr.InvalidConfigf("impact-plan --base must be an ancestor of the checked-out HEAD")
	}
	return baseSHA, headSHA, nil
}

// planRange diffs a resolved range, plans it, proves that planning left the
// checked-out source state unchanged, and returns the planner data a plan
// projects.
func planRange(ctx context.Context, wsRoot, baseSHA, headSHA string, opts PlanOptions, planner Planner) (changeplan.Input, error) {
	changedFiles, err := git.DiffCommitFiles(wsRoot, baseSHA, headSHA)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan cannot diff revisions: %v", err)
	}

	if planner == nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan planner is not configured")
	}
	result, err := planner(ctx, baseSHA, opts.NoCache)
	if err != nil {
		return changeplan.Input{}, err
	}
	if !result.Complete {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan could not produce a complete %s plan",
			strings.Join(result.Commands, ","))
	}

	// Engine hooks and extension discovery are allowed to run before planning;
	// prove afterwards that none changed the revision-bound source state.
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan cannot recheck the worktree: %v", err)
	}
	currentHead, err := git.HeadSHA(wsRoot)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan cannot recheck HEAD: %v", err)
	}
	if !clean || currentHead != headSHA {
		return changeplan.Input{}, cmderr.InvalidConfigf("impact-plan planning changed the checked-out source state")
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return changeplan.Input{}, fmt.Errorf("load workspace for impact-plan: %w", err)
	}
	return changeplan.Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: opts.CLIVersion},
		Commands:         result.Commands,
		BaseSHA:          baseSHA,
		HeadSHA:          headSHA,
		ChangedFiles:     changedFiles,
		DirectProjects:   planDirectProjects(ws, changedFiles),
		ImpactedProjects: result.Projects,
		Planned:          result.Jobs,
		Cache:            buildPlanCache(ctx, ws, result.Jobs, result.Versions, opts.NoCache),
	}, nil
}

func planDirectProjects(ws *workspace.Workspace, paths []string) []*workspace.Project {
	byID := make(map[string]*workspace.Project)
	for _, path := range paths {
		for _, project := range workspace.ProjectOwnersForPath(ws, path) {
			if project != nil && project.ID != "" {
				byID[project.ID] = project
			}
		}
	}
	projects := make([]*workspace.Project, 0, len(byID))
	for _, project := range byID {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects
}

func buildPlanCache(ctx context.Context, ws *workspace.Workspace, planned []*jobs.ScheduledJob, versions cachekey.RunVersions, noCache bool) ciproto.ChangePlanCache {
	if noCache {
		return ciproto.ChangePlanCache{Status: "disabled"}
	}
	if !cachekey.CacheKeysRecomputableAtRevision(ws, planned, false) {
		return ciproto.ChangePlanCache{Status: "unavailable", Reason: "ambient-input"}
	}
	cache := store.NewCacheManager(store.NewLocalStore(store.ResolveStoreRoot(ws.Root)))
	// The keys expand their input selectors through the extensions the plan's
	// jobs run under, as the run's keys do.
	var extensions []*extension.ExtensionDescription
	for _, job := range planned {
		if job != nil && job.Extension != nil {
			extensions = append(extensions, job.Extension)
		}
	}
	cache.SetInputResolver(inputresolve.New(ctx, ws, extensions, nil))
	keys, err := cachekey.PrecomputeKeys(ws, planned, nil, versions, cache, cachekey.CacheBypass{})
	if err != nil {
		return ciproto.ChangePlanCache{Status: "unavailable", Reason: "key-computation-failed"}
	}
	byKey := make(map[string]*jobs.ScheduledJob, len(planned))
	for _, job := range planned {
		if job != nil {
			byKey[job.Key()] = job
		}
	}
	entries := make([]ciproto.ChangePlanCacheEntry, 0, len(keys))
	for taskKey, key := range keys {
		job := byKey[taskKey]
		present, err := cachekey.CacheEntryPresent(cache, job, key)
		if err != nil {
			return ciproto.ChangePlanCache{Status: "unavailable", Reason: "presence-lookup-failed"}
		}
		entries = append(entries, ciproto.ChangePlanCacheEntry{TaskKey: taskKey, Key: key, Present: present})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TaskKey < entries[j].TaskKey })
	return ciproto.ChangePlanCache{Status: "available", Entries: entries}
}

// RenderImpactPlan writes a full ResultV2 document for extensions and CI
// callers, with the ImpactPlan as its data, and a short summary for people.
// Structured output is a single envelope, so a consumer validates
// Document.data with no stdout parsing.
func RenderImpactPlan(outputFormat string, document ciproto.ImpactPlan) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("impact-plan", document, nil))
		return err
	}
	iox.Fprintf(os.Stdout, "\n  ImpactPlan %s\n", strings.Join(document.Commands, ","))
	iox.Fprintf(os.Stdout, "    %s..%s (%d changed file(s), %d impacted project(s), %d task(s))\n\n",
		shared.ShortSHA(document.BaseSHA), shared.ShortSHA(document.HeadSHA), len(document.ChangedFiles), len(document.Impact.Projects), len(document.Tasks))
	return nil
}
