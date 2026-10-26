package engine

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// registryTraits are the resolved traits of a registry publication.
var registryTraits = extension.CommandTraits{SideEffects: extension.SideEffectsRegistry}

// channelPublisher is a planned publish job of a project whose task declares
// the given publish channels, the way a publishing extension's own job does.
// Its dependency is the project's own package step, as in a real plan. With
// no channel, its task declares none, as Cloud's configuration upload does.
func channelPublisher(projID string, channels ...string) *jobs.ScheduledJob {
	declaration := &extension.TaskDeclaration{Effects: []string{extension.EffectRegistry}, PublishChannels: channels}
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projID},
		JobDef:    &extension.JobDefinition{Name: "publish~upload", CommandName: "publish", Traits: registryTraits},
		Step:      &extension.PipelineStep{ID: "upload", Task: "upload"},
		Extension: &extension.ExtensionDescription{Tasks: map[string]extension.TaskDefinition{"upload": {Declares: declaration}}},
		DependsOn: []string{projID + ":package~bundle"},
	}
}

// publisherOf is a publish job of one project that lists a job of another
// project in DependsOn: the edge that serves the other project the channels
// its task declares.
func publisherOf(ownerID, servedID string, channels ...string) *jobs.ScheduledJob {
	job := channelPublisher(ownerID, channels...)
	job.DependsOn = []string{servedID + ":package~bundle"}
	return job
}

func publishingProject(id, name string, channels ...string) *workspace.Project {
	return &workspace.Project{ID: id, Name: name, Publish: channels}
}

func unservedNames(unserved []unservedProject) []string {
	var names []string
	for _, u := range unserved {
		names = append(names, u.name+":"+strings.Join(u.channels, ","))
	}
	return names
}

// guardCase is one project, the run's command parameters and its plan, and the
// unserved channels the guard reports, as "<name>:<channel>,<channel>".
type guardCase struct {
	name      string
	project   *workspace.Project
	publishes bool
	params    extension.ParamMap
	planned   []*jobs.ScheduledJob
	want      []string
}

func runGuardCases(t *testing.T, tests []guardCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unannounced := unpublishedProjects(tt.publishes, tt.params, []*workspace.Project{tt.project}, tt.planned, nil)
			if names := unservedNames(got); !reflect.DeepEqual(names, tt.want) {
				t.Errorf("got %v, want %v", names, tt.want)
			}
			// No release set coordinates this session (nil members), so the
			// release-set cause never applies: the report keeps the base wording.
			if unannounced != nil {
				t.Errorf("unannounced = %v, want nil without a release set", unannounced)
			}
		})
	}
}

const guardID, guardName = "/tooling/cli", "@putnami/cli"

func TestUnpublishedProjects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-run-checks-declared-channels", "a-declared-channel-no-publish-step-serves-fails-the-run")
	cli := publishingProject(guardID, guardName, "archives")
	multi := publishingProject(guardID, guardName, "archives", "npm")
	plain := publishingProject(guardID, guardName)
	optionOnly := &workspace.Project{ID: guardID, Name: guardName, Config: &wsproto.ProjectConfig{
		Options: map[string]map[string]any{"publish": {"archives": true}},
	}}

	runGuardCases(t, []guardCase{
		{name: "publish not selected — no failure", project: cli},
		{name: "the project's own job serves the channel its task declares", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher(guardID, "archives")}},
		{name: "no publish job at all", project: cli, publishes: true, want: []string{guardName + ":archives"}},
		{name: "a job of an unrelated project does not serve it", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher("/other", "archives")}, want: []string{guardName + ":archives"}},
		{name: "every declared channel is named", project: multi, publishes: true, want: []string{guardName + ":archives,npm"}},
		{name: "--<channel>=false opts the channel out", project: cli, publishes: true, params: extension.ParamMap{"archives": "false"}},
		{name: "--no-<channel> opts the channel out", project: cli, publishes: true, params: extension.ParamMap{"archives": false}},
		{name: "a malformed opt-out does not weaken the guard", project: cli, publishes: true,
			params: extension.ParamMap{"archives": "FALSE"}, want: []string{guardName + ":archives"}},
		{name: "a true flag is not an opt-out", project: cli, publishes: true,
			params: extension.ParamMap{"archives": true}, want: []string{guardName + ":archives"}},
		{name: "an opt-out of one channel leaves the other checked", project: multi, publishes: true,
			params: extension.ParamMap{"archives": false}, want: []string{guardName + ":npm"}},
		{name: "an opt-out of every declared channel skips the project", project: multi, publishes: true,
			params: extension.ParamMap{"archives": false, "npm": "false"}},
		{name: "an opt-out names the parameter of a declared channel only", project: cli, publishes: true,
			params: extension.ParamMap{"npm": false}, want: []string{guardName + ":archives"}},
		{name: "a project that declares no channel is never checked", project: plain, publishes: true},
		{name: "options.publish.archives without a declared channel is not read", project: optionOnly, publishes: true},
	})
}

// A job serves the channels its task declares and no other, whatever its
// command's name or traits: a project that declares two channels and has a
// publish job for one has the other reported, and a registry publication whose
// task declares no channel serves nothing.
func TestUnpublishedProjects_AJobServesOnlyTheChannelsItsTaskDeclares(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-run-checks-declared-channels", "a-job-serves-only-the-channels-its-task-declares")
	cli := publishingProject(guardID, guardName, "archives")
	mixed := publishingProject(guardID, guardName, "archives", "docker")
	untraced := channelPublisher(guardID, "archives")
	untraced.JobDef.Name, untraced.JobDef.CommandName, untraced.JobDef.Traits = "ship~upload", "ship", extension.CommandTraits{}
	undeclared := channelPublisher(guardID, "archives")
	undeclared.Step = nil

	runGuardCases(t, []guardCase{
		{name: "a publish job of one channel leaves the other unserved", project: mixed, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher(guardID, "docker")}, want: []string{guardName + ":archives"}},
		{name: "two jobs serve the two channels their tasks declare", project: mixed, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher(guardID, "docker"), channelPublisher(guardID, "archives")}},
		{name: "one job serves every channel its task declares", project: mixed, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher(guardID, "archives", "docker")}},
		{name: "a registry job whose task declares no channel serves nothing", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{channelPublisher(guardID)}, want: []string{guardName + ":archives"}},
		{name: "a registry job without a task declaration serves nothing", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{undeclared}, want: []string{guardName + ":archives"}},
		{name: "a job of another command without the registry trait serves the channels its task declares",
			project: cli, publishes: true, planned: []*jobs.ScheduledJob{untraced}},
		{name: "a served channel and an opted-out channel leave nothing to report", project: mixed, publishes: true,
			params: extension.ParamMap{"archives": false}, planned: []*jobs.ScheduledJob{channelPublisher(guardID, "docker")}},
	})
}

// A job of another project that depends on one of the project's jobs serves
// the project the channels its task declares, and only those, and a job that
// depends on a third project serves only that one.
func TestUnpublishedProjects_ADependentJobServesTheChannelsItsTaskDeclares(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-run-checks-declared-channels", "a-dependent-job-of-another-project-serves-the-channels-its-task-declares")
	const publisher = "/tooling/publisher"
	cli := publishingProject(guardID, guardName, "archives")
	multi := publishingProject(guardID, guardName, "archives", "npm")

	runGuardCases(t, []guardCase{
		{name: "a dependent job that declares the channel serves it", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{publisherOf(publisher, guardID, "archives")}},
		{name: "a dependent job serves only the channels its task declares", project: multi, publishes: true,
			planned: []*jobs.ScheduledJob{publisherOf(publisher, guardID, "npm")}, want: []string{guardName + ":archives"}},
		{name: "a dependent job whose task declares no channel serves nothing", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{publisherOf(publisher, guardID)}, want: []string{guardName + ":archives"}},
		{name: "a job that depends on a third project serves only the third", project: cli, publishes: true,
			planned: []*jobs.ScheduledJob{publisherOf(publisher, "/tooling/other", "archives")}, want: []string{guardName + ":archives"}},
	})
}

// cloudPublishPlan plans `publish` for a project that declares the given
// channels, the way a run does: the real manifest of the Cloud extension,
// loaded from this repository, planned by the engine's planner with the
// project's declared channels injected. Without Cloud the workspace holds no
// extension that publishes.
func cloudPublishPlan(t *testing.T, withCloud bool, channels ...string) (*workspace.Project, []*jobs.ScheduledJob) {
	t.Helper()
	cli := &workspace.Project{ID: guardID, Name: guardName, Path: "tooling/cli", Publish: channels, Config: &wsproto.ProjectConfig{}}
	root := filepath.Join(engineModuleRoot(t), "..", "..")
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{cli})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	var exts []*extension.ExtensionDescription
	if withCloud {
		exts = append(exts, extension.LoadExtensionFromDir(filepath.Join(root, "cloud", "extension"), "@putnami/cloud"))
	}
	planned, err := planner.Plan(ws, []string{"publish"}, ws.Projects, exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan publish: %v", err)
	}
	return cli, planned
}

// Cloud's cloud-publish-archives step is planned as the project's OWN job —
// publish~cloud-publish-archives of the project it publishes — and its task
// declares the archives channel. The real manifest is planned, so the guard is
// proven against the plan shape a run holds: a workspace with Cloud active
// serves the project, a workspace without it does not. Cloud also plans its
// configuration, migration and deployment uploads for the project, and their
// tasks declare no channel, so a docker channel stays unserved beside it.
func TestUnpublishedProjects_CloudArchivePublishIsTheProjectsOwnJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-run-checks-declared-channels", "cloud-archive-publication-serves-the-declaring-project")

	cli, planned := cloudPublishPlan(t, true, "archives")
	var upload *jobs.ScheduledJob
	others := 0
	for _, j := range planned {
		switch {
		case j.Key() == cli.ID+":publish~cloud-publish-archives":
			upload = j
		case j.JobDef != nil && j.JobDef.CommandName == "publish":
			others++
		}
	}
	if upload == nil {
		t.Fatalf("plan holds no %s:publish~cloud-publish-archives, got %v", cli.ID, sortedJobKeys(planned))
	}
	if declaration := jobs.TaskDeclarationOf(upload); declaration == nil || !slices.Contains(declaration.PublishChannels, "archives") || upload.Project.ID != cli.ID {
		t.Fatalf("the archive upload is %+v, want the project's own job whose task declares archives", upload)
	}
	if others == 0 {
		t.Fatalf("plan holds no other Cloud publish job, got %v", sortedJobKeys(planned))
	}
	if got, _ := unpublishedProjects(true, nil, []*workspace.Project{cli}, planned, nil); got != nil {
		t.Errorf("Cloud active: got %v, want the project served", unservedNames(got))
	}

	cli, planned = cloudPublishPlan(t, true, "archives", "docker")
	got, _ := unpublishedProjects(true, nil, []*workspace.Project{cli}, planned, nil)
	if want := []string{guardName + ":docker"}; !reflect.DeepEqual(unservedNames(got), want) {
		t.Errorf("Cloud active, docker declared: got %v, want %v", unservedNames(got), want)
	}

	cli, planned = cloudPublishPlan(t, false, "archives")
	got, _ = unpublishedProjects(true, nil, []*workspace.Project{cli}, planned, nil)
	if want := []string{guardName + ":archives"}; !reflect.DeepEqual(unservedNames(got), want) {
		t.Errorf("Cloud absent: got %v, want %v", unservedNames(got), want)
	}
}

// A release-set publish whose plan selects no member (every fingerprint matches
// the channel head) keeps every project selected for verification but plans no
// publish job at all. This is the shape of a lockfile-only commit on main: the
// run re-verifies everything and republishes nothing. The declaring projects
// own release-set members, so the coordinator accounts for them and the guard
// must stay silent instead of failing the run.
func TestUnpublishedProjects_NoImpactReleaseSetIsHealthy(t *testing.T) {
	t.Parallel()
	cli := publishingProject("/tooling/cli", "@putnami/cli", "archives")
	tmpl := publishingProject("/go/templates/go-server", "go-server", "template-archives")
	members := map[string]struct{}{cli.ID: {}, tmpl.ID: {}}
	verificationOnly := []*jobs.ScheduledJob{{
		Project: &workspace.Project{ID: cli.ID},
		JobDef:  &extension.JobDefinition{Name: "test~test", CommandName: "test"},
	}}

	got, unannounced := unpublishedProjects(true, nil, []*workspace.Project{cli, tmpl}, verificationOnly, members)
	if got != nil || unannounced != nil {
		t.Fatalf("no-impact release set must not report unpublished channels, got %v / %v", unservedNames(got), unannounced)
	}
}

// A release set covers only the projects that own its members. A declaring
// project outside the plan — for example because no installed extension
// declared its member — has no coordinator behind it, so the guard still
// reports it.
func TestUnpublishedProjects_ReleaseSetNonMemberStillChecked(t *testing.T) {
	t.Parallel()
	member := publishingProject("/tooling/cli", "@putnami/cli", "archives")
	outsider := publishingProject("/tooling/other-cli", "other-cli", "archives")
	members := map[string]struct{}{member.ID: {}}

	got, unannounced := unpublishedProjects(true, nil, []*workspace.Project{member, outsider}, nil, members)
	if want := []string{"other-cli:archives"}; !reflect.DeepEqual(unservedNames(got), want) {
		t.Fatalf("got %v, want %v", unservedNames(got), want)
	}
	if want := []string{"other-cli"}; !reflect.DeepEqual(unannounced, want) {
		t.Fatalf("unannounced = %v, want %v", unannounced, want)
	}
}

// A release-set publish narrows the selection to the owners of the
// plan's members (ScopePlanning), so a project that declares a publish
// channel and owns NO member is dropped from selectedProjects AND from the
// plan — the guard saw neither and the run published nothing for it while
// reporting success. The guard reads the selection as the session made it, so
// the drop is what it reports, and it names the cause.
func TestUnpublishedProjects_ReleaseSetScopingDropIsReported(t *testing.T) {
	t.Parallel()
	cli := publishingProject("/tooling/cli", "@putnami/cli", "archives")
	agent := publishingProject("/tooling/agent-workflows", "@putnami/agent-workflows", "archives")
	// Only the CLI owns a release-set member, so scoping keeps only the CLI and
	// the final plan carries only the CLI's publisher.
	members := map[string]struct{}{cli.ID: {}}
	planned := []*jobs.ScheduledJob{channelPublisher(cli.ID, "archives")}

	got, unannounced := unpublishedProjects(
		true, nil, []*workspace.Project{cli, agent}, planned, members)

	want := []string{"@putnami/agent-workflows"}
	if names := unservedNames(got); !reflect.DeepEqual(names, []string{"@putnami/agent-workflows:archives"}) {
		t.Fatalf("got %v, want %v — a project dropped by release-set scoping must stay visible to the publication guard", names, want)
	}
	if !reflect.DeepEqual(unannounced, want) {
		t.Fatalf("unannounced = %v, want %v — the report must name the missing release-set member as the cause", unannounced, want)
	}
}

// The same project once an extension announces a release-set member for it:
// the coordinator owns its package and publish steps, so the guard stays silent
// even though scoping dropped it from this session's plan (its member's
// fingerprint matched the head).
func TestUnpublishedProjects_ReleaseSetMemberOwnerIsNotReported(t *testing.T) {
	t.Parallel()
	cli := publishingProject("/tooling/cli", "@putnami/cli", "archives")
	agent := publishingProject("/tooling/agent-workflows", "@putnami/agent-workflows", "archives")
	members := map[string]struct{}{cli.ID: {}, agent.ID: {}}
	planned := []*jobs.ScheduledJob{channelPublisher(cli.ID, "archives")}

	got, unannounced := unpublishedProjects(
		true, nil, []*workspace.Project{cli, agent}, planned, members)
	if got != nil || unannounced != nil {
		t.Fatalf("a release-set member owner is the coordinator's business, got %v / %v", unservedNames(got), unannounced)
	}
}

// An explicit opt-out stays the one way out, scoping or not: it removes the
// channel on purpose and must not fail an otherwise valid publication.
func TestUnpublishedProjects_ReleaseSetScopingDropRespectsOptOut(t *testing.T) {
	t.Parallel()
	cli := publishingProject("/tooling/cli", "@putnami/cli", "archives")
	agent := publishingProject("/tooling/agent-workflows", "@putnami/agent-workflows", "archives")
	members := map[string]struct{}{cli.ID: {}}

	for _, optOut := range []any{false, "false"} {
		got, unannounced := unpublishedProjects(
			true, extension.ParamMap{"archives": optOut},
			[]*workspace.Project{cli, agent}, nil, members)
		if got != nil || unannounced != nil {
			t.Fatalf("archives=%v opt-out must silence the guard, got %v / %v", optOut, unservedNames(got), unannounced)
		}
	}
}

func TestPublicationGuardExitCode(t *testing.T) {
	t.Parallel()
	unserved := []unservedProject{{name: "@putnami/cli", channels: []string{"archives"}}}
	if got := publicationGuardExitCode(ExitSuccess, unserved, true); got != ExitError {
		t.Errorf("an unserved channel should fail publish: got %d, want %d", got, ExitError)
	}
	if got := publicationGuardExitCode(ExitError, unserved, true); got != ExitError {
		t.Errorf("existing failure should be preserved: got %d, want %d", got, ExitError)
	}
	if got := publicationGuardExitCode(ExitSuccess, unserved, false); got != ExitSuccess {
		t.Errorf("advisory warning should keep success: got %d, want %d", got, ExitSuccess)
	}
	if got := publicationGuardExitCode(ExitSuccess, nil, true); got != ExitSuccess {
		t.Errorf("healthy publish should keep success: got %d, want %d", got, ExitSuccess)
	}
}

func TestUnpublishedFailureAddsResult(t *testing.T) {
	t.Parallel()
	results := map[string]*jobs.JobResult{
		"/tooling/cli:publish": {Status: "success"},
	}
	unpublishedFailure([]unservedProject{{name: "@putnami/cli", channels: []string{"archives"}}}, nil, nil)(results)

	result := results[unpublishedFailureKey]
	if result == nil {
		t.Fatal("missing unpublished channels failure result")
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	const want = "@putnami/cli: no planned publish step serves its declared publish channel archives"
	if result.Error == nil || !strings.Contains(result.Error.Message, want) {
		t.Fatalf("error = %#v, want %q", result.Error, want)
	}
}

// TestUnpublishedFailureCitesSkippedExtension pins the root-cause chaining:
// when the guard fires and discovery skipped an extension that would have been
// the publisher, the failure names the skip and its reason instead of only the
// generic remedy.
func TestUnpublishedFailureCitesSkippedExtension(t *testing.T) {
	t.Parallel()
	skipped := []extension.SkippedExtension{{
		Ref:    "@putnami/cloud",
		Name:   "@putnami/cloud",
		Reason: errors.New("extension manifest x requires a newer putnami (contract 2 > 1)"),
	}}
	results := map[string]*jobs.JobResult{}
	unpublishedFailure([]unservedProject{{name: "@putnami/cli", channels: []string{"archives"}}}, nil, skipped)(results)

	result := results[unpublishedFailureKey]
	if result == nil || result.Error == nil {
		t.Fatalf("missing unpublished channels failure result: %#v", result)
	}
	for _, want := range []string{"@putnami/cloud was skipped", "requires a newer putnami", "putnami upgrade"} {
		if !strings.Contains(result.Error.Message, want) {
			t.Errorf("error missing %q: %s", want, result.Error.Message)
		}
	}
}

func TestSkippedExtensionsNote_EmptyWithoutSkips(t *testing.T) {
	t.Parallel()
	if note := skippedExtensionsNote(nil); note != "" {
		t.Fatalf("no skips must produce no note (absent-provider messages stay unchanged), got %q", note)
	}
}

// The release-set failure must say WHY the channels have no publish step: the
// release-set plan announces no member for the project, so installing a
// publisher is not the fix — declaring the member is.
func TestUnpublishedFailureNamesMissingReleaseSetMember(t *testing.T) {
	t.Parallel()
	results := map[string]*jobs.JobResult{}
	names := []string{"@putnami/agent-workflows", "@putnami/maintainer-workflows"}
	unserved := []unservedProject{
		{name: names[0], channels: []string{"archives"}},
		{name: names[1], channels: []string{"archives", "npm"}},
	}
	unpublishedFailure(unserved, names, nil)(results)

	result := results[unpublishedFailureKey]
	if result == nil || result.Error == nil {
		t.Fatalf("missing unpublished channels failure result: %#v", result)
	}
	for _, want := range []string{
		"@putnami/agent-workflows: no planned publish step serves its declared publish channel archives;",
		"@putnami/maintainer-workflows: no planned publish step serves its declared publish channels archives, npm;",
		"no extension this run planned announces a release-set member",
	} {
		if !strings.Contains(result.Error.Message, want) {
			t.Errorf("error missing %q: %s", want, result.Error.Message)
		}
	}
}

// The advisory --plan warning carries the same cause line. It cannot run in
// parallel: captureStderr swaps the process-wide os.Stderr.
func TestReportUnpublishedProjectsNamesMissingReleaseSetMember(t *testing.T) {
	unserved := []unservedProject{{name: "@putnami/agent-workflows", channels: []string{"archives"}}}
	names := []string{"@putnami/agent-workflows"}
	out := captureStderr(t, func() {
		reportUnpublishedProjects(unserved, names, nil, GlobalFlags{}, false)
	})
	for _, want := range []string{
		"warning", "@putnami/agent-workflows: no planned publish step serves its declared publish channel archives",
		"no extension this run planned announces a release-set member",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}

	// Without a release set there is no member to announce, so the base
	// wording stays exactly as it was.
	plain := captureStderr(t, func() {
		reportUnpublishedProjects(unserved, nil, nil, GlobalFlags{}, false)
	})
	if strings.Contains(plain, "announces a release-set member") {
		t.Errorf("the unserved-channel report must not claim a missing release-set member:\n%s", plain)
	}
}

// The remedy names the capability the run lacks, never one extension: any
// extension whose publish step serves the channel satisfies the guard. It
// cannot run in parallel: captureStderr swaps the process-wide os.Stderr.
func TestUnpublishedRemedyNamesNoExtension(t *testing.T) {
	const remedy = "install the extension that publishes each channel, and check that it is active"
	unserved := []unservedProject{{name: "example-cli", channels: []string{"archives"}}}
	results := map[string]*jobs.JobResult{}
	unpublishedFailure(unserved, nil, nil)(results)
	failure := results[unpublishedFailureKey]
	if failure == nil || failure.Error == nil {
		t.Fatalf("missing unpublished channels failure result: %#v", failure)
	}
	report := captureStderr(t, func() {
		reportUnpublishedProjects(unserved, nil, nil, GlobalFlags{}, false)
	})
	for source, text := range map[string]string{"failure": failure.Error.Message, "report": report} {
		if !strings.Contains(text, remedy) {
			t.Errorf("%s does not state the remedy %q:\n%s", source, remedy, text)
		}
		if strings.Contains(text, "@putnami/") || strings.Contains(strings.ToLower(text), "cloud") {
			t.Errorf("%s names a specific extension or service:\n%s", source, text)
		}
	}
}

// TestPublicationGuardReadsTheSessionSelection pins the WIRING, not the guard's
// rule: engine.run must hand unpublishedProjects the selection the
// SESSION made, captured before a release-set publish narrows it to the
// owners of the plan's members. Handing it the narrowed list compiles, passes
// every guard unit test, and silently republishes the same hole — nothing
// else in this package can observe the difference, because driving it
// through Engine.Run needs a release-set provider process.
func TestPublicationGuardReadsTheSessionSelection(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}
	var name func(ast.Expr) string
	name = func(e ast.Expr) string {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		if sel, ok := e.(*ast.SelectorExpr); ok {
			return name(sel.X) + "." + sel.Sel.Name
		}
		return ""
	}

	var capture, narrow, call token.Pos
	var guardArg string
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for i := range node.Lhs {
				// sessionProjects := selectedProjects, wherever it rides.
				if name(node.Lhs[i]) == "sessionProjects" && name(node.Rhs[i]) == "selectedProjects" {
					capture = node.Pos()
				}
				// selectedProjects = preparation.Projects: the narrowing itself.
				if name(node.Lhs[i]) == "selectedProjects" && name(node.Rhs[i]) == "preparation.Projects" {
					narrow = node.Pos()
				}
			}
		case *ast.CallExpr:
			if name(node.Fun) == "unpublishedProjects" {
				call = node.Pos()
				if len(node.Args) > 2 {
					guardArg = name(node.Args[2])
				}
			}
		}
		return true
	})

	if !capture.IsValid() {
		t.Fatal("engine.run no longer captures `sessionProjects := selectedProjects` — the publication guard has lost the pre-scope selection")
	}
	if !narrow.IsValid() {
		t.Fatal("engine.run no longer narrows selectedProjects to preparation.Projects — move this pin with the release-set scoping seam")
	}
	if capture > narrow {
		t.Fatal("the session selection is captured AFTER release-set scoping narrowed it — it is then the narrowed list under another name")
	}
	if !call.IsValid() || call < narrow {
		t.Fatal("the publication guard no longer runs after release-set scoping — the plan it reads would not be the plan that executes")
	}
	if guardArg != "sessionProjects" {
		t.Fatalf("publication guard reads %q, want sessionProjects: a release-set publish drops every project that owns no member, and the guard would never see their missing publish step", guardArg)
	}
}
