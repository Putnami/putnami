// Package ci implements the immutable CI ChangePlan command.
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

// ChangePlanOptions are the command-specific inputs after CLI parsing.
type ChangePlanOptions struct {
	Base       string
	Head       string
	NoCache    bool
	CLIVersion string
}

// ChangePlanPlannerResult is the engine-shaped information a ChangePlan needs
// after the CLI adapter has planned the exact impacted job set. It deliberately
// exposes no engine request or invocation details: the command document can
// serialize project and task metadata, but not the mechanics used to plan it.
// Commands is the command set the planner planned, which is the set a
// diagnostic about this plan names.
type ChangePlanPlannerResult struct {
	Commands []string
	Complete bool
	Projects []*workspace.Project
	Jobs     []*jobs.ScheduledJob
	Versions jobs.RunVersions
}

// ChangePlanPlanner is supplied by the CLI adapter, which is the sole owner of
// Engine.Run and of the command set it plans. An error it returns names that
// command set. Keeping this callback small lets commands remain engine-free, so
// engine tests can exercise command-provided preflight seams without a cycle.
type ChangePlanPlanner func(ctx context.Context, baseSHA string, noCache bool) (ChangePlanPlannerResult, error)

// EmitChangePlan builds one immutable impact plan for the exact base-to-HEAD
// delta. The head must be the current checkout and the worktree must be clean:
// otherwise task planning can observe source bytes that the emitted revisions
// do not identify.
func EmitChangePlan(ctx context.Context, wsRoot string, opts ChangePlanOptions, planner ChangePlanPlanner) (ciproto.ChangePlan, error) {
	if strings.TrimSpace(opts.Base) == "" {
		return ciproto.ChangePlan{}, cmderr.Usagef("change-plan requires --base <commit>")
	}
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot inspect the worktree: %v", err)
	}
	if !clean {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan requires a clean worktree")
	}

	headRef := strings.TrimSpace(opts.Head)
	if headRef == "" {
		headRef = "HEAD"
	}
	headSHA, err := git.ResolveCommit(wsRoot, headRef)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot resolve --head: %v", err)
	}
	checkedOutSHA, err := git.HeadSHA(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot resolve checked-out HEAD: %v", err)
	}
	if headSHA != checkedOutSHA {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan --head must resolve to the checked-out HEAD")
	}
	baseSHA, err := git.ResolveCommit(wsRoot, opts.Base)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot resolve --base: %v", err)
	}
	if !git.CommitReachableFromHead(wsRoot, baseSHA) {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan --base must be an ancestor of the checked-out HEAD")
	}

	repository, err := changePlanRepository(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	changedFiles, err := git.DiffCommitFiles(wsRoot, baseSHA, headSHA)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot diff revisions: %v", err)
	}

	if planner == nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan planner is not configured")
	}
	result, err := planner(ctx, baseSHA, opts.NoCache)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	if !result.Complete {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan could not produce a complete %s plan",
			strings.Join(result.Commands, ","))
	}

	// Engine hooks and extension discovery are allowed to run before planning;
	// prove afterwards that none changed the revision-bound source state.
	clean, err = git.WorktreeClean(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot recheck the worktree: %v", err)
	}
	currentHead, err := git.HeadSHA(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan cannot recheck HEAD: %v", err)
	}
	if !clean || currentHead != headSHA {
		return ciproto.ChangePlan{}, cmderr.InvalidConfigf("change-plan planning changed the checked-out source state")
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return ciproto.ChangePlan{}, fmt.Errorf("load workspace for change-plan: %w", err)
	}
	cache := buildChangePlanCache(ws, result.Jobs, result.Versions, opts.NoCache)
	return changeplan.Build(changeplan.Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: opts.CLIVersion},
		Repository:       repository,
		BaseSHA:          baseSHA,
		HeadSHA:          headSHA,
		ChangedFiles:     changedFiles,
		DirectProjects:   changePlanDirectProjects(ws, changedFiles),
		ImpactedProjects: result.Projects,
		Planned:          result.Jobs,
		Cache:            cache,
	})
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
