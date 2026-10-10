package engine

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// archivePublisher is a planned publish job that reads (uploads) a project's
// package archives, the way an extension's archive upload job does.
func archivePublisher(projID string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projID},
		JobDef:    &extension.JobDefinition{Name: "publish~cloud-publish-archives", CommandName: "publish"},
		DependsOn: []string{projID + ":package~archives"},
	}
}

// namespacedArchivePublisher models the dep key form used when another
// extension drives the Go packager (e.g. the TypeScript extension):
// "<projID>:package~@putnami/go~archives".
func namespacedArchivePublisher(projID string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projID},
		JobDef:    &extension.JobDefinition{Name: "publish~cloud-publish-archives", CommandName: "publish"},
		DependsOn: []string{projID + ":package~@putnami/go~archives"},
	}
}

// templateArchivePublisher models the publish job scaffold templates get: the
// same cloud-publish-archives uploader, reading the scaffold packager's
// "package~template" step (published on the template-archives channel).
func templateArchivePublisher(projID string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projID},
		JobDef:    &extension.JobDefinition{Name: "publish~cloud-publish-archives", CommandName: "publish"},
		DependsOn: []string{projID + ":package~template"},
	}
}

// nonArchivePublisher is a planned publish job that does not touch archives.
func nonArchivePublisher(projID string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projID},
		JobDef:    &extension.JobDefinition{Name: "publish~publish-docker", CommandName: "publish"},
		DependsOn: []string{projID + ":build~cross-compile"},
	}
}

// archivePackage models the archive artifact node that survives planning only
// when the archives ecosystem is selected.
func archivePackage(projID string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project: &workspace.Project{ID: projID},
		JobDef:  &extension.JobDefinition{Name: "package~archives", CommandName: "package"},
	}
}

func archiveProject(id, name string) *workspace.Project {
	return &workspace.Project{ID: id, Name: name, Publish: []string{"archives"}}
}

// templateArchiveProject models a scaffold template: it declares archive
// publishing through options.publish.archives and publishes on the
// template-archives channel.
func templateArchiveProject(id, name string) *workspace.Project {
	return &workspace.Project{
		ID:      id,
		Name:    name,
		Publish: []string{"template-archives"},
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{"publish": {"archives": true}},
		},
	}
}

func TestUnpublishedArchiveProjects(t *testing.T) {
	t.Parallel()
	const id, name = "/tooling/cli", "@putnami/cli"
	cli := archiveProject(id, name)
	projects := []*workspace.Project{cli}

	tests := []struct {
		name     string
		commands []string
		params   extension.ParamMap
		planned  []*jobs.ScheduledJob
		want     []string
	}{
		{
			name:     "publish not selected — no warning",
			commands: []string{"package"},
			planned:  nil,
			want:     nil,
		},
		{
			name:     "declares archives and a publish job uploads them — healthy",
			commands: []string{"publish"},
			planned:  []*jobs.ScheduledJob{archivePublisher(id)},
			want:     nil,
		},
		{
			name:     "uploader uses the namespaced package~<ext>~archives key — healthy",
			commands: []string{"publish"},
			planned:  []*jobs.ScheduledJob{namespacedArchivePublisher(id)},
			want:     nil,
		},
		{
			name:     "archives disabled with equals syntax leaves only another ecosystem — ignored",
			commands: []string{"publish"},
			params:   extension.ParamMap{"archives": "false"},
			planned:  []*jobs.ScheduledJob{nonArchivePublisher("/other")},
			want:     nil,
		},
		{
			name:     "archives disabled with negative flag leaves only a non-archive publish — ignored",
			commands: []string{"publish"},
			params:   extension.ParamMap{"archives": false},
			planned:  []*jobs.ScheduledJob{nonArchivePublisher(id)},
			want:     nil,
		},
		{
			name:     "archive provider entirely absent without opt-out — orphaned",
			commands: []string{"publish"},
			planned:  []*jobs.ScheduledJob{nonArchivePublisher(id)},
			want:     []string{name},
		},
		{
			name:     "malformed archive opt-out does not weaken guard",
			commands: []string{"publish"},
			params:   extension.ParamMap{"archives": "FALSE"},
			planned:  []*jobs.ScheduledJob{nonArchivePublisher(id)},
			want:     []string{name},
		},
		{
			name:     "archive packaging selected but publish uploads nothing — orphaned",
			commands: []string{"publish"},
			planned:  []*jobs.ScheduledJob{archivePackage(id), nonArchivePublisher(id)},
			want:     []string{name},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unannounced := unpublishedArchiveProjects(tt.commands, tt.params, projects, tt.planned, nil)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
			// No release set coordinates this session (nil members), so the
			// release-set cause never applies: the report keeps the base wording.
			if unannounced != nil {
				t.Errorf("unannounced = %v, want nil without a release set", unannounced)
			}
		})
	}
}

// The Cloud managed-library boundary invokes publish with --archives=false
// (and archive project excludes), leaving npm/Go jobs in the final plan. The
// archive declarations on selected CLI projects must not leak past that plan
// boundary and fail the otherwise valid publication.
func TestUnpublishedArchiveProjects_DisabledArchivesManagedLibraryPlan(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		archiveProject("/surfaces/workloads/cli", "cli"),
		archiveProject("/surfaces/workloads/operator-cli", "operator-cli"),
		{ID: "/runtime/libs/runtime", Name: "@putnami/runtime", Publish: []string{"npm"}},
	}
	planned := []*jobs.ScheduledJob{nonArchivePublisher("/runtime/libs/runtime")}

	if got, _ := unpublishedArchiveProjects([]string{"publish"}, extension.ParamMap{"archives": "false"}, projects, planned, nil); got != nil {
		t.Fatalf("--archives=false plan must not trigger the archive guard, got %v", got)
	}
}

// A scaffold template declares archive publishing via options.publish.archives
// and is uploaded by cloud-publish-archives reading its "package~template" step.
// It must NOT be flagged as unpublished (regression for the false-positive
// warning on go-*/python-*/typescript-* templates).
func TestUnpublishedArchiveProjects_TemplateArchivesHealthy(t *testing.T) {
	t.Parallel()
	const id, name = "/go/templates/go-server", "go-server"
	tmpl := templateArchiveProject(id, name)
	got, _ := unpublishedArchiveProjects(
		[]string{"publish"},
		nil,
		[]*workspace.Project{tmpl},
		[]*jobs.ScheduledJob{templateArchivePublisher(id)},
		nil,
	)
	if got != nil {
		t.Errorf("template with a cloud-publish-archives uploader must not be flagged, got %v", got)
	}
}

func TestUnpublishedArchiveProjects_NonArchiveProjectIgnored(t *testing.T) {
	t.Parallel()
	// A project that does not declare archive publishing is never flagged,
	// even when publish uploads nothing for it.
	plain := &workspace.Project{ID: "/lib", Name: "@putnami/lib"}
	got, _ := unpublishedArchiveProjects([]string{"publish"}, nil, []*workspace.Project{plain}, nil, nil)
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// A release-set publish whose plan selects no member (every fingerprint matches
// the channel head) keeps every project selected for verification but plans no
// publish job at all. This is the shape of a lockfile-only commit on main: the
// run re-verifies everything and republishes nothing. The archive projects own
// release-set members, so the coordinator accounts for them and the guard must
// stay silent instead of failing the run.
func TestUnpublishedArchiveProjects_NoImpactReleaseSetIsHealthy(t *testing.T) {
	t.Parallel()
	cli := archiveProject("/tooling/cli", "@putnami/cli")
	tmpl := templateArchiveProject("/go/templates/go-server", "go-server")
	members := map[string]struct{}{cli.ID: {}, tmpl.ID: {}}
	verificationOnly := []*jobs.ScheduledJob{{
		Project: &workspace.Project{ID: cli.ID},
		JobDef:  &extension.JobDefinition{Name: "test~test", CommandName: "test"},
	}}

	got, unannounced := unpublishedArchiveProjects([]string{"test", "publish"}, nil, []*workspace.Project{cli, tmpl}, verificationOnly, members)
	if got != nil || unannounced != nil {
		t.Fatalf("no-impact release set must not report unpublished archives, got %v / %v", got, unannounced)
	}
}

// A release set covers only the projects that own its members. An archive
// project outside the plan — for example because no installed extension
// declared its archive member — has no coordinator behind it, so the guard
// still reports it.
func TestUnpublishedArchiveProjects_ReleaseSetNonMemberStillChecked(t *testing.T) {
	t.Parallel()
	member := archiveProject("/tooling/cli", "@putnami/cli")
	outsider := archiveProject("/tooling/other-cli", "other-cli")
	members := map[string]struct{}{member.ID: {}}

	got, unannounced := unpublishedArchiveProjects([]string{"publish"}, nil, []*workspace.Project{member, outsider}, nil, members)
	if want := []string{"other-cli"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if want := []string{"other-cli"}; !reflect.DeepEqual(unannounced, want) {
		t.Fatalf("unannounced = %v, want %v", unannounced, want)
	}
}

// A release-set publish narrows the selection to the owners of the
// plan's members (ScopePlanning), so a project that declares an archives
// publish and owns NO member is dropped from selectedProjects AND from the
// plan — the guard saw neither and the run published nothing for it while
// reporting success. The guard reads the selection as the session made it, so
// the drop is what it reports, and it names the cause.
func TestUnpublishedArchiveProjects_ReleaseSetScopingDropIsReported(t *testing.T) {
	t.Parallel()
	cli := archiveProject("/tooling/cli", "@putnami/cli")
	agent := archiveProject("/tooling/agent-workflows", "@putnami/agent-workflows")
	// Only the CLI owns a release-set member, so scoping keeps only the CLI and
	// the final plan carries only the CLI's uploader.
	members := map[string]struct{}{cli.ID: {}}
	planned := []*jobs.ScheduledJob{archivePublisher(cli.ID)}

	got, unannounced := unpublishedArchiveProjects(
		[]string{"publish"}, nil, []*workspace.Project{cli, agent}, planned, members)

	want := []string{"@putnami/agent-workflows"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v — a project dropped by release-set scoping must stay visible to the archive guard", got, want)
	}
	if !reflect.DeepEqual(unannounced, want) {
		t.Fatalf("unannounced = %v, want %v — the report must name the missing release-set member as the cause", unannounced, want)
	}
}

// The same project once an extension announces an archive release-set member
// for it: the coordinator owns its package and publish steps, so the guard
// stays silent even though scoping dropped it from this session's plan (its
// member's fingerprint matched the head).
func TestUnpublishedArchiveProjects_ReleaseSetMemberOwnerIsNotReported(t *testing.T) {
	t.Parallel()
	cli := archiveProject("/tooling/cli", "@putnami/cli")
	agent := archiveProject("/tooling/agent-workflows", "@putnami/agent-workflows")
	members := map[string]struct{}{cli.ID: {}, agent.ID: {}}
	planned := []*jobs.ScheduledJob{archivePublisher(cli.ID)}

	got, unannounced := unpublishedArchiveProjects(
		[]string{"publish"}, nil, []*workspace.Project{cli, agent}, planned, members)
	if got != nil || unannounced != nil {
		t.Fatalf("a release-set member owner is the coordinator's business, got %v / %v", got, unannounced)
	}
}

// --archives=false stays the one explicit opt-out, scoping or not: it removes
// the archive ecosystem on purpose and must not fail an otherwise valid
// npm/Go publication.
func TestUnpublishedArchiveProjects_ReleaseSetScopingDropRespectsOptOut(t *testing.T) {
	t.Parallel()
	cli := archiveProject("/tooling/cli", "@putnami/cli")
	agent := archiveProject("/tooling/agent-workflows", "@putnami/agent-workflows")
	members := map[string]struct{}{cli.ID: {}}

	for _, optOut := range []any{false, "false"} {
		got, unannounced := unpublishedArchiveProjects(
			[]string{"publish"}, extension.ParamMap{"archives": optOut},
			[]*workspace.Project{cli, agent}, nil, members)
		if got != nil || unannounced != nil {
			t.Fatalf("archives=%v opt-out must silence the guard, got %v / %v", optOut, got, unannounced)
		}
	}
}

func TestArchivePublishExitCode(t *testing.T) {
	t.Parallel()
	if got := archivePublishExitCode(ExitSuccess, []string{"@putnami/cli"}, true); got != ExitError {
		t.Errorf("missing archive uploader should fail publish: got %d, want %d", got, ExitError)
	}
	if got := archivePublishExitCode(ExitError, []string{"@putnami/cli"}, true); got != ExitError {
		t.Errorf("existing failure should be preserved: got %d, want %d", got, ExitError)
	}
	if got := archivePublishExitCode(ExitSuccess, []string{"@putnami/cli"}, false); got != ExitSuccess {
		t.Errorf("advisory archive warning should keep success: got %d, want %d", got, ExitSuccess)
	}
	if got := archivePublishExitCode(ExitSuccess, nil, true); got != ExitSuccess {
		t.Errorf("healthy publish should keep success: got %d, want %d", got, ExitSuccess)
	}
}

func TestUnpublishedArchiveFailureAddsResult(t *testing.T) {
	t.Parallel()
	results := map[string]*jobs.JobResult{
		"/tooling/cli:publish": {Status: "success"},
	}
	unpublishedArchiveFailure([]string{"@putnami/cli"}, nil, nil)(results)

	result := results[unpublishedArchiveFailureKey]
	if result == nil {
		t.Fatal("missing unpublished archive failure result")
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.Error == nil || !strings.Contains(result.Error.Message, "@putnami/cli") {
		t.Fatalf("error = %#v, want project name", result.Error)
	}
}

// TestUnpublishedArchiveFailureCitesSkippedExtension pins the root-cause
// chaining: when the archives guard fires and discovery skipped an
// extension that would have been the uploader, the failure names the skip and
// its reason instead of only the generic remedy.
func TestUnpublishedArchiveFailureCitesSkippedExtension(t *testing.T) {
	t.Parallel()
	skipped := []extension.SkippedExtension{{
		Ref:    "@putnami/cloud",
		Name:   "@putnami/cloud",
		Reason: errors.New("extension manifest x requires a newer putnami (contract 2 > 1)"),
	}}
	results := map[string]*jobs.JobResult{}
	unpublishedArchiveFailure([]string{"@putnami/cli"}, nil, skipped)(results)

	result := results[unpublishedArchiveFailureKey]
	if result == nil || result.Error == nil {
		t.Fatalf("missing unpublished archive failure result: %#v", result)
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

func TestArchiveDepProject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		dep    string
		want   string
		wantOK bool
	}{
		{"/tooling/cli:package~archives", "/tooling/cli", true},
		{"/typescript/extension:package~@putnami/go~archives", "/typescript/extension", true},
		{"/go/templates/go-server:package~template", "/go/templates/go-server", true},
		{"/tooling/contributor:package~agent-content", "/tooling/contributor", true},
		{"/tooling/contributor:package~@putnami/scaffold~agent-content", "/tooling/contributor", true},
		{"/go/templates/x:package~@putnami/scaffold~template", "/go/templates/x", true},
		{"/tooling/cli:build~cross-compile", "", false},
		{"/tooling/cli:package~cross-compile", "", false},
		{"no-colon", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.dep, func(t *testing.T) {
			got, ok := archiveDepProject(tt.dep)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("archiveDepProject(%q) = (%q, %v), want (%q, %v)", tt.dep, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestProjectDeclaresArchivePublish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		proj *workspace.Project
		want bool
	}{
		{"archives publish channel", &workspace.Project{Publish: []string{"archives"}}, true},
		{"options.publish.archives", &workspace.Project{Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{"publish": {"archives": true}},
		}}, true},
		{"options.package.archives only (build, not publish)", &workspace.Project{Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{"package": {"archives": true}},
		}}, false},
		{"options.publish.archives false", &workspace.Project{Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{"publish": {"archives": false}},
		}}, false},
		{"plain library", &workspace.Project{Publish: []string{"go"}}, false},
		{"nil config", &workspace.Project{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectDeclaresArchivePublish(tt.proj); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// The release-set failure must say WHY the archives have no uploader: the release-set
// plan announces no member for the project, so installing a publisher is not
// the fix — declaring the member is.
func TestUnpublishedArchiveFailureNamesMissingReleaseSetMember(t *testing.T) {
	t.Parallel()
	results := map[string]*jobs.JobResult{}
	projects := []string{"@putnami/agent-workflows", "@putnami/maintainer-workflows"}
	unpublishedArchiveFailure(projects, projects, nil)(results)

	result := results[unpublishedArchiveFailureKey]
	if result == nil || result.Error == nil {
		t.Fatalf("missing unpublished archive failure result: %#v", result)
	}
	for _, want := range []string{
		"@putnami/agent-workflows", "@putnami/maintainer-workflows",
		"no extension this run planned announces an archive release-set member",
	} {
		if !strings.Contains(result.Error.Message, want) {
			t.Errorf("error missing %q: %s", want, result.Error.Message)
		}
	}
}

// The advisory --plan warning carries the same cause line. It cannot run in
// parallel: captureStderr swaps the process-wide os.Stderr.
func TestReportUnpublishedArchivesNamesMissingReleaseSetMember(t *testing.T) {
	projects := []string{"@putnami/agent-workflows"}
	out := captureStderr(t, func() {
		reportUnpublishedArchives(projects, projects, nil, GlobalFlags{}, false)
	})
	for _, want := range []string{
		"warning", "@putnami/agent-workflows",
		"no extension this run planned announces an archive release-set member",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}

	// Without a release set there is no member to announce, so the base
	// wording stays exactly as it was.
	plain := captureStderr(t, func() {
		reportUnpublishedArchives(projects, nil, nil, GlobalFlags{}, false)
	})
	if strings.Contains(plain, "no extension this run planned announces an archive release-set member") {
		t.Errorf("the orphaned-archive report must not claim a missing release-set member:\n%s", plain)
	}
}

// The remedy names the capability the run lacks, never one extension: any
// extension whose publish step uploads release archives satisfies the guard.
// It cannot run in parallel: captureStderr swaps the process-wide os.Stderr.
func TestUnpublishedArchivesRemedyNamesNoExtension(t *testing.T) {
	const remedy = "install an extension whose publish step uploads release archives, and check that it is active"
	projects := []string{"example-cli"}
	results := map[string]*jobs.JobResult{}
	unpublishedArchiveFailure(projects, nil, nil)(results)
	failure := results[unpublishedArchiveFailureKey]
	if failure == nil || failure.Error == nil {
		t.Fatalf("missing unpublished archive failure result: %#v", failure)
	}
	report := captureStderr(t, func() {
		reportUnpublishedArchives(projects, nil, nil, GlobalFlags{}, false)
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

// TestArchiveGuardReadsTheSessionSelection pins the WIRING, not the guard's
// rule: engine.run must hand unpublishedArchiveProjects the selection the
// SESSION made, captured before a release-set publish narrows it to the
// owners of the plan's members. Handing it the narrowed list compiles, passes
// every guard unit test, and silently republishes the same hole — nothing
// else in this package can observe the difference, because driving it
// through Engine.Run needs a release-set provider process.
func TestArchiveGuardReadsTheSessionSelection(t *testing.T) {
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
			if name(node.Fun) == "unpublishedArchiveProjects" {
				call = node.Pos()
				if len(node.Args) > 2 {
					guardArg = name(node.Args[2])
				}
			}
		}
		return true
	})

	if !capture.IsValid() {
		t.Fatal("engine.run no longer captures `sessionProjects := selectedProjects` — the archives guard has lost the pre-scope selection")
	}
	if !narrow.IsValid() {
		t.Fatal("engine.run no longer narrows selectedProjects to preparation.Projects — move this pin with the release-set scoping seam")
	}
	if capture > narrow {
		t.Fatal("the session selection is captured AFTER release-set scoping narrowed it — it is then the narrowed list under another name")
	}
	if !call.IsValid() || call < narrow {
		t.Fatal("the archives guard no longer runs after release-set scoping — the plan it reads would not be the plan that executes")
	}
	if guardArg != "sessionProjects" {
		t.Fatalf("archives guard reads %q, want sessionProjects: a release-set publish drops every project that owns no member, and the guard would never see their missing uploader", guardArg)
	}
}
