// Package ci implements the impact-plan and change-plan commands. Both plan
// one checked-out commit range through the same impacted planner; a ChangePlan
// is the protocol projection of the ImpactPlan for the repository's origin.
package ci

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/changeplan"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// PlanOptions are the command-specific inputs of impact-plan and change-plan
// after CLI parsing.
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
	Versions jobs.RunVersions
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
	const command = "impact-plan"
	baseSHA, headSHA, err := resolvePlanRange(wsRoot, command, opts)
	if err != nil {
		return ciproto.ImpactPlan{}, err
	}
	input, err := planRange(ctx, wsRoot, command, baseSHA, headSHA, opts, planner)
	if err != nil {
		return ciproto.ImpactPlan{}, err
	}
	return changeplan.BuildImpact(input)
}

// EmitChangePlan builds one immutable ChangePlan for the exact base-to-HEAD
// delta: the ImpactPlan EmitImpactPlan would build with the same planner,
// projected for the origin remote. It requires the same checkout as
// EmitImpactPlan and a credential-free origin URL, checked before planning.
func EmitChangePlan(ctx context.Context, wsRoot string, opts PlanOptions, planner Planner) (ciproto.ChangePlan, error) {
	const command = "change-plan"
	baseSHA, headSHA, err := resolvePlanRange(wsRoot, command, opts)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	repository, err := changePlanRepository(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	input, err := planRange(ctx, wsRoot, command, baseSHA, headSHA, opts, planner)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	return changeplan.Build(input, repository)
}

// resolvePlanRange resolves the base and head of a plan to full commit IDs. It
// refuses a missing --base, a dirty worktree, a head other than the checked-out
// HEAD, and a base that is not its ancestor. command names the refusing
// command.
func resolvePlanRange(wsRoot, command string, opts PlanOptions) (baseSHA, headSHA string, err error) {
	if strings.TrimSpace(opts.Base) == "" {
		return "", "", cmderr.Usagef("%s requires --base <commit>", command)
	}
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("%s cannot inspect the worktree: %v", command, err)
	}
	if !clean {
		return "", "", cmderr.InvalidConfigf("%s requires a clean worktree", command)
	}

	headRef := strings.TrimSpace(opts.Head)
	if headRef == "" {
		headRef = "HEAD"
	}
	headSHA, err = git.ResolveCommit(wsRoot, headRef)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("%s cannot resolve --head: %v", command, err)
	}
	checkedOutSHA, err := git.HeadSHA(wsRoot)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("%s cannot resolve checked-out HEAD: %v", command, err)
	}
	if headSHA != checkedOutSHA {
		return "", "", cmderr.InvalidConfigf("%s --head must resolve to the checked-out HEAD", command)
	}
	baseSHA, err = git.ResolveCommit(wsRoot, opts.Base)
	if err != nil {
		return "", "", cmderr.InvalidConfigf("%s cannot resolve --base: %v", command, err)
	}
	if !git.CommitReachableFromHead(wsRoot, baseSHA) {
		return "", "", cmderr.InvalidConfigf("%s --base must be an ancestor of the checked-out HEAD", command)
	}
	return baseSHA, headSHA, nil
}

// planRange diffs a resolved range, plans it, proves that planning left the
// checked-out source state unchanged, and returns the planner data a plan
// projects. command names the refusing command.
func planRange(ctx context.Context, wsRoot, command, baseSHA, headSHA string, opts PlanOptions, planner Planner) (changeplan.Input, error) {
	changedFiles, err := git.DiffCommitFiles(wsRoot, baseSHA, headSHA)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s cannot diff revisions: %v", command, err)
	}

	if planner == nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s planner is not configured", command)
	}
	result, err := planner(ctx, baseSHA, opts.NoCache)
	if err != nil {
		return changeplan.Input{}, err
	}
	if !result.Complete {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s could not produce a complete %s plan",
			command, strings.Join(result.Commands, ","))
	}

	// Engine hooks and extension discovery are allowed to run before planning;
	// prove afterwards that none changed the revision-bound source state.
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s cannot recheck the worktree: %v", command, err)
	}
	currentHead, err := git.HeadSHA(wsRoot)
	if err != nil {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s cannot recheck HEAD: %v", command, err)
	}
	if !clean || currentHead != headSHA {
		return changeplan.Input{}, cmderr.InvalidConfigf("%s planning changed the checked-out source state", command)
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return changeplan.Input{}, fmt.Errorf("load workspace for %s: %w", command, err)
	}
	return changeplan.Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: opts.CLIVersion},
		Commands:         result.Commands,
		BaseSHA:          baseSHA,
		HeadSHA:          headSHA,
		ChangedFiles:     changedFiles,
		DirectProjects:   changePlanDirectProjects(ws, changedFiles),
		ImpactedProjects: result.Projects,
		Planned:          result.Jobs,
		Cache:            buildChangePlanCache(ws, result.Jobs, result.Versions, opts.NoCache),
	}, nil
}

func changePlanDirectProjects(ws *workspace.Workspace, paths []string) []*workspace.Project {
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

func buildChangePlanCache(ws *workspace.Workspace, planned []*jobs.ScheduledJob, versions jobs.RunVersions, noCache bool) ciproto.ChangePlanCache {
	if noCache {
		return ciproto.ChangePlanCache{Status: "disabled"}
	}
	if !jobs.CacheKeysRecomputableAtRevision(ws, planned, false) {
		return ciproto.ChangePlanCache{Status: "unavailable", Reason: "ambient-input"}
	}
	cache := store.NewCacheManager(store.NewLocalStore(store.ResolveStoreRoot(ws.Root)))
	keys, err := jobs.PrecomputeKeys(ws, planned, nil, versions, cache, jobs.CacheBypass{})
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
		present, err := jobs.CacheEntryPresent(cache, job, key)
		if err != nil {
			return ciproto.ChangePlanCache{Status: "unavailable", Reason: "presence-lookup-failed"}
		}
		entries = append(entries, ciproto.ChangePlanCacheEntry{TaskKey: taskKey, Key: key, Present: present})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TaskKey < entries[j].TaskKey })
	return ciproto.ChangePlanCache{Status: "available", Entries: entries}
}

func changePlanRepository(wsRoot string) (ciproto.ChangePlanRepository, error) {
	const remote = "origin"
	raw := git.RemoteURL(wsRoot, remote)
	url, ok := credentialFreeRepositoryURL(raw)
	if !ok {
		return ciproto.ChangePlanRepository{}, cmderr.InvalidConfigf("change-plan requires a credential-free %s remote URL", remote)
	}
	return ciproto.ChangePlanRepository{Remote: remote, URL: url}, nil
}

// credentialFreeRepositoryURL normalizes supported git remote spellings to a
// URL with no user info, query, or fragment. Rejecting unknown local paths is
// intentional: Cloud admission needs a portable repository identity, not a
// machine-specific checkout path.
func credentialFreeRepositoryURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if before, after, found := strings.Cut(raw, "@"); found && !strings.Contains(before, "://") {
		if host, path, found := strings.Cut(after, ":"); found && host != "" && path != "" {
			return "ssh://" + host + "/" + strings.TrimPrefix(path, "/"), true
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http" && parsed.Scheme != "ssh") {
		return "", false
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return strings.TrimSuffix(parsed.String(), "/"), true
}

// RenderChangePlan writes a full ResultV2 document for CI callers and a short
// digest-oriented confirmation for people. Structured output is deliberately a
// single envelope so Cloud can validate Document.data with no stdout parsing.
func RenderChangePlan(outputFormat string, document ciproto.ChangePlan) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("change-plan", document, nil))
		return err
	}
	iox.Fprintf(os.Stdout, "\n  ChangePlan %s\n", document.Digest)
	iox.Fprintf(os.Stdout, "    %s..%s (%d changed file(s), %d impacted project(s), %d task(s))\n\n",
		shared.ShortSHA(document.BaseSHA), shared.ShortSHA(document.HeadSHA), len(document.ChangedFiles), len(document.Impact.Projects), len(document.Tasks))
	return nil
}

// RenderImpactPlan writes a full ResultV2 document for extensions and CI
// callers, with the ImpactPlan as its data, and a short summary for people.
// Structured output is a single envelope, like RenderChangePlan's.
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
