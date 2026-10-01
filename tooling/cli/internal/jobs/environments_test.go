package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The deploy side of distribution v2: an environment follows a channel, the
// CLI resolves that channel ONCE, selects the workloads the environment's
// rules name, and hands each deploy task the exact set and the members that
// workload owns. Nothing here talks to a hosting provider: the traffic split,
// the migration order, and the skip-per-member rule are the backend's, and the
// CLI only carries the contract (ADR 0021 §13, §14).

const deployEnvironmentDocument = `{
  "version": 3,
  "commands": ["build"],
  "distribution": {"namespace": "putnami"},
  "envs": {
    "local": {},
    "prod": {
      "channel": "canary",
      "constraints": {"approval": "manual"},
      "workloads": [
        {"select": "tag:api", "rollout": {"strategy": "progressive", "steps": [10, 100]}},
        {"select": "tag:api", "rollout": {"strategy": "all-at-once"}},
        {"select": "tag:worker", "channel": "canary", "constraints": {}}
      ]
    }
  }
}`

// deployNoRuleEnvironmentDocument is the same environment without a single
// workload rule: every project the run selected is a workload, at the
// environment's own values. It is the branch where a synthetic project would
// otherwise become a workload of its own workspace.
const deployNoRuleEnvironmentDocument = `{
  "version": 3,
  "commands": ["build"],
  "distribution": {"namespace": "putnami"},
  "envs": {
    "prod": {
      "channel": "canary",
      "constraints": {"approval": "manual"}
    }
  }
}`

// deployWorkspace builds the two-workload fixture every test below reads: an
// api project publishing an image and its configuration, a worker project
// publishing an image, and a web project no workload rule selects.
func deployWorkspace(t *testing.T, document string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := []*workspace.Project{
		{
			ID: "/api", Name: "api", Path: "api", Tags: []string{"api"},
			Metadata: releaseSetProjectMembers(t,
				releaseset.MemberDeclaration{Ecosystem: "oci", Coordinate: "putnami/api", PackageStep: "oci", PublishStep: "oci"},
				releaseset.MemberDeclaration{Ecosystem: "put", Coordinate: "putnami/api-config", PackageStep: "put", PublishStep: "put"},
			),
		},
		{
			ID: "/worker", Name: "worker", Path: "worker", Tags: []string{"worker"},
			Metadata: releaseSetProjectMetadata(t, "oci", "putnami/worker"),
		},
		{
			ID: "/web", Name: "web", Path: "web", Tags: []string{"web"},
			Metadata: releaseSetProjectMetadata(t, "oci", "putnami/web"),
		},
	}
	return workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, projects)
}

// deployHead is the head the environment's channel points at: three images and
// one configuration member, one set.
func deployHead(t *testing.T) map[string]*distribution.ChannelHead {
	t.Helper()
	set := distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: "oci", Coordinate: "putnami/api", Version: "0.2.0", ArtifactDigest: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/api")},
			{Ecosystem: "put", Coordinate: "putnami/api-config", Version: "0.2.0", ArtifactDigest: digestFor('b'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/api-config")},
			{Ecosystem: "oci", Coordinate: "putnami/worker", Version: "0.2.0", ArtifactDigest: digestFor('c'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/worker")},
			{Ecosystem: "oci", Coordinate: "putnami/web", Version: "0.2.0", ArtifactDigest: digestFor('d'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/web")},
		},
	}
	head := &distribution.ChannelHead{Generation: 7, ReleaseSet: distribution.NormalizeReleaseSet(&set)}
	refreshHeadRef(t, head)
	return map[string]*distribution.ChannelHead{"canary": head}
}

// deployJobsFor plans one deploy task per named project.
func deployJobsFor(ws *workspace.Workspace, ids ...string) []*ScheduledJob {
	ext := &modelextension.ExtensionDescription{Name: "@putnami/cloud"}
	planned := make([]*ScheduledJob, 0, len(ids))
	for _, id := range ids {
		for _, project := range ws.Projects {
			if project.ID != id {
				continue
			}
			planned = append(planned, &ScheduledJob{
				Project: project, Extension: ext,
				JobDef: &modelextension.JobDefinition{
					Name: "deploy~cloud", CommandName: "deploy", ExtensionName: ext.Name, Cache: true,
				},
			})
		}
	}
	return planned
}

// deployWorkspaceJobFor plans the shape an extension produces when one deploy
// task synchronizes the whole workspace: ONE job whose project is the
// synthetic workspace-once project — its id is the workspace name, it is in no
// ws.Projects and no selector can name it — carrying the real workloads in
// SelectedProjects. It sits beside deployJobsFor rather than replacing it:
// both shapes are legitimate and both must bind.
func deployWorkspaceJobFor(ws *workspace.Workspace, ids ...string) *ScheduledJob {
	ext := &modelextension.ExtensionDescription{Name: "@putnami/cloud"}
	selected := make([]*workspace.Project, 0, len(ids))
	for _, id := range ids {
		for _, project := range ws.Projects {
			if project.ID == id {
				selected = append(selected, project)
			}
		}
	}
	return &ScheduledJob{
		Project: workspaceOnceProject(ws), Extension: ext, SelectedProjects: selected,
		JobDef: &modelextension.JobDefinition{
			Name: "deploy~cloud-release", CommandName: "deploy", ExtensionName: ext.Name,
			Kind: "cloud-deploy", Activation: "workspace-once", Cache: true,
		},
	}
}

// deployWorkloadOf reads the contract one deploy job carries for one workload,
// by the project id that entry names. A consumer reads it the same way,
// whatever the scope of the job it runs in.
func deployWorkloadOf(t *testing.T, job *ScheduledJob, projectID string) DeployWorkloadTarget {
	t.Helper()
	target, bound := deployTargetOf(job)
	if !bound {
		t.Fatalf("deploy job %s carries no workload contract", job.Key())
	}
	for _, workload := range target.Workloads {
		if workload.Project == projectID {
			return workload
		}
	}
	t.Fatalf("deploy job %s carries no contract for %s: %+v", job.Key(), projectID, target.Workloads)
	return DeployWorkloadTarget{}
}

// deployWorkloadIDs lists, in order, the workloads one deploy job's contract
// names.
func deployWorkloadIDs(job *ScheduledJob) []string {
	target, bound := deployTargetOf(job)
	if !bound {
		return nil
	}
	ids := make([]string, 0, len(target.Workloads))
	for _, workload := range target.Workloads {
		ids = append(ids, workload.Project)
	}
	return ids
}

func deployOptions(environment, release string) DeployOptions {
	return DeployOptions{
		Commands: []string{"deploy"}, Environment: environment, Release: release,
	}
}

// TestDeployEnvResolvesTheChannelOnce pins the one provider call. Two
// workloads of one environment follow the same channel; reading it twice could
// answer two different heads and split the environment across two sets.
func TestDeployEnvResolvesTheChannelOnce(t *testing.T) {
	spectest.Proves(t, "cli/deploy-environments", "deploy-follows-the-environment-channel", "env-channel-is-resolved-once")

	ws := deployWorkspace(t, deployEnvironmentDocument)
	provider := &fakeReleaseSetProvider{heads: deployHead(t)}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	planned := deployJobsFor(ws, "/api", "/worker")
	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, false); err != nil {
		t.Fatalf("prepare deploy targets: %v", err)
	}
	if provider.resolveCalls != 1 {
		t.Fatalf("resolve calls = %d, want exactly one", provider.resolveCalls)
	}
	if got := provider.resolveRequests[0].Channels; len(got) != 1 || got[0] != "canary" {
		t.Fatalf("resolved channels = %v, want [canary] named once", got)
	}
	for _, job := range planned {
		target, bound := deployTargetOf(job)
		if !bound || target.Environment != "prod" {
			t.Fatalf("deploy target of %s = %+v, bound=%v", job.Project.ID, target, bound)
		}
		if workload := deployWorkloadOf(t, job, job.Project.ID); workload.ReleaseSet != provider.heads["canary"].Ref {
			t.Fatalf("deploy target of %s = %+v", job.Project.ID, workload)
		}
	}
}

// TestDeployEnvTakesTheExactReleaseWhenNamed pins that --release replaces the
// channel read entirely: the environment is still the one being synchronized,
// but the set is the immutable one the caller named.
func TestDeployEnvTakesTheExactReleaseWhenNamed(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	heads := deployHead(t)
	// An immutable set is named by no channel, so it reports generation 0.
	named := &distribution.ChannelHead{Ref: heads["canary"].Ref, ReleaseSet: heads["canary"].ReleaseSet}
	provider := &fakeReleaseSetProvider{heads: heads, release: named}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", heads["canary"].Ref.ID)
	options.WorkspaceRoot = ws.Root
	planned := deployJobsFor(ws, "/api")
	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, false); err != nil {
		t.Fatalf("prepare deploy targets: %v", err)
	}
	if provider.resolveCalls != 1 || provider.resolveRequests[0].ReleaseID != heads["canary"].Ref.ID ||
		len(provider.resolveRequests[0].Channels) != 0 {
		t.Fatalf("resolve requests = %+v, want one release-id lookup", provider.resolveRequests)
	}
	api := deployWorkloadOf(t, planned[0], "/api")
	if api.ReleaseSet != heads["canary"].Ref {
		t.Fatalf("deploy target set = %+v, want the named release", api.ReleaseSet)
	}
}

// TestWorkloadRulesSelectFirstMatch pins the two halves of the rule contract:
// the FIRST rule that names a workload decides its overrides, and a workload
// no rule names is not part of the environment at all.
func TestWorkloadRulesSelectFirstMatch(t *testing.T) {
	spectest.Proves(t, "cli/deploy-environments", "deploy-follows-the-environment-channel", "workload-rules-select-first-match")

	ws := deployWorkspace(t, deployEnvironmentDocument)
	environment, err := loadEnvironment(ws.Root, "prod")
	if err != nil {
		t.Fatalf("load environment: %v", err)
	}
	workloads, err := selectWorkloads(ws, "prod", environment, ws.Projects)
	if err != nil {
		t.Fatalf("select workloads: %v", err)
	}
	byProject := make(map[string]deployWorkload, len(workloads))
	for _, workload := range workloads {
		byProject[workload.project.ID] = workload
	}
	if _, selected := byProject["/web"]; selected {
		t.Fatalf("web is selected by no rule but belongs to the environment: %+v", byProject)
	}
	api, selected := byProject["/api"]
	if !selected || api.rollout == nil || api.rollout.Strategy != "progressive" {
		t.Fatalf("api workload = %+v, want the FIRST matching rule's progressive rollout", api)
	}
	if api.channel != "canary" || len(api.constraints) != 1 {
		t.Fatalf("api workload = %+v, want the environment's channel and constraints", api)
	}
	worker, selected := byProject["/worker"]
	if !selected || worker.rollout != nil || len(worker.constraints) != 0 {
		t.Fatalf("worker workload = %+v, want its rule's empty constraints and no rollout", worker)
	}
}

// TestDeployJobsReceiveTheMembers pins the workload contract itself: every
// deploy task is handed the exact set and only the members its own project
// published into it — its image, and the configuration published beside it.
func TestDeployJobsReceiveTheMembers(t *testing.T) {
	spectest.Proves(t, "cli/deploy-environments", "deploy-follows-the-environment-channel", "deploy-jobs-receive-the-members")

	ws := deployWorkspace(t, deployEnvironmentDocument)
	provider := &fakeReleaseSetProvider{heads: deployHead(t)}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	planned := deployJobsFor(ws, "/api", "/worker")
	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, false); err != nil {
		t.Fatalf("prepare deploy targets: %v", err)
	}
	api := deployWorkloadOf(t, planned[0], "/api")
	if got := memberCoordinates(api.Members); strings.Join(got, ",") != "oci putnami/api,put putnami/api-config" {
		t.Fatalf("api members = %v, want its image and its configuration", got)
	}
	if api.Members[0].ArtifactDigest != digestFor('a') || api.Members[1].ArtifactDigest != digestFor('b') {
		t.Fatalf("api members carry the wrong digests: %+v", api.Members)
	}
	worker := deployWorkloadOf(t, planned[1], "/worker")
	if got := memberCoordinates(worker.Members); strings.Join(got, ",") != "oci putnami/worker" {
		t.Fatalf("worker members = %v, want only its own image", got)
	}
}

// TestDeployWithoutProviderIsRefused pins A13: an environment follows a
// channel, and a channel exists only through a release-set provider. Without
// one the command is refused instead of deploying whatever the tree holds.
func TestDeployWithoutProviderIsRefused(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	planned := deployJobsFor(ws, "/api")

	err := PrepareDeployTargets(context.Background(), options, ws, &extension.DiscoveryResult{}, planned, false)
	if !errors.Is(err, releaseset.ErrProviderAbsent) {
		t.Fatalf("deploy --env without a provider = %v, want the absent-provider error", err)
	}
	if _, bound := deployTargetOf(planned[0]); bound {
		t.Fatal("a refused deploy still bound a workload contract")
	}
}

// TestDeployUndeclaredEnvironmentIsRefused keeps the answer to a typo in the
// file that owns the vocabulary, not in a second command.
func TestDeployUndeclaredEnvironmentIsRefused(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	options := deployOptions("preprod", "")
	options.WorkspaceRoot = ws.Root

	err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), deployJobsFor(ws, "/api"), false)
	if err == nil || !strings.Contains(err.Error(), "declared environments: local, prod") {
		t.Fatalf("deploy --env preprod = %v, want the declared list", err)
	}
}

// TestDeployWithoutEnvironmentBindsNothing pins that the workload contract is
// opt-in: `putnami deploy` without --env plans exactly what it planned before.
func TestDeployWithoutEnvironmentBindsNothing(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	planned := deployJobsFor(ws, "/api")
	if err := PrepareDeployTargets(context.Background(), deployOptions("", ""), ws, nil, planned, false); err != nil {
		t.Fatalf("deploy without an environment: %v", err)
	}
	if _, bound := deployTargetOf(planned[0]); bound {
		t.Fatal("a deploy that names no environment received a workload contract")
	}
}

// TestSameSessionDeployTakesTheReleasedSet pins the barrier handoff that
// replaced the private project-keyed image proof: a deploy sharing a session
// with a publish is bound without a set, and the barrier completes its
// contract with the member digests of the set it has just released.
func TestSameSessionDeployTakesTheReleasedSet(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	planned := deployJobsFor(ws, "/api")

	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, true); err != nil {
		t.Fatalf("prepare same-session deploy targets: %v", err)
	}
	before := deployWorkloadOf(t, planned[0], "/api")
	if before.ReleaseSet.ID != "" || len(before.Members) != 0 {
		t.Fatalf("same-session target before the release = %+v", before)
	}
	if planned[0].JobDef.Cache {
		t.Fatal("a deploy completed by this session's own release is still cacheable")
	}

	head := deployHead(t)["canary"]
	if err := completeDeployTargets(planned, head.Ref, head.ReleaseSet.Members); err != nil {
		t.Fatalf("complete deploy targets: %v", err)
	}
	after := deployWorkloadOf(t, planned[0], "/api")
	if after.ReleaseSet != head.Ref {
		t.Fatalf("completed target set = %+v, want the released set", after.ReleaseSet)
	}
	if got := memberCoordinates(after.Members); strings.Join(got, ",") != "oci putnami/api,put putnami/api-config" {
		t.Fatalf("completed members = %v", got)
	}
}

func TestSameSessionDeployRefusesAnExplicitOlderRelease(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	options := deployOptions("prod", "rs_"+strings.Repeat("a", 64))
	options.WorkspaceRoot = ws.Root

	err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), deployJobsFor(ws, "/api"), true)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with a publish") {
		t.Fatalf("same-session publish plus explicit release = %v, want an ambiguity refusal", err)
	}
}

// TestDeployWorkloadWithoutMemberIsRefused pins the fail-closed answer for a
// workload the set never carried: deploying it would converge on nothing.
func TestDeployWorkloadWithoutMemberIsRefused(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	heads := deployHead(t)
	heads["canary"].ReleaseSet.Members = heads["canary"].ReleaseSet.Members[:1]
	refreshHeadRef(t, heads["canary"])
	provider := &fakeReleaseSetProvider{heads: heads}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), deployJobsFor(ws, "/api", "/worker"), false)
	if err == nil || !strings.Contains(err.Error(), `workload "/worker" has no member`) {
		t.Fatalf("deploy of an unpublished workload = %v", err)
	}
}

func TestDeployWorkloadWithoutOciImageIsRefused(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	api := ws.Projects[0]
	api.Metadata = releaseSetProjectMetadata(t, "npm", "@putnami/api")
	heads := deployHead(t)
	heads["canary"].ReleaseSet.Members = []distribution.ReleaseSetMember{{
		Ecosystem: "npm", Coordinate: "@putnami/api", Version: "0.2.0",
		ArtifactDigest: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{},
		SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/api"),
	}}
	refreshHeadRef(t, heads["canary"])
	provider := &fakeReleaseSetProvider{heads: heads}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), deployJobsFor(ws, "/api"), false)
	if err == nil || !strings.Contains(err.Error(), `workload "/api" has no oci image member`) {
		t.Fatalf("deploy of a package-only project = %v, want the missing-image error", err)
	}
}

// TestDeployBuildOptionsReadsTheCoreFlags pins where --env and --release come
// from: the run's parameter bag, because no manifest declares either.
func TestDeployBuildOptionsReadsTheCoreFlags(t *testing.T) {
	options := BuildDeployOptions([]string{"deploy"}, "/ws", map[string]any{
		"env": "prod", "release": "rs_" + strings.Repeat("0", 64), "dry-run": true,
	})
	if options.Environment != "prod" || !strings.HasPrefix(options.Release, "rs_") || options.WorkspaceRoot != "/ws" {
		t.Fatalf("built deploy options = %+v", options)
	}
	if requestedDeployEnvironment(BuildDeployOptions([]string{"build"}, "/ws", map[string]any{"env": "prod"})) {
		t.Fatal("a run that does not deploy asked for an environment")
	}
}

// TestWorkspaceScopedDeployJobReceivesEveryWorkload pins the bind over the
// shape an extension produces when ONE deploy task synchronizes the whole
// workspace — one submission into a control plane with a serial per-workspace
// release queue, instead of one per workload. That job's own project is
// synthetic and is no workload of anything: the workloads it acts on are the
// ones it selected, and it receives the contract of every one of them, under
// an environment with no rule as much as under exact selectors.
func TestWorkspaceScopedDeployJobReceivesEveryWorkload(t *testing.T) {
	spectest.Proves(t, "cli/deploy-environments", "deploy-follows-the-environment-channel", "one-deploy-task-carries-every-workload-it-covers")

	for _, testCase := range []struct {
		name              string
		document          string
		apiRollout        string
		workerConstraints int
	}{
		{name: "no workload rule", document: deployNoRuleEnvironmentDocument, apiRollout: "", workerConstraints: 1},
		{name: "exact workload selectors", document: deployEnvironmentDocument, apiRollout: "progressive", workerConstraints: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ws := deployWorkspace(t, testCase.document)
			provider := &fakeReleaseSetProvider{heads: deployHead(t)}
			useReleaseSetProviderFake(t, provider)

			options := deployOptions("prod", "")
			options.WorkspaceRoot = ws.Root
			job := deployWorkspaceJobFor(ws, "/api", "/worker")
			if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), []*ScheduledJob{job}, false); err != nil {
				t.Fatalf("prepare deploy targets: %v", err)
			}
			target, bound := deployTargetOf(job)
			if !bound || target.Environment != "prod" {
				t.Fatalf("workspace-scoped deploy target = %+v, bound=%v", target, bound)
			}
			if got := deployWorkloadIDs(job); strings.Join(got, ",") != "/api,/worker" {
				t.Fatalf("workload contracts = %v, want both workloads ordered by project id", got)
			}
			api := deployWorkloadOf(t, job, "/api")
			if api.ReleaseSet != provider.heads["canary"].Ref {
				t.Fatalf("api set = %+v, want the resolved head", api.ReleaseSet)
			}
			if got := memberCoordinates(api.Members); strings.Join(got, ",") != "oci putnami/api,put putnami/api-config" {
				t.Fatalf("api members = %v, want its image and its configuration", got)
			}
			worker := deployWorkloadOf(t, job, "/worker")
			if worker.ReleaseSet != provider.heads["canary"].Ref {
				t.Fatalf("worker set = %+v, want the resolved head", worker.ReleaseSet)
			}
			if got := memberCoordinates(worker.Members); strings.Join(got, ",") != "oci putnami/worker" {
				t.Fatalf("worker members = %v, want only its own image", got)
			}
			// Each entry owns its own rollout and constraints: a rule refines the
			// environment's defaults per workload, which is why neither can be
			// hoisted out of the entry.
			if testCase.apiRollout == "" {
				if api.Rollout != nil {
					t.Fatalf("api rollout = %+v, want the environment's absent default", api.Rollout)
				}
			} else if api.Rollout == nil || api.Rollout.Strategy != testCase.apiRollout {
				t.Fatalf("api rollout = %+v, want %q from its first matching rule", api.Rollout, testCase.apiRollout)
			}
			if len(api.Constraints) != 1 {
				t.Fatalf("api constraints = %+v, want the environment's", api.Constraints)
			}
			if len(worker.Constraints) != testCase.workerConstraints {
				t.Fatalf("worker constraints = %+v, want %d entries", worker.Constraints, testCase.workerConstraints)
			}
		})
	}
}

// TestSelectWorkloadsExcludesTheWorkspaceOnceProject pins the membership rule
// the issue's probe read wrong: the synthetic project a workspace-scoped task
// is planned for IS one of the plan's deploy projects, and it is a workload of
// nothing. It holds on both branches, and decisively on the no-rule one, where
// no selector is there to leave it out.
func TestSelectWorkloadsExcludesTheWorkspaceOnceProject(t *testing.T) {
	for _, document := range []string{deployNoRuleEnvironmentDocument, deployEnvironmentDocument} {
		ws := deployWorkspace(t, document)
		environment, err := loadEnvironment(ws.Root, "prod")
		if err != nil {
			t.Fatalf("load environment: %v", err)
		}
		planned := []*ScheduledJob{deployWorkspaceJobFor(ws, "/api", "/worker")}
		candidates := PlannedCommandProjects(planned, "deploy")
		if got := projectIDs(candidates); strings.Join(got, ",") != "/api,/worker,putnami" {
			t.Fatalf("planned deploy projects = %v, want the synthetic project among them", got)
		}
		workloads, err := selectWorkloads(ws, "prod", environment, candidates)
		if err != nil {
			t.Fatalf("select workloads: %v", err)
		}
		for _, workload := range workloads {
			if ws.ProjectByID(workload.project.ID) == nil {
				t.Fatalf("workload %q is not a project of this workspace", workload.project.ID)
			}
		}
		selected := make([]string, 0, len(workloads))
		for _, workload := range workloads {
			selected = append(selected, workload.project.ID)
		}
		if strings.Join(selected, ",") != "/api,/worker" {
			t.Fatalf("selected workloads = %v, want the two real ones", selected)
		}
	}
}

// TestMixedScopeDeployJobsBindTheirOwnWorkloads pins that both shapes coexist
// in one plan: the workspace-scoped job takes the workloads it selected, and a
// project-scoped job beside it keeps taking its own single workload.
func TestMixedScopeDeployJobsBindTheirOwnWorkloads(t *testing.T) {
	ws := deployWorkspace(t, deployNoRuleEnvironmentDocument)
	provider := &fakeReleaseSetProvider{heads: deployHead(t)}
	useReleaseSetProviderFake(t, provider)

	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	aggregate := deployWorkspaceJobFor(ws, "/api", "/worker")
	perProject := deployJobsFor(ws, "/web")
	planned := append([]*ScheduledJob{aggregate}, perProject...)
	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, false); err != nil {
		t.Fatalf("prepare deploy targets: %v", err)
	}
	if got := deployWorkloadIDs(aggregate); strings.Join(got, ",") != "/api,/worker" {
		t.Fatalf("workspace-scoped contracts = %v", got)
	}
	if got := deployWorkloadIDs(perProject[0]); strings.Join(got, ",") != "/web" {
		t.Fatalf("project-scoped contracts = %v, want only its own workload", got)
	}
	web := deployWorkloadOf(t, perProject[0], "/web")
	if got := memberCoordinates(web.Members); strings.Join(got, ",") != "oci putnami/web" {
		t.Fatalf("web members = %v", got)
	}
}

// TestSameSessionAggregateDeployTakesTheReleasedSet pins the release barrier
// over the aggregate job: it owns no member itself, so every contract it
// carries is completed against its own workload project.
func TestSameSessionAggregateDeployTakesTheReleasedSet(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	options := deployOptions("prod", "")
	options.WorkspaceRoot = ws.Root
	job := deployWorkspaceJobFor(ws, "/api", "/worker")
	planned := []*ScheduledJob{job}

	if err := PrepareDeployTargets(context.Background(), options, ws, providerDiscovery(), planned, true); err != nil {
		t.Fatalf("prepare same-session deploy targets: %v", err)
	}
	if job.JobDef.Cache {
		t.Fatal("a deploy completed by this session's own release is still cacheable")
	}
	head := deployHead(t)["canary"]
	if err := completeDeployTargets(planned, head.Ref, head.ReleaseSet.Members); err != nil {
		t.Fatalf("complete deploy targets: %v", err)
	}
	api := deployWorkloadOf(t, job, "/api")
	if api.ReleaseSet != head.Ref {
		t.Fatalf("completed api set = %+v, want the released set", api.ReleaseSet)
	}
	if got := memberCoordinates(api.Members); strings.Join(got, ",") != "oci putnami/api,put putnami/api-config" {
		t.Fatalf("completed api members = %v", got)
	}
	worker := deployWorkloadOf(t, job, "/worker")
	if worker.ReleaseSet != head.Ref || len(worker.Members) != 1 {
		t.Fatalf("completed worker contract = %+v", worker)
	}
}

// TestBindDeployTargetsRefusesAWorkloadNoDeployJobCovers keeps the fail-closed
// answer for a workload of the environment that no planned deploy task can
// synchronize — neither as its own project nor among the projects a
// workspace-scoped task selected.
func TestBindDeployTargetsRefusesAWorkloadNoDeployJobCovers(t *testing.T) {
	ws := deployWorkspace(t, deployEnvironmentDocument)
	targets := map[string]*DeployWorkloadTarget{
		"/api":    {Project: "/api"},
		"/worker": {Project: "/worker"},
	}
	planned := []*ScheduledJob{deployWorkspaceJobFor(ws, "/api")}
	err := bindDeployTargets(planned, "prod", targets, false)
	if err == nil || !strings.Contains(err.Error(), "environment workloads [/worker] have no deploy task in this plan") {
		t.Fatalf("bind with an uncovered workload = %v", err)
	}
}

// projectIDs renders a project list by id, in order.
func projectIDs(projects []*workspace.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids
}

// memberCoordinates renders a member list as "<ecosystem> <coordinate>" pairs.
func memberCoordinates(members []distribution.ReleaseSetMember) []string {
	rendered := make([]string, 0, len(members))
	for _, member := range members {
		rendered = append(rendered, string(member.Ecosystem)+" "+member.Coordinate)
	}
	return rendered
}
