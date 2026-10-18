package changeplan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestBuildImpactIsCanonical holds the projection to one encoding per plan:
// planner data that differs only in order and repetition emits the same bytes.
func TestBuildImpactIsCanonical(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "the-projection-is-canonical")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	first, err := BuildImpact(changePlanInput(app, lib, []string{"apps/app/main.go", "libs/lib/lib.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/lib:build", Key: "key-lib"}, {TaskKey: "/app:lint", Key: "key-app"}},
	}))
	if err != nil {
		t.Fatalf("BuildImpact first: %v", err)
	}
	second, err := BuildImpact(changePlanInput(app, lib, []string{"libs/lib/lib.go", "apps/app/main.go", "apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app"}, {TaskKey: "/lib:build", Key: "key-lib"}},
	}))
	if err != nil {
		t.Fatalf("BuildImpact second: %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("equivalent inputs emitted different documents:\n%s\n%s", firstJSON, secondJSON)
	}
	if err := ciproto.ValidateImpactPlan(first); err != nil {
		t.Fatalf("ValidateImpactPlan: %v", err)
	}
}

func TestBuildImpactDoesNotSerializeInvocationDetails(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	job := testJob(app, "lint~secret", 0)
	job.JobDef.Command = "private-command"
	job.JobDef.Args = []string{"--token", "super-secret-token"}
	job.JobDef.Cwd = "private-working-directory"
	job.JobDef.Env = map[string]string{"API_TOKEN": "super-secret-token"}
	document, err := BuildImpact(Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Commands:         []string{"lint"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     []string{"apps/app/main.go"},
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{app},
		Planned:          []*jobs.ScheduledJob{job},
		Cache:            ciproto.ChangePlanCache{Status: "disabled"},
	})
	if err != nil {
		t.Fatalf("BuildImpact: %v", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	for _, forbidden := range []string{"private-command", "super-secret-token", "private-working-directory", "API_TOKEN"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("document leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildImpactRejectsTaskWithoutHardDeadline(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	in.Planned = []*jobs.ScheduledJob{testJob(app, "lint~unbounded", -1)}
	if _, err := BuildImpact(in); err == nil || !strings.Contains(err.Error(), "no hard deadline") {
		t.Fatalf("BuildImpact error = %v, want hard-deadline rejection", err)
	}
}

// TestBuildImpactKeepsThePlannedCommandOrder holds the ImpactPlan to the
// planned command list in its order, and derives the transitive dependents
// from the impacted closure.
func TestBuildImpactKeepsThePlannedCommandOrder(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "the-projection-is-canonical")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	in := changePlanInput(app, lib, []string{"libs/lib/lib.go", "apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	impact, err := BuildImpact(in)
	if err != nil {
		t.Fatalf("BuildImpact: %v", err)
	}
	if err := ciproto.ValidateImpactPlan(impact); err != nil {
		t.Fatalf("ValidateImpactPlan: %v", err)
	}
	if strings.Join(impact.Commands, ",") != "lint,build" {
		t.Fatalf("commands = %v, want the planned order lint,build", impact.Commands)
	}
	if len(impact.Impact.TransitiveDependents) != 1 || impact.Impact.TransitiveDependents[0].ID != "/lib" {
		t.Fatalf("transitive dependents = %#v, want /lib", impact.Impact.TransitiveDependents)
	}
}

// TestBuildImpactRefusesWhatTheProtocolRefuses holds BuildImpact to
// ValidateImpactPlan: a command list the planner did not name, or names twice,
// is refused rather than emitted.
func TestBuildImpactRefusesWhatTheProtocolRefuses(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "an-invalid-plan-is-never-emitted")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	for _, commands := range [][]string{nil, {"lint", "lint"}, {"lint,test"}} {
		in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
		in.Commands = commands
		if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan commands are not canonical" {
			t.Fatalf("commands %q: BuildImpact error = %v, want the protocol's command refusal", commands, err)
		}
	}
	in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	in.BaseSHA = "abc"
	if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan revisions must be full lowercase commit IDs" {
		t.Fatalf("BuildImpact error = %v, want the protocol's revision refusal", err)
	}
	in.Planned = []*jobs.ScheduledJob{{Project: app}}
	if _, err := BuildImpact(in); err == nil || err.Error() != "impact plan contains incomplete planned task" {
		t.Fatalf("BuildImpact error = %v, want the projection's incomplete-task refusal", err)
	}
}

// TestProjectionRefusalsNameTheDocument holds every refusal of the projection
// to the document it was building: BuildImpact refuses as the impact plan,
// while ProjectTasks keeps the change plan's messages byte for byte.
func TestProjectionRefusalsNameTheDocument(t *testing.T) {
	t.Parallel()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	conflicting := &workspace.Project{ID: "/app", Name: "other", Path: "apps/app"}
	slower := testJob(app, "lint", 30_000)
	for _, tc := range []struct {
		name    string
		edit    func(*Input)
		message string
	}{
		{"incomplete project identity", func(in *Input) {
			in.DirectProjects = []*workspace.Project{{ID: "/app"}}
		}, "%s project identity is incomplete"},
		{"conflicting project identities", func(in *Input) {
			in.ImpactedProjects = []*workspace.Project{app, conflicting}
		}, `%s has conflicting project identities for "/app"`},
		{"incomplete planned task", func(in *Input) {
			in.Planned = []*jobs.ScheduledJob{{Project: app}}
		}, "%s contains incomplete planned task"},
		{"incomplete task identity", func(in *Input) {
			job := testJob(app, "lint", 0)
			job.Identity = &protocolcli.TaskIdentity{}
			in.Planned = []*jobs.ScheduledJob{job}
		}, "%s contains incomplete task identity"},
		{"conflicting tasks", func(in *Input) {
			in.Planned = []*jobs.ScheduledJob{testJob(app, "lint", 0), slower}
		}, `%s has conflicting tasks for "` + slower.TypedIdentity().Key + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
			tc.edit(&in)
			if _, err := BuildImpact(in); err == nil || err.Error() != fmt.Sprintf(tc.message, "impact plan") {
				t.Errorf("BuildImpact error = %v, want %q", err, fmt.Sprintf(tc.message, "impact plan"))
			}
			if !strings.Contains(tc.message, "task") {
				return
			}
			if _, err := ProjectTasks(in.Planned); err == nil || err.Error() != fmt.Sprintf(tc.message, "change plan") {
				t.Errorf("ProjectTasks error = %v, want %q", err, fmt.Sprintf(tc.message, "change plan"))
			}
		})
	}
}

func changePlanInput(app, lib *workspace.Project, changed []string, cache ciproto.ChangePlanCache) Input {
	return Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Commands:         []string{"lint", "build"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     changed,
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{lib, app},
		Planned:          []*jobs.ScheduledJob{testJob(lib, "build", 12_000), testJob(app, "lint", 0)},
		Cache:            cache,
	}
}

func testJob(project *workspace.Project, name string, timeout int) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.0.0"},
		JobDef: &extension.JobDefinition{
			Name:           name,
			CommandName:    strings.Split(name, "~")[0],
			TimeoutMs:      timeout,
			ContractDigest: "tc1:test-contract",
			Reads:          []extension.ResourceRef{{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}},
		},
	}
}
