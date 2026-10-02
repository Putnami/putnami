package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// DeployTargetParamName is the one structured job-context parameter through
// which the orchestrator hands a deploy job its workload contract. It is not a
// user flag and no manifest declares it: the environment, the set, and the
// members are decided by the CLI from the repository's CI document and the
// release-set provider (ADR 0021 §13, §14).
const DeployTargetParamName = "deployTarget"

// DeployTarget is what ONE deploy job receives: the environment being
// synchronized and the contract of every workload that job covers.
//
// The shape does not depend on the job's scope. A project-scoped deploy job
// carries exactly one entry, its own workload; a workspace-scoped one — a
// single task an extension plans once for the whole workspace, so a control
// plane with a serial per-workspace release queue receives one submission
// instead of N — carries every workload it selected. A consumer parses one
// shape and never branches on how its task was planned.
//
// Only the environment is shared: one `--env` per run. Everything else is per
// workload, because a workload rule refines the environment's defaults for the
// workloads it names and may send two of them to two different channels, whose
// heads are two different sets (A6.1). `releaseSet` therefore stays inside each
// entry and is never hoisted.
type DeployTarget struct {
	// Environment is the name of the `envs` entry being synchronized.
	Environment string `json:"environment"`
	// Workloads is the contract of every workload this job covers, ordered by
	// project id.
	Workloads []DeployWorkloadTarget `json:"workloads"`
}

// DeployWorkloadTarget is one workload's own contract: the immutable set it
// converges on, the members of that set it owns, and the rollout and
// constraints the environment declared for it.
//
// The member list is the workload's own slice of the set — its `oci` image,
// its `put` configuration, and its `put` migration members — because a
// workload is a project with an image plus the artifacts published beside it
// from the same set (ADR 0021 §14). The CLI executes no migration order: the
// backend applies migrations forward, then the configuration, then the image,
// and refuses a rollback behind migrations that are neither reversible nor
// compatible.
type DeployWorkloadTarget struct {
	// Project is the workspace id of the workload this contract is for. It is
	// named here because a workspace-scoped deploy job carries several of them
	// and its own project is none of them.
	Project string `json:"project"`
	// ReleaseSet is the immutable set this workload converges on. It is per
	// workload because a workload rule can name its own channel, and a channel
	// resolves to its own head.
	ReleaseSet distribution.ReleaseSetRef `json:"releaseSet"`
	// Members are the set members this workload owns, in the set's canonical
	// order.
	Members []distribution.ReleaseSetMember `json:"members"`
	// Rollout is how the image switch advances, when the environment or the
	// workload rule declares one.
	Rollout *ciproto.Rollout `json:"rollout,omitempty"`
	// Constraints gates the synchronization; `approval: manual` waits for a
	// human.
	Constraints map[string]any `json:"constraints,omitempty"`
}

// DeployOptions is the typed run input the engine extracts from the command
// line for a deploy. It exists for the same reason ReleaseSetOptions does: the
// engine assembles runs and owns no environment policy (ADR 0001 §3).
type DeployOptions struct {
	// Commands is the run's command list.
	Commands []string
	// WorkspaceRoot is where putnami.ci.json is read from.
	WorkspaceRoot string
	// Environment is the `--env <name>` value: which declared environment this
	// run synchronizes.
	Environment string
	// Release is the `--release <rs_id>` value: an exact immutable set instead
	// of the head of the channel the environment follows.
	Release string
}

// BuildDeployOptions reads the two job flags a deploy takes out of the run's
// parameter bag. `--env` and `--release` are core flags: no manifest declares
// them, because the environment and the set are decided by the CLI from the
// repository's CI document and the release-set provider.
func BuildDeployOptions(commands []string, workspaceRoot string, params map[string]any) DeployOptions {
	environment, _ := params["env"].(string)
	release, _ := params["release"].(string)
	return DeployOptions{
		Commands: commands, WorkspaceRoot: workspaceRoot,
		Environment: environment, Release: release,
	}
}

// requestedDeployEnvironment reports a deploy that names the environment it
// synchronizes. A deploy without `--env` keeps its previous behavior: the run
// plans deploy jobs and hands them no workload contract.
func requestedDeployEnvironment(options DeployOptions) bool {
	return slices.Contains(options.Commands, "deploy") && strings.TrimSpace(options.Environment) != ""
}

// loadEnvironment reads one declared environment from the repository's CI
// document. An environment is the state a channel is followed into, so it is
// declared once, in the file Putnami Cloud's runner already reads, and never
// on the command line (D3).
func loadEnvironment(wsRoot, name string) (ciproto.Environment, error) {
	if strings.TrimSpace(wsRoot) == "" {
		return ciproto.Environment{}, cmderr.Usagef("deploy --env %s needs a workspace", name)
	}
	data, err := os.ReadFile(filepath.Join(wsRoot, ciproto.Filename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ciproto.Environment{}, cmderr.Usagef(
				"deploy --env %s: this workspace declares no %s; add an envs entry naming the channel the environment follows",
				name, ciproto.Filename)
		}
		return ciproto.Environment{}, fmt.Errorf("read %s: %w", ciproto.Filename, err)
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return ciproto.Environment{}, cmderr.InvalidConfigf("parse %s: %v", ciproto.Filename, err)
	}
	environment, declared := document.Envs[name]
	if !declared {
		return ciproto.Environment{}, cmderr.Usagef("deploy --env %s: %s declares no environment %q; %s",
			name, ciproto.Filename, name, declaredEnvironmentNames(document))
	}
	return environment, nil
}

// declaredEnvironmentNames renders what the document does declare, so a typo
// is answered with the list instead of a second command to run.
func declaredEnvironmentNames(document ciproto.Document) string {
	if len(document.Envs) == 0 {
		return "it declares no environment at all"
	}
	return "declared environments: " + strings.Join(slices.Sorted(maps.Keys(document.Envs)), ", ")
}

// deployWorkload is one selected workload: the project, the channel it
// follows, and the rollout and constraints that apply to it after the
// environment's defaults and its first matching rule are merged.
type deployWorkload struct {
	project     *workspace.Project
	channel     string
	rollout     *ciproto.Rollout
	constraints map[string]any
}

// selectWorkloads intersects the environment's workload rules with the run's
// own selection. Rules apply in declaration order and the FIRST match wins:
// a rule refines the environment's defaults for the workloads it names, and a
// workload no rule selects does not belong to the environment at all (A6.1).
//
// An environment that declares no rule has no refinement to make, so every
// selected project is one of its workloads at the environment's own values.
//
// A candidate that is not a project of this workspace is never a workload, on
// either branch. The planner hands a workspace-scoped task a synthetic project
// whose id is the workspace name, and that project has no manifest, no
// release-set member and no selector that can name it: it is a task, not a
// workload, and the no-rule branch must not silently make it one.
func selectWorkloads(
	ws *workspace.Workspace,
	name string,
	environment ciproto.Environment,
	selected []*workspace.Project,
) ([]deployWorkload, error) {
	matched, err := workloadRuleMatches(ws, name, environment)
	if err != nil {
		return nil, err
	}
	candidates := append([]*workspace.Project(nil), selected...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	workloads := make([]deployWorkload, 0, len(candidates))
	for _, project := range candidates {
		if project == nil || ws.ProjectByID(project.ID) == nil {
			continue
		}
		workload := deployWorkload{
			project: project, channel: environment.Channel,
			rollout: environment.Rollout, constraints: environment.Constraints,
		}
		if len(environment.Workloads) > 0 {
			index, selects := matched[project.ID]
			if !selects {
				continue
			}
			applyWorkloadRule(&workload, environment.Workloads[index])
		}
		workloads = append(workloads, workload)
	}
	return workloads, nil
}

// applyWorkloadRule overrides the environment's defaults with what the first
// matching rule declares. An empty constraints object is an override, not an
// omission: it clears the environment's constraints for these workloads.
func applyWorkloadRule(workload *deployWorkload, rule ciproto.WorkloadRule) {
	if strings.TrimSpace(rule.Channel) != "" {
		workload.channel = rule.Channel
	}
	if rule.Rollout != nil {
		workload.rollout = rule.Rollout
	}
	if rule.Constraints != nil {
		workload.constraints = rule.Constraints
	}
}

// workloadRuleMatches resolves every rule's selectors once and records, per
// project, the index of the first rule that names it.
func workloadRuleMatches(ws *workspace.Workspace, name string, environment ciproto.Environment) (map[string]int, error) {
	matched := make(map[string]int, len(environment.Workloads))
	for index, rule := range environment.Workloads {
		field := fmt.Sprintf("envs.%s.workloads[%d].select", name, index)
		projects, err := resolveMemberSelectors(ws, field, rule.Select)
		if err != nil {
			return nil, err
		}
		for _, projectID := range projects {
			if _, earlier := matched[projectID]; !earlier {
				matched[projectID] = index
			}
		}
	}
	return matched, nil
}

// PrepareDeployTargets is the ONE deploy seam the engine calls, after the plan
// exists and before the release-set barrier is attached.
//
// It resolves the set every selected workload synchronizes on — the head of
// the channel its environment follows, or the exact set `--release` names —
// and binds, on every deploy job, the contract of each workload that job
// covers — its own when it is scoped to a workload, all of them when one task
// deploys the whole workspace. The provider is asked ONCE:
// every channel an environment and its workload rules follow is resolved in a
// single call, so two workloads of the same environment can never converge on
// two different reads of the same channel.
//
// releasing marks a session that publishes in the same DAG. Its set does not
// exist yet: the contract is bound without one and the release-set barrier
// completes it from the set it just released, which is what keeps a publish
// and the deploy that follows it on exactly one snapshot (ADR 0021 §13).
func PrepareDeployTargets(
	ctx context.Context,
	options DeployOptions,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
	planned []*ScheduledJob,
	releasing bool,
) error {
	if !requestedDeployEnvironment(options) {
		return nil
	}
	if releasing && strings.TrimSpace(options.Release) != "" {
		return cmderr.Usagef("deploy --release cannot be combined with a publish in the same session: the deploy follows the set that session releases")
	}
	name := strings.TrimSpace(options.Environment)
	environment, err := loadEnvironment(options.WorkspaceRoot, name)
	if err != nil {
		return err
	}
	if err := requireReleaseSetProvider(discovered, name); err != nil {
		return err
	}
	workloads, err := selectWorkloads(ws, name, environment, PlannedCommandProjects(planned, "deploy"))
	if err != nil {
		return err
	}
	if len(workloads) == 0 {
		return nil
	}
	targets := make(map[string]*DeployWorkloadTarget, len(workloads))
	for _, workload := range workloads {
		targets[workload.project.ID] = &DeployWorkloadTarget{
			Project: workload.project.ID, Rollout: workload.rollout, Constraints: workload.constraints,
		}
	}
	if !releasing {
		if err := resolveDeployTargets(ctx, options, ws, discovered, workloads, targets); err != nil {
			return err
		}
	}
	return bindDeployTargets(planned, name, targets, releasing)
}

// requireReleaseSetProvider refuses an environment in a workspace with no
// release-set provider. A channel, and therefore an environment that follows
// one, exists only through a provider (A13): without one there is no set to
// synchronize on and the command would otherwise deploy whatever the last
// build happened to leave behind.
func requireReleaseSetProvider(discovered *extension.DiscoveryResult, name string) error {
	provider, err := extension.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return fmt.Errorf("resolve release-set provider: %w", err)
	}
	if provider != nil {
		return nil
	}
	message := fmt.Sprintf(
		"deploy --env %s requires exactly one installed extension declaring %q: an environment follows a channel, and a channel exists only through a release-set provider",
		name, distribution.ProviderCommandName)
	if cause := discovered.ProviderCause(distribution.ProviderCommandName); cause != "" {
		message += ". " + cause
	}
	return fmt.Errorf("%w: %s", releaseset.ErrProviderAbsent, message)
}

// resolveDeployTargets performs the ONE provider call of a deploy and fills
// every workload contract from its answer.
func resolveDeployTargets(
	ctx context.Context,
	options DeployOptions,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
	workloads []deployWorkload,
	targets map[string]*DeployWorkloadTarget,
) error {
	heads, err := resolveDeployHeads(ctx, options, ws, discovered, workloads)
	if err != nil {
		return err
	}
	for _, workload := range workloads {
		head := heads[deployHeadKey(options, workload)]
		if head == nil || head.ReleaseSet == nil {
			return fmt.Errorf(
				"environment %q follows channel %q, which carries no release set yet; publish it first or name an exact set with --release",
				options.Environment, workload.channel)
		}
		target := targets[workload.project.ID]
		target.ReleaseSet = head.Ref
		if target.Members, err = workloadMembers(workload.project, head.ReleaseSet.Members); err != nil {
			return err
		}
	}
	return nil
}

// deployHeadKey names the resolved answer one workload reads: the exact set
// when `--release` named one, its own channel otherwise.
func deployHeadKey(options DeployOptions, workload deployWorkload) string {
	if release := strings.TrimSpace(options.Release); release != "" {
		return release
	}
	return workload.channel
}

// resolveDeployHeads asks the provider exactly once. `--release` names one
// immutable set for every workload; otherwise every distinct channel the
// environment and its workload rules follow is resolved in the same call, so
// the environment converges on one consistent read.
func resolveDeployHeads(
	ctx context.Context,
	options DeployOptions,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
	workloads []deployWorkload,
) (map[string]*distribution.ChannelHead, error) {
	provider, err := newDeployProvider(ctx, discovered)
	if err != nil {
		return nil, err
	}
	request := &distribution.ResolveRequest{ProtocolVersion: distribution.ProtocolVersion, Namespace: ws.Name}
	release := strings.TrimSpace(options.Release)
	if release != "" {
		request.ReleaseID = release
	} else if request.Channels, err = deployChannels(options.Environment, workloads); err != nil {
		return nil, err
	}
	response, err := provider.Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("resolve environment %q: %w", options.Environment, err)
	}
	if response == nil {
		return nil, fmt.Errorf("resolve environment %q: provider returned no response", options.Environment)
	}
	if diagnostics := distribution.ValidateResolveExchange(request, response); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("resolve environment %q: invalid provider exchange: %s",
			options.Environment, firstReleaseSetDiagnostic(diagnostics))
	}
	if release != "" {
		return map[string]*distribution.ChannelHead{release: response.Release}, nil
	}
	return response.Heads, nil
}

// newDeployProvider builds the provider client a deploy resolves through. It
// is the same seam a publish uses, so a test replaces both at once.
func newDeployProvider(ctx context.Context, discovered *extension.DiscoveryResult) (releaseSetProvider, error) {
	description, err := extension.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return nil, fmt.Errorf("resolve release-set provider: %w", err)
	}
	return newReleaseSetProvider(ctx, description)
}

// deployChannels is the exact channel list one deploy resolves: every distinct
// channel a selected workload follows, in first-seen order.
func deployChannels(name string, workloads []deployWorkload) ([]string, error) {
	var channels []string
	seen := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		channel := strings.TrimSpace(workload.channel)
		if channel == "" {
			return nil, cmderr.Usagef(
				"environment %q declares no channel for %s; declare one or name an exact set with --release",
				name, workload.project.ID)
		}
		if _, duplicate := seen[channel]; duplicate {
			continue
		}
		seen[channel] = struct{}{}
		channels = append(channels, channel)
	}
	if len(channels) > distribution.MaxChannelsPerRelease {
		return nil, cmderr.Usagef("environment %q follows %d channels, exceeding the limit of %d",
			name, len(channels), distribution.MaxChannelsPerRelease)
	}
	return channels, nil
}

// workloadMembers is the workload's own slice of the set: every member the
// project declares as its own, whatever its ecosystem, in the set's canonical
// order. A project publishing an image, its configuration, and its migrations
// receives all three; a project the set does not carry receives none, which is
// a deploy of a workload that was never published.
func workloadMembers(project *workspace.Project, members []distribution.ReleaseSetMember) ([]distribution.ReleaseSetMember, error) {
	owned, err := deployMemberKeys(project)
	if err != nil {
		return nil, err
	}
	result := make([]distribution.ReleaseSetMember, 0, len(owned))
	hasImage := false
	for _, member := range members {
		if _, mine := owned[releaseset.MemberKey(member.Ecosystem, member.Coordinate)]; mine {
			result = append(result, member)
			if member.Ecosystem == "oci" {
				hasImage = true
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("workload %q has no member in the release set; publish it before deploying it", project.ID)
	}
	if !hasImage {
		return nil, fmt.Errorf("workload %q has no oci image member in the release set; a deployable workload must publish an image", project.ID)
	}
	return result, nil
}

// deployMemberKeys reads the member identities a project declares. It is the
// same probe-recorded metadata a publish keys members by, so a workload and
// its published members can never be matched by two different rules.
func deployMemberKeys(project *workspace.Project) (map[string]struct{}, error) {
	metadata, found, err := releaseset.ProjectReleaseMetadata(project.Metadata)
	if err != nil {
		return nil, fmt.Errorf("workload %q metadata: %w", project.ID, err)
	}
	keys := make(map[string]struct{}, len(metadata.Ecosystems))
	if !found {
		return keys, nil
	}
	for _, declaration := range metadata.Ecosystems {
		keys[releaseset.MemberKey(declaration.Ecosystem, declaration.Coordinate)] = struct{}{}
	}
	return keys, nil
}

// bindDeployTargets carries every workload contract to the deploy job that
// covers it, whatever the scope of that job.
//
// A contract the release completes is bound on a job that can no longer be
// served from the cache: its input exists only after this session's release,
// so an entry keyed before it would answer for a different set.
func bindDeployTargets(planned []*ScheduledJob, environment string, targets map[string]*DeployWorkloadTarget, releasing bool) error {
	bound := make(map[string]struct{}, len(targets))
	for _, job := range schedulableJobs(planned) {
		if job == nil || job.JobDef == nil || job.Project == nil || job.CommandName() != "deploy" {
			continue
		}
		covered := deployJobWorkloads(job, targets)
		if len(covered) == 0 {
			continue
		}
		definition := *job.JobDef
		definition.BoundParams = maps.Clone(job.JobDef.BoundParams)
		if definition.BoundParams == nil {
			definition.BoundParams = extension.ParamMap{}
		}
		definition.BoundParams[DeployTargetParamName] = &DeployTarget{Environment: environment, Workloads: covered}
		if releasing {
			definition.Cache = false
		}
		job.JobDef = &definition
		for _, workload := range covered {
			bound[workload.Project] = struct{}{}
		}
	}
	if len(bound) == len(targets) {
		return nil
	}
	missing := make([]string, 0, len(targets)-len(bound))
	for projectID := range targets {
		if _, ok := bound[projectID]; !ok {
			missing = append(missing, projectID)
		}
	}
	sort.Strings(missing)
	return fmt.Errorf("environment workloads %v have no deploy task in this plan", missing)
}

// deployJobWorkloads is the contract list one deploy job receives: its own
// workload when the job is scoped to a workload, every workload it selected
// when it is not.
//
// A workspace-scoped deploy job is planned for a synthetic project no workload
// rule can name, and the workloads it acts on are the projects the run
// selected under it — the same projects whose package and publish tasks it
// already depends on. Entries are ordered by project id, so two runs of the
// same plan bind the same bytes.
func deployJobWorkloads(job *ScheduledJob, targets map[string]*DeployWorkloadTarget) []DeployWorkloadTarget {
	if target, selected := targets[job.Project.ID]; selected {
		return []DeployWorkloadTarget{*target}
	}
	covered := make(map[string]struct{}, len(job.SelectedProjects))
	for _, project := range job.SelectedProjects {
		if project != nil {
			covered[project.ID] = struct{}{}
		}
	}
	workloads := make([]DeployWorkloadTarget, 0, len(covered))
	for _, projectID := range slices.Sorted(maps.Keys(targets)) {
		if _, carries := covered[projectID]; carries {
			workloads = append(workloads, *targets[projectID])
		}
	}
	return workloads
}

// completeDeployTargets fills the contracts a session's own release produces.
//
// The set a same-session deploy synchronizes on is the one the publish just
// released: its identity and its members exist only after the release, so the
// barrier hands them forward here, between the commit and the first deploy
// task. This is the deploy handoff — member digests from the released set,
// keyed by ecosystem and coordinate like every other member.
//
// Every contract a job carries is completed, each against its own workload
// project: a job that deploys the whole workspace owns no member itself, and
// completing it against its own project would answer that every workload is
// unpublished.
func completeDeployTargets(deployJobs []*ScheduledJob, ref distribution.ReleaseSetRef, members []distribution.ReleaseSetMember) error {
	for _, job := range deployJobs {
		target, bound := deployTargetOf(job)
		if !bound {
			continue
		}
		for index := range target.Workloads {
			workload := deployWorkloadProject(job, target.Workloads[index].Project)
			if workload == nil {
				return fmt.Errorf("deploy task %q carries a contract for workload %q, which it does not cover",
					job.Key(), target.Workloads[index].Project)
			}
			owned, err := workloadMembers(workload, members)
			if err != nil {
				return err
			}
			target.Workloads[index].ReleaseSet = ref
			target.Workloads[index].Members = owned
		}
	}
	return nil
}

// deployWorkloadProject finds the project one contract of a deploy job is for,
// in that job's own scope: its project when the job is scoped to a workload,
// one of the projects it selected otherwise. It reads the workload from the
// same place bindDeployTargets covered it from, so the barrier completes every
// contract against the very project that earned it.
func deployWorkloadProject(job *ScheduledJob, projectID string) *workspace.Project {
	if job.Project != nil && job.Project.ID == projectID {
		return job.Project
	}
	for _, project := range job.SelectedProjects {
		if project != nil && project.ID == projectID {
			return project
		}
	}
	return nil
}

// deployTargetOf reads the workload contract bound to one deploy job, so the
// parameter name is spelled in exactly one place.
func deployTargetOf(job *ScheduledJob) (*DeployTarget, bool) {
	if job == nil || job.JobDef == nil || job.Project == nil {
		return nil, false
	}
	target, bound := job.JobDef.BoundParams[DeployTargetParamName].(*DeployTarget)
	return target, bound
}
